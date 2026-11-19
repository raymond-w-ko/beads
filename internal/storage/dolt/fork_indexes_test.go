package dolt

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/storage/schema"
	"github.com/steveyegge/beads/internal/types"
)

// forkIndexNames are the fork-only issues indexes schema.ensureForkIndexes adds
// (internal/storage/schema/fork_indexes.go).
var forkIndexNames = []string{
	"idx_issues_closed_at",
	"idx_issues_status_closed_at",
	"idx_issues_everturn_action",
	"idx_issues_everturn_key",
	"idx_issues_everturn_relay_source",
	"idx_issues_everturn_bead",
	"idx_issues_everturn_corrects",
}

// everturnMailKeyColumns are the four generated columns everturn's mail reads
// filter on, each on issues and on wisps and indexed on both as
// (column, created_at), with the metadata path each holds. The names and paths
// are everturn's reader contract (bd-iqmdg).
var everturnMailKeyColumns = []struct{ name, path string }{
	{name: "everturn_key", path: "$.everturn.key"},
	{name: "everturn_relay_source", path: "$.everturn.relay.source"},
	{name: "everturn_bead", path: "$.everturn.bead"},
	{name: "everturn_corrects", path: "$.everturn.corrects"},
}

var forkTables = []string{"issues", "wisps"}

func issuesIndexExists(t *testing.T, ctx context.Context, db *sql.DB, name string) bool {
	t.Helper()
	return tableIndexExists(t, ctx, db, "issues", name)
}

func tableIndexExists(t *testing.T, ctx context.Context, db *sql.DB, table, name string) bool {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM INFORMATION_SCHEMA.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = ?
	`, table, name).Scan(&count); err != nil {
		t.Fatalf("probe index %s on %s: %v", name, table, err)
	}
	return count > 0
}

func doltStatusCount(t *testing.T, ctx context.Context, db *sql.DB) int {
	t.Helper()
	rows, err := db.QueryContext(ctx, "SELECT table_name, status FROM dolt_status")
	if err != nil {
		t.Fatalf("read dolt_status: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var table, status string
		if err := rows.Scan(&table, &status); err != nil {
			t.Fatalf("scan dolt_status: %v", err)
		}
		t.Logf("dolt_status: %s %s", table, status)
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read dolt_status: %v", err)
	}
	return count
}

// seedClosedIssues inserts n closed issues (closed_at one hour apart, ending
// in October 2026) and a few open ones, so the EXPLAIN cases below see a
// populated table rather than planning against an empty one.
func seedClosedIssues(t *testing.T, ctx context.Context, db *sql.DB, n int) {
	t.Helper()
	const batch = 100
	base := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	for start := 0; start < n; start += batch {
		var values []string
		for i := start; i < n && i < start+batch; i++ {
			closedAt := base.Add(-time.Duration(i) * time.Hour).Format("2006-01-02 15:04:05")
			values = append(values, fmt.Sprintf("('fx-closed-%d', 'closed fixture', '', '', '', '', 'closed', 2, 'task', '%s')", i, closedAt))
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type, closed_at) VALUES "+strings.Join(values, ", ")); err != nil {
			t.Fatalf("seed closed issues: %v", err)
		}
	}
	for i := 0; i < 20; i++ {
		if _, err := db.ExecContext(ctx, fmt.Sprintf("INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type) VALUES ('fx-open-%d', 'open fixture', '', '', '', '', 'open', 2, 'task')", i)); err != nil {
			t.Fatalf("seed open issues: %v", err)
		}
	}
}

// TestForkIndexesPresentAndUsed checks that an initialized store carries every
// fork index and that the closed_at query shapes everturn issues read
// through them as index reads: IndexedTableAccess on the expected index, and
// no TopN or Sort above it.
func TestForkIndexesPresentAndUsed(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	for _, name := range forkIndexNames {
		if !issuesIndexExists(t, ctx, store.db, name) {
			t.Errorf("issues lacks %s after schema init", name)
		}
	}
	seedClosedIssues(t, ctx, store.db, 300)

	for _, tc := range []struct {
		query string
		index string // how EXPLAIN names the index it reads
	}{
		{
			query: "SELECT id FROM issues WHERE closed_at IS NOT NULL ORDER BY closed_at DESC LIMIT 5",
			index: "[issues.closed_at]",
		},
		{
			query: "SELECT id FROM issues ORDER BY closed_at DESC LIMIT 5",
			index: "[issues.closed_at]",
		},
		{
			// status DESC in the ORDER BY: with ORDER BY closed_at DESC alone,
			// Dolt 2.2.0 keeps a TopN over the range (2.4.2 does not).
			query: "SELECT id FROM issues WHERE status = 'closed' AND closed_at > '2026-10-01' ORDER BY status DESC, closed_at DESC LIMIT 5",
			index: "[issues.status,issues.closed_at]",
		},
		{
			query: "SELECT id FROM issues WHERE closed_at > '2026-01-01'",
			index: "[issues.closed_at]",
		},
	} {
		plan := explainPlan(t, ctx, store.db, tc.query)
		t.Logf("EXPLAIN FORMAT=TREE %s\n%s", tc.query, plan)
		if !strings.Contains(plan, "IndexedTableAccess") || !strings.Contains(plan, tc.index) {
			t.Errorf("%s: want IndexedTableAccess on %s, plan:\n%s", tc.query, tc.index, plan)
		}
		if strings.Contains(plan, "TopN") || strings.Contains(plan, "Sort") {
			t.Errorf("%s: want no TopN or Sort above the index read, plan:\n%s", tc.query, plan)
		}
	}

	// Logged, not asserted: the unbounded status shape still reads and sorts
	// every closed row (TopN), with or without idx_issues_status_closed_at, and
	// the ranged shape ordered by closed_at alone is version-dependent.
	for _, query := range []string{
		"SELECT id FROM issues WHERE status = 'closed' ORDER BY closed_at DESC LIMIT 5",
		"SELECT id FROM issues WHERE status = 'closed' AND closed_at > '2026-10-01' ORDER BY closed_at DESC LIMIT 5",
	} {
		t.Logf("EXPLAIN FORMAT=TREE %s\n%s", query, explainPlan(t, ctx, store.db, query))
	}
}

// TestForkIndexesIdempotentAndRepaired checks that a second schema init on a
// converged database is a no-op, that a dropped index is recreated by the next
// init, and that the step leaves a pre-existing dirty issues table alone.
func TestForkIndexesIdempotentAndRepaired(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	applied, err := initSchemaOnDB(ctx, store.db)
	if err != nil {
		t.Fatalf("second initSchemaOnDB: %v", err)
	}
	if applied != 0 {
		t.Errorf("second initSchemaOnDB applied = %d, want 0", applied)
	}
	if n := doltStatusCount(t, ctx, store.db); n != 0 {
		t.Errorf("dolt_status has %d rows after a no-op init, want 0", n)
	}

	// Dropped and left dirty: the step skips a dirty issues table without error
	// (the changed-signature guard would otherwise trip).
	if _, err := store.db.ExecContext(ctx, "ALTER TABLE issues DROP INDEX idx_issues_closed_at"); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	if _, err := initSchemaOnDB(ctx, store.db); err != nil {
		t.Fatalf("initSchemaOnDB with dirty issues: %v", err)
	}
	if issuesIndexExists(t, ctx, store.db, "idx_issues_closed_at") {
		t.Errorf("idx_issues_closed_at recreated over a pre-existing dirty issues table")
	}

	// Once the drop is committed, the next init recreates and commits it.
	if _, err := store.db.ExecContext(ctx, "CALL DOLT_ADD('issues')"); err != nil {
		t.Fatalf("stage index drop: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, "CALL DOLT_COMMIT('-m', 'test: drop idx_issues_closed_at')"); err != nil {
		t.Fatalf("commit index drop: %v", err)
	}
	if _, err := initSchemaOnDB(ctx, store.db); err != nil {
		t.Fatalf("initSchemaOnDB after drop: %v", err)
	}
	if !issuesIndexExists(t, ctx, store.db, "idx_issues_closed_at") {
		t.Errorf("idx_issues_closed_at not recreated after a committed drop")
	}
	if n := doltStatusCount(t, ctx, store.db); n != 0 {
		t.Errorf("dolt_status has %d rows after the recreating init, want 0 (committed by the pass)", n)
	}
}

func tableColumnExists(t *testing.T, ctx context.Context, db *sql.DB, table, column string) bool {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?
	`, table, column).Scan(&count); err != nil {
		t.Fatalf("probe column %s.%s: %v", table, column, err)
	}
	return count > 0
}

func issuesIndexColumns(t *testing.T, ctx context.Context, db *sql.DB, name string) []string {
	t.Helper()
	return tableIndexColumns(t, ctx, db, "issues", name)
}

func tableIndexColumns(t *testing.T, ctx context.Context, db *sql.DB, table, name string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
		SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = ?
		ORDER BY SEQ_IN_INDEX
	`, table, name)
	if err != nil {
		t.Fatalf("index columns %s: %v", name, err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var col sql.NullString
		if err := rows.Scan(&col); err != nil {
			t.Fatalf("scan index column %s: %v", name, err)
		}
		cols = append(cols, col.String)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("index columns %s: %v", name, err)
	}
	return cols
}

func everturnActionOf(t *testing.T, ctx context.Context, db *sql.DB, id string) sql.NullString {
	t.Helper()
	var action sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT everturn_action FROM issues WHERE id = ?", id).Scan(&action); err != nil {
		t.Fatalf("read everturn_action of %s: %v", id, err)
	}
	return action
}

// seedEverturnActionIssues inserts n issues whose metadata carries an
// everturn action (cycling wake, claim, close; every fourth row has none),
// created one minute apart, so the EXPLAIN cases plan against a populated
// table.
func seedEverturnActionIssues(t *testing.T, ctx context.Context, db *sql.DB, n int) {
	t.Helper()
	const batch = 100
	actions := []string{"wake", "claim", "close"}
	base := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	for start := 0; start < n; start += batch {
		var values []string
		for i := start; i < n && i < start+batch; i++ {
			metadata := "{}"
			if i%4 != 3 {
				metadata = fmt.Sprintf(`{"everturn":{"action":"%s"}}`, actions[i%len(actions)])
			}
			createdAt := base.Add(-time.Duration(i) * time.Minute).Format("2006-01-02 15:04:05")
			values = append(values, fmt.Sprintf("('fx-action-%d', 'action fixture', '', '', '', '', 'open', 2, 'task', '%s', '%s')", i, metadata, createdAt))
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type, metadata, created_at) VALUES "+strings.Join(values, ", ")); err != nil {
			t.Fatalf("seed everturn action issues: %v", err)
		}
	}
}

// TestForkIndexesEverturnActionColumn checks the everturn_action generated
// column: present on issues and wisps, indexed on issues as
// (everturn_action, created_at), computed from metadata on insert and
// recomputed on update through the store, and read through the index by the
// audit lookup shapes.
func TestForkIndexesEverturnActionColumn(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	for _, table := range []string{"issues", "wisps"} {
		if !tableColumnExists(t, ctx, store.db, table, "everturn_action") {
			t.Errorf("%s lacks everturn_action after schema init", table)
		}
	}
	if got, want := issuesIndexColumns(t, ctx, store.db, "idx_issues_everturn_action"), []string{"everturn_action", "created_at"}; !slices.Equal(got, want) {
		t.Errorf("idx_issues_everturn_action columns = %v, want %v", got, want)
	}

	withAction := &types.Issue{ID: "fx-ea-wake", Title: "with action", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask,
		Metadata: json.RawMessage(`{"everturn":{"action":"wake"}}`)}
	withoutAction := &types.Issue{ID: "fx-ea-none", Title: "without action", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	for _, iss := range []*types.Issue{withAction, withoutAction} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("create %s: %v", iss.ID, err)
		}
	}
	if got := everturnActionOf(t, ctx, store.db, withAction.ID); !got.Valid || got.String != "wake" {
		t.Errorf("everturn_action of %s = %v, want wake", withAction.ID, got)
	}
	if got := everturnActionOf(t, ctx, store.db, withoutAction.ID); got.Valid {
		t.Errorf("everturn_action of %s = %q, want NULL", withoutAction.ID, got.String)
	}
	if err := store.UpdateIssue(ctx, withAction.ID, map[string]interface{}{"metadata": `{"everturn":{"action":"claim"}}`}, "tester"); err != nil {
		t.Fatalf("update metadata of %s: %v", withAction.ID, err)
	}
	if got := everturnActionOf(t, ctx, store.db, withAction.ID); !got.Valid || got.String != "claim" {
		t.Errorf("everturn_action of %s after update = %v, want claim", withAction.ID, got)
	}

	seedEverturnActionIssues(t, ctx, store.db, 300)
	const index = "[issues.everturn_action,issues.created_at]"
	for _, tc := range []struct {
		query  string
		noTopN bool // asserted: no TopN or Sort above the index read
	}{
		{
			// The served ordered shape: everturn_action named in the ORDER BY
			// gives a reverse index read with no sort.
			query:  "SELECT id FROM issues WHERE everturn_action = 'wake' ORDER BY everturn_action DESC, created_at DESC LIMIT 5",
			noTopN: true,
		},
		{
			query:  "SELECT id FROM issues WHERE everturn_action = 'wake' AND created_at > '2026-01-01'",
			noTopN: true,
		},
		{
			// Logged, not asserted: whether a TopN remains above the index read
			// when the ORDER BY names created_at alone.
			query: "SELECT id FROM issues WHERE everturn_action = 'wake' ORDER BY created_at DESC LIMIT 5",
		},
	} {
		plan := explainPlan(t, ctx, store.db, tc.query)
		t.Logf("EXPLAIN FORMAT=TREE %s (TopN: %v)\n%s", tc.query, strings.Contains(plan, "TopN"), plan)
		if !strings.Contains(plan, "IndexedTableAccess") || !strings.Contains(plan, index) {
			t.Errorf("%s: want IndexedTableAccess on %s, plan:\n%s", tc.query, index, plan)
		}
		if tc.noTopN && (strings.Contains(plan, "TopN") || strings.Contains(plan, "Sort")) {
			t.Errorf("%s: want no TopN or Sort above the index read, plan:\n%s", tc.query, plan)
		}
	}
}

// TestForkIndexesEverturnActionUpsert is review bd-gv9ou's P1 case: with the
// generated columns present (everturn_action and the four mail key columns),
// the issue upsert (issueops.InsertIssueIntoTable, INSERT ... ON DUPLICATE KEY
// UPDATE col = VALUES(col)) of an existing row succeeds on issues and on
// wisps, and every stored column follows the new metadata. A VIRTUAL column
// failed here on Dolt 2.2.0, VALUES() shifted by one column.
func TestForkIndexesEverturnActionUpsert(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	for _, table := range []string{"issues", "wisps"} {
		t.Run(table, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Second)
			issue := &types.Issue{ID: "fx-upsert-" + table, Title: "before upsert", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask,
				CreatedAt: now, UpdatedAt: now, Ephemeral: table == "wisps", Metadata: allEverturnKeysMetadata("wake", "1")}
			if err := issueops.InsertIssueIntoTable(ctx, store.db, table, issue); err != nil {
				t.Fatalf("first upsert into %s: %v", table, err)
			}
			issue.Title = "after upsert"
			issue.UpdatedAt = now.Add(time.Second)
			issue.Metadata = allEverturnKeysMetadata("claim", "2")
			if err := issueops.InsertIssueIntoTable(ctx, store.db, table, issue); err != nil {
				t.Fatalf("second upsert into %s of an existing row: %v", table, err)
			}
			var title string
			if err := store.db.QueryRowContext(ctx, "SELECT title FROM "+table+" WHERE id = ?", issue.ID).Scan(&title); err != nil {
				t.Fatalf("read %s.%s after upsert: %v", table, issue.ID, err)
			}
			if title != "after upsert" {
				t.Errorf("%s.%s title after upsert = %q, want \"after upsert\"", table, issue.ID, title)
			}
			assertEverturnKeys(t, ctx, store.db, table, issue.ID, wantAllEverturnKeys("claim", "2"))
		})
	}
}

func everturnActionLength(t *testing.T, ctx context.Context, db *sql.DB, table, id string) (int, string) {
	t.Helper()
	var n int
	var action string
	if err := db.QueryRowContext(ctx, "SELECT LENGTH(everturn_action), everturn_action FROM "+table+" WHERE id = ?", id).Scan(&n, &action); err != nil {
		t.Fatalf("read everturn_action of %s.%s: %v", table, id, err)
	}
	return n, action
}

// TestForkIndexesEverturnActionLongValues checks that an action rendering
// longer than the VARCHAR(255) (a long string, or an object) does not reject
// the write: the column's LEFT(..., 255) stores its first 255 characters, on
// create and on a metadata update, on issues and on wisps.
func TestForkIndexesEverturnActionLongValues(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	long := strings.Repeat("a", 300)
	for _, tc := range []struct {
		id, table, action, prefix string
		ephemeral                 bool
	}{
		{id: "fx-ea-long", table: "issues", action: `"` + long + `"`, prefix: long[:255]},
		{id: "fx-ea-object", table: "issues", action: `{"note":"` + strings.Repeat("b", 300) + `"}`, prefix: `{"note": "bbb`},
		{id: "fx-ea-long-wisp", table: "wisps", action: `"` + long + `"`, prefix: long[:255], ephemeral: true},
	} {
		iss := &types.Issue{ID: tc.id, Title: "long action", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask,
			Ephemeral: tc.ephemeral, Metadata: json.RawMessage(`{"everturn":{"action":` + tc.action + `}}`)}
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Errorf("create %s with a long action: %v", tc.id, err)
			continue
		}
		n, got := everturnActionLength(t, ctx, store.db, tc.table, tc.id)
		if n != 255 || !strings.HasPrefix(got, tc.prefix) {
			t.Errorf("everturn_action of %s.%s = %d chars %.40q..., want 255 starting %.40q", tc.table, tc.id, n, got, tc.prefix)
		}
	}

	// A bd update --metadata style write with a long action.
	update := strings.Repeat("c", 400)
	if err := store.UpdateIssue(ctx, "fx-ea-object", map[string]interface{}{"metadata": `{"everturn":{"action":"` + update + `"}}`}, "tester"); err != nil {
		t.Fatalf("update fx-ea-object with a long action: %v", err)
	}
	if n, got := everturnActionLength(t, ctx, store.db, "issues", "fx-ea-object"); n != 255 || got != update[:255] {
		t.Errorf("everturn_action of fx-ea-object after update = %d chars %.40q..., want 255 c's", n, got)
	}
}

