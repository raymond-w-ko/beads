package doctor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	mrand "math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage/dolt"
	"github.com/steveyegge/beads/internal/testutil"
)

func TestCheckDatabaseIntegrity(t *testing.T) {
	tests := []struct {
		name           string
		setup          func(t *testing.T, dir string)
		expectedStatus string
		expectMessage  string
	}{
		{
			name: "no database",
			setup: func(t *testing.T, dir string) {
				// No database directory created
			},
			expectedStatus: "ok",
			expectMessage:  "N/A (no database)",
		},
		{
			name: "empty beads dir",
			setup: func(t *testing.T, dir string) {
				// .beads exists but no dolt/ directory
			},
			expectedStatus: "ok",
			expectMessage:  "N/A (no database)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			beadsDir := filepath.Join(tmpDir, ".beads")
			if err := os.MkdirAll(beadsDir, 0755); err != nil {
				t.Fatal(err)
			}

			tt.setup(t, tmpDir)

			check := CheckDatabaseIntegrity(tmpDir)

			if check.Status != tt.expectedStatus {
				t.Errorf("expected status %q, got %q", tt.expectedStatus, check.Status)
			}
			if tt.expectMessage != "" && check.Message != tt.expectMessage {
				t.Errorf("expected message %q, got %q", tt.expectMessage, check.Message)
			}
		})
	}
}

func TestServerModeIntegrityManualRecoveryDetail(t *testing.T) {
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	doltDir := filepath.Join(beadsDir, "dolt")
	if err := os.MkdirAll(doltDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &configfile.Config{
		Backend:        configfile.BackendDolt,
		DoltMode:       configfile.DoltModeServer,
		DoltServerHost: "127.0.0.1",
		DoltServerPort: 1,
		DoltDatabase:   "doctest_missing_server_db",
	}
	if err := cfg.Save(beadsDir); err != nil {
		t.Fatalf("failed to save config: %v", err)
	}

	detail := serverModeIntegrityManualRecoveryDetail(beadsDir)
	if detail == "" {
		t.Fatal("expected server-mode manual recovery detail")
	}
	if !strings.Contains(detail, "Automatic integrity recovery is disabled for server-mode repos") {
		t.Fatalf("detail missing manual recovery guidance:\n%s", detail)
	}
	if !strings.Contains(detail, "doctest_missing_server_db") {
		t.Fatalf("detail missing configured database name:\n%s", detail)
	}
	if !strings.Contains(detail, doltDir) {
		t.Fatalf("detail missing dolt root path:\n%s", detail)
	}
}

func TestCheckDatabaseVersion(t *testing.T) {
	tests := []struct {
		name           string
		setup          func(t *testing.T, dir string)
		expectedStatus string
	}{
		{
			name: "no database",
			setup: func(t *testing.T, dir string) {
				// No dolt/ directory - error (need to run bd init)
			},
			expectedStatus: "error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			beadsDir := filepath.Join(tmpDir, ".beads")
			if err := os.MkdirAll(beadsDir, 0755); err != nil {
				t.Fatal(err)
			}

			tt.setup(t, tmpDir)

			check := CheckDatabaseVersion(tmpDir, "0.1.0")

			if check.Status != tt.expectedStatus {
				t.Errorf("expected status %q, got %q (message: %s)", tt.expectedStatus, check.Status, check.Message)
			}
		})
	}
}

func TestCheckDatabaseVersion_BareParentWorktreeFallback(t *testing.T) {
	bareDir, worktreeDir := setupDoctorBareParentWorktree(t)
	bareBeadsDir := filepath.Join(bareDir, ".beads")
	bareDoltDir := filepath.Join(bareBeadsDir, "dolt")
	if err := os.MkdirAll(bareDoltDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bareBeadsDir, "config.yaml"), []byte("dolt:\n  shared-server: true\n"), 0644); err != nil {
		t.Fatal(err)
	}

	check := CheckDatabaseVersion(worktreeDir, "0.1.0")
	if check.Message == "No dolt database found" {
		t.Fatalf("CheckDatabaseVersion() should resolve bare-parent fallback, got message %q", check.Message)
	}
}

func TestCheckSchemaCompatibility(t *testing.T) {
	tests := []struct {
		name           string
		setup          func(t *testing.T, dir string)
		expectedStatus string
	}{
		{
			name: "no database",
			setup: func(t *testing.T, dir string) {
				// No database created
			},
			expectedStatus: "ok",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			beadsDir := filepath.Join(tmpDir, ".beads")
			if err := os.MkdirAll(beadsDir, 0755); err != nil {
				t.Fatal(err)
			}

			tt.setup(t, tmpDir)

			check := CheckSchemaCompatibility(tmpDir)

			if check.Status != tt.expectedStatus {
				t.Errorf("expected status %q, got %q (message: %s)", tt.expectedStatus, check.Status, check.Message)
			}
		})
	}
}

