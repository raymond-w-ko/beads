package dolt

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// The dependents reads (issueops.DepTargetRowsQuery behind
// GetDependentsWithMetadata, iterDependentsQuery behind
// IterDependentsWithMetadata) and the dependents count (DepTargetCountQuery)
// seek one index per target column instead of filtering on the COALESCE
// target expression (bd-o24uj, bd-6kzwe). These tests compare each with the
// old COALESCE statement run directly on the same database (review bd-lwx6j).

// legacyDependentsSlice is the old slice-path read: per table, with no ORDER
// BY (a scan of the primary key id), dependencies before wisp_dependencies.
func legacyDependentsSlice(t *testing.T, ctx context.Context, store *DoltStore, target string) []string {
	t.Helper()
	var out []string
	for _, table := range []string{"dependencies", "wisp_dependencies"} {
		out = append(out, depRowPairs(t, ctx, store,
			"SELECT issue_id, type FROM "+table+" WHERE "+depTargetExpr+" = ?", target)...)
	}
	return out
}

// legacyDependentsJoin is the old iterator statement's rows (sources joined
// to their issue or wisp), as a sorted set.
func legacyDependentsJoin(t *testing.T, ctx context.Context, store *DoltStore, target string) []string {
	t.Helper()
	out := depRowPairs(t, ctx, store, `
		SELECT i.id, d.type FROM issues i JOIN dependencies d ON d.issue_id = i.id
		WHERE COALESCE(d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external) = ?
		UNION ALL
		SELECT w.id, d.type FROM wisps w JOIN wisp_dependencies d ON d.issue_id = w.id
		WHERE COALESCE(d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external) = ?`,
		target, target)
	sort.Strings(out)
	return out
}

func dependentsSlice(t *testing.T, ctx context.Context, store *DoltStore, target string) []string {
	t.Helper()
	deps, err := store.GetDependentsWithMetadata(ctx, target)
	if err != nil {
		t.Fatalf("GetDependentsWithMetadata(%s): %v", target, err)
	}
	var out []string
	for _, d := range deps {
		out = append(out, d.Issue.ID+"/"+string(d.DependencyType))
	}
	return out
}

func dependentsIterSet(t *testing.T, ctx context.Context, store *DoltStore, target string) []string {
	t.Helper()
	it, err := store.IterDependentsWithMetadata(ctx, target)
	if err != nil {
		t.Fatalf("IterDependentsWithMetadata(%s): %v", target, err)
	}
	// An open iterator holds a pooled connection; close it before any write.
	defer it.Close() //nolint:errcheck
	var out []string
	for it.Next(ctx) {
		v := it.Value()
		out = append(out, v.Issue.ID+"/"+string(v.DependencyType))
	}
	if err := it.Err(); err != nil {
		t.Fatalf("IterDependentsWithMetadata(%s) iterate: %v", target, err)
	}
	sort.Strings(out)
	return out
}