// TestForkIndexesEverturnActionColumnRepaired extends the drop-then-recreate
// case to the column: dropping issues.everturn_action drops its index too,
// and the next init restores both and commits them. Along the way it checks
// that both history readers (dolt_history_issues through SELECT *) still scan
// an issue whose history spans commits with and without the column.
func TestForkIndexesEverturnActionColumnRepaired(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	const spans = "fx-ea-history"
	if _, err := store.db.ExecContext(ctx, "INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type, metadata) VALUES ('"+spans+"', 'history spans the column', '', '', '', '', 'open', 2, 'task', '{\"everturn\":{\"action\":\"wake\"}}')"); err != nil {
		t.Fatalf("insert %s: %v", spans, err)
	}
	if _, err := store.db.ExecContext(ctx, "CALL DOLT_ADD('issues')"); err != nil {
		t.Fatalf("stage %s: %v", spans, err)
	}
	if _, err := store.db.ExecContext(ctx, "CALL DOLT_COMMIT('-m', 'test: issue before column drop')"); err != nil {
		t.Fatalf("commit %s: %v", spans, err)
	}

	if _, err := store.db.ExecContext(ctx, "ALTER TABLE issues DROP COLUMN everturn_action"); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	if issuesIndexExists(t, ctx, store.db, "idx_issues_everturn_action") {
		t.Fatalf("idx_issues_everturn_action survived dropping its column")
	}
	if _, err := store.db.ExecContext(ctx, "CALL DOLT_ADD('issues')"); err != nil {
		t.Fatalf("stage column drop: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, "CALL DOLT_COMMIT('-m', 'test: drop everturn_action')"); err != nil {
		t.Fatalf("commit column drop: %v", err)
	}
	if _, err := initSchemaOnDB(ctx, store.db); err != nil {
		t.Fatalf("initSchemaOnDB after column drop: %v", err)
	}
	if !tableColumnExists(t, ctx, store.db, "issues", "everturn_action") {
		t.Errorf("issues.everturn_action not restored after a committed drop")
	}
	if got, want := issuesIndexColumns(t, ctx, store.db, "idx_issues_everturn_action"), []string{"everturn_action", "created_at"}; !slices.Equal(got, want) {
		t.Errorf("idx_issues_everturn_action columns after restore = %v, want %v", got, want)
	}
	if n := doltStatusCount(t, ctx, store.db); n != 0 {
		t.Errorf("dolt_status has %d rows after the restoring init, want 0 (committed by the pass)", n)
	}
	if entries, err := store.History(ctx, spans); err != nil || len(entries) == 0 {
		t.Errorf("History(%s) across the column drop = %d entries, err %v; want entries, no error", spans, len(entries), err)
	}
	if entries, err := store.getIssueHistory(ctx, spans); err != nil || len(entries) == 0 {
		t.Errorf("getIssueHistory(%s) across the column drop = %d entries, err %v; want entries, no error", spans, len(entries), err)
	}
	applied, err := initSchemaOnDB(ctx, store.db)
	if err != nil {
		t.Fatalf("initSchemaOnDB after restore: %v", err)
	}
	if applied != 0 {
		t.Errorf("initSchemaOnDB after restore applied = %d, want 0", applied)
	}
	if n := doltStatusCount(t, ctx, store.db); n != 0 {
		t.Errorf("dolt_status has %d rows after a no-op init, want 0", n)
	}
}

func doltStatusRowsFor(t *testing.T, ctx context.Context, db *sql.DB, table string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_status WHERE table_name = ?", table).Scan(&count); err != nil {
		t.Fatalf("count dolt_status rows for %s: %v", table, err)
	}
	return count
}

// TestForkIndexesDirtyWispsStillHealed checks that the dirty skip is for
// committable tables only: a dirty wisps (dolt_ignored, never staged or
// committed) still gets its missing everturn_action column back, the wisp row
// survives, and issues stays clean. The contrasting committable case (dirty
// issues is skipped, then healed on a clean open) is
// TestForkIndexesIdempotentAndRepaired.
func TestForkIndexesDirtyWispsStillHealed(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	if _, err := store.db.ExecContext(ctx, "ALTER TABLE wisps DROP COLUMN everturn_action"); err != nil {
		t.Fatalf("drop wisps.everturn_action: %v", err)
	}
	wisp := &types.Issue{ID: "fx-dirty-wisp", Title: "dirty wisp", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask,
		NoHistory: true, Metadata: json.RawMessage(`{"everturn":{"action":"wake"}}`)}
	if err := store.CreateIssue(ctx, wisp, "tester"); err != nil {
		t.Fatalf("create no-history %s: %v", wisp.ID, err)
	}
	if n := doltStatusRowsFor(t, ctx, store.db, "wisps"); n == 0 {
		t.Fatalf("wisps is not dirty after creating a no-history issue; the case under test did not arise")
	}

	if _, err := initSchemaOnDB(ctx, store.db); err != nil {
		t.Fatalf("initSchemaOnDB with dirty wisps: %v", err)
	}
	if !tableColumnExists(t, ctx, store.db, "wisps", "everturn_action") {
		t.Errorf("wisps.everturn_action not restored while wisps was dirty")
	}
	var title, status string
	var noHistory bool
	var action sql.NullString
	if err := store.db.QueryRowContext(ctx, "SELECT title, status, no_history, everturn_action FROM wisps WHERE id = ?", wisp.ID).
		Scan(&title, &status, &noHistory, &action); err != nil {
		t.Fatalf("read wisp %s after init: %v", wisp.ID, err)
	}
	if title != wisp.Title || status != string(types.StatusOpen) || !noHistory || !action.Valid || action.String != "wake" {
		t.Errorf("wisp %s after init = (%q, %q, no_history=%v, everturn_action=%v), want (%q, open, true, wake)", wisp.ID, title, status, noHistory, action, wisp.Title)
	}
	if n := doltStatusRowsFor(t, ctx, store.db, "issues"); n != 0 {
		t.Errorf("issues has %d dolt_status rows after healing wisps, want 0", n)
	}
}