// TestCheckSchemaCompatibilityProbeCountsNothing pins the statements the
// schema check sends: the statistics query's comparisons on at most one
// issues row, then the plan, not the rows, of the blocked count's filter, and
// no statement that counts issues (bd-v5qbz, bd-pbioz, bd-b4n1i). The mock
// fails the check on any statement it was not told to expect.
func TestCheckSchemaCompatibilityProbeCountsNothing(t *testing.T) {
	want := []string{
		"SELECT status = 'open', status = 'in_progress', status = 'closed', status = 'deferred', pinned = 1, " +
			"is_blocked = 1, status <> 'pinned' FROM issues LIMIT 1",
		"EXPLAIN FORMAT=TREE SELECT 1 FROM issues WHERE is_blocked = 1 AND status <> 'closed' AND status <> 'pinned'",
	}
	if len(schemaCompatibilityProbes) != len(want) {
		t.Fatalf("probes = %q, want %q", schemaCompatibilityProbes, want)
	}
	for i := range want {
		if schemaCompatibilityProbes[i] != want[i] {
			t.Fatalf("probe %d = %q, want %q", i, schemaCompatibilityProbes[i], want[i])
		}
	}
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectQuery(want[0]).
		WillReturnRows(sqlmock.NewRows([]string{"c1", "c2", "c3", "c4", "c5", "c6", "c7"}).AddRow(1, 0, 0, 0, 0, 0, 1))
	mock.ExpectQuery(want[1]).
		WillReturnRows(sqlmock.NewRows([]string{"plan"}).AddRow("Project").AddRow(" └─ Table"))

	check := schemaCompatibilityCheck(db)
	if check.Status != StatusOK || check.Message != "Basic queries succeeded" {
		t.Errorf("check = %q %q, want %q %q", check.Status, check.Message, StatusOK, "Basic queries succeeded")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("statements: %v", err)
	}
	for _, probe := range schemaCompatibilityProbes {
		if regexp.MustCompile(`(?i)\bCOUNT\s*\(|\bSUM\s*\(`).MatchString(probe) {
			t.Errorf("probe %q counts issues", probe)
		}
		if !strings.HasSuffix(probe, " LIMIT 1") && !strings.HasPrefix(probe, "EXPLAIN FORMAT=TREE SELECT ") {
			t.Errorf("probe %q may read more than one row", probe)
		}
	}
}

// TestCheckSchemaCompatibilityProbeErrorFails: a probe error is the check's
// error result, with the text it had.
func TestCheckSchemaCompatibilityProbeErrorFails(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectQuery(schemaCompatibilityProbes[0]).
		WillReturnError(errors.New("Error 1054 (42S22): column \"pinned\" could not be found in any table in scope"))

	check := schemaCompatibilityCheck(db)
	if check.Status != StatusError || check.Message != "Database schema is incomplete or incompatible" {
		t.Errorf("check = %q %q, want %q %q", check.Status, check.Message, StatusError, "Database schema is incomplete or incompatible")
	}
	if !strings.Contains(check.Detail, "Error 1054") {
		t.Errorf("detail %q lacks the probe error", check.Detail)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("statements: %v", err)
	}
}

