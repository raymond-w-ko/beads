package dolt

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// forkIndexNames are the fork-only issues indexes schema.ensureForkIndexes adds
// (internal/storage/schema/fork_indexes.go).
var forkIndexNames = []string{
	"idx_issues_closed_at",
	"idx_issues_status_closed_at",
	"idx_issues_everturn_action",
}

func issuesIndexExists(t *testing.T, ctx context.Context, db *sql.DB, name string) bool {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM INFORMATION_SCHEMA.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'issues' AND INDEX_NAME = ?
	`, name).Scan(&count); err != nil {
		t.Fatalf("probe index %s: %v", name, err)
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
	rows, err := db.QueryContext(ctx, `
		SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'issues' AND INDEX_NAME = ?
		ORDER BY SEQ_IN_INDEX
	`, name)
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
// generated column present, the issue upsert (issueops.InsertIssueIntoTable,
// INSERT ... ON DUPLICATE KEY UPDATE col = VALUES(col)) of an existing row
// succeeds on issues and on wisps, and the stored everturn_action follows the
// new metadata. A VIRTUAL column failed here on Dolt 2.2.0, VALUES() shifted
// by one column.
func TestForkIndexesEverturnActionUpsert(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	for _, table := range []string{"issues", "wisps"} {
		t.Run(table, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Second)
			issue := &types.Issue{ID: "fx-upsert-" + table, Title: "before upsert", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask,
				CreatedAt: now, UpdatedAt: now, Ephemeral: table == "wisps", Metadata: json.RawMessage(`{"everturn":{"action":"wake"}}`)}
			if err := issueops.InsertIssueIntoTable(ctx, store.db, table, issue); err != nil {
				t.Fatalf("first upsert into %s: %v", table, err)
			}
			issue.Title = "after upsert"
			issue.UpdatedAt = now.Add(time.Second)
			issue.Metadata = json.RawMessage(`{"everturn":{"action":"claim"}}`)
			if err := issueops.InsertIssueIntoTable(ctx, store.db, table, issue); err != nil {
				t.Fatalf("second upsert into %s of an existing row: %v", table, err)
			}
			var title string
			var action sql.NullString
			if err := store.db.QueryRowContext(ctx, "SELECT title, everturn_action FROM "+table+" WHERE id = ?", issue.ID).Scan(&title, &action); err != nil {
				t.Fatalf("read %s.%s after upsert: %v", table, issue.ID, err)
			}
			if title != "after upsert" || !action.Valid || action.String != "claim" {
				t.Errorf("%s.%s after upsert = (%q, everturn_action=%v), want (\"after upsert\", claim)", table, issue.ID, title, action)
			}
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