// allEverturnKeysMetadata is metadata carrying everturn.action and every mail
// key, the mail keys ending in suffix.
func allEverturnKeysMetadata(action, suffix string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"everturn":{"action":%q,"key":"k-%s","relay":{"source":"src-%s"},"bead":"bd-%s","corrects":"mail-%s"}}`,
		action, suffix, suffix, suffix, suffix))
}

// wantAllEverturnKeys is what the generated columns of a row written with
// allEverturnKeysMetadata(action, suffix) hold.
func wantAllEverturnKeys(action, suffix string) map[string]sql.NullString {
	v := func(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
	return map[string]sql.NullString{
		"everturn_action":       v(action),
		"everturn_key":          v("k-" + suffix),
		"everturn_relay_source": v("src-" + suffix),
		"everturn_bead":         v("bd-" + suffix),
		"everturn_corrects":     v("mail-" + suffix),
	}
}

// everturnKeysOf reads every everturn generated column of table.id.
func everturnKeysOf(t *testing.T, ctx context.Context, db *sql.DB, table, id string) map[string]sql.NullString {
	t.Helper()
	names := []string{"everturn_action"}
	for _, c := range everturnMailKeyColumns {
		names = append(names, c.name)
	}
	vals := make([]sql.NullString, len(names))
	dest := make([]any, len(names))
	for i := range vals {
		dest[i] = &vals[i]
	}
	//nolint:gosec // test-only identifiers
	if err := db.QueryRowContext(ctx, "SELECT "+strings.Join(names, ", ")+" FROM "+table+" WHERE id = ?", id).Scan(dest...); err != nil {
		t.Fatalf("read everturn columns of %s.%s: %v", table, id, err)
	}
	got := make(map[string]sql.NullString, len(names))
	for i, name := range names {
		got[name] = vals[i]
	}
	return got
}

func assertEverturnKeys(t *testing.T, ctx context.Context, db *sql.DB, table, id string, want map[string]sql.NullString) {
	t.Helper()
	got := everturnKeysOf(t, ctx, db, table, id)
	for name, w := range want {
		if g := got[name]; g != w {
			t.Errorf("%s.%s %s = %v, want %v", table, id, name, g, w)
		}
	}
}

// metadataAt is metadata holding the JSON text value at path ($.a.b.c).
func metadataAt(path, value string) string {
	parts := strings.Split(strings.TrimPrefix(path, "$."), ".")
	out := value
	for i := len(parts) - 1; i >= 0; i-- {
		out = fmt.Sprintf(`{%q:%s}`, parts[i], out)
	}
	return out
}

// seedEverturnMailKeyRows inserts n rows into table whose metadata carries
// every mail key (values cycling over three), created one minute apart, so
// the EXPLAIN cases plan against a populated table. Every fourth row has no
// keys, and every fifth an array key.
func seedEverturnMailKeyRows(t *testing.T, ctx context.Context, db *sql.DB, table string, n int) {
	t.Helper()
	const batch = 100
	base := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	for start := 0; start < n; start += batch {
		var values []string
		for i := start; i < n && i < start+batch; i++ {
			metadata := "{}"
			switch {
			case i%4 == 3:
			case i%5 == 4:
				metadata = fmt.Sprintf(`{"everturn":{"key":["k%d","k%d"]}}`, i%3, i%3+1)
			default:
				metadata = string(allEverturnKeysMetadata("wake", fmt.Sprint(i%3)))
			}
			createdAt := base.Add(-time.Duration(i) * time.Minute).Format("2006-01-02 15:04:05")
			values = append(values, fmt.Sprintf("('fx-mail-%s-%d', 'mail fixture', '', '', '', '', 'open', 2, 'task', '%s', '%s')", table, i, metadata, createdAt))
		}
		//nolint:gosec // test-only identifiers
		if _, err := db.ExecContext(ctx, "INSERT INTO "+table+" (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type, metadata, created_at) VALUES "+strings.Join(values, ", ")); err != nil {
			t.Fatalf("seed mail key rows into %s: %v", table, err)
		}
	}
}

// assertEverturnForkSchema checks that every everturn generated column is on
// issues and wisps and every fork index on its table, (column, created_at)
// for the everturn ones.
func assertEverturnForkSchema(t *testing.T, ctx context.Context, db *sql.DB, when string) {
	t.Helper()
	for _, table := range forkTables {
		if !tableColumnExists(t, ctx, db, table, "everturn_action") {
			t.Errorf("%s: %s lacks everturn_action", when, table)
		}
		for _, c := range everturnMailKeyColumns {
			if !tableColumnExists(t, ctx, db, table, c.name) {
				t.Errorf("%s: %s lacks %s", when, table, c.name)
			}
			index := "idx_" + table + "_" + c.name
			if got, want := tableIndexColumns(t, ctx, db, table, index), []string{c.name, "created_at"}; !slices.Equal(got, want) {
				t.Errorf("%s: %s columns on %s = %v, want %v", when, index, table, got, want)
			}
		}
	}
	for _, name := range forkIndexNames {
		if !issuesIndexExists(t, ctx, db, name) {
			t.Errorf("%s: issues lacks %s", when, name)
		}
	}
}

// TestForkIndexesEverturnMailKeysPresentAndUsed checks the four mail key
// columns: on issues and wisps after an open, each indexed on both as
// (column, created_at), and read through that index by et's ids-first reader
// shape and by everturn_key's array-prefix LIKE.
func TestForkIndexesEverturnMailKeysPresentAndUsed(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	assertEverturnForkSchema(t, ctx, store.db, "after schema init")
	for _, table := range forkTables {
		seedEverturnMailKeyRows(t, ctx, store.db, table, 300)
	}

	for _, table := range forkTables {
		for _, c := range everturnMailKeyColumns {
			index := "[" + table + "." + c.name + "," + table + ".created_at]"
			value := map[string]string{
				"everturn_key": "k-1", "everturn_relay_source": "src-1", "everturn_bead": "bd-1", "everturn_corrects": "mail-1",
			}[c.name]
			queries := []string{
				fmt.Sprintf("SELECT m.id FROM (SELECT id FROM %s WHERE %s = '%s') k JOIN %s m ON m.id = k.id", table, c.name, value, table),
			}
			if c.name == "everturn_key" {
				queries = append(queries, fmt.Sprintf("SELECT id FROM %s WHERE everturn_key LIKE '[%%'", table))
			}
			for _, query := range queries {
				plan := explainPlan(t, ctx, store.db, query)
				t.Logf("EXPLAIN FORMAT=TREE %s\n%s", query, plan)
				if !strings.Contains(plan, "IndexedTableAccess") || !strings.Contains(plan, index) {
					t.Errorf("%s: want IndexedTableAccess on %s, plan:\n%s", query, index, plan)
				}
			}
		}
	}
	// The array-prefix LIKE finds exactly the array keys.
	for _, table := range forkTables {
		var n int
		//nolint:gosec // test-only identifiers
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE everturn_key LIKE '[%'").Scan(&n); err != nil {
			t.Fatalf("count array keys on %s: %v", table, err)
		}
		if want := 45; n != want { // i%5 == 4 and i%4 != 3, i < 300
			t.Errorf("%s rows with an array everturn_key = %d, want %d", table, n, want)
		}
	}
}

// TestForkIndexesEverturnMailKeyValues checks what each mail key column stores,
// on issues and on wisps, through the store: a string key, a missing key
// (NULL), an array key (its JSON text), a 300-character value (its first 255),
// relay.source read from the nested path, and a metadata update recomputing it.
func TestForkIndexesEverturnMailKeyValues(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	long := strings.Repeat("x", 300)
	for _, table := range forkTables {
		for _, c := range everturnMailKeyColumns {
			for _, tc := range []struct {
				name, metadata string
				want           sql.NullString
			}{
				{name: "string", metadata: metadataAt(c.path, `"v1"`), want: sql.NullString{String: "v1", Valid: true}},
				{name: "missing", metadata: `{"everturn":{"action":"wake"}}`},
				{name: "array", metadata: metadataAt(c.path, `["k1","k2"]`), want: sql.NullString{String: `["k1", "k2"]`, Valid: true}},
				{name: "long", metadata: metadataAt(c.path, `"`+long+`"`), want: sql.NullString{String: long[:255], Valid: true}},
			} {
				id := fmt.Sprintf("fx-mk-%s-%s-%s", table, strings.TrimPrefix(c.name, "everturn_"), tc.name)
				iss := &types.Issue{ID: id, Title: "mail key value", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask,
					Ephemeral: table == "wisps", Metadata: json.RawMessage(tc.metadata)}
				if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
					t.Errorf("create %s.%s: %v", table, id, err)
					continue
				}
				assertEverturnKeys(t, ctx, store.db, table, id, map[string]sql.NullString{c.name: tc.want})
			}

			id := fmt.Sprintf("fx-mk-%s-%s-string", table, strings.TrimPrefix(c.name, "everturn_"))
			if err := store.UpdateIssue(ctx, id, map[string]interface{}{"metadata": metadataAt(c.path, `"v2"`)}, "tester"); err != nil {
				t.Fatalf("update metadata of %s.%s: %v", table, id, err)
			}
			assertEverturnKeys(t, ctx, store.db, table, id, map[string]sql.NullString{c.name: {String: "v2", Valid: true}})
		}
	}
}

// TestForkIndexesEverturnMailKeysPromotion checks the wisp promotion to issues
// with every everturn column present on both tables: the promoted row's
// columns hold the wisp's keys.
func TestForkIndexesEverturnMailKeysPromotion(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	wisp := &types.Issue{ID: "fx-mk-promote", Title: "promoted mail", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask,
		Ephemeral: true, Metadata: allEverturnKeysMetadata("wake", "p")}
	if err := store.CreateIssue(ctx, wisp, "tester"); err != nil {
		t.Fatalf("create wisp: %v", err)
	}
	assertEverturnKeys(t, ctx, store.db, "wisps", wisp.ID, wantAllEverturnKeys("wake", "p"))
	if err := store.PromoteFromEphemeral(ctx, wisp.ID, "tester"); err != nil {
		t.Fatalf("PromoteFromEphemeral: %v", err)
	}
	assertEverturnKeys(t, ctx, store.db, "issues", wisp.ID, wantAllEverturnKeys("wake", "p"))
	var left int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM wisps WHERE id = ?", wisp.ID).Scan(&left); err != nil {
		t.Fatalf("count wisp after promote: %v", err)
	}
	if left != 0 {
		t.Errorf("wisp %s still in wisps after promotion", wisp.ID)
	}
}

func doltHead(t *testing.T, ctx context.Context, db *sql.DB) string {
	t.Helper()
	var head string
	if err := db.QueryRowContext(ctx, "SELECT DOLT_HASHOF('HEAD')").Scan(&head); err != nil {
		t.Fatalf("read HEAD: %v", err)
	}
	return head
}

func showCreateTable(t *testing.T, ctx context.Context, db *sql.DB, table string) string {
	t.Helper()
	var name, ddl string
	//nolint:gosec // test-only identifiers
	if err := db.QueryRowContext(ctx, "SHOW CREATE TABLE `"+table+"`").Scan(&name, &ddl); err != nil {
		t.Fatalf("SHOW CREATE TABLE %s: %v", table, err)
	}
	return ddl
}

func execAll(t *testing.T, ctx context.Context, db *sql.DB, queries ...string) {
	t.Helper()
	for _, q := range queries {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// TestForkIndexesEverturnMailKeysRepaired checks that a dropped mail key column
// (its index goes with it) and a dropped mail key index come back at the next
// writable open, on issues and on wisps, and that an open of a database that
// has them all runs no ALTER: no commit, nothing dirty, wisps unchanged.
func TestForkIndexesEverturnMailKeysRepaired(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	execAll(t, ctx, store.db,
		"ALTER TABLE issues DROP COLUMN everturn_key",
		"ALTER TABLE issues DROP INDEX idx_issues_everturn_bead",
		"CALL DOLT_ADD('issues')",
		"CALL DOLT_COMMIT('-m', 'test: drop a mail key column and index on issues')",
		"ALTER TABLE wisps DROP COLUMN everturn_corrects",
		"ALTER TABLE wisps DROP INDEX idx_wisps_everturn_relay_source",
	)
	if tableColumnExists(t, ctx, store.db, "issues", "everturn_key") || tableIndexExists(t, ctx, store.db, "issues", "idx_issues_everturn_key") ||
		tableColumnExists(t, ctx, store.db, "wisps", "everturn_corrects") || tableIndexExists(t, ctx, store.db, "wisps", "idx_wisps_everturn_corrects") {
		t.Fatalf("a dropped column or its index survived; the case under test did not arise")
	}
	if _, err := initSchemaOnDB(ctx, store.db); err != nil {
		t.Fatalf("initSchemaOnDB after drops: %v", err)
	}
	assertEverturnForkSchema(t, ctx, store.db, "after the repairing open")
	// wisps is clone-local and stays dirty; issues is committed by the pass.
	if n := doltStatusRowsFor(t, ctx, store.db, "issues"); n != 0 {
		t.Errorf("issues has %d dolt_status rows after the repairing open, want 0 (committed by the pass)", n)
	}

	head, wisps := doltHead(t, ctx, store.db), showCreateTable(t, ctx, store.db, "wisps")
	applied, err := initSchemaOnDB(ctx, store.db)
	if err != nil {
		t.Fatalf("initSchemaOnDB on a repaired database: %v", err)
	}
	if applied != 0 {
		t.Errorf("initSchemaOnDB on a repaired database applied = %d, want 0", applied)
	}
	if got := doltHead(t, ctx, store.db); got != head {
		t.Errorf("an open with nothing to do committed: HEAD %s -> %s", head, got)
	}
	if got := showCreateTable(t, ctx, store.db, "wisps"); got != wisps {
		t.Errorf("an open with nothing to do changed wisps:\nbefore %s\nafter %s", wisps, got)
	}
	if n := doltStatusRowsFor(t, ctx, store.db, "issues"); n != 0 {
		t.Errorf("issues has %d dolt_status rows after a no-op open, want 0", n)
	}
}

// TestForkIndexesEverturnMailKeysBackfilled is a database written by the build
// before the mail key columns (columns absent on issues and wisps), then
// opened by this one: the open adds the columns and the rows written before
// get their values.
func TestForkIndexesEverturnMailKeysBackfilled(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	for _, table := range forkTables {
		for _, c := range everturnMailKeyColumns {
			execAll(t, ctx, store.db, "ALTER TABLE "+table+" DROP COLUMN "+c.name)
		}
	}
	execAll(t, ctx, store.db, "CALL DOLT_ADD('issues')", "CALL DOLT_COMMIT('-m', 'test: the schema before the mail key columns')")
	for _, table := range forkTables {
		if tableColumnExists(t, ctx, store.db, table, "everturn_key") {
			t.Fatalf("%s.everturn_key survived its drop; the case under test did not arise", table)
		}
		//nolint:gosec // test-only identifiers
		execAll(t, ctx, store.db, "INSERT INTO "+table+" (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type, metadata) VALUES "+
			"('fx-mk-old-"+table+"', 'written before', '', '', '', '', 'open', 2, 'task', '"+string(allEverturnKeysMetadata("wake", "old"))+"'), "+
			"('fx-mk-old-none-"+table+"', 'written before, no keys', '', '', '', '', 'open', 2, 'task', '{}')")
	}
	execAll(t, ctx, store.db, "CALL DOLT_ADD('issues')", "CALL DOLT_COMMIT('-m', 'test: rows written before the mail key columns')")

	if _, err := initSchemaOnDB(ctx, store.db); err != nil {
		t.Fatalf("initSchemaOnDB on the older database: %v", err)
	}
	assertEverturnForkSchema(t, ctx, store.db, "after the upgrading open")
	for _, table := range forkTables {
		assertEverturnKeys(t, ctx, store.db, table, "fx-mk-old-"+table, wantAllEverturnKeys("wake", "old"))
		assertEverturnKeys(t, ctx, store.db, table, "fx-mk-old-none-"+table, map[string]sql.NullString{
			"everturn_key": {}, "everturn_relay_source": {}, "everturn_bead": {}, "everturn_corrects": {},
		})
	}
	if n := doltStatusRowsFor(t, ctx, store.db, "issues"); n != 0 {
		t.Errorf("issues has %d dolt_status rows after the upgrading open, want 0 (committed by the pass)", n)
	}
}

// TestForkIndexesWispsMadeAgain: wisps made again from its definition without
// the fork columns (as its numbered migrations define it) gets every fork
// column and index back at the next open. The open of a database with no
// wisps table at all is TestForkIndexesSkipAbsentWisps (schema package): a
// dropped wisps cannot reach the fork step live, the ignored series' dirty
// guard refuses the pass first.
func TestForkIndexesWispsMadeAgain(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	execAll(t, ctx, store.db, "ALTER TABLE wisps DROP COLUMN everturn_action")
	for _, c := range everturnMailKeyColumns {
		execAll(t, ctx, store.db, "ALTER TABLE wisps DROP COLUMN "+c.name)
	}
	definition := showCreateTable(t, ctx, store.db, "wisps")
	if strings.Contains(definition, "everturn_") {
		t.Fatalf("wisps definition still has a fork column:\n%s", definition)
	}
	// The aux wisp tables reference wisps, so drop and make it with the
	// checks off (the store's pool is one session).
	execAll(t, ctx, store.db, "SET FOREIGN_KEY_CHECKS = 0", "DROP TABLE wisps", definition, "SET FOREIGN_KEY_CHECKS = 1")
	if tableColumnExists(t, ctx, store.db, "wisps", "everturn_key") {
		t.Fatalf("the remade wisps has everturn_key; the case under test did not arise")
	}
	if _, err := initSchemaOnDB(ctx, store.db); err != nil {
		t.Fatalf("initSchemaOnDB after wisps is made again: %v", err)
	}
	assertEverturnForkSchema(t, ctx, store.db, "after wisps is made again")
}

// forkMailKeyStatements are the fork step's statements for issues on a
// database of the previous build (1.3.5-dev.1: everturn_action and its index
// present, the four mail key columns absent), in the step's order: the four
// columns, then their four indexes.
func forkMailKeyStatements() []string {
	var out []string
	for _, c := range everturnMailKeyColumns {
		out = append(out, "ALTER TABLE issues ADD COLUMN "+c.name+" VARCHAR(255) GENERATED ALWAYS AS (LEFT(JSON_UNQUOTE(JSON_EXTRACT(metadata, '"+c.path+"')), 255)) STORED")
	}
	for _, c := range everturnMailKeyColumns {
		out = append(out, "ALTER TABLE issues ADD INDEX idx_issues_"+c.name+" ("+c.name+", created_at)")
	}
	return out
}

const (
	forkSeedKeys = "fx-int-keys" // committed before the upgrade, every key set
	forkSeedNone = "fx-int-none" // committed before the upgrade, no key
)

// toForkBuildSchema makes the store's schema that of an older build, drops
// every column in drop on issues and on wisps (an index goes with its
// column), and commits issues with two seed rows, so issues is clean.
func toForkBuildSchema(t *testing.T, ctx context.Context, db *sql.DB, drop []string) {
	t.Helper()
	for _, table := range forkTables {
		for _, c := range drop {
			//nolint:gosec // test-only identifiers
			execAll(t, ctx, db, "ALTER TABLE "+table+" DROP COLUMN "+c)
		}
	}
	execAll(t, ctx, db,
		"INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type, metadata) VALUES "+
			"('"+forkSeedKeys+"', 'before the upgrade', '', '', '', '', 'open', 2, 'task', '"+string(allEverturnKeysMetadata("wake", "seed"))+"'), "+
			"('"+forkSeedNone+"', 'before the upgrade, no keys', '', '', '', '', 'open', 2, 'task', '{}')",
		"CALL DOLT_ADD('issues')",
		"CALL DOLT_COMMIT('-m', 'test: the schema of an older build')",
	)
	if n := doltStatusRowsFor(t, ctx, db, "issues"); n != 0 {
		t.Fatalf("issues has %d dolt_status rows on the older build's schema, want 0", n)
	}
}

// toPreviousBuildSchema is the schema of 1.3.5-dev.1: no mail key columns.
func toPreviousBuildSchema(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	var drop []string
	for _, c := range everturnMailKeyColumns {
		drop = append(drop, c.name)
	}
	toForkBuildSchema(t, ctx, db, drop)
}

func doltLogCount(t *testing.T, ctx context.Context, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_log").Scan(&n); err != nil {
		t.Fatalf("count dolt_log: %v", err)
	}
	return n
}

// forkInterruptedCases are the states an open killed inside the fork step can
// leave on a clean issues: the first k of this build's statements applied and
// uncommitted, for every k but the last, and the state 1.3.5-dev.1 can leave
// (everturn_action added, its index not yet).
func forkInterruptedCases() []struct {
	name    string
	prepare func(t *testing.T, ctx context.Context, db *sql.DB)
} {
	stmts := forkMailKeyStatements()
	var cases []struct {
		name    string
		prepare func(t *testing.T, ctx context.Context, db *sql.DB)
	}
	for k := 1; k < len(stmts); k++ {
		applied := stmts[:k]
		cases = append(cases, struct {
			name    string
			prepare func(t *testing.T, ctx context.Context, db *sql.DB)
		}{fmt.Sprintf("k=%d", k), func(t *testing.T, ctx context.Context, db *sql.DB) {
			toPreviousBuildSchema(t, ctx, db)
			execAll(t, ctx, db, applied...)
		}})
	}
	cases = append(cases, struct {
		name    string
		prepare func(t *testing.T, ctx context.Context, db *sql.DB)
	}{"dev.1 everturn_action without its index", func(t *testing.T, ctx context.Context, db *sql.DB) {
		drop := []string{"everturn_action"}
		for _, c := range everturnMailKeyColumns {
			drop = append(drop, c.name)
		}
		toForkBuildSchema(t, ctx, db, drop)
		execAll(t, ctx, db, "ALTER TABLE issues ADD COLUMN everturn_action VARCHAR(255) GENERATED ALWAYS AS (LEFT(JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.everturn.action')), 255)) STORED")
	}})
	return cases
}

// TestForkIndexesInterruptedUpgradeFinished (bd-gcmmv, review bd-suuqd): an
// open killed between two of the fork step's statements leaves issues dirty
// with only the step's own DDL. The next writable open must take that for
// its own work, not a user's: it adds what is missing, stages and commits
// issues as an uninterrupted pass does (HEAD moves once, issues clean), and
// the open after that runs no DDL and finds every column and index present.
func TestForkIndexesInterruptedUpgradeFinished(t *testing.T) {
	for _, c := range forkInterruptedCases() {
		t.Run(c.name, func(t *testing.T) {
			store, cleanup := setupTestStore(t)
			defer cleanup()
			ctx, cancel := testContext(t)
			defer cancel()

			c.prepare(t, ctx, store.db)
			if n := doltStatusRowsFor(t, ctx, store.db, "issues"); n == 0 {
				t.Fatalf("issues is clean after the interrupted statements; the case under test did not arise")
			}
			commits := doltLogCount(t, ctx, store.db)
			if _, err := rerunSchemaInitOnTestStore(ctx, store); err != nil {
				t.Fatalf("open after the interrupted step: %v", err)
			}
			assertEverturnForkSchema(t, ctx, store.db, c.name+": after the resuming open")
			if n := doltStatusRowsFor(t, ctx, store.db, "issues"); n != 0 {
				t.Errorf("%s: issues has %d dolt_status rows after the resuming open, want 0 (committed by the pass)", c.name, n)
			}
			if got := doltLogCount(t, ctx, store.db); got != commits+1 {
				t.Errorf("%s: the resuming open made %d commits, want 1", c.name, got-commits)
			}
			assertEverturnKeys(t, ctx, store.db, "issues", forkSeedKeys, wantAllEverturnKeys("wake", "seed"))
			assertEverturnKeys(t, ctx, store.db, "issues", forkSeedNone, map[string]sql.NullString{
				"everturn_action": {}, "everturn_key": {}, "everturn_relay_source": {}, "everturn_bead": {}, "everturn_corrects": {},
			})

			head, issues, wisps := doltHead(t, ctx, store.db), showCreateTable(t, ctx, store.db, "issues"), showCreateTable(t, ctx, store.db, "wisps")
			applied, err := rerunSchemaInitOnTestStore(ctx, store)
			if err != nil {
				t.Fatalf("%s: open after the resuming open: %v", c.name, err)
			}
			if applied != 0 {
				t.Errorf("%s: open after the resuming open applied = %d, want 0", c.name, applied)
			}
			if got := doltHead(t, ctx, store.db); got != head {
				t.Errorf("%s: the open after the resuming open committed: HEAD %s -> %s", c.name, head, got)
			}
			if got := showCreateTable(t, ctx, store.db, "issues"); got != issues {
				t.Errorf("%s: the open after the resuming open changed issues:\nbefore %s\nafter %s", c.name, issues, got)
			}
			if got := showCreateTable(t, ctx, store.db, "wisps"); got != wisps {
				t.Errorf("%s: the open after the resuming open changed wisps:\nbefore %s\nafter %s", c.name, wisps, got)
			}
			if n := doltStatusRowsFor(t, ctx, store.db, "issues"); n != 0 {
				t.Errorf("%s: issues has %d dolt_status rows after a no-op open, want 0", c.name, n)
			}
		})
	}
}

// forkUserRowChange is a user's uncommitted row change on issues: one seed
// row retitled and one row added.
const forkUserRowID = "fx-int-user"

func forkUserRowChange(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	execAll(t, ctx, db,
		"UPDATE issues SET title = 'retitled by the user' WHERE id = '"+forkSeedNone+"'",
		"INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type, metadata) VALUES "+
			"('"+forkUserRowID+"', 'added by the user', '', '', '', '', 'open', 2, 'task', '"+string(allEverturnKeysMetadata("wake", "user"))+"')",
	)
}

// assertForkUserRowsUncommitted checks the user's change of forkUserRowChange
// is in the working set, unchanged, and not at HEAD.
func assertForkUserRowsUncommitted(t *testing.T, ctx context.Context, db *sql.DB, when string) {
	t.Helper()
	var title string
	if err := db.QueryRowContext(ctx, "SELECT title FROM issues WHERE id = ?", forkSeedNone).Scan(&title); err != nil {
		t.Fatalf("%s: read %s: %v", when, forkSeedNone, err)
	}
	if title != "retitled by the user" {
		t.Errorf("%s: %s title = %q, want the user's %q", when, forkSeedNone, title, "retitled by the user")
	}
	var userTitle string
	if err := db.QueryRowContext(ctx, "SELECT title FROM issues WHERE id = ?", forkUserRowID).Scan(&userTitle); err != nil {
		t.Errorf("%s: the user's row %s: %v", when, forkUserRowID, err)
	} else if userTitle != "added by the user" {
		t.Errorf("%s: %s title = %q, want %q", when, forkUserRowID, userTitle, "added by the user")
	}
	var headTitle string
	if err := db.QueryRowContext(ctx, "SELECT title FROM issues AS OF 'HEAD' WHERE id = ?", forkSeedNone).Scan(&headTitle); err != nil {
		t.Fatalf("%s: read %s at HEAD: %v", when, forkSeedNone, err)
	}
	if headTitle != "before the upgrade, no keys" {
		t.Errorf("%s: %s title at HEAD = %q: the user's change was committed", when, forkSeedNone, headTitle)
	}
	var atHead int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM issues AS OF 'HEAD' WHERE id = ?", forkUserRowID).Scan(&atHead); err != nil {
		t.Fatalf("%s: count %s at HEAD: %v", when, forkUserRowID, err)
	}
	if atHead != 0 {
		t.Errorf("%s: the user's row %s is at HEAD: it was committed", when, forkUserRowID)
	}
}

// TestForkIndexesUserRowsStillSkipped: a user's uncommitted rows alone on
// issues, no fork statement applied, still make the step skip issues and
// leave it untouched (the next open after the user's commit adds the columns).
func TestForkIndexesUserRowsStillSkipped(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	toPreviousBuildSchema(t, ctx, store.db)
	forkUserRowChange(t, ctx, store.db)
	issues := showCreateTable(t, ctx, store.db, "issues")
	if _, err := rerunSchemaInitOnTestStore(ctx, store); err != nil {
		t.Fatalf("open with a user's uncommitted rows: %v", err)
	}
	if got := showCreateTable(t, ctx, store.db, "issues"); got != issues {
		t.Errorf("the step changed issues under a user's uncommitted rows:\nbefore %s\nafter %s", issues, got)
	}
	for _, c := range everturnMailKeyColumns {
		if tableColumnExists(t, ctx, store.db, "issues", c.name) {
			t.Errorf("issues.%s added under a user's uncommitted rows", c.name)
		}
	}
	assertForkUserRowsUncommitted(t, ctx, store.db, "after the skipping open")

	execAll(t, ctx, store.db, "CALL DOLT_ADD('issues')", "CALL DOLT_COMMIT('-m', 'test: the user commits')")
	if _, err := rerunSchemaInitOnTestStore(ctx, store); err != nil {
		t.Fatalf("open after the user's commit: %v", err)
	}
	assertEverturnForkSchema(t, ctx, store.db, "after the user's commit")
	if n := doltStatusRowsFor(t, ctx, store.db, "issues"); n != 0 {
		t.Errorf("issues has %d dolt_status rows after the open that follows the user's commit, want 0", n)
	}
}

// TestForkIndexesInterruptedUpgradeUnderUserRows is an interrupted step with
// a user's uncommitted row change on top. Chosen behaviour (bd-gcmmv item 3):
// the open adds the missing columns and indexes so reads work, neither stages
// nor commits issues (HEAD does not move), and leaves the user's rows
// uncommitted and unchanged; the open after the user's commit has nothing to
// do.
func TestForkIndexesInterruptedUpgradeUnderUserRows(t *testing.T) {
	for _, c := range forkInterruptedCases() {
		t.Run(c.name, func(t *testing.T) {
			store, cleanup := setupTestStore(t)
			defer cleanup()
			ctx, cancel := testContext(t)
			defer cancel()

			c.prepare(t, ctx, store.db)
			forkUserRowChange(t, ctx, store.db)
			head := doltHead(t, ctx, store.db)
			if _, err := rerunSchemaInitOnTestStore(ctx, store); err != nil {
				t.Fatalf("%s: open over the interrupted step and a user's rows: %v", c.name, err)
			}
			assertEverturnForkSchema(t, ctx, store.db, c.name+": after the open over a user's rows")
			var source sql.NullString
			if err := store.db.QueryRowContext(ctx, "SELECT everturn_relay_source FROM issues WHERE id = ?", forkUserRowID).Scan(&source); err != nil {
				t.Errorf("%s: read by everturn_relay_source after the open: %v", c.name, err)
			}
			assertEverturnKeys(t, ctx, store.db, "issues", forkUserRowID, wantAllEverturnKeys("wake", "user"))
			assertEverturnKeys(t, ctx, store.db, "issues", forkSeedKeys, wantAllEverturnKeys("wake", "seed"))
			if got := doltHead(t, ctx, store.db); got != head {
				t.Errorf("%s: the open committed over a user's uncommitted rows: HEAD %s -> %s", c.name, head, got)
			}
			if n := doltStatusRowsFor(t, ctx, store.db, "issues"); n == 0 {
				t.Errorf("%s: issues is clean after the open: the user's rows were committed", c.name)
			}
			assertForkUserRowsUncommitted(t, ctx, store.db, c.name+": after the open")

			execAll(t, ctx, store.db, "CALL DOLT_ADD('issues')", "CALL DOLT_COMMIT('-m', 'test: the user commits')")
			head, issues := doltHead(t, ctx, store.db), showCreateTable(t, ctx, store.db, "issues")
			applied, err := rerunSchemaInitOnTestStore(ctx, store)
			if err != nil {
				t.Fatalf("%s: open after the user's commit: %v", c.name, err)
			}
			if applied != 0 {
				t.Errorf("%s: open after the user's commit applied = %d, want 0", c.name, applied)
			}
			if got := doltHead(t, ctx, store.db); got != head {
				t.Errorf("%s: the open after the user's commit committed: HEAD %s -> %s", c.name, head, got)
			}
			if got := showCreateTable(t, ctx, store.db, "issues"); got != issues {
				t.Errorf("%s: the open after the user's commit changed issues:\nbefore %s\nafter %s", c.name, issues, got)
			}
			if n := doltStatusRowsFor(t, ctx, store.db, "issues"); n != 0 {
				t.Errorf("%s: issues has %d dolt_status rows after the open that follows the user's commit, want 0", c.name, n)
			}
		})
	}
}

// startForkGateProxy forwards each connection to upstream and calls gate with
// the text of each client command (as startBudgetProxy describes it) before
// forwarding it; a false answer drops the connection there, unforwarded.
func startForkGateProxy(t *testing.T, upstream string, gate func(desc string) bool) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("gate proxy listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			go func(client net.Conn) {
				defer func() { _ = client.Close() }()
				server, err := net.Dial("tcp", upstream)
				if err != nil {
					return
				}
				defer func() { _ = server.Close() }()
				go func() {
					_, _ = io.Copy(client, server)
					_ = client.Close()
				}()
				for {
					seq, payload, raw, err := readBudgetPacket(client)
					if err != nil {
						return
					}
					if seq == 0 && len(payload) > 0 && (payload[0] == 0x03 || payload[0] == 0x16) {
						if !gate(strings.Join(strings.Fields(string(payload[1:])), " ")) {
							return
						}
					}
					if _, err := server.Write(raw); err != nil {
						return
					}
				}
			}(client)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestForkIndexesCancelledUpgradeFinishedByWaiter is the reviewer's sequence
// (bd-suuqd): the first writable open of a 1.3.5-dev.1 database adds
// issues.everturn_key and is cancelled before its next ALTER, while it holds
// the migration lock and a second writable open waits for it. The waiter
// takes the lock when the first open's session goes, finds the step's own
// uncommitted DDL, and returns the complete schema, committed; a read of
// issues by everturn_relay_source succeeds.
func TestForkIndexesCancelledUpgradeFinishedByWaiter(t *testing.T) {
	store, cleanup := setupConcurrentTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	toPreviousBuildSchema(t, ctx, store.db)
	upstream := net.JoinHostPort("127.0.0.1", strconv.Itoa(testServerPort))
	openConfig := func(port int) *Config {
		return &Config{Path: t.TempDir(), Database: store.database, ServerHost: "127.0.0.1", ServerPort: port,
			ServerUser: "root", CommitterName: "test", CommitterEmail: "test@example.com"}
	}
	open := func(ctx context.Context, cfg *Config) <-chan error {
		done := make(chan error, 1)
		go func() {
			s, err := New(ctx, cfg)
			if err == nil {
				err = s.Close()
			}
			done <- err
		}()
		return done
	}

	firstCtx, cancelFirst := context.WithCancel(ctx)
	defer cancelFirst()
	atSecondALTER := make(chan struct{})
	var atSecondOnce sync.Once
	firstPort := startForkGateProxy(t, upstream, func(desc string) bool {
		if !strings.HasPrefix(desc, "ALTER TABLE issues ADD COLUMN everturn_relay_source ") {
			return true
		}
		atSecondOnce.Do(func() { close(atSecondALTER) })
		<-firstCtx.Done()
		return false
	})
	first := open(firstCtx, openConfig(firstPort))
	select {
	case <-atSecondALTER:
	case err := <-first:
		t.Fatalf("the first open returned before its second ALTER: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !tableColumnExists(t, ctx, store.db, "issues", "everturn_key") || tableColumnExists(t, ctx, store.db, "issues", "everturn_relay_source") {
		t.Fatalf("the first open did not stop after exactly its first ALTER")
	}
	var free int
	if err := store.db.QueryRowContext(ctx, "SELECT IS_FREE_LOCK(?)", schema.MigrationLockName(store.database)).Scan(&free); err != nil || free != 0 {
		t.Fatalf("the first open does not hold the migration lock: IS_FREE_LOCK = %d, %v", free, err)
	}

	waiting := make(chan struct{})
	var waitingOnce sync.Once
	secondPort := startForkGateProxy(t, upstream, func(desc string) bool {
		if strings.Contains(desc, "GET_LOCK(") {
			waitingOnce.Do(func() { close(waiting) })
		}
		return true
	})
	second := open(ctx, openConfig(secondPort))
	select {
	case <-waiting:
	case err := <-second:
		t.Fatalf("the second open returned before it asked for the migration lock: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	cancelFirst()
	if err := <-first; err == nil {
		t.Errorf("the cancelled first open returned no error")
	} else {
		t.Logf("cancelled first open: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("the waiting open: %v", err)
	}
	assertEverturnForkSchema(t, ctx, store.db, "after the waiting open")
	if n := doltStatusRowsFor(t, ctx, store.db, "issues"); n != 0 {
		t.Errorf("issues has %d dolt_status rows after the waiting open, want 0 (committed by its pass)", n)
	}
	var source sql.NullString
	if err := store.db.QueryRowContext(ctx, "SELECT everturn_relay_source FROM issues WHERE id = ?", forkSeedKeys).Scan(&source); err != nil {
		t.Fatalf("read issues by everturn_relay_source: %v", err)
	}
	if !source.Valid || source.String != "src-seed" {
		t.Errorf("everturn_relay_source of %s = %v, want src-seed", forkSeedKeys, source)
	}
}

// assertForkCommittedOnce checks the state a pass that commits the fork step
// leaves: every column and index present, issues out of dolt_status, HEAD
// moved from head by exactly one commit, and a read by everturn_relay_source.
func assertForkCommittedOnce(t *testing.T, ctx context.Context, db *sql.DB, when, head string, commits int) {
	t.Helper()
	assertEverturnForkSchema(t, ctx, db, when)
	if got := doltHead(t, ctx, db); got == head {
		t.Errorf("%s: HEAD did not move: the step's DDL was not committed", when)
	}
	if got := doltLogCount(t, ctx, db); got != commits+1 {
		t.Errorf("%s: %d new commits, want 1", when, got-commits)
	}
	if n := doltStatusRowsFor(t, ctx, db, "issues"); n != 0 {
		t.Errorf("%s: issues has %d dolt_status rows, want 0", when, n)
	}
	for _, c := range everturnMailKeyColumns {
		var n int
		//nolint:gosec // test-only identifiers
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM issues AS OF 'HEAD' WHERE "+c.name+" IS NOT NULL").Scan(&n); err != nil {
			t.Errorf("%s: HEAD lacks issues.%s: %v", when, c.name, err)
		}
	}
	var source sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT everturn_relay_source FROM issues WHERE id = ?", forkSeedKeys).Scan(&source); err != nil || source.String != "src-seed" {
		t.Errorf("%s: everturn_relay_source of %s = %v, %v; want src-seed", when, forkSeedKeys, source, err)
	}
}

// TestForkIndexesCommitBoundaryFinished (bd-7bqv6, review bd-313ml finding
// 1): an upgrade cancelled after every statement of the step, before
// CALL DOLT_COMMIT('-m', 'schema: apply migrations'), leaves a complete
// working schema and HEAD without it, issues dirty. The working table looks
// converged, so the gate (needsForkIndexes) reads HEAD as well: the next
// writable open commits issues once, and the open after it does nothing.
// Two shapes: the real cancel at the commit (issues staged, through a proxy
// that holds the commit), and every statement applied by hand, unstaged.
//
// It is also the test that turns red when a Dolt version prints a fork column
// or index line in a form normalizeDefinition does not know: the classifier
// then reads the step's own line as a foreign definition, skips issues, and
// HEAD does not move.
func TestForkIndexesCommitBoundaryFinished(t *testing.T) {
	reopenTwice := func(t *testing.T, ctx context.Context, store *DoltStore, head string, commits int) {
		t.Helper()
		cfg := &Config{Path: t.TempDir(), Database: store.database, ServerHost: "127.0.0.1", ServerPort: testServerPort,
			ServerUser: "root", CommitterName: "test", CommitterEmail: "test@example.com"}
		for attempt := 1; attempt <= 2; attempt++ {
			s, err := New(ctx, cfg)
			if err != nil {
				t.Fatalf("writable open %d after the lost commit: %v", attempt, err)
			}
			if err := s.Close(); err != nil {
				t.Fatalf("close open %d: %v", attempt, err)
			}
			assertForkCommittedOnce(t, ctx, store.db, fmt.Sprintf("after writable open %d", attempt), head, commits)
		}
	}

	t.Run("cancelled at the commit, staged", func(t *testing.T) {
		store, cleanup := setupConcurrentTestStore(t)
		defer cleanup()
		ctx, cancel := testContext(t)
		defer cancel()

		toPreviousBuildSchema(t, ctx, store.db)
		head, commits := doltHead(t, ctx, store.db), doltLogCount(t, ctx, store.db)
		firstCtx, cancelFirst := context.WithCancel(ctx)
		defer cancelFirst()
		atCommit := make(chan struct{})
		var once sync.Once
		port := startForkGateProxy(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(testServerPort)), func(desc string) bool {
			if !strings.Contains(desc, "'schema: apply migrations'") {
				return true
			}
			once.Do(func() { close(atCommit) })
			<-firstCtx.Done()
			return false
		})
		done := make(chan error, 1)
		go func() {
			s, err := New(firstCtx, &Config{Path: t.TempDir(), Database: store.database, ServerHost: "127.0.0.1", ServerPort: port,
				ServerUser: "root", CommitterName: "test", CommitterEmail: "test@example.com"})
			if err == nil {
				err = s.Close()
			}
			done <- err
		}()
		select {
		case <-atCommit:
		case err := <-done:
			t.Fatalf("the first open returned before its commit: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		assertEverturnForkSchema(t, ctx, store.db, "at the held commit")
		var staged int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_status WHERE table_name = 'issues' AND staged").Scan(&staged); err != nil || staged != 1 {
			t.Fatalf("issues is not staged at the held commit: %d, %v", staged, err)
		}
		cancelFirst()
		if err := <-done; err == nil {
			t.Fatal("the first open succeeded though cancelled at its commit")
		}
		if got := doltHead(t, ctx, store.db); got != head {
			t.Fatalf("the cancelled commit landed: HEAD %s -> %s", head, got)
		}
		reopenTwice(t, ctx, store, head, commits)
	})

	t.Run("every statement applied, unstaged", func(t *testing.T) {
		store, cleanup := setupConcurrentTestStore(t)
		defer cleanup()
		ctx, cancel := testContext(t)
		defer cancel()

		toPreviousBuildSchema(t, ctx, store.db)
		head, commits := doltHead(t, ctx, store.db), doltLogCount(t, ctx, store.db)
		// The wisps statements too (wisps is dolt_ignored, so they leave
		// nothing in dolt_status): with any of them missing, wisps alone would
		// send the open down the pass and hide what the gate reads of issues.
		stmts := forkMailKeyStatements()
		for _, s := range forkMailKeyStatements() {
			stmts = append(stmts, strings.ReplaceAll(s, "issues", "wisps"))
		}
		execAll(t, ctx, store.db, stmts...)
		assertEverturnForkSchema(t, ctx, store.db, "with every statement applied")
		if n := doltStatusRowsFor(t, ctx, store.db, "issues"); n == 0 {
			t.Fatal("issues is clean with every statement applied; the case under test did not arise")
		}
		reopenTwice(t, ctx, store, head, commits)
	})
}

// TestForkIndexesUserOwnedDefinitionsSkipped (bd-7bqv6, review bd-313ml
// finding 2): a fork-named column or index a user added, uncommitted, with
// another definition than the step's is a user's change: the open leaves
// issues untouched and uncommitted. The controls, an unrelated user column
// and index, are skipped the same way.
func TestForkIndexesUserOwnedDefinitionsSkipped(t *testing.T) {
	for _, tc := range []struct {
		name          string
		ddl           string
		ownFirstAlter bool
	}{
		{name: "fork-named column", ddl: "ALTER TABLE issues ADD COLUMN everturn_key VARCHAR(255) DEFAULT 'user-owned'"},
		{name: "fork-named index", ddl: "ALTER TABLE issues ADD INDEX idx_issues_everturn_key (title)", ownFirstAlter: true},
		{name: "control: unrelated column", ddl: "ALTER TABLE issues ADD COLUMN user_column VARCHAR(255)"},
		{name: "control: unrelated index", ddl: "ALTER TABLE issues ADD INDEX user_title_index (title)", ownFirstAlter: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, cleanup := setupTestStore(t)
			defer cleanup()
			ctx, cancel := testContext(t)
			defer cancel()

			toPreviousBuildSchema(t, ctx, store.db)
			if tc.ownFirstAlter {
				execAll(t, ctx, store.db, forkMailKeyStatements()[0])
			}
			execAll(t, ctx, store.db, tc.ddl)
			head, issues := doltHead(t, ctx, store.db), showCreateTable(t, ctx, store.db, "issues")
			if _, err := rerunSchemaInitOnTestStore(ctx, store); err != nil {
				t.Fatalf("open over the user's DDL: %v", err)
			}
			if got := doltHead(t, ctx, store.db); got != head {
				t.Errorf("the open committed the user's DDL: HEAD %s -> %s", head, got)
			}
			if n := doltStatusRowsFor(t, ctx, store.db, "issues"); n == 0 {
				t.Errorf("issues is clean after the open: the user's DDL was committed")
			}
			if got := showCreateTable(t, ctx, store.db, "issues"); got != issues {
				t.Errorf("the open changed issues over the user's DDL:\nbefore %s\nafter %s", issues, got)
			}
		})
	}
}

// TestForkIndexesCaseOnlyRowEdit (bd-7bqv6, review bd-313ml finding 3): with
// a case-insensitive issues.title, a case-only edit of a row is a user's row
// change. At classification (over the step's interrupted first ALTER) it
// makes issues "own DDL and a user's rows": the columns are finished, nothing
// is staged or committed, and the user's value stays in the working set only.
// The control is the reviewer's case-only edit inside metadata. During the
// pass (another session's edit after the guard's first read) it trips the
// changed-signature guard; the control is a substantive edit.
func TestForkIndexesCaseOnlyRowEdit(t *testing.T) {
	const ciTitle = "ALTER TABLE issues MODIFY title VARCHAR(500) COLLATE utf8mb4_0900_ai_ci NOT NULL"
	for _, tc := range []struct {
		name, change, probe, original, changed string
	}{
		{name: "at classification: case-only title", change: "UPDATE issues SET title = 'Before the upgrade', updated_at = updated_at WHERE id = '" + forkSeedKeys + "'",
			probe: "title", original: "before the upgrade", changed: "Before the upgrade"},
		{name: "at classification: control, case-only metadata", change: "UPDATE issues SET metadata = JSON_SET(metadata, '$.everturn.relay.source', 'Src-Seed'), updated_at = updated_at WHERE id = '" + forkSeedKeys + "'",
			probe: "JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.everturn.relay.source'))", original: "src-seed", changed: "Src-Seed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, cleanup := setupTestStore(t)
			defer cleanup()
			ctx, cancel := testContext(t)
			defer cancel()

			execAll(t, ctx, store.db, ciTitle)
			toPreviousBuildSchema(t, ctx, store.db)
			execAll(t, ctx, store.db, forkMailKeyStatements()[0], tc.change)
			head := doltHead(t, ctx, store.db)
			if _, err := rerunSchemaInitOnTestStore(ctx, store); err != nil {
				t.Fatalf("open over the user's edit: %v", err)
			}
			assertEverturnForkSchema(t, ctx, store.db, "after the open over the user's edit")
			var atHead, working string
			//nolint:gosec // test-only expressions
			if err := store.db.QueryRowContext(ctx, "SELECT "+tc.probe+" FROM issues AS OF 'HEAD' WHERE id = ?", forkSeedKeys).Scan(&atHead); err != nil {
				t.Fatalf("read at HEAD: %v", err)
			}
			//nolint:gosec // test-only expressions
			if err := store.db.QueryRowContext(ctx, "SELECT "+tc.probe+" FROM issues WHERE id = ?", forkSeedKeys).Scan(&working); err != nil {
				t.Fatalf("read the working value: %v", err)
			}
			if got := doltHead(t, ctx, store.db); got != head {
				t.Errorf("the open committed over the user's edit: HEAD %s -> %s", head, got)
			}
			if working != tc.changed || atHead != tc.original {
				t.Errorf("the user's edit: working %q (want %q), HEAD %q (want %q)", working, tc.changed, atHead, tc.original)
			}
			if n := doltStatusRowsFor(t, ctx, store.db, "issues"); n == 0 {
				t.Errorf("issues is clean after the open: the user's edit was committed")
			}
		})
	}

	for _, tc := range []struct{ name, title string }{
		{name: "during the pass: case-only title", title: "Before the upgrade, no keys"},
		{name: "during the pass: control, substantive title", title: "substantively different"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, cleanup := setupConcurrentTestStore(t)
			defer cleanup()
			ctx, cancel := testContext(t)
			defer cancel()

			execAll(t, ctx, store.db, ciTitle)
			toPreviousBuildSchema(t, ctx, store.db)
			// Own DDL and a user's rows: the guard reads forkRowsSignature.
			execAll(t, ctx, store.db, forkMailKeyStatements()[0],
				"UPDATE issues SET metadata = JSON_SET(metadata, '$.everturn.relay.source', 'different-source') WHERE id = '"+forkSeedKeys+"'")
			head := doltHead(t, ctx, store.db)
			atAlter, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			port := startForkGateProxy(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(testServerPort)), func(desc string) bool {
				if !strings.HasPrefix(desc, "ALTER TABLE issues ADD COLUMN everturn_relay_source ") {
					return true
				}
				once.Do(func() { close(atAlter) })
				select {
				case <-release:
					return true
				case <-ctx.Done():
					return false
				}
			})
			done := make(chan error, 1)
			go func() {
				s, err := New(ctx, &Config{Path: t.TempDir(), Database: store.database, ServerHost: "127.0.0.1", ServerPort: port,
					ServerUser: "root", CommitterName: "test", CommitterEmail: "test@example.com"})
				if err == nil {
					err = s.Close()
				}
				done <- err
			}()
			select {
			case <-atAlter:
			case err := <-done:
				t.Fatalf("the open returned before its next ALTER: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			execAll(t, ctx, store.db, "UPDATE issues SET title = '"+tc.title+"', updated_at = updated_at WHERE id = '"+forkSeedNone+"'")
			close(release)
			err := <-done
			if err == nil || !strings.Contains(err.Error(), "pre-existing dirty tables changed during schema migration: issues") {
				t.Errorf("an edit of title %q during the pass: open = %v, want the changed-signature refusal", tc.title, err)
			}
			if got := doltHead(t, ctx, store.db); got != head {
				t.Errorf("the refused open committed: HEAD %s -> %s", head, got)
			}
		})
	}
}

// TestForkIndexesStandingUserRowsLeftAlone (bd-7bqv6): a finished step under a
// user's uncommitted rows leaves HEAD without the fork columns until the
// user's commit, so each writable open in between takes the locked pass. That
// open changes nothing (issues definition, rows, HEAD), and the open after the
// user's commit has nothing to do.
func TestForkIndexesStandingUserRowsLeftAlone(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	toPreviousBuildSchema(t, ctx, store.db)
	execAll(t, ctx, store.db, forkMailKeyStatements()[0])
	forkUserRowChange(t, ctx, store.db)
	if _, err := rerunSchemaInitOnTestStore(ctx, store); err != nil {
		t.Fatalf("open finishing the step under the user's rows: %v", err)
	}
	assertEverturnForkSchema(t, ctx, store.db, "after the finishing open")
	head, issues := doltHead(t, ctx, store.db), showCreateTable(t, ctx, store.db, "issues")
	for attempt := 1; attempt <= 2; attempt++ {
		start := time.Now()
		applied, err := rerunSchemaInitOnTestStore(ctx, store)
		if err != nil {
			t.Fatalf("standing open %d: %v", attempt, err)
		}
		t.Logf("standing open %d: %v (applied %d)", attempt, time.Since(start), applied)
		if got := doltHead(t, ctx, store.db); got != head {
			t.Errorf("standing open %d committed: HEAD %s -> %s", attempt, head, got)
		}
		if got := showCreateTable(t, ctx, store.db, "issues"); got != issues {
			t.Errorf("standing open %d changed issues:\nbefore %s\nafter %s", attempt, issues, got)
		}
		assertForkUserRowsUncommitted(t, ctx, store.db, fmt.Sprintf("after standing open %d", attempt))
	}

	execAll(t, ctx, store.db, "CALL DOLT_ADD('issues')", "CALL DOLT_COMMIT('-m', 'test: the user commits')")
	head = doltHead(t, ctx, store.db)
	applied, err := rerunSchemaInitOnTestStore(ctx, store)
	if err != nil {
		t.Fatalf("open after the user's commit: %v", err)
	}
	if applied != 0 {
		t.Errorf("open after the user's commit applied = %d, want 0", applied)
	}
	if got := doltHead(t, ctx, store.db); got != head {
		t.Errorf("open after the user's commit committed: HEAD %s -> %s", head, got)
	}
	if n := doltStatusRowsFor(t, ctx, store.db, "issues"); n != 0 {
		t.Errorf("issues has %d dolt_status rows after the user's commit and an open, want 0", n)
	}
}

// wispDepExternalTarget is the wisp_dependencies index the fork step adds for
// the dependents count's external branch (bd-1gxdn, bd-hapoo), with the name
// and definition of migrations 0047 and ignored/0003.
const (
	wispDepExternalTarget    = "idx_wisp_dep_external_target"
	wispDepExternalTargetDDL = "ALTER TABLE wisp_dependencies ADD INDEX idx_wisp_dep_external_target (depends_on_external)"
)

// wispDepExternalTargetColumns returns the column list of
// idx_wisp_dep_external_target on wisp_dependencies, in index order, or nil
// when it is absent.
func wispDepExternalTargetColumns(t *testing.T, ctx context.Context, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
		SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'wisp_dependencies' AND INDEX_NAME = ?
		ORDER BY SEQ_IN_INDEX`, wispDepExternalTarget)
	if err != nil {
		t.Fatalf("read %s columns: %v", wispDepExternalTarget, err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan %s column: %v", wispDepExternalTarget, err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s columns: %v", wispDepExternalTarget, err)
	}
	return cols
}