// TestCheckSchemaCompatibilityLiveSchema runs the check against a real
// migrated database on the package's test Dolt server: it passes as built,
// and fails with each probed issues column renamed away.
func TestCheckSchemaCompatibilityLiveSchema(t *testing.T) {
	port := testutil.DoltContainerPortInt()
	if port == 0 {
		t.Skip("Dolt test server not available, skipping")
	}
	if testutil.DoltContainerCrashed() {
		t.Skipf("Dolt test server crashed: %v", testutil.DoltContainerCrashError())
	}
	ctx := context.Background()
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	dbName := "doctest_schema_" + hex.EncodeToString(buf)
	store, err := dolt.New(ctx, &dolt.Config{
		Path:            filepath.Join(t.TempDir(), "dolt"),
		ServerHost:      "127.0.0.1",
		ServerPort:      port,
		Database:        dbName,
		CreateIfMissing: true,
		MaxOpenConns:    1,
	})
	if err != nil {
		t.Fatalf("create %s: %v", dbName, err)
	}
	t.Cleanup(func() {
		_, _ = store.DB().ExecContext(context.Background(), "DROP DATABASE IF EXISTS `"+dbName+"`")
		_ = store.Close()
	})

	if check := checkSchemaCompatibilityWithStore(store); check.Status != StatusOK {
		t.Fatalf("migrated database: check = %q %q (%s), want %q", check.Status, check.Message, check.Detail, StatusOK)
	}
	for _, column := range []string{"status", "pinned", "is_blocked"} {
		t.Run("missing "+column, func(t *testing.T) {
			gone := column + "_gone"
			if _, err := store.DB().ExecContext(ctx, "ALTER TABLE issues RENAME COLUMN "+column+" TO "+gone); err != nil {
				t.Fatalf("rename %s: %v", column, err)
			}
			defer func() {
				if _, err := store.DB().ExecContext(ctx, "ALTER TABLE issues RENAME COLUMN "+gone+" TO "+column); err != nil {
					t.Fatalf("restore %s: %v", column, err)
				}
			}()
			check := checkSchemaCompatibilityWithStore(store)
			if check.Status != StatusError || check.Message != "Database schema is incomplete or incompatible" {
				t.Errorf("issues without %s: check = %q %q, want %q", column, check.Status, check.Message, StatusError)
			}
			if !strings.Contains(check.Detail, column) {
				t.Errorf("issues without %s: detail %q does not name it", column, check.Detail)
			}
		})
	}
	if check := checkSchemaCompatibilityWithStore(store); check.Status != StatusOK {
		t.Errorf("after the columns were restored: check = %q %q (%s), want %q", check.Status, check.Message, check.Detail, StatusOK)
	}
}

// openSchemaScratchStore opens a fresh migrated database, named with prefix,
// on the package's test Dolt server, dropped at cleanup. It skips without the
// server.
func openSchemaScratchStore(t *testing.T, prefix string) *dolt.DoltStore {
	t.Helper()
	port := testutil.DoltContainerPortInt()
	if port == 0 {
		t.Skip("Dolt test server not available, skipping")
	}
	if testutil.DoltContainerCrashed() {
		t.Skipf("Dolt test server crashed: %v", testutil.DoltContainerCrashError())
	}
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	dbName := prefix + hex.EncodeToString(buf)
	store, err := dolt.New(context.Background(), &dolt.Config{
		Path:            filepath.Join(t.TempDir(), "dolt"),
		ServerHost:      "127.0.0.1",
		ServerPort:      port,
		Database:        dbName,
		CreateIfMissing: true,
		MaxOpenConns:    1,
	})
	if err != nil {
		t.Fatalf("create %s: %v", dbName, err)
	}
	t.Cleanup(func() {
		_, _ = store.DB().ExecContext(context.Background(), "DROP DATABASE IF EXISTS `"+dbName+"`")
		_ = store.Close()
	})
	return store
}

// schemaIndexState is a set of secondary indexes of issues dropped for a
// case: every index whose first column is one of drop.
type schemaIndexState struct {
	name string
	drop []string
}

// schemaIndexStates: the migrated indexes; without idx_issues_is_blocked
// (is_blocked, status), which bounded the blocked filter's read before
// bd-8tpd1; without the status indexes the filter's plan uses instead
// (idx_issues_status_closed_at, idx_issues_status_updated_at); without both.
var schemaIndexStates = []schemaIndexState{
	{name: "indexed"},
	{name: "no_blocked_index", drop: []string{"is_blocked"}},
	{name: "no_status_index", drop: []string{"status"}},
	{name: "no_index", drop: []string{"is_blocked", "status"}},
}

// dropSchemaIndexes drops the indexes of state from issues and returns what
// re-adds them. It fails when the state drops nothing it names: the migrated
// schema is expected to index both columns.
func dropSchemaIndexes(t *testing.T, store *dolt.DoltStore, state schemaIndexState) (restore func()) {
	t.Helper()
	ctx := context.Background()
	rows, err := store.DB().QueryContext(ctx, "SELECT INDEX_NAME, COLUMN_NAME FROM information_schema.STATISTICS "+
		"WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'issues' AND INDEX_NAME <> 'PRIMARY' ORDER BY INDEX_NAME, SEQ_IN_INDEX")
	if err != nil {
		t.Fatalf("list issues indexes: %v", err)
	}
	var names []string
	columns := map[string][]string{}
	for rows.Next() {
		var name, column string
		if err := rows.Scan(&name, &column); err != nil {
			t.Fatalf("scan issues index: %v", err)
		}
		if columns[name] == nil {
			names = append(names, name)
		}
		columns[name] = append(columns[name], "`"+column+"`")
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("list issues indexes: %v", err)
	}
	var dropped []string
	for _, lead := range state.drop {
		n := 0
		for _, name := range names {
			if columns[name][0] == "`"+lead+"`" {
				if _, err := store.DB().ExecContext(ctx, "DROP INDEX `"+name+"` ON issues"); err != nil {
					t.Fatalf("drop index %s: %v", name, err)
				}
				dropped = append(dropped, name)
				n++
			}
		}
		if n == 0 {
			t.Fatalf("%s: no index of issues leads with %s", state.name, lead)
		}
	}
	t.Logf("%s: dropped %v", state.name, dropped)
	return func() {
		for _, name := range dropped {
			if _, err := store.DB().ExecContext(ctx, "ALTER TABLE issues ADD INDEX `"+name+"` ("+strings.Join(columns[name], ", ")+")"); err != nil {
				t.Fatalf("re-add index %s: %v", name, err)
			}
		}
	}
}

