package dolt

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/storage/sqlbuild"
	"github.com/steveyegge/beads/internal/types"
)

// legacyReadyStatusClause is the status clause the ready read sent before
// bd-tu8yl. Dolt cannot turn its OR-with-subquery into an index range, so it
// read the [is_blocked,status] index as [0,0],[NULL,∞): every unblocked issue
// of every status, closed included (bd-7c0u4 Q4). The test keeps it only to
// prove the bound clause returns the same rows.
const legacyReadyStatusClause = "(status = ? OR status IN (SELECT name FROM custom_statuses WHERE category = 'active'))"

// TestReadyStatusRangeIsBounded EXPLAINs the ready read `bd ready` runs
// (Status open) and asserts that Dolt reads the issues index over a bounded
// status range, not the open-ended [NULL, ∞) that scanned every closed issue.
// The statement is built by sqlbuild.BuildReadyWorkWhere from the active
// custom statuses the store's own ready path resolves
// (issueops.ActiveCustomStatusesInTx), not from a hand-written list. It also
// proves that the store's ready read returns exactly the rows the legacy
// subquery clause returned, with a mixed-case status that only a
// case-insensitive comparison would admit.
func TestReadyStatusRangeIsBounded(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	if err := store.SetConfig(ctx, "status.custom", "review:active,parked:frozen"); err != nil {
		t.Fatalf("set custom statuses: %v", err)
	}

	// The create path validates status against the comma-split config string,
	// so issues are created open and then moved, as custom-status issues are.
	mk := func(id string, status types.Status) {
		t.Helper()
		iss := &types.Issue{ID: id, Title: "Issue " + id, Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		if status != types.StatusOpen {
			if err := store.UpdateIssue(ctx, id, map[string]interface{}{"status": string(status)}, "tester"); err != nil {
				t.Fatalf("move %s to %s: %v", id, status, err)
			}
		}
	}
	mk("rp-open-1", types.StatusOpen)
	mk("rp-open-2", types.StatusOpen)
	mk("rp-wip-1", types.StatusInProgress)
	mk("rp-review-1", types.Status("review"))
	mk("rp-review-2", types.Status("review"))
	mk("rp-parked-1", types.Status("parked"))
	mk("rp-mixed-1", types.StatusOpen)
	const closedCount = 40
	for i := 0; i < closedCount; i++ {
		mk(fmt.Sprintf("rp-closed-%02d", i), types.StatusOpen)
	}
	// Bulk-close and set the mixed-case status directly: the plan and the row
	// sets are what is under test, not the close or update paths.
	if _, err := store.db.ExecContext(ctx,
		"UPDATE issues SET status = 'closed', closed_at = UTC_TIMESTAMP() WHERE id LIKE 'rp-closed-%'"); err != nil {
		t.Fatalf("close seed issues: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE issues SET status = 'REVIEW' WHERE id = 'rp-mixed-1'"); err != nil {
		t.Fatalf("set mixed-case status: %v", err)
	}

	filter := types.WorkFilter{Status: types.StatusOpen}
	if !sqlbuild.NeedsActiveCustomStatuses(filter) {
		t.Fatalf("the bd ready filter must resolve active custom statuses")
	}
	active, sameCollation, err := issueops.ActiveCustomStatusesInTx(ctx, store.db, "issues")
	if err != nil {
		t.Fatalf("ActiveCustomStatusesInTx: %v", err)
	}
	if want := []string{"review"}; !reflect.DeepEqual(active, want) {
		t.Fatalf("active custom statuses = %v, want %v", active, want)
	}
	// The schema pins no collation on either column, so they match and the
	// literal list is what the ready path sends.
	if !sameCollation {
		t.Fatalf("default schema: custom_statuses.name and issues.status collations reported different, want same")
	}

	whereSQL, whereArgs, err := sqlbuild.BuildReadyWorkWhere(filter, issueops.IssuesFilterTables,
		sqlbuild.ReadyWorkWhereInputs{ActiveCustomStatuses: active, StatusCollationMatches: sameCollation})
	if err != nil {
		t.Fatalf("BuildReadyWorkWhere: %v", err)
	}
	order := sqlbuild.BuildReadyWorkOrder(filter.SortPolicy, "created_at", "priority")
	readyQuery := "SELECT id FROM issues " + whereSQL + " " + order.SQL
	readyArgs := append(append([]any{}, whereArgs...), order.Args...)

	plan := explainPlan(t, ctx, store.db, literalizeParams(readyQuery, sqlLiterals(t, readyArgs)...))
	if !looksLikeDoltPlan(plan) {
		t.Skipf("EXPLAIN output not in a recognized Dolt plan format, skipping range assertion; plan=\n%s", plan)
	}
	if !strings.Contains(plan, "IndexedTableAccess(issues)") {
		t.Fatalf("ready read does not seek an issues index.\nquery: %s\nplan:\n%s", readyQuery, plan)
	}
	if strings.Contains(whereSQL, "custom_statuses") {
		t.Fatalf("default schema: ready clause kept the custom_statuses subquery:\n%s", whereSQL)
	}
	if strings.Contains(plan, "[NULL, ∞)") {
		t.Fatalf("ready read scans the open-ended status range [NULL, ∞), every unblocked issue of every status.\nquery: %s\nplan:\n%s", readyQuery, plan)
	}

	// Row equality with the legacy clause, which the builder no longer emits.
	// Built without custom statuses, the clause is "status = ?" bound to open,
	// the same single argument the legacy clause took.
	exactWhere, exactArgs, err := sqlbuild.BuildReadyWorkWhere(filter, issueops.IssuesFilterTables, sqlbuild.ReadyWorkWhereInputs{})
	if err != nil {
		t.Fatalf("BuildReadyWorkWhere (no custom statuses): %v", err)
	}
	const exactPrefix = "WHERE status = ? AND "
	if !strings.HasPrefix(exactWhere, exactPrefix) {
		t.Fatalf("clause without custom statuses = %q, want prefix %q", exactWhere, exactPrefix)
	}
	legacyQuery := "SELECT id FROM issues WHERE " + legacyReadyStatusClause + " AND " + strings.TrimPrefix(exactWhere, exactPrefix)
	legacyIDs := queryIDSet(t, ctx, store.db, legacyQuery, exactArgs)
	boundIDs := queryIDSet(t, ctx, store.db, "SELECT id FROM issues "+whereSQL, whereArgs)
	if !reflect.DeepEqual(boundIDs, legacyIDs) {
		t.Fatalf("bound clause rows = %v, legacy clause rows = %v", boundIDs, legacyIDs)
	}

	ready, err := store.GetReadyWork(ctx, filter)
	if err != nil {
		t.Fatalf("GetReadyWork: %v", err)
	}
	var readyIDs []string
	for _, iss := range ready {
		if strings.HasPrefix(iss.ID, "rp-") {
			readyIDs = append(readyIDs, iss.ID)
		}
	}
	sort.Strings(readyIDs)
	var legacySeeded []string
	for _, id := range legacyIDs {
		if strings.HasPrefix(id, "rp-") {
			legacySeeded = append(legacySeeded, id)
		}
	}
	if !reflect.DeepEqual(readyIDs, legacySeeded) {
		t.Fatalf("GetReadyWork ids = %v, legacy clause ids = %v", readyIDs, legacySeeded)
	}
	if want := []string{"rp-open-1", "rp-open-2", "rp-review-1", "rp-review-2"}; !reflect.DeepEqual(readyIDs, want) {
		t.Fatalf("GetReadyWork ids = %v, want %v", readyIDs, want)
	}
}

// queryIDSet runs query and returns the sorted ids it selects.
func queryIDSet(t *testing.T, ctx context.Context, db *sql.DB, query string, args []any) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		t.Fatalf("query %s: %v", query, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	sort.Strings(ids)
	return ids
}

// sqlLiterals renders bound arguments as SQL literals for literalizeParams.
func sqlLiterals(t *testing.T, args []any) []string {
	t.Helper()
	out := make([]string, len(args))
	for i, a := range args {
		switch v := a.(type) {
		case string:
			out[i] = "'" + strings.ReplaceAll(v, "'", "''") + "'"
		case time.Time:
			out[i] = "'" + v.UTC().Format("2006-01-02 15:04:05") + "'"
		case int:
			out[i] = fmt.Sprint(v)
		default:
			t.Fatalf("sqlLiterals: unsupported argument %T (%v)", a, a)
		}
	}
	return out
}

// TestReadyStatusMixedCollations covers review bd-s9czb: a literal status IN
// list compares under issues.status's collation, the legacy subquery under
// custom_statuses.name's. On a database whose two columns differ, the ready
// path must keep the legacy clause's rows: here GetReadyWork,
// GetReadyWorkWithCounts and CountReadyWork are each compared with the legacy
// clause run directly. Each fixture ALTERs the columns on its own test branch
// (branch-local, dropped with it) and seeds an active "review" status plus an
// issue stored as "REVIEW", which only a case-insensitive comparison admits.
func TestReadyStatusMixedCollations(t *testing.T) {
	const (
		bin = "utf8mb4_0900_bin"
		ci  = "utf8mb4_0900_ai_ci"
	)
	for _, fx := range []struct {
		name            string
		statusCollation string
		nameCollation   string
		wantSame        bool
		wantReview      bool // does the legacy clause admit the "REVIEW" issue?
	}{
		{name: "binary status, case-insensitive name", statusCollation: bin, nameCollation: ci, wantSame: false, wantReview: true},
		{name: "case-insensitive status, binary name", statusCollation: ci, nameCollation: bin, wantSame: false, wantReview: false},
		{name: "both case-insensitive", statusCollation: ci, nameCollation: ci, wantSame: true, wantReview: true},
	} {
		t.Run(fx.name, func(t *testing.T) {
			store, cleanup := setupTestStore(t)
			defer cleanup()
			ctx, cancel := testContext(t)
			defer cancel()

			if err := store.SetConfig(ctx, "status.custom", "review:active,parked:frozen"); err != nil {
				t.Fatalf("set custom statuses: %v", err)
			}
			for _, ddl := range []string{
				"ALTER TABLE issues MODIFY status VARCHAR(32) COLLATE " + fx.statusCollation + " NOT NULL DEFAULT 'open'",
				"ALTER TABLE custom_statuses MODIFY name VARCHAR(64) COLLATE " + fx.nameCollation + " NOT NULL",
			} {
				if _, err := store.db.ExecContext(ctx, ddl); err != nil {
					t.Fatalf("%s: %v", ddl, err)
				}
			}

			mk := func(id string) {
				t.Helper()
				iss := &types.Issue{ID: id, Title: "Issue " + id, Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
				if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
					t.Fatalf("create %s: %v", id, err)
				}
			}
			for _, id := range []string{"mc-open", "mc-review", "mc-upper", "mc-parked", "mc-wip", "mc-closed"} {
				mk(id)
			}
			for id, status := range map[string]string{
				"mc-review": "review", "mc-upper": "REVIEW", "mc-parked": "parked",
				"mc-wip": "in_progress", "mc-closed": "closed",
			} {
				if _, err := store.db.ExecContext(ctx, "UPDATE issues SET status = ? WHERE id = ?", status, id); err != nil {
					t.Fatalf("set %s status %s: %v", id, status, err)
				}
			}

			_, same, err := issueops.ActiveCustomStatusesInTx(ctx, store.db, "issues")
			if err != nil {
				t.Fatalf("ActiveCustomStatusesInTx: %v", err)
			}
			if same != fx.wantSame {
				t.Errorf("collations reported same = %v, want %v", same, fx.wantSame)
			}

			// The legacy clause, run directly with the rest of the ready
			// predicate exactly as the builder emits it.
			filter := types.WorkFilter{Status: types.StatusOpen}
			exactWhere, exactArgs, err := sqlbuild.BuildReadyWorkWhere(filter, issueops.IssuesFilterTables, sqlbuild.ReadyWorkWhereInputs{})
			if err != nil {
				t.Fatalf("BuildReadyWorkWhere: %v", err)
			}
			const exactPrefix = "WHERE status = ? AND "
			if !strings.HasPrefix(exactWhere, exactPrefix) {
				t.Fatalf("clause without custom statuses = %q, want prefix %q", exactWhere, exactPrefix)
			}
			legacyAll := queryIDSet(t, ctx, store.db,
				"SELECT id FROM issues WHERE "+legacyReadyStatusClause+" AND "+strings.TrimPrefix(exactWhere, exactPrefix), exactArgs)
			legacy := withPrefix(legacyAll, "mc-")
			wantLegacy := []string{"mc-open", "mc-review"}
			if fx.wantReview {
				wantLegacy = []string{"mc-open", "mc-review", "mc-upper"}
			}
			if !reflect.DeepEqual(legacy, wantLegacy) {
				t.Fatalf("fixture check: legacy clause ids = %v, want %v", legacy, wantLegacy)
			}

			ready, err := store.GetReadyWork(ctx, filter)
			if err != nil {
				t.Fatalf("GetReadyWork: %v", err)
			}
			var readyIDs []string
			for _, iss := range ready {
				readyIDs = append(readyIDs, iss.ID)
			}
			if got := withPrefix(readyIDs, "mc-"); !reflect.DeepEqual(got, legacy) {
				t.Errorf("GetReadyWork ids = %v, legacy clause ids = %v", got, legacy)
			}

			withCounts, err := store.GetReadyWorkWithCounts(ctx, filter)
			if err != nil {
				t.Fatalf("GetReadyWorkWithCounts: %v", err)
			}
			var countedIDs []string
			for _, iwc := range withCounts {
				if iwc != nil && iwc.Issue != nil {
					countedIDs = append(countedIDs, iwc.Issue.ID)
				}
			}
			if got := withPrefix(countedIDs, "mc-"); !reflect.DeepEqual(got, legacy) {
				t.Errorf("GetReadyWorkWithCounts ids = %v, legacy clause ids = %v", got, legacy)
			}

			n, err := store.CountReadyWork(ctx, filter)
			if err != nil {
				t.Fatalf("CountReadyWork: %v", err)
			}
			if n != len(legacyAll) {
				t.Errorf("CountReadyWork = %d, legacy clause count = %d (%v)", n, len(legacyAll), legacyAll)
			}
		})
	}
}

// withPrefix returns the sorted ids that start with prefix.
func withPrefix(ids []string, prefix string) []string {
	var out []string
	for _, id := range ids {
		if strings.HasPrefix(id, prefix) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}