// forkStatementLog records the DDL statements an open sends through a gate
// proxy (forkStatementProxy).
type forkStatementLog struct {
	mu   sync.Mutex
	ddls []string
}

func (l *forkStatementLog) record(desc string) {
	upper := strings.ToUpper(desc)
	if strings.HasPrefix(upper, "ALTER TABLE") || strings.HasPrefix(upper, "CREATE INDEX") || strings.HasPrefix(upper, "DROP INDEX") {
		l.mu.Lock()
		l.ddls = append(l.ddls, desc)
		l.mu.Unlock()
	}
}

func (l *forkStatementLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.ddls)
}

// forkStatementProxy is a gate proxy to the test server that forwards every
// command and records the DDL among them.
func forkStatementProxy(t *testing.T) (int, *forkStatementLog) {
	t.Helper()
	log := &forkStatementLog{}
	port := startForkGateProxy(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(testServerPort)), func(desc string) bool {
		log.record(desc)
		return true
	})
	return port, log
}

// forkOpenConfig is a writable open of the store's database through port.
func forkOpenConfig(t *testing.T, store *DoltStore, port int) *Config {
	return &Config{Path: t.TempDir(), Database: store.database, ServerHost: "127.0.0.1", ServerPort: port,
		ServerUser: "root", CommitterName: "test", CommitterEmail: "test@example.com"}
}