var (
	planAttribute = regexp.MustCompile(`^([a-zA-Z]+): (.*)$`)
	planLimit     = regexp.MustCompile(`^Limit\((\d+)\)$`)
)

// probeReadBound is the most rows of issues one probe can read, and the plan
// it is proved from. An EXPLAIN reads none by construction: the server plans
// the statement and returns the plan, it does not run it. A statement the
// server cannot plan reads none: it fails before a read. Any other probe must
// plan, under EXPLAIN FORMAT=TREE, as Limit(n) over only a Project and one
// read of issues, the whole table or index ranges, with no Filter: every row
// such a read returns counts toward the limit, so it stops after n rows
// whatever the table holds. Another plan is an error quoting it.
func probeReadBound(ctx context.Context, store *dolt.DoltStore, probe string) (int, string, error) {
	if strings.HasPrefix(probe, "EXPLAIN FORMAT=TREE SELECT ") && !strings.Contains(probe, ";") {
		return 0, "EXPLAIN: plan only", nil
	}
	rows, err := store.DB().QueryContext(ctx, "EXPLAIN FORMAT=TREE "+probe)
	if err != nil {
		return 0, "not planned: " + err.Error(), nil
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return 0, "", err
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return 0, "", err
	}
	plan := strings.Join(lines, "\n")
	limit, reads := -1, 0
	for i, raw := range lines {
		line := strings.TrimLeft(raw, " │├└─")
		if m := planAttribute.FindStringSubmatch(line); m != nil {
			switch m[1] {
			case "columns", "index", "filters":
			case "name":
				if m[2] != "issues" {
					return 0, plan, fmt.Errorf("plan reads %s, not issues:\n%s", m[2], plan)
				}
			default:
				return 0, plan, fmt.Errorf("plan line %q is not a bounded read:\n%s", line, plan)
			}
			continue
		}
		if i == 0 {
			m := planLimit.FindStringSubmatch(line)
			if m == nil {
				return 0, plan, fmt.Errorf("plan has no Limit at its root:\n%s", plan)
			}
			if _, err := fmt.Sscan(m[1], &limit); err != nil {
				return 0, plan, err
			}
			continue
		}
		switch line {
		case "Project":
		case "Table", "IndexedTableAccess(issues)":
			reads++
		default:
			return 0, plan, fmt.Errorf("plan line %q is not a bounded read:\n%s", line, plan)
		}
	}
	if limit < 0 || reads != 1 {
		return 0, plan, fmt.Errorf("plan is not one limited read of issues:\n%s", plan)
	}
	return limit, plan, nil
}

// requireProbesBounded fails t unless every probe of the check reads at most
// one row of issues, by probeReadBound.
func requireProbesBounded(t *testing.T, store *dolt.DoltStore, label string) {
	t.Helper()
	for _, probe := range schemaCompatibilityProbes {
		bound, plan, err := probeReadBound(context.Background(), store, probe)
		if err != nil {
			t.Errorf("%s: probe %q: %v", label, probe, err)
			continue
		}
		if bound > 1 {
			t.Errorf("%s: probe %q reads up to %d rows:\n%s", label, probe, bound, plan)
		}
	}
}