// TestDependentsRowsKeepLegacyOrder: one target id can sit in two columns of
// one table. A same-prefix child holds it in depends_on_issue_id; a
// cross-prefix child holds it in depends_on_external even though the target
// exists locally. The old read returned a table's rows in primary-key order;
// a union of per-column branches alone returns them grouped by column, and
// `bd show --children` and the mol reader's current step follow the slice's
// order. GetDependentsWithMetadata must return the old statement's rows in
// the old order.
func TestDependentsRowsKeepLegacyOrder(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	const target = "rev-target"
	createPerm(t, ctx, store, target)
	var sources []string
	for i := 1; i <= 3; i++ {
		for _, prefix := range []string{"rev", "ext"} {
			perm := fmt.Sprintf("%s-p%d", prefix, i)
			wisp := fmt.Sprintf("%s-w%d", prefix, i)
			createPerm(t, ctx, store, perm)
			createWisp(t, ctx, store, wisp)
			sources = append(sources, perm, wisp)
		}
	}
	for _, src := range sources {
		if err := store.AddDependency(ctx, &types.Dependency{IssueID: src, DependsOnID: target, Type: types.DepParentChild}, "tester"); err != nil {
			t.Fatalf("add dependency %s -> %s: %v", src, target, err)
		}
	}

	// Precondition: each table holds the target in two columns, three rows
	// each, and the old order interleaves them, so a read grouped by column
	// cannot pass by chance.
	interleaved := false
	for _, table := range []string{"dependencies", "wisp_dependencies"} {
		var inIssue, inExternal int
		if err := store.db.QueryRowContext(ctx,
			"SELECT SUM(depends_on_issue_id = ? IS TRUE), SUM(depends_on_external = ? IS TRUE) FROM "+table,
			target, target).Scan(&inIssue, &inExternal); err != nil {
			t.Fatalf("column counts on %s: %v", table, err)
		}
		if inIssue != 3 || inExternal != 3 {
			t.Fatalf("precondition: %s holds %s in depends_on_issue_id %d times and depends_on_external %d times, want 3 and 3", table, target, inIssue, inExternal)
		}
		var cols []string
		rows, err := store.db.QueryContext(ctx,
			"SELECT depends_on_issue_id IS NOT NULL FROM "+table+" WHERE "+depTargetExpr+" = ?", target)
		if err != nil {
			t.Fatalf("legacy column order on %s: %v", table, err)
		}
		for rows.Next() {
			var issueCol bool
			if err := rows.Scan(&issueCol); err != nil {
				t.Fatalf("scan: %v", err)
			}
			cols = append(cols, fmt.Sprint(issueCol))
		}
		_ = rows.Close()
		changes := 0
		for i := 1; i < len(cols); i++ {
			if cols[i] != cols[i-1] {
				changes++
			}
		}
		if changes > 1 {
			interleaved = true
		}
	}
	if !interleaved {
		t.Fatalf("precondition: the old order groups rows by column in both tables; pick other source ids")
	}

	old := legacyDependentsSlice(t, ctx, store, target)
	if len(old) != 12 {
		t.Fatalf("precondition: old statement gives %d dependents, want 12: %v", len(old), old)
	}
	if got := dependentsSlice(t, ctx, store, target); !reflect.DeepEqual(got, old) {
		t.Errorf("GetDependentsWithMetadata(%s) order differs from the old statement:\n got %v\nwant %v", target, got, old)
	}
	if got, want := dependentsIterSet(t, ctx, store, target), legacyDependentsJoin(t, ctx, store, target); !reflect.DeepEqual(got, want) {
		t.Errorf("IterDependentsWithMetadata(%s) = %v, old statement = %v", target, got, want)
	}
	if n, err := store.CountDependents(ctx, target); err != nil {
		t.Fatalf("CountDependents: %v", err)
	} else if n != 12 {
		t.Errorf("CountDependents(%s) = %d, want 12", target, n)
	}
}