// openAndClose runs one writable open of cfg and closes the store.
func openAndClose(ctx context.Context, cfg *Config) error {
	s, err := New(ctx, cfg)
	if err != nil {
		return err
	}
	return s.Close()
}

// TestForkIndexWispDepExternalTarget (bd-1gxdn): migration 0021 creates
// wisp_dependencies without an index led by depends_on_external; the fork step
// adds idx_wisp_dep_external_target on a fresh database, and a dropped one
// comes back at the next writable open with nothing added to dolt_status and
// HEAD unchanged (the table is dolt_ignored: nothing is staged or committed).
func TestForkIndexWispDepExternalTarget(t *testing.T) {
	store, cleanup := setupConcurrentTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	if got := wispDepExternalTargetColumns(t, ctx, store.db); !slices.Equal(got, []string{"depends_on_external"}) {
		t.Fatalf("a fresh database's %s columns = %v, want [depends_on_external]", wispDepExternalTarget, got)
	}
	execAll(t, ctx, store.db, "ALTER TABLE wisp_dependencies DROP INDEX "+wispDepExternalTarget)
	if got := wispDepExternalTargetColumns(t, ctx, store.db); got != nil {
		t.Fatalf("%s still present after DROP INDEX: %v", wispDepExternalTarget, got)
	}
	head, commits := doltHead(t, ctx, store.db), doltLogCount(t, ctx, store.db)
	// The fixture's own rows (config) may sit in dolt_status; the step must
	// add none, and wisp_dependencies, dolt_ignored, never shows.
	status := doltStatusCount(t, ctx, store.db)
	if n := doltStatusRowsFor(t, ctx, store.db, "wisp_dependencies"); n != 0 {
		t.Fatalf("wisp_dependencies has %d dolt_status rows after dropping the index, want 0 (dolt_ignored)", n)
	}

	port, log := forkStatementProxy(t)
	if err := openAndClose(ctx, forkOpenConfig(t, store, port)); err != nil {
		t.Fatalf("writable open after the drop: %v", err)
	}
	if got := wispDepExternalTargetColumns(t, ctx, store.db); !slices.Equal(got, []string{"depends_on_external"}) {
		t.Errorf("%s columns after the next open = %v, want [depends_on_external]", wispDepExternalTarget, got)
	}
	if ddls := log.all(); !slices.Equal(ddls, []string{wispDepExternalTargetDDL}) {
		t.Errorf("the open sent DDL %q, want only %q", ddls, wispDepExternalTargetDDL)
	}
	if n := doltStatusRowsFor(t, ctx, store.db, "wisp_dependencies"); n != 0 {
		t.Errorf("wisp_dependencies has %d dolt_status rows after the index came back, want 0", n)
	}
	if n := doltStatusCount(t, ctx, store.db); n != status {
		t.Errorf("dolt_status has %d rows after the index came back, want the %d from before the open", n, status)
	}
	if got := doltHead(t, ctx, store.db); got != head {
		t.Errorf("HEAD moved %s -> %s: something was committed for a dolt_ignored table", head, got)
	}
	if got := doltLogCount(t, ctx, store.db); got != commits {
		t.Errorf("%d new commits, want 0", got-commits)
	}
}