// TestCheckSchemaCompatibilityReadsBoundedRows (review bd-b4n1i, bd-8tpd1):
// on a migrated table of 64 open issues, none blocked, in each index state of
// schemaIndexStates, every probe of the check reads at most one row of issues
// (probeReadBound: the select-list probe from its EXPLAIN FORMAT=TREE plan,
// the EXPLAIN probe by construction), and the check passes.
func TestCheckSchemaCompatibilityReadsBoundedRows(t *testing.T) {
	const openRows = 64
	store := openSchemaScratchStore(t, "doctest_schemabound_")
	ctx := context.Background()
	var tuples []string
	for i := 0; i < openRows; i++ {
		tuples = append(tuples, fmt.Sprintf("('doctest-bound-%02d', 'bound', '', '', '', '', 'open', 0)", i))
	}
	if _, err := store.DB().ExecContext(ctx, "INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, is_blocked) VALUES "+
		strings.Join(tuples, ", ")); err != nil {
		t.Fatalf("insert %d open rows: %v", openRows, err)
	}
	var n, blocked int
	if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(SUM(is_blocked), 0) FROM issues WHERE status = 'open'").Scan(&n, &blocked); err != nil {
		t.Fatal(err)
	}
	if n != openRows || blocked != 0 {
		t.Fatalf("fixture: %d open rows, %d blocked; want %d, 0", n, blocked, openRows)
	}
	for _, state := range schemaIndexStates {
		t.Run(state.name, func(t *testing.T) {
			restore := dropSchemaIndexes(t, store, state)
			defer restore()
			for _, probe := range schemaCompatibilityProbes {
				bound, plan, err := probeReadBound(ctx, store, probe)
				if err != nil {
					t.Errorf("%d open rows, none blocked: probe %q: %v", openRows, probe, err)
					continue
				}
				t.Logf("probe %q reads at most %d rows:\n%s", probe, bound, plan)
				if bound > 1 {
					t.Errorf("%d open rows, none blocked: probe %q reads up to %d rows", openRows, probe, bound)
				}
			}
			if check := checkSchemaCompatibilityWithStore(store); check.Status != StatusOK {
				t.Errorf("check = %q %q (%s), want %q", check.Status, check.Message, check.Detail, StatusOK)
			}
		})
	}
}

// schemaFault is one fault the comparison test makes on the scratch
// database's issues table: the table renamed away (column ""), a column
// renamed away (sqlType ""), or a column altered to sqlType with value, a SQL
// literal valid for that type, for the one-row fixture, and bulk, another
// one that is not blocked, for each row of the 64-row fixture.
type schemaFault struct {
	column  string
	sqlType string
	value   string
	bulk    string
}

func (f schemaFault) String() string {
	switch {
	case f.column == "":
		return "missing issues"
	case f.sqlType == "":
		return "missing " + f.column
	default:
		return f.column + " " + f.sqlType
	}
}

// schemaFaultCatalogue: the issues table missing, each probed column missing,
// and each probed column altered to DATE, INT, VARCHAR(8), JSON and BLOB.
func schemaFaultCatalogue() []schemaFault {
	faults := []schemaFault{{}}
	values := map[string]map[string][2]string{
		"status":     {"DATE": {"'2026-11-19'", "'2026-11-19'"}, "INT": {"1", "0"}, "VARCHAR(8)": {"'closed'", "'open'"}, "JSON": {`'"closed"'`, `'"open"'`}, "BLOB": {"'closed'", "'open'"}},
		"pinned":     {"DATE": {"'2026-11-19'", "'2026-11-19'"}, "INT": {"1", "0"}, "VARCHAR(8)": {"'1'", "'0'"}, "JSON": {"'1'", "'0'"}, "BLOB": {"'1'", "'0'"}},
		"is_blocked": {"DATE": {"'2026-11-19'", "'2026-11-19'"}, "INT": {"1", "0"}, "VARCHAR(8)": {"'1'", "'0'"}, "JSON": {"'1'", "'0'"}, "BLOB": {"'1'", "'0'"}},
	}
	for _, column := range []string{"status", "pinned", "is_blocked"} {
		faults = append(faults, schemaFault{column: column})
		for _, sqlType := range []string{"DATE", "INT", "VARCHAR(8)", "JSON", "BLOB"} {
			v := values[column][sqlType]
			faults = append(faults, schemaFault{column: column, sqlType: sqlType, value: v[0], bulk: v[1]})
		}
	}
	return faults
}

// schemaFixtureRows are the fixture populations of a case: empty, one row,
// and 64 rows (none blocked unless a fault makes is_blocked something else).
var schemaFixtureRows = []struct {
	name string
	rows int
}{{"empty", 0}, {"one_row", 1}, {"64_rows", 64}}

// schemaStatusVariants are the status values of the collation cases, each in
// a row flagged blocked: the blocked filter's literals in other letter cases.
var schemaStatusVariants = []string{"closed", "Closed", "CLOSED", "pinned", "Pinned", "PINNED", "open", "Open"}

