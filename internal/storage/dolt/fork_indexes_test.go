package dolt

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// forkIndexNames are the fork-only issues indexes schema.ensureForkIndexes adds
// (internal/storage/schema/fork_indexes.go).
var forkIndexNames = []string{
	"idx_issues_closed_at",
	"idx_issues_status_closed_at",
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
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_status").Scan(&count); err != nil {
		t.Fatalf("count dolt_status: %v", err)
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