// TestForkIndexWispDepExternalTargetAlreadyPresent: a wisp_dependencies that
// already has the index, as one that came through migration 0047 or
// ignored/0003 does (their statement, run here by hand), gets no DDL at the
// next writable open.
func TestForkIndexWispDepExternalTargetAlreadyPresent(t *testing.T) {
	store, cleanup := setupConcurrentTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	execAll(t, ctx, store.db, "ALTER TABLE wisp_dependencies DROP INDEX "+wispDepExternalTarget, wispDepExternalTargetDDL)
	head := doltHead(t, ctx, store.db)
	port, log := forkStatementProxy(t)
	if err := openAndClose(ctx, forkOpenConfig(t, store, port)); err != nil {
		t.Fatalf("writable open: %v", err)
	}
	if ddls := log.all(); len(ddls) != 0 {
		t.Errorf("the open of a database that has %s sent DDL %q, want none", wispDepExternalTarget, ddls)
	}
	if got := wispDepExternalTargetColumns(t, ctx, store.db); !slices.Equal(got, []string{"depends_on_external"}) {
		t.Errorf("%s columns = %v, want [depends_on_external]", wispDepExternalTarget, got)
	}
	if got := doltHead(t, ctx, store.db); got != head {
		t.Errorf("HEAD moved %s -> %s on an open with nothing to do", head, got)
	}
}