// TestCheckSchemaCompatibilityMatchesStatistics is a comparison test
// (bd-pbioz, review bd-mh4fb; index states and bounds: bd-8tpd1, review
// bd-b4n1i): the oracle is store.GetStatistics, the query the schema check
// ran before bd-v5qbz; the piece under test is the check, on the same
// scratch database. Relation: whenever the oracle returns an error, the check
// does not answer ok; with no fault both pass. In each index state of
// schemaIndexStates: every fault of schemaFaultCatalogue on each population
// of schemaFixtureRows; the status column under a binary and under a
// case-insensitive collation holding schemaStatusVariants (no fault: both
// pass); then schemaPairSeedCount seeded picks of two faults on two columns
// at once (seeds 1 to schemaPairSeedCount, each picking from the faults whose
// ALTER the server accepted in that state, and a population), within
// schemaPairBudget for all states; a failure names its state and seed. An
// ALTER the server refuses is logged by name and its case not run. In every
// case the check's probes are also held to probeReadBound's one-row bound.
func TestCheckSchemaCompatibilityMatchesStatistics(t *testing.T) {
	const (
		schemaPairSeedCount = 24
		schemaPairBudget    = 4 * time.Minute
	)
	start := time.Now()
	store := openSchemaScratchStore(t, "doctest_schemacmp_")
	ctx := context.Background()

	createTable := func(t *testing.T) string {
		t.Helper()
		var name, ddl string
		if err := store.DB().QueryRowContext(ctx, "SHOW CREATE TABLE issues").Scan(&name, &ddl); err != nil {
			t.Fatalf("show create table issues: %v", err)
		}
		return ddl
	}
	healthy := createTable(t)
	// The migrated definition of each probed column, to restore an ALTER.
	columnDef := map[string]string{}
	for _, line := range strings.Split(healthy, "\n") {
		line = strings.TrimSuffix(strings.TrimSpace(line), ",")
		for _, column := range []string{"status", "pinned", "is_blocked"} {
			if strings.HasPrefix(line, "`"+column+"` ") {
				columnDef[column] = line
			}
		}
	}
	for _, column := range []string{"status", "pinned", "is_blocked"} {
		if columnDef[column] == "" {
			t.Fatalf("no definition of issues.%s in\n%s", column, healthy)
		}
	}

	exec := func(q string) error {
		_, err := store.DB().ExecContext(ctx, q)
		return err
	}
	const rowPrefix = "doctest-cmp-"
	// insertRows inserts n fixture rows, with each altered column set to the
	// fault's value (one row) or bulk value (more), and extra columns set to
	// extra's literals.
	insertRows := func(faults []schemaFault, n int, extra map[string][]string) error {
		extraColumns := make([]string, 0, len(extra))
		for column := range extra {
			extraColumns = append(extraColumns, column)
		}
		sort.Strings(extraColumns)
		var tuples []string
		for i := 0; i < n; i++ {
			columns := []string{"id", "title", "description", "design", "acceptance_criteria", "notes"}
			values := []string{fmt.Sprintf("'%s%02d'", rowPrefix, i), "'comparison'", "''", "''", "''", "''"}
			for _, f := range faults {
				if f.column != "" && f.sqlType != "" {
					columns = append(columns, f.column)
					if n == 1 {
						values = append(values, f.value)
					} else {
						values = append(values, f.bulk)
					}
				}
			}
			for _, column := range extraColumns {
				columns = append(columns, column)
				values = append(values, extra[column][i])
			}
			if i == 0 {
				tuples = append(tuples, "INSERT INTO issues ("+strings.Join(columns, ", ")+") VALUES")
			}
			tuples = append(tuples, "("+strings.Join(values, ", ")+")")
		}
		if n == 0 {
			return nil
		}
		return exec(tuples[0] + " " + strings.Join(tuples[1:], ", "))
	}
	const deleteRows = "DELETE FROM issues WHERE id LIKE '" + rowPrefix + "%'"

	cases := 0
	// run makes the faults (with n fixture rows), holds the probes to their
	// bound, compares the oracle with the check, undoes everything and checks
	// the table is back to want. It returns the oracle's error and the check,
	// or refusal when the server refused an ALTER.
	run := func(t *testing.T, want string, faults []schemaFault, n int) (oracleErr error, check DoctorCheck, refusal error) {
		t.Helper()
		var undo []string
		defer func() {
			for i := len(undo) - 1; i >= 0; i-- {
				if err := exec(undo[i]); err != nil {
					t.Fatalf("undo %s: %v", undo[i], err)
				}
			}
			if got := createTable(t); got != want {
				t.Fatalf("issues not restored after %v:\n%s\nwant\n%s", faults, got, want)
			}
		}()
		tableGone := len(faults) == 1 && faults[0].column == ""
		if n > 0 && tableGone {
			if err := insertRows(nil, n, nil); err != nil {
				t.Fatalf("insert %d fixture rows: %v", n, err)
			}
			undo = append(undo, deleteRows)
		}
		for _, f := range faults {
			switch {
			case f.column == "":
				if err := exec("RENAME TABLE issues TO issues_gone"); err != nil {
					t.Fatalf("rename issues: %v", err)
				}
				undo = append(undo, "RENAME TABLE issues_gone TO issues")
			case f.sqlType == "":
				if err := exec("ALTER TABLE issues RENAME COLUMN " + f.column + " TO " + f.column + "_gone"); err != nil {
					t.Fatalf("rename %s: %v", f.column, err)
				}
				undo = append(undo, "ALTER TABLE issues RENAME COLUMN "+f.column+"_gone TO "+f.column)
			default:
				if err := exec("ALTER TABLE issues MODIFY COLUMN " + f.column + " " + f.sqlType + " NULL"); err != nil {
					return nil, DoctorCheck{}, fmt.Errorf("ALTER %s to %s: %w", f.column, f.sqlType, err)
				}
				undo = append(undo, "ALTER TABLE issues MODIFY COLUMN "+columnDef[f.column])
			}
		}
		if n > 0 && !tableGone {
			if err := insertRows(faults, n, nil); err != nil {
				t.Fatalf("insert %d fixture rows for %v: %v", n, faults, err)
			}
			undo = append(undo, deleteRows)
		}
		cases++
		requireProbesBounded(t, store, fmt.Sprintf("%v (%d rows)", faults, n))
		_, oracleErr = store.GetStatistics(ctx)
		return oracleErr, checkSchemaCompatibilityWithStore(store), nil
	}
	compare := func(t *testing.T, label string, oracleErr error, check DoctorCheck) {
		t.Helper()
		t.Logf("%s: oracle=%v check=%s", label, oracleErr, check.Status)
		if oracleErr != nil && check.Status == StatusOK {
			t.Errorf("%s: GetStatistics fails (%v) but the check answers %q %q", label, oracleErr, check.Status, check.Message)
		}
	}

	catalogue := schemaFaultCatalogue()
	deadline := start.Add(schemaPairBudget)
	pairsRan := 0
	refused := map[string][]string{}
	for _, state := range schemaIndexStates {
		t.Run(state.name, func(t *testing.T) {
			restore := dropSchemaIndexes(t, store, state)
			defer func() {
				restore()
				if got := createTable(t); got != healthy {
					t.Fatalf("issues indexes not restored after %s:\n%s\nwant\n%s", state.name, got, healthy)
				}
			}()
			want := createTable(t)

			t.Run("healthy", func(t *testing.T) {
				for _, pop := range schemaFixtureRows {
					oracleErr, check, _ := run(t, want, nil, pop.rows)
					if oracleErr != nil || check.Status != StatusOK {
						t.Errorf("no fault (%s): oracle=%v check=%q %q (%s), want both to pass", pop.name, oracleErr, check.Status, check.Message, check.Detail)
					}
				}
			})

			// The blocked filter's literals against status values in other
			// letter cases, under a binary and a case-insensitive collation.
			for _, collation := range []string{"utf8mb4_0900_bin", "utf8mb4_0900_ai_ci"} {
				t.Run("status_"+collation, func(t *testing.T) {
					if err := exec("ALTER TABLE issues MODIFY COLUMN `status` varchar(32) COLLATE " + collation + " NOT NULL DEFAULT 'open'"); err != nil {
						t.Fatalf("collate status %s: %v", collation, err)
					}
					defer func() {
						if err := exec("ALTER TABLE issues MODIFY COLUMN " + columnDef["status"]); err != nil {
							t.Fatalf("restore status: %v", err)
						}
						if got := createTable(t); got != want {
							t.Fatalf("issues not restored after %s:\n%s\nwant\n%s", collation, got, want)
						}
					}()
					var statuses, flags []string
					for _, s := range schemaStatusVariants {
						statuses = append(statuses, "'"+s+"'")
						flags = append(flags, "1")
					}
					if err := insertRows(nil, len(statuses), map[string][]string{"status": statuses, "is_blocked": flags}); err != nil {
						t.Fatalf("insert status variants: %v", err)
					}
					defer func() {
						if err := exec(deleteRows); err != nil {
							t.Fatalf("delete status variants: %v", err)
						}
					}()
					cases++
					requireProbesBounded(t, store, "status "+collation)
					stats, oracleErr := store.GetStatistics(ctx)
					check := checkSchemaCompatibilityWithStore(store)
					compare(t, "status "+collation, oracleErr, check)
					if oracleErr == nil {
						t.Logf("status %s: oracle blocked=%d", collation, *stats.BlockedIssues)
						if check.Status != StatusOK {
							t.Errorf("status %s, no fault: check = %q %q (%s), want %q", collation, check.Status, check.Message, check.Detail, StatusOK)
						}
					}
				})
			}

			accepted := map[string]bool{}
			for _, f := range catalogue {
				for _, pop := range schemaFixtureRows {
					name := strings.NewReplacer(" ", "_", "(", "", ")", "").Replace(f.String()) + "/" + pop.name
					t.Run(name, func(t *testing.T) {
						oracleErr, check, refusal := run(t, want, []schemaFault{f}, pop.rows)
						if refusal != nil {
							if pop.rows == 1 {
								refused[state.name] = append(refused[state.name], f.String())
							}
							t.Logf("server refused, case not run: %v", refusal)
							return
						}
						accepted[f.String()] = true
						compare(t, state.name+"/"+name, oracleErr, check)
					})
				}
			}

			// Two faults at once, on two different columns, from the faults
			// whose ALTER the server accepted above.
			var columnFaults []schemaFault
			for _, f := range catalogue {
				if f.column != "" && accepted[f.String()] {
					columnFaults = append(columnFaults, f)
				}
			}
			t.Run("pairs", func(t *testing.T) {
				for seed := int64(1); seed <= schemaPairSeedCount; seed++ {
					if time.Now().After(deadline) {
						t.Logf("pair budget %v spent after %d pairs, at %s seed %d", schemaPairBudget, pairsRan, state.name, seed)
						break
					}
					rng := mrand.New(mrand.NewSource(seed))
					a := columnFaults[rng.Intn(len(columnFaults))]
					b := a
					for b.column == a.column {
						b = columnFaults[rng.Intn(len(columnFaults))]
					}
					pop := schemaFixtureRows[rng.Intn(len(schemaFixtureRows))]
					oracleErr, check, refusal := run(t, want, []schemaFault{a, b}, pop.rows)
					pairsRan++
					label := fmt.Sprintf("%s seed %d: %v + %v (%s)", state.name, seed, a, b, pop.name)
					if refusal != nil {
						t.Logf("%s: server refused, case not run: %v", label, refusal)
						continue
					}
					compare(t, label, oracleErr, check)
				}
			})
		})
	}
	t.Logf("refused ALTERs by index state: %v", refused)
	t.Logf("%d cases compared (%d pairs) in %v", cases, pairsRan, time.Since(start).Round(time.Millisecond))
}

