package dolt

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

// TestCountAndIterIncludeWispDependencies is the regression guard for the
// PR #4010 review (be-xbl83). The Count*/Iter* paths that feed
// `bd show --json` (DependentCount/DependencyCount and --include-dependents)
// must include `wisp_dependencies` edges, matching the slice-path relationship
// APIs (GetDependentsWithMetadata / GetDependenciesWithMetadata) that the
// human-readable `bd show` already uses.
//
// Before the fix, CountDependents, CountDependencies, CountDependentsByStatus,
// and the streaming IterDependentsWithMetadata queried only the `dependencies`
// table. A permanent issue with a wisp dependent therefore reported a
// too-small dependent_count and omitted the wisp from --include-dependents,
// contradicting the `dependents` slice in the same output.
func TestCountAndIterIncludeWispDependencies(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Build:  wc-perm-dep --blocks--> wc-target   (edge in `dependencies`)
	//         wc-wisp-dep --blocks--> wc-target   (edge in `wisp_dependencies`)
	// so wc-target has one permanent and one wisp dependent.
	createPerm(t, ctx, store, "wc-target")
	createPerm(t, ctx, store, "wc-perm-dep")
	createWisp(t, ctx, store, "wc-wisp-dep")

	addBlocks := func(src, tgt string) {
		t.Helper()
		if err := store.AddDependency(ctx, &types.Dependency{
			IssueID:     src,
			DependsOnID: tgt,
			Type:        types.DepBlocks,
		}, "tester"); err != nil {
			t.Fatalf("add dependency %s -> %s: %v", src, tgt, err)
		}
	}
	addBlocks("wc-perm-dep", "wc-target")
	addBlocks("wc-wisp-dep", "wc-target")

	// Precondition: the slice path bd show's human output uses already counts
	// both dependents. The new count/iter paths must agree with it.
	deps, err := store.GetDependentsWithMetadata(ctx, "wc-target")
	if err != nil {
		t.Fatalf("GetDependentsWithMetadata: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("precondition: GetDependentsWithMetadata(wc-target) = %d, want 2", len(deps))
	}

	// CountDependents must include the wisp dependent.
	if n, err := store.CountDependents(ctx, "wc-target"); err != nil {
		t.Fatalf("CountDependents: %v", err)
	} else if n != 2 {
		t.Errorf("CountDependents(wc-target) = %d, want 2 (1 perm + 1 wisp); wisp_dependencies edge dropped", n)
	}

	// CountDependentsByStatus must include the wisp dependent (both are open).
	if n, err := store.CountDependentsByStatus(ctx, "wc-target", types.StatusOpen); err != nil {
		t.Fatalf("CountDependentsByStatus: %v", err)
	} else if n != 2 {
		t.Errorf("CountDependentsByStatus(wc-target, open) = %d, want 2", n)
	}

	// CountDependencies on the wisp source must count its wisp_dependencies edge.
	if n, err := store.CountDependencies(ctx, "wc-wisp-dep"); err != nil {
		t.Fatalf("CountDependencies: %v", err)
	} else if n != 1 {
		t.Errorf("CountDependencies(wc-wisp-dep) = %d, want 1 (edge lives in wisp_dependencies)", n)
	}

	// CountDependents matches each target column on its own index
	// (issueops.DepTargetCountQuery), so it must still equal the COALESCE
	// count for a wisp target (depends_on_wisp_id) and an external target
	// (depends_on_external), in both tables (bd-4gimb). New sources keep the
	// counts above unchanged.
	createWisp(t, ctx, store, "wc-wisp-target")
	createPerm(t, ctx, store, "wc-perm-src")
	createWisp(t, ctx, store, "wc-wisp-src")
	const extTarget = "external:wc:capability"
	for _, tgt := range []string{"wc-wisp-target", extTarget} {
		addBlocks("wc-perm-src", tgt)
		addBlocks("wc-wisp-src", tgt)
	}
	for _, c := range []struct{ target, column string }{
		{"wc-target", "depends_on_issue_id"},
		{"wc-wisp-target", "depends_on_wisp_id"},
		{extTarget, "depends_on_external"},
	} {
		// Precondition: the edges sit in the column this case covers.
		var inColumn int64
		if err := store.db.QueryRowContext(ctx,
			"SELECT (SELECT count(*) FROM dependencies WHERE "+c.column+" = ?) + (SELECT count(*) FROM wisp_dependencies WHERE "+c.column+" = ?)",
			c.target, c.target).Scan(&inColumn); err != nil {
			t.Fatalf("column count %s: %v", c.target, err)
		}
		if inColumn != 2 {
			t.Fatalf("precondition: %d edges to %s in %s, want 2 (1 perm + 1 wisp source)", inColumn, c.target, c.column)
		}
		var oracle int64
		if err := store.db.QueryRowContext(ctx,
			"SELECT (SELECT count(*) FROM dependencies WHERE "+depTargetExpr+" = ?) + (SELECT count(*) FROM wisp_dependencies WHERE "+depTargetExpr+" = ?)",
			c.target, c.target).Scan(&oracle); err != nil {
			t.Fatalf("COALESCE oracle %s: %v", c.target, err)
		}
		if n, err := store.CountDependents(ctx, c.target); err != nil {
			t.Fatalf("CountDependents(%s): %v", c.target, err)
		} else if n != oracle || n != 2 {
			t.Errorf("CountDependents(%s) = %d, COALESCE oracle = %d, want both 2", c.target, n, oracle)
		}
	}

	// IterDependentsWithMetadata must stream BOTH dependents.
	iter, err := store.IterDependentsWithMetadata(ctx, "wc-target")
	if err != nil {
		t.Fatalf("IterDependentsWithMetadata: %v", err)
	}
	defer iter.Close() //nolint:errcheck
	seen := map[string]bool{}
	for iter.Next(ctx) {
		seen[iter.Value().Issue.ID] = true
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("IterDependentsWithMetadata iterate: %v", err)
	}
	if !seen["wc-perm-dep"] || !seen["wc-wisp-dep"] {
		t.Errorf("IterDependentsWithMetadata(wc-target) streamed %v, want both wc-perm-dep and wc-wisp-dep", seen)
	}

	// Both dependents reads match one target column per UNION ALL branch
	// (issueops.DepTargetRowsQuery, iterDependentsQuery), so for an issue, a
	// wisp and an external target they must return exactly the rows of the
	// old COALESCE statements run directly (bd-6kzwe). More sources on both
	// planes give each target several dependents per table, so the slice
	// path's order (it has no ORDER BY; text `bd show`, dep tree and
	// `--children` print it as returned) is compared too. The iterator above
	// holds a pooled connection until closed, which stalls the writes below.
	_ = iter.Close()
	createPerm(t, ctx, store, "wc-o1")
	createPerm(t, ctx, store, "wc-o2")
	createWisp(t, ctx, store, "wc-ow1")
	for _, src := range []string{"wc-o1", "wc-o2", "wc-ow1"} {
		for _, tgt := range []string{"wc-target", "wc-wisp-target", extTarget} {
			addBlocks(src, tgt)
		}
	}
	for _, target := range []string{"wc-target", "wc-wisp-target", extTarget} {
		var oldSlice []string
		for _, table := range []string{"dependencies", "wisp_dependencies"} {
			oldSlice = append(oldSlice, depRowPairs(t, ctx, store,
				"SELECT issue_id, type FROM "+table+" WHERE "+depTargetExpr+" = ?", target)...)
		}
		if len(oldSlice) != 5 {
			t.Fatalf("precondition: old COALESCE statement gives %d dependents of %s, want 5: %v", len(oldSlice), target, oldSlice)
		}
		deps, err := store.GetDependentsWithMetadata(ctx, target)
		if err != nil {
			t.Fatalf("GetDependentsWithMetadata(%s): %v", target, err)
		}
		var gotSlice []string
		for _, d := range deps {
			gotSlice = append(gotSlice, d.Issue.ID+"/"+string(d.DependencyType))
		}
		if !reflect.DeepEqual(gotSlice, oldSlice) {
			t.Errorf("GetDependentsWithMetadata(%s) = %v, old COALESCE rows in order = %v", target, gotSlice, oldSlice)
		}

		oldIter := depRowPairs(t, ctx, store, `
			SELECT i.id, d.type FROM issues i JOIN dependencies d ON d.issue_id = i.id
			WHERE COALESCE(d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external) = ?
			UNION ALL
			SELECT w.id, d.type FROM wisps w JOIN wisp_dependencies d ON d.issue_id = w.id
			WHERE COALESCE(d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external) = ?`,
			target, target)
		it, err := store.IterDependentsWithMetadata(ctx, target)
		if err != nil {
			t.Fatalf("IterDependentsWithMetadata(%s): %v", target, err)
		}
		var gotIter []string
		var lastCreated time.Time
		for it.Next(ctx) {
			v := it.Value()
			if v.Issue.CreatedAt.Before(lastCreated) {
				t.Errorf("IterDependentsWithMetadata(%s): %s created %v after a row created %v; want ORDER BY created_at", target, v.Issue.ID, v.Issue.CreatedAt, lastCreated)
			}
			lastCreated = v.Issue.CreatedAt
			gotIter = append(gotIter, v.Issue.ID+"/"+string(v.DependencyType))
		}
		if err := it.Err(); err != nil {
			t.Fatalf("IterDependentsWithMetadata(%s) iterate: %v", target, err)
		}
		_ = it.Close()
		// The iterator orders by created_at, which can tie between rows, so
		// its rows are compared as a set.
		sort.Strings(gotIter)
		sort.Strings(oldIter)
		if !reflect.DeepEqual(gotIter, oldIter) || len(gotIter) != 5 {
			t.Errorf("IterDependentsWithMetadata(%s) = %v, old COALESCE join rows = %v, want the same 5", target, gotIter, oldIter)
		}
	}
}

// depRowPairs runs query (selecting an issue id and a dependency type) and
// returns its rows as "id/type", in the order the database returned them.
func depRowPairs(t *testing.T, ctx context.Context, store *DoltStore, query string, args ...any) []string {
	t.Helper()
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		t.Fatalf("query %s: %v", query, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, typ string
		if err := rows.Scan(&id, &typ); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, id+"/"+typ)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}