// TestForkIndexCloneLocalCancelledFinished: the first writable open after a
// clone-local fork index is lost (the wisp_dependencies index; a wisps key
// index) is cancelled at the step's ALTER while it holds the migration lock (a
// gate proxy holds the statement until the cancel, then drops the session).
// Two more writable opens wait for the lock meanwhile. When the first session
// goes, one of them finishes the step, and the other, taking the lock after
// it, finds nothing to do: the ALTER is sent once between them, the index is
// back, nothing is added to dolt_status, and HEAD does not move (the short
// clone-local step, bd-30lnk).
func TestForkIndexCloneLocalCancelledFinished(t *testing.T) {
	for _, target := range cloneLocalForkTargets {
		t.Run(target.name, func(t *testing.T) {
			store, cleanup := setupConcurrentTestStore(t)
			defer cleanup()
			ctx, cancel := testContext(t)
			defer cancel()

			execAll(t, ctx, store.db, target.drop())
			head, status := doltHead(t, ctx, store.db), doltStatusCount(t, ctx, store.db)
			upstream := net.JoinHostPort("127.0.0.1", strconv.Itoa(testServerPort))

			firstCtx, cancelFirst := context.WithCancel(ctx)
			defer cancelFirst()
			atALTER := make(chan struct{})
			var atOnce sync.Once
			firstPort := startForkGateProxy(t, upstream, func(desc string) bool {
				if desc != target.ddl {
					return true
				}
				atOnce.Do(func() { close(atALTER) })
				<-firstCtx.Done()
				return false
			})
			first := make(chan error, 1)
			go func() { first <- openAndClose(firstCtx, forkOpenConfig(t, store, firstPort)) }()
			select {
			case <-atALTER:
			case err := <-first:
				t.Fatalf("the first open returned before the step's ALTER: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var free int
			if err := store.db.QueryRowContext(ctx, "SELECT IS_FREE_LOCK(?)", schema.MigrationLockName(store.database)).Scan(&free); err != nil || free != 0 {
				t.Fatalf("the first open does not hold the migration lock at its ALTER: IS_FREE_LOCK = %d, %v", free, err)
			}

			type waiter struct {
				log     *forkStatementLog
				waiting chan struct{}
				done    chan error
			}
			var waiters []*waiter
			for range 2 {
				w := &waiter{log: &forkStatementLog{}, waiting: make(chan struct{}), done: make(chan error, 1)}
				var once sync.Once
				port := startForkGateProxy(t, upstream, func(desc string) bool {
					if strings.Contains(desc, "GET_LOCK(") {
						once.Do(func() { close(w.waiting) })
					}
					w.log.record(desc)
					return true
				})
				go func() { w.done <- openAndClose(ctx, forkOpenConfig(t, store, port)) }()
				waiters = append(waiters, w)
			}
			for i, w := range waiters {
				select {
				case <-w.waiting:
				case err := <-w.done:
					t.Fatalf("waiting open %d returned before it asked for the migration lock: %v", i+1, err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}

			cancelFirst()
			if err := <-first; err == nil {
				t.Errorf("the cancelled first open returned no error")
			} else {
				t.Logf("cancelled first open: %v", err)
			}
			alters := 0
			for i, w := range waiters {
				if err := <-w.done; err != nil {
					t.Fatalf("waiting open %d: %v", i+1, err)
				}
				ddls := w.log.all()
				t.Logf("waiting open %d sent DDL %q", i+1, ddls)
				for _, d := range ddls {
					if d != target.ddl {
						t.Errorf("waiting open %d sent unexpected DDL %q", i+1, d)
					}
				}
				alters += len(ddls)
			}
			if alters != 1 {
				t.Errorf("the two waiting opens sent the step's ALTER %d times between them, want 1 (one finishes, the other finds nothing to do)", alters)
			}
			if got := tableIndexColumns(t, ctx, store.db, target.table, target.index); !slices.Equal(got, target.columns) {
				t.Errorf("%s columns after the waiting opens = %v, want %v", target.index, got, target.columns)
			}
			if n := doltStatusRowsFor(t, ctx, store.db, target.table); n != 0 {
				t.Errorf("%s has %d dolt_status rows after the waiting opens, want 0", target.table, n)
			}
			if n := doltStatusCount(t, ctx, store.db); n != status {
				t.Errorf("dolt_status has %d rows after the waiting opens, want the %d from before", n, status)
			}
			if got := doltHead(t, ctx, store.db); got != head {
				t.Errorf("HEAD moved %s -> %s: something was committed for a dolt_ignored table", head, got)
			}
		})
	}
}

// TestForkIndexWispDepExternalTargetOtherDefinitionKept: the step goes by
// name on wisp_dependencies, as it does on wisps. An index named
// idx_wisp_dep_external_target with another column list is left as it is: the
// open succeeds and sends no DDL.
func TestForkIndexWispDepExternalTargetOtherDefinitionKept(t *testing.T) {
	store, cleanup := setupConcurrentTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	execAll(t, ctx, store.db,
		"ALTER TABLE wisp_dependencies DROP INDEX "+wispDepExternalTarget,
		"ALTER TABLE wisp_dependencies ADD INDEX "+wispDepExternalTarget+" (type, depends_on_external)")
	port, log := forkStatementProxy(t)
	if err := openAndClose(ctx, forkOpenConfig(t, store, port)); err != nil {
		t.Fatalf("writable open with a foreign %s: %v", wispDepExternalTarget, err)
	}
	if ddls := log.all(); len(ddls) != 0 {
		t.Errorf("the open sent DDL %q, want none", ddls)
	}
	if got := wispDepExternalTargetColumns(t, ctx, store.db); !slices.Equal(got, []string{"type", "depends_on_external"}) {
		t.Errorf("%s columns = %v, want the user's [type depends_on_external] left as it was", wispDepExternalTarget, got)
	}
}

// cloneLocalForkTarget is a fork index on a dolt_ignored, clone-local table:
// the open whose only need it is takes the short clone-local step
// (schema.ensureCloneLocalForkObjects, bd-30lnk), not the whole pass.
type cloneLocalForkTarget struct {
	name, table, index, ddl string
	columns                 []string
}

func (c cloneLocalForkTarget) drop() string {
	return "ALTER TABLE " + c.table + " DROP INDEX " + c.index
}

var cloneLocalForkTargets = []cloneLocalForkTarget{
	{name: "wisp_dependencies external target", table: "wisp_dependencies", index: wispDepExternalTarget, ddl: wispDepExternalTargetDDL,
		columns: []string{"depends_on_external"}},
	{name: "wisps key index", table: "wisps", index: "idx_wisps_everturn_key", ddl: "ALTER TABLE wisps ADD INDEX idx_wisps_everturn_key (everturn_key, created_at)",
		columns: []string{"everturn_key", "created_at"}},
}

// settleForkFixture commits what the concurrent fixture leaves uncommitted
// (its config rows), so the test's own uncommitted changes are the only ones.
func settleForkFixture(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	if doltStatusRowsFor(t, ctx, db, "config") > 0 {
		execAll(t, ctx, db, "CALL DOLT_ADD('config')", "CALL DOLT_COMMIT('-m', 'test: settle fixture config')")
	}
}

// TestForkIndexCloneLocalKeepsStagedRows is review bd-8b72l finding 1: on a
// database whose only missing schema object is a clone-local fork index, a
// user has staged one issues title and edited it again, unstaged. The open
// adds the index and leaves the staging alone: issues is still staged, the
// staged title is the one the user staged, the working title the one after,
// and HEAD does not move. The whole pass unstaged it (unstagePreExistingTables).
func TestForkIndexCloneLocalKeepsStagedRows(t *testing.T) {
	for _, target := range cloneLocalForkTargets {
		t.Run(target.name, func(t *testing.T) {
			store, cleanup := setupConcurrentTestStore(t)
			defer cleanup()
			ctx, cancel := testContext(t)
			defer cancel()

			createPerm(t, ctx, store, "fx-staged")
			settleForkFixture(t, ctx, store.db)
			execAll(t, ctx, store.db,
				"UPDATE issues SET title = 'chosen staging version' WHERE id = 'fx-staged'",
				"CALL DOLT_ADD('issues')",
				"UPDATE issues SET title = 'different working version' WHERE id = 'fx-staged'",
				target.drop())
			stagedRows := func() int {
				t.Helper()
				var n int
				if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_status WHERE table_name = 'issues' AND staged").Scan(&n); err != nil {
					t.Fatalf("count staged issues: %v", err)
				}
				return n
			}
			if stagedRows() != 1 {
				t.Fatal("the fixture did not stage issues")
			}
			head := doltHead(t, ctx, store.db)

			port, log := forkStatementProxy(t)
			if err := openAndClose(ctx, forkOpenConfig(t, store, port)); err != nil {
				t.Fatalf("writable open: %v", err)
			}
			var stagedTitle, workingTitle string
			if err := store.db.QueryRowContext(ctx, "SELECT title FROM issues AS OF 'STAGED' WHERE id = 'fx-staged'").Scan(&stagedTitle); err != nil {
				t.Fatalf("read the staged title: %v", err)
			}
			if err := store.db.QueryRowContext(ctx, "SELECT title FROM issues WHERE id = 'fx-staged'").Scan(&workingTitle); err != nil {
				t.Fatalf("read the working title: %v", err)
			}
			t.Logf("staged_rows=%d STAGED_title=%q WORKING_title=%q DDL=%q", stagedRows(), stagedTitle, workingTitle, log.all())
			if got := tableIndexColumns(t, ctx, store.db, target.table, target.index); !slices.Equal(got, target.columns) {
				t.Errorf("%s columns after the open = %v, want %v", target.index, got, target.columns)
			}
			if ddls := log.all(); !slices.Equal(ddls, []string{target.ddl}) {
				t.Errorf("the open sent DDL %q, want only %q", ddls, target.ddl)
			}
			if n := stagedRows(); n != 1 {
				t.Errorf("issues staged rows = %d after the open, want 1 (the user's staging kept)", n)
			}
			if stagedTitle != "chosen staging version" {
				t.Errorf("STAGED title = %q after the open, want the user's staged %q", stagedTitle, "chosen staging version")
			}
			if workingTitle != "different working version" {
				t.Errorf("WORKING title = %q after the open, want %q", workingTitle, "different working version")
			}
			if got := doltHead(t, ctx, store.db); got != head {
				t.Errorf("HEAD moved %s -> %s on an open whose only work is a clone-local index", head, got)
			}
		})
	}
}

// TestForkIndexCloneLocalLeavesDirtyDependency is review bd-8b72l finding 2:
// with a clone-local fork index missing and one uncommitted durable
// dependency keyed by a random UUID, two writable opens succeed (the first
// adds the index, the second has nothing to do) and leave the dependency's key
// and HEAD alone; after the user commits it, the next open leaves the key and
// the user's HEAD alone too. The whole pass refused the dirty dependencies
// (re-key) and, after the user's commit, re-keyed it into a new HEAD.
func TestForkIndexCloneLocalLeavesDirtyDependency(t *testing.T) {
	for _, target := range cloneLocalForkTargets {
		t.Run(target.name, func(t *testing.T) {
			store, cleanup := setupConcurrentTestStore(t)
			defer cleanup()
			ctx, cancel := testContext(t)
			defer cancel()

			createPerm(t, ctx, store, "fx-dirty-src")
			settleForkFixture(t, ctx, store.db)
			execAll(t, ctx, store.db,
				"INSERT INTO dependencies (id, issue_id, depends_on_external, created_by) VALUES (UUID(), 'fx-dirty-src', 'external:dirty:one', 'tester')",
				target.drop())
			head := doltHead(t, ctx, store.db)
			key := func(asOf string) string {
				t.Helper()
				var id string
				if err := store.db.QueryRowContext(ctx, "SELECT id FROM dependencies"+asOf+" WHERE issue_id = 'fx-dirty-src'").Scan(&id); err != nil {
					t.Fatalf("read the dependency key%s: %v", asOf, err)
				}
				return id
			}
			before := key("")

			for i, want := range [][]string{{target.ddl}, nil} {
				port, log := forkStatementProxy(t)
				if err := openAndClose(ctx, forkOpenConfig(t, store, port)); err != nil {
					t.Fatalf("writable open %d with a dirty dependencies: %v", i+1, err)
				}
				if ddls := log.all(); !slices.Equal(ddls, want) {
					t.Errorf("writable open %d sent DDL %q, want %q", i+1, ddls, want)
				}
				if got := tableIndexColumns(t, ctx, store.db, target.table, target.index); !slices.Equal(got, target.columns) {
					t.Errorf("%s columns after open %d = %v, want %v", target.index, i+1, got, target.columns)
				}
				if got := key(""); got != before {
					t.Errorf("open %d re-keyed the user's uncommitted dependency %s -> %s", i+1, before, got)
				}
				if got := doltHead(t, ctx, store.db); got != head {
					t.Errorf("open %d moved HEAD %s -> %s", i+1, head, got)
				}
				if n := doltStatusRowsFor(t, ctx, store.db, "dependencies"); n != 1 {
					t.Errorf("dependencies has %d dolt_status rows after open %d, want 1 (the user's row, uncommitted)", n, i+1)
				}
			}

			execAll(t, ctx, store.db, "CALL DOLT_ADD('dependencies')", "CALL DOLT_COMMIT('-m', 'test: the user commits the dependency')")
			committed := doltHead(t, ctx, store.db)
			port, log := forkStatementProxy(t)
			if err := openAndClose(ctx, forkOpenConfig(t, store, port)); err != nil {
				t.Fatalf("writable open after the user's commit: %v", err)
			}
			if ddls := log.all(); len(ddls) != 0 {
				t.Errorf("the open after the user's commit sent DDL %q, want none", ddls)
			}
			if got := doltHead(t, ctx, store.db); got != committed {
				t.Errorf("the open after the user's commit moved HEAD %s -> %s", committed, got)
			}
			if got := key(" AS OF 'HEAD'"); got != before {
				t.Errorf("the dependency key at HEAD = %s after the open, want the user's %s", got, before)
			}
		})
	}
}

// TestForkIndexCommittedAndCloneLocalTakeWholePass: a database missing an
// issues fork index (its drop committed, so issues is clean) and a clone-local
// one takes the whole pass, as before bd-30lnk: the pass adds both, commits
// issues once ('schema: apply migrations'), and leaves nothing dirty.
func TestForkIndexCommittedAndCloneLocalTakeWholePass(t *testing.T) {
	for _, target := range cloneLocalForkTargets {
		t.Run(target.name, func(t *testing.T) {
			store, cleanup := setupConcurrentTestStore(t)
			defer cleanup()
			ctx, cancel := testContext(t)
			defer cancel()

			settleForkFixture(t, ctx, store.db)
			execAll(t, ctx, store.db,
				"ALTER TABLE issues DROP INDEX idx_issues_closed_at",
				"CALL DOLT_ADD('issues')",
				"CALL DOLT_COMMIT('-m', 'test: drop idx_issues_closed_at')",
				target.drop())
			head, commits := doltHead(t, ctx, store.db), doltLogCount(t, ctx, store.db)

			port, log := forkStatementProxy(t)
			if err := openAndClose(ctx, forkOpenConfig(t, store, port)); err != nil {
				t.Fatalf("writable open: %v", err)
			}
			const issuesDDL = "ALTER TABLE issues ADD INDEX idx_issues_closed_at (closed_at)"
			if ddls := log.all(); !slices.Equal(ddls, []string{issuesDDL, target.ddl}) {
				t.Errorf("the open sent DDL %q, want %q", ddls, []string{issuesDDL, target.ddl})
			}
			if !issuesIndexExists(t, ctx, store.db, "idx_issues_closed_at") {
				t.Errorf("idx_issues_closed_at not added by the whole pass")
			}
			if got := tableIndexColumns(t, ctx, store.db, target.table, target.index); !slices.Equal(got, target.columns) {
				t.Errorf("%s columns after the whole pass = %v, want %v", target.index, got, target.columns)
			}
			if got := doltHead(t, ctx, store.db); got == head {
				t.Errorf("HEAD did not move: the whole pass did not commit issues")
			}
			if got := doltLogCount(t, ctx, store.db); got != commits+1 {
				t.Errorf("%d new commits, want 1", got-commits)
			}
			var message string
			if err := store.db.QueryRowContext(ctx, "SELECT message FROM dolt_log LIMIT 1").Scan(&message); err != nil || message != "schema: apply migrations" {
				t.Errorf("HEAD commit message = %q, %v; want the pass's %q", message, err, "schema: apply migrations")
			}
			if n := doltStatusCount(t, ctx, store.db); n != 0 {
				t.Errorf("dolt_status has %d rows after the whole pass, want 0", n)
			}
		})
	}
}

// TestForkIndexCaseVariantIsConverged is review bd-8b72l finding 3: the
// server compares index and column names without regard to case, so the step
// does too. With a fork object present under another spelling, two writable
// opens send no ALTER, take no migration lock, and send as many commands as
// an open of the converged database did before the change of spelling.
func TestForkIndexCaseVariantIsConverged(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		{"wisp_dependencies index", []string{
			"ALTER TABLE wisp_dependencies DROP INDEX idx_wisp_dep_external_target",
			"ALTER TABLE wisp_dependencies ADD INDEX IDX_WISP_DEP_EXTERNAL_TARGET (depends_on_external)",
		}},
		{"wisps key index", []string{
			"ALTER TABLE wisps DROP INDEX idx_wisps_everturn_key",
			"ALTER TABLE wisps ADD INDEX IDX_WISPS_EVERTURN_KEY (everturn_key, created_at)",
		}},
		{"issues index", []string{
			"ALTER TABLE issues DROP INDEX idx_issues_closed_at",
			"ALTER TABLE issues ADD INDEX IDX_ISSUES_CLOSED_AT (closed_at)",
			"CALL DOLT_ADD('issues')",
			"CALL DOLT_COMMIT('-m', 'test: idx_issues_closed_at spelled in upper case')",
		}},
		{"issues key column", []string{
			"ALTER TABLE issues DROP INDEX idx_issues_everturn_bead",
			"ALTER TABLE issues DROP COLUMN everturn_bead",
			"ALTER TABLE issues ADD COLUMN EVERTURN_BEAD VARCHAR(255) GENERATED ALWAYS AS (LEFT(JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.everturn.bead')), 255)) STORED",
			"ALTER TABLE issues ADD INDEX idx_issues_everturn_bead (EVERTURN_BEAD, created_at)",
			"CALL DOLT_ADD('issues')",
			"CALL DOLT_COMMIT('-m', 'test: issues.everturn_bead spelled in upper case')",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, cleanup := setupConcurrentTestStore(t)
			defer cleanup()
			ctx, cancel := testContext(t)
			defer cancel()

			settleForkFixture(t, ctx, store.db)
			rec := &openRecorder{}
			port := startBudgetProxy(t, rec, net.JoinHostPort("127.0.0.1", strconv.Itoa(testServerPort)))
			measure := func(when string) (int, int, []string) {
				t.Helper()
				rec.start()
				err := openAndClose(ctx, forkOpenConfig(t, store, port))
				_, cmds := rec.stop()
				if err != nil {
					t.Fatalf("writable open %s: %v", when, err)
				}
				locks := 0
				var ddls []string
				for _, cmd := range cmds {
					if strings.Contains(cmd, "GET_LOCK(") {
						locks++
					}
					if strings.Contains(strings.ToUpper(cmd), "ALTER TABLE") {
						ddls = append(ddls, cmd)
					}
				}
				return len(cmds), locks, ddls
			}
			converged, locks, ddls := measure("of the converged database")
			if locks != 0 || len(ddls) != 0 {
				t.Fatalf("the converged control took %d migration locks and sent DDL %q; the fixture is not converged", locks, ddls)
			}
			execAll(t, ctx, store.db, tc.stmts...)
			head := doltHead(t, ctx, store.db)
			for i := 1; i <= 2; i++ {
				n, locks, ddls := measure(fmt.Sprintf("%d with the other spelling", i))
				t.Logf("open %d: %d commands (converged %d), %d migration locks, DDL %q", i, n, converged, locks, ddls)
				if locks != 0 {
					t.Errorf("open %d took the migration lock %d times, want 0 (converged)", i, locks)
				}
				if len(ddls) != 0 {
					t.Errorf("open %d sent DDL %q, want none", i, ddls)
				}
				if n != converged {
					t.Errorf("open %d sent %d commands, want the converged %d", i, n, converged)
				}
			}
			if got := doltHead(t, ctx, store.db); got != head {
				t.Errorf("HEAD moved %s -> %s", head, got)
			}
		})
	}
}

// forkSeedPattern is the canonical dolt_ignore row the seed fixtures leave out
// of the committed baseline, so the open has a seed to write.
const forkSeedPattern = "repo_mtimes"

// forkSeedRows counts the dolt_ignore rows for forkSeedPattern at revision
// ("WORKING", "HEAD").
func forkSeedRows(t *testing.T, ctx context.Context, db *sql.DB, revision string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_ignore AS OF '"+revision+"' WHERE pattern = ?", forkSeedPattern).Scan(&n); err != nil {
		t.Fatalf("count the %s seed row at %s: %v", forkSeedPattern, revision, err)
	}
	return n
}

// forkIssueTitle reads fx-seed's title at revision ("HEAD", "STAGED", "WORKING").
func forkIssueTitle(t *testing.T, ctx context.Context, db *sql.DB, revision string) string {
	t.Helper()
	var title string
	if err := db.QueryRowContext(ctx, "SELECT title FROM issues AS OF '"+revision+"' WHERE id = 'fx-seed'").Scan(&title); err != nil {
		t.Fatalf("read the %s title: %v", revision, err)
	}
	return title
}

// TestForkIndexSeedCommitCarriesNothingElse is review bd-8fwl1 finding 1: at
// the latest cursors, the committed baseline lacks one canonical dolt_ignore
// row, so the open has a seed to write, and an issues title is edited (staged
// or not) and edited again unstaged. With a table staged, the seed commit took
// the staging area into HEAD (DOLT_ADD('dolt_ignore') stages only the seed,
// DOLT_COMMIT commits all that is staged). Now, with issues staged, the seed
// is not committed: it stays a working-set change of dolt_ignore, HEAD (and
// its title) does not move, and the staging and the working title are as the
// user left them; a second open has no seed to write and leaves it so. With
// nothing staged, the seed is committed as before, alone. Each runs with the
// clone-local index missing (the clone-local step) and with nothing missing
// (HEAD's no-work branch); both call commitSeededDoltIgnore.
func TestForkIndexSeedCommitCarriesNothingElse(t *testing.T) {
	for _, tc := range []struct {
		name              string
		stage, dropIndex  bool
		wantSeedCommitted bool
	}{
		{name: "clone-local step, issues staged", stage: true, dropIndex: true},
		{name: "clone-local step, nothing staged", dropIndex: true, wantSeedCommitted: true},
		{name: "no-work branch, issues staged", stage: true},
		{name: "no-work branch, nothing staged", wantSeedCommitted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, cleanup := setupConcurrentTestStore(t)
			defer cleanup()
			ctx, cancel := testContext(t)
			defer cancel()
			db := store.db

			createPerm(t, ctx, store, "fx-seed")
			settleForkFixture(t, ctx, db)
			execAll(t, ctx, db,
				"DELETE FROM dolt_ignore WHERE pattern = '"+forkSeedPattern+"'",
				"CALL DOLT_ADD('dolt_ignore')",
				"CALL DOLT_COMMIT('-m', 'test: a baseline that lacks one canonical seed row')")
			headTitle := forkIssueTitle(t, ctx, db, "HEAD")
			stagedTitle := headTitle
			if tc.stage {
				stagedTitle = "staged title"
				execAll(t, ctx, db,
					"UPDATE issues SET title = 'staged title' WHERE id = 'fx-seed'",
					"CALL DOLT_ADD('issues')")
			}
			execAll(t, ctx, db, "UPDATE issues SET title = 'working title' WHERE id = 'fx-seed'")
			if tc.dropIndex {
				execAll(t, ctx, db, "ALTER TABLE wisp_dependencies DROP INDEX "+wispDepExternalTarget)
			}
			stagedRows := func() int {
				t.Helper()
				var n int
				if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_status WHERE table_name = 'issues' AND staged").Scan(&n); err != nil {
					t.Fatalf("count staged issues: %v", err)
				}
				return n
			}
			wantStaged := 0
			if tc.stage {
				wantStaged = 1
			}
			if got := stagedRows(); got != wantStaged || forkSeedRows(t, ctx, db, "HEAD") != 0 {
				t.Fatalf("fixture: staged issues rows = %d (want %d), HEAD seed rows = %d (want 0)", got, wantStaged, forkSeedRows(t, ctx, db, "HEAD"))
			}
			head, commits := doltHead(t, ctx, db), doltLogCount(t, ctx, db)

			check := func(when string, wantSeedCommitted bool) {
				t.Helper()
				t.Logf("%s: HEAD_moved=%v commits=+%d HEAD_title=%q STAGED_title=%q WORKING_title=%q staged_rows=%d seed_WORKING=%d seed_HEAD=%d dolt_ignore_status=%d",
					when, doltHead(t, ctx, db) != head, doltLogCount(t, ctx, db)-commits,
					forkIssueTitle(t, ctx, db, "HEAD"), forkIssueTitle(t, ctx, db, "STAGED"), forkIssueTitle(t, ctx, db, "WORKING"),
					stagedRows(), forkSeedRows(t, ctx, db, "WORKING"), forkSeedRows(t, ctx, db, "HEAD"), doltStatusRowsFor(t, ctx, db, "dolt_ignore"))
				if got := wispDepExternalTargetColumns(t, ctx, db); !slices.Equal(got, []string{"depends_on_external"}) {
					t.Errorf("%s: %s columns = %v, want [depends_on_external]", when, wispDepExternalTarget, got)
				}
				if got := forkIssueTitle(t, ctx, db, "HEAD"); got != headTitle {
					t.Errorf("%s: HEAD title = %q, want the original %q (a staged title rode the seed commit)", when, got, headTitle)
				}
				if got := stagedRows(); got != wantStaged {
					t.Errorf("%s: issues staged rows = %d, want %d", when, got, wantStaged)
				}
				if got := forkIssueTitle(t, ctx, db, "STAGED"); got != stagedTitle {
					t.Errorf("%s: STAGED title = %q, want %q", when, got, stagedTitle)
				}
				if got := forkIssueTitle(t, ctx, db, "WORKING"); got != "working title" {
					t.Errorf("%s: WORKING title = %q, want %q", when, got, "working title")
				}
				if got := forkSeedRows(t, ctx, db, "WORKING"); got != 1 {
					t.Errorf("%s: dolt_ignore holds %d %s rows, want the seed row", when, got, forkSeedPattern)
				}
				if wantSeedCommitted {
					if got := forkSeedRows(t, ctx, db, "HEAD"); got != 1 {
						t.Errorf("%s: the seed row is not at HEAD with nothing else staged", when)
					}
					if n := doltStatusRowsFor(t, ctx, db, "dolt_ignore"); n != 0 {
						t.Errorf("%s: dolt_ignore has %d status rows, want 0 (the seed committed)", when, n)
					}
					if got := doltLogCount(t, ctx, db) - commits; got != 1 {
						t.Errorf("%s: %d new commits, want 1 (the seed commit)", when, got)
					}
					var message string
					if err := db.QueryRowContext(ctx, "SELECT message FROM dolt_log LIMIT 1").Scan(&message); err != nil {
						t.Fatalf("read the last commit: %v", err)
					}
					if message != "schema: seed dolt_ignore patterns" {
						t.Errorf("%s: the last commit is %q, want the seed commit", when, message)
					}
					return
				}
				if got := forkSeedRows(t, ctx, db, "HEAD"); got != 0 {
					t.Errorf("%s: the seed row reached HEAD while issues was staged", when)
				}
				if n := doltStatusRowsFor(t, ctx, db, "dolt_ignore"); n != 1 {
					t.Errorf("%s: dolt_ignore has %d status rows, want 1 (the seed left uncommitted)", when, n)
				}
				if got := doltHead(t, ctx, db); got != head {
					t.Errorf("%s: HEAD moved %s -> %s while issues was staged", when, head, got)
				}
			}

			port, log := forkStatementProxy(t)
			if err := openAndClose(ctx, forkOpenConfig(t, store, port)); err != nil {
				t.Fatalf("writable open: %v", err)
			}
			wantDDL := []string(nil)
			if tc.dropIndex {
				wantDDL = []string{wispDepExternalTargetDDL}
			}
			if ddls := log.all(); !slices.Equal(ddls, wantDDL) {
				t.Errorf("the open sent DDL %q, want %q", ddls, wantDDL)
			}
			check("after the open", tc.wantSeedCommitted)
			if tc.wantSeedCommitted {
				return
			}
			// The seed rows are in the working set now: the next open has no
			// seed to write, so it commits nothing and leaves them as they are.
			port, log = forkStatementProxy(t)
			if err := openAndClose(ctx, forkOpenConfig(t, store, port)); err != nil {
				t.Fatalf("second writable open: %v", err)
			}
			if ddls := log.all(); len(ddls) != 0 {
				t.Errorf("the second open sent DDL %q, want none", ddls)
			}
			check("after a second open", false)
		})
	}
}