func runDoctorGitInDir(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed in %s: %v\n%s", args, dir, err, output)
	}

	return strings.TrimSpace(string(output))
}

func setupDoctorBareParentWorktree(t *testing.T) (string, string) {
	t.Helper()

	tmpDir := t.TempDir()
	bareDir := filepath.Join(tmpDir, "repo.git")
	mainWorktreeDir := filepath.Join(tmpDir, "main")
	featureWorktreeDir := filepath.Join(tmpDir, "feature")

	runDoctorGitInDir(t, tmpDir, "init", "--bare", bareDir)
	runDoctorGitInDir(t, tmpDir, "--git-dir", bareDir, "symbolic-ref", "HEAD", "refs/heads/main")
	runDoctorGitInDir(t, tmpDir, "--git-dir", bareDir, "config", "user.email", "test@example.com")
	runDoctorGitInDir(t, tmpDir, "--git-dir", bareDir, "config", "user.name", "Test User")
	emptyTree := runDoctorGitInDir(t, tmpDir, "--git-dir", bareDir, "hash-object", "-t", "tree", "/dev/null")
	initCommit := runDoctorGitInDir(t, tmpDir, "--git-dir", bareDir, "commit-tree", "-m", "Initial commit", emptyTree)
	runDoctorGitInDir(t, tmpDir, "--git-dir", bareDir, "update-ref", "HEAD", initCommit)
	runDoctorGitInDir(t, tmpDir, "--git-dir", bareDir, "worktree", "add", mainWorktreeDir, "main")
	runDoctorGitInDir(t, mainWorktreeDir, "branch", "feature")
	runDoctorGitInDir(t, tmpDir, "--git-dir", bareDir, "worktree", "add", featureWorktreeDir, "feature")

	return bareDir, featureWorktreeDir
}

// TestLargeDatabaseWarningRecommendsRealCommand pins the Large Database
// remedy to a command that exists: the check used to recommend 'bd cleanup',
// which is not a root-level command in this build (cleanup lives under
// 'bd admin cleanup'); the operator-facing lifecycle command is 'bd prune'.
func TestLargeDatabaseWarningRecommendsRealCommand(t *testing.T) {
	dc := largeDatabaseWarning(6000, 5000)

	if dc.Status != StatusWarning {
		t.Fatalf("expected StatusWarning, got %v", dc.Status)
	}
	if strings.Contains(dc.Fix, "bd cleanup") {
		t.Fatalf("Large Database fix recommends nonexistent 'bd cleanup': %q", dc.Fix)
	}
	if !strings.Contains(dc.Fix, "bd prune") {
		t.Fatalf("Large Database fix should recommend 'bd prune': %q", dc.Fix)
	}
}