// TestDependentsMixedCollations: a bare column equality compares under the
// column's collation, the old COALESCE filter under its result's. The
// migrations pin no collation on the three target columns, so they agree on
// every database bd creates; on a database where they do not, each read must
// still return exactly the old statement's rows. Each subtest sets one mix on
// both tables and holds, in every column of both tables, one row targeting
// the id read and one targeting it in another case.
//
// Dolt refuses to change the collation of a column a foreign key uses
// ("unable to change type of column ... as it is used by foreign keys"):
// depends_on_issue_id on both tables (to issues) and
// wisp_dependencies.depends_on_wisp_id (to wisps). So there is no
// issue-column mix (it is refused on both tables), wisp_ci switches
// dependencies.depends_on_wisp_id only, and all_ci switches every column that
// accepts it (dependencies.depends_on_wisp_id and depends_on_external on both
// tables); a refused column keeps its collation and the subtest logs it.
func TestDependentsMixedCollations(t *testing.T) {
	const ci = "utf8mb4_0900_ai_ci"
	for _, c := range []struct {
		name string
		ci   []string // target columns switched to a case-insensitive collation
	}{
		{"external_ci", []string{"depends_on_external"}},
		{"wisp_ci", []string{"depends_on_wisp_id"}},
		{"all_ci", []string{"depends_on_issue_id", "depends_on_wisp_id", "depends_on_external"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			store, cleanup := setupTestStore(t)
			defer cleanup()
			ctx, cancel := testContext(t)
			defer cancel()

			const target = "mc-target"
			createPerm(t, ctx, store, target)
			createWisp(t, ctx, store, "mc-wtarget")

			// Sources first: the fixture's own connection below would starve
			// the store's write path while it is held.
			type row struct{ table, col, source, target string }
			var fixture []row
			for _, plane := range []struct{ table, prefix string }{
				{"dependencies", "mc-p"},
				{"wisp_dependencies", "mc-w"},
			} {
				for _, col := range []string{"depends_on_issue_id", "depends_on_wisp_id", "depends_on_external"} {
					for _, tgt := range []string{target, strings.ToUpper(target)} {
						src := fmt.Sprintf("%s%02d", plane.prefix, len(fixture)+1)
						if plane.table == "dependencies" {
							createPerm(t, ctx, store, src)
						} else {
							createWisp(t, ctx, store, src)
						}
						fixture = append(fixture, row{plane.table, col, src, tgt})
					}
				}
			}

			conn, err := store.db.Conn(ctx)
			if err != nil {
				t.Fatalf("conn: %v", err)
			}
			// Also on a failed fixture step: a held connection starves the
			// pool and the cleanup's drop never returns.
			defer conn.Close() //nolint:errcheck
			exec := func(q string, args ...any) {
				t.Helper()
				if _, err := conn.ExecContext(ctx, q, args...); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			changed := 0
			for _, table := range []string{"dependencies", "wisp_dependencies"} {
				for _, col := range c.ci {
					var colType string
					if err := conn.QueryRowContext(ctx,
						"SELECT COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?",
						table, col).Scan(&colType); err != nil {
						t.Fatalf("column type %s.%s: %v", table, col, err)
					}
					q := fmt.Sprintf("ALTER TABLE %s MODIFY %s %s CHARACTER SET utf8mb4 COLLATE %s NULL", table, col, colType, ci)
					if _, err := conn.ExecContext(ctx, q); err != nil {
						if strings.Contains(err.Error(), "used by foreign keys") {
							t.Logf("left out %s.%s: %v", table, col, err)
							continue
						}
						t.Fatalf("%s: %v", q, err)
					}
					changed++
				}
			}
			if changed == 0 {
				t.Fatalf("precondition: no column of the %s mix accepted a case-insensitive collation", c.name)
			}
			var mix []string
			for _, table := range []string{"dependencies", "wisp_dependencies"} {
				for _, col := range []string{"depends_on_issue_id", "depends_on_wisp_id", "depends_on_external"} {
					var got string
					if err := conn.QueryRowContext(ctx,
						"SELECT COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?",
						table, col).Scan(&got); err != nil {
						t.Fatalf("collation %s.%s: %v", table, col, err)
					}
					mix = append(mix, table+"."+col+"="+got)
				}
			}
			t.Logf("collations: %v", mix)

			// Rows written directly: a target that differs from a local id
			// only by case has no issue to point at, so the fixture turns
			// foreign key checks off on its own connection.
			exec("SET FOREIGN_KEY_CHECKS = 0")
			for i, r := range fixture {
				exec(fmt.Sprintf("INSERT INTO %s (id, issue_id, %s, type, created_at, created_by) VALUES (?, ?, ?, 'blocks', UTC_TIMESTAMP(), 'tester')", r.table, r.col),
					fmt.Sprintf("00000000-0000-0000-0000-%012d", i+1), r.source, r.target)
			}
			exec("SET FOREIGN_KEY_CHECKS = 1")
			if err := conn.Close(); err != nil {
				t.Fatalf("close conn: %v", err)
			}

			for _, read := range []string{target, strings.ToUpper(target)} {
				old := legacyDependentsSlice(t, ctx, store, read)
				t.Logf("read %s: old statement rows %v", read, old)
				if read == target && len(old) < 6 {
					t.Fatalf("precondition: old statement gives %d rows for %s, want at least the 6 exact-case rows: %v", len(old), read, old)
				}
				if got := dependentsSlice(t, ctx, store, read); !reflect.DeepEqual(got, old) {
					t.Errorf("GetDependentsWithMetadata(%s):\n got %v\nwant %v (old statement, in order)", read, got, old)
				}
				if got, want := dependentsIterSet(t, ctx, store, read), legacyDependentsJoin(t, ctx, store, read); !reflect.DeepEqual(got, want) {
					t.Errorf("IterDependentsWithMetadata(%s):\n got %v\nwant %v (old statement)", read, got, want)
				}
				if got, err := store.CountDependents(ctx, read); err != nil {
					t.Fatalf("CountDependents(%s): %v", read, err)
				} else if want := legacyDependentsCount(t, ctx, store.db, read); got != want {
					t.Errorf("CountDependents(%s) = %d, old statement = %d", read, got, want)
				}
			}
		})
	}
}

func legacyDependentsCount(t *testing.T, ctx context.Context, db *sql.DB, target string) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRowContext(ctx,
		"SELECT (SELECT count(*) FROM dependencies WHERE "+depTargetExpr+" = ?) + (SELECT count(*) FROM wisp_dependencies WHERE "+depTargetExpr+" = ?)",
		target, target).Scan(&n); err != nil {
		t.Fatalf("old count %s: %v", target, err)
	}
	return n
}