// TestForkIndexTrackedCloneLocalTakesWholePass is review bd-8fwl1 finding 2:
// an operator has un-ignored the wisp tables (wisp_% and wisps set to
// ignored=false in dolt_ignore, which the seeder keeps) and committed both
// tables, so they are versioned here, and a clone-local fork index is missing
// at HEAD. The open takes the whole pass, which commits the index: it is at
// HEAD and in WORKING, and wisps and wisp_dependencies have no status row. Routed by its name, the
// open added the index to WORKING only, and a later open, which probes only
// WORKING for such a table, never committed it. A second open sends no DDL
// and does not move HEAD.
func TestForkIndexTrackedCloneLocalTakesWholePass(t *testing.T) {
	for _, target := range cloneLocalForkTargets {
		t.Run(target.name, func(t *testing.T) {
			store, cleanup := setupConcurrentTestStore(t)
			defer cleanup()
			ctx, cancel := testContext(t)
			defer cancel()
			db := store.db

			settleForkFixture(t, ctx, db)
			execAll(t, ctx, db,
				"UPDATE dolt_ignore SET ignored = false WHERE pattern IN ('wisp_%', 'wisps')",
				target.drop(),
				"CALL DOLT_ADD('dolt_ignore', 'wisps', 'wisp_dependencies')",
				"CALL DOLT_COMMIT('-m', 'test: version the wisp tables')")
			headHasIndex := func() bool {
				t.Helper()
				var name, ddl string
				if err := db.QueryRowContext(ctx, "SHOW CREATE TABLE `"+target.table+"` AS OF 'HEAD'").Scan(&name, &ddl); err != nil {
					t.Fatalf("SHOW CREATE TABLE %s AS OF HEAD: %v", target.table, err)
				}
				return strings.Contains(ddl, "`"+target.index+"`")
			}
			// Un-ignoring wisp_% also shows the other wisp_* tables as new,
			// uncommitted tables; the two the test versions are counted.
			wispStatusRows := func() int {
				t.Helper()
				return doltStatusRowsFor(t, ctx, db, "wisps") + doltStatusRowsFor(t, ctx, db, "wisp_dependencies")
			}
			if n := wispStatusRows(); n != 0 || headHasIndex() {
				t.Fatalf("fixture: %d wisps and wisp_dependencies status rows (want 0), index at HEAD = %v (want false)", n, headHasIndex())
			}
			head := doltHead(t, ctx, db)

			port, log := forkStatementProxy(t)
			if err := openAndClose(ctx, forkOpenConfig(t, store, port)); err != nil {
				t.Fatalf("writable open: %v", err)
			}
			afterFirst := doltHead(t, ctx, db)
			t.Logf("open 1: HEAD_moved=%v HEAD_index=%v WORKING_index=%v status_rows=%d DDL=%q",
				afterFirst != head, headHasIndex(), tableIndexColumns(t, ctx, db, target.table, target.index), wispStatusRows(), log.all())
			if got := tableIndexColumns(t, ctx, db, target.table, target.index); !slices.Equal(got, target.columns) {
				t.Errorf("%s columns in WORKING = %v, want %v", target.index, got, target.columns)
			}
			if !headHasIndex() {
				t.Errorf("%s is not at HEAD after the open: a versioned %s was routed as clone-local", target.index, target.table)
			}
			if n := wispStatusRows(); n != 0 {
				t.Errorf("%d wisps and wisp_dependencies status rows after the open, want 0", n)
			}
			if afterFirst == head {
				t.Error("HEAD did not move: the whole pass commits the index")
			}

			port, log = forkStatementProxy(t)
			if err := openAndClose(ctx, forkOpenConfig(t, store, port)); err != nil {
				t.Fatalf("second writable open: %v", err)
			}
			if ddls := log.all(); len(ddls) != 0 {
				t.Errorf("the second open sent DDL %q, want none", ddls)
			}
			if got := doltHead(t, ctx, db); got != afterFirst {
				t.Errorf("the second open moved HEAD %s -> %s", afterFirst, got)
			}
		})
	}
}
