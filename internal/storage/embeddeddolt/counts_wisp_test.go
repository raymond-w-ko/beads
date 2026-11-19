//go:build cgo

package embeddeddolt_test

import (
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// TestCountsIncludeWispDependencies is the regression guard for the PR #4010
// review (be-xbl83). The Count* methods that feed `bd show --json`
// (DependentCount/DependencyCount) must include `wisp_dependencies` edges,
// matching the slice-path relationship APIs (GetDependentsWithMetadata) that
// the human-readable `bd show` already uses.
//
// Before the fix, CountDependents/CountDependencies/CountDependentsByStatus
// queried only the `dependencies` table, so a permanent issue with a wisp
// dependent reported a too-small dependent_count — contradicting the
// dependents the same tool returns elsewhere.
func TestCountsIncludeWispDependencies(t *testing.T) {
	skipUnlessEmbeddedDolt(t)

	te := newTestEnv(t, "ec")
	ctx := t.Context()

	// ec-perm-dep --blocks--> ec-target   (edge in `dependencies`)
	// ec-wisp-dep --blocks--> ec-target   (edge in `wisp_dependencies`)
	for _, issue := range []*types.Issue{
		{ID: "ec-target", Title: "target", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask},
		{ID: "ec-perm-dep", Title: "perm dependent", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask},
		{ID: "ec-wisp-dep", Title: "wisp dependent", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask, Ephemeral: true},
	} {
		if err := te.store.CreateIssue(ctx, issue, "tester"); err != nil {
			t.Fatalf("CreateIssue %s: %v", issue.ID, err)
		}
	}
	for _, dep := range []*types.Dependency{
		{IssueID: "ec-perm-dep", DependsOnID: "ec-target", Type: types.DepBlocks},
		{IssueID: "ec-wisp-dep", DependsOnID: "ec-target", Type: types.DepBlocks},
	} {
		if err := te.store.AddDependency(ctx, dep, "tester"); err != nil {
			t.Fatalf("AddDependency %s->%s: %v", dep.IssueID, dep.DependsOnID, err)
		}
	}

	// Precondition: the slice path bd show's human output uses counts both.
	deps, err := te.store.GetDependentsWithMetadata(ctx, "ec-target")
	if err != nil {
		t.Fatalf("GetDependentsWithMetadata: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("precondition: GetDependentsWithMetadata(ec-target) = %d, want 2", len(deps))
	}

	if n, err := te.store.CountDependents(ctx, "ec-target"); err != nil {
		t.Fatalf("CountDependents: %v", err)
	} else if n != 2 {
		t.Errorf("CountDependents(ec-target) = %d, want 2 (1 perm + 1 wisp); wisp_dependencies edge dropped", n)
	}

	if n, err := te.store.CountDependentsByStatus(ctx, "ec-target", types.StatusOpen); err != nil {
		t.Fatalf("CountDependentsByStatus: %v", err)
	} else if n != 2 {
		t.Errorf("CountDependentsByStatus(ec-target, open) = %d, want 2", n)
	}

	if n, err := te.store.CountDependencies(ctx, "ec-wisp-dep"); err != nil {
		t.Fatalf("CountDependencies: %v", err)
	} else if n != 1 {
		t.Errorf("CountDependencies(ec-wisp-dep) = %d, want 1 (edge lives in wisp_dependencies)", n)
	}

	// CountDependents matches each target column on its own index
	// (issueops.DepTargetCountQuery), so it must still equal the COALESCE
	// count for a wisp target (depends_on_wisp_id) and an external target
	// (depends_on_external), in both tables (bd-4gimb). New sources keep the
	// counts above unchanged.
	const extTarget = "external:ec:capability"
	for _, issue := range []*types.Issue{
		{ID: "ec-wisp-target", Title: "wisp target", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask, Ephemeral: true},
		{ID: "ec-perm-src", Title: "perm source", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask},
		{ID: "ec-wisp-src", Title: "wisp source", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask, Ephemeral: true},
	} {
		if err := te.store.CreateIssue(ctx, issue, "tester"); err != nil {
			t.Fatalf("CreateIssue %s: %v", issue.ID, err)
		}
	}
	for _, tgt := range []string{"ec-wisp-target", extTarget} {
		for _, src := range []string{"ec-perm-src", "ec-wisp-src"} {
			if err := te.store.AddDependency(ctx, &types.Dependency{IssueID: src, DependsOnID: tgt, Type: types.DepBlocks}, "tester"); err != nil {
				t.Fatalf("AddDependency %s->%s: %v", src, tgt, err)
			}
		}
	}
	const coalesce = "COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external)"
	for _, c := range []struct{ target, column string }{
		{"ec-target", "depends_on_issue_id"},
		{"ec-wisp-target", "depends_on_wisp_id"},
		{extTarget, "depends_on_external"},
	} {
		// Precondition: the edges sit in the column this case covers.
		var inColumn int64
		te.queryScalar(t, ctx,
			"SELECT (SELECT count(*) FROM dependencies WHERE "+c.column+" = ?) + (SELECT count(*) FROM wisp_dependencies WHERE "+c.column+" = ?)",
			[]any{c.target, c.target}, &inColumn)
		if inColumn != 2 {
			t.Fatalf("precondition: %d edges to %s in %s, want 2 (1 perm + 1 wisp source)", inColumn, c.target, c.column)
		}
		var oracle int64
		te.queryScalar(t, ctx,
			"SELECT (SELECT count(*) FROM dependencies WHERE "+coalesce+" = ?) + (SELECT count(*) FROM wisp_dependencies WHERE "+coalesce+" = ?)",
			[]any{c.target, c.target}, &oracle)
		if n, err := te.store.CountDependents(ctx, c.target); err != nil {
			t.Fatalf("CountDependents(%s): %v", c.target, err)
		} else if n != oracle || n != 2 {
			t.Errorf("CountDependents(%s) = %d, COALESCE oracle = %d, want both 2", c.target, n, oracle)
		}
	}
}
