//go:build cgo

package embeddeddolt_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/storage/embeddeddolt"
	"github.com/steveyegge/beads/internal/types"
)

// The cycle and ancestry probes walk the dependency graph with one recursive
// branch per dependency table, each joined directly on issue_id
// (issueops.cycleReachabilityQuery, isAncestorInTx). These tests pin that the
// rewritten probes still traverse a deep chain end to end: they refuse the
// edge that closes it and admit the edges that do not. The shape tests in
// issueops guard the query form; a graph this small cannot show the old
// per-step materialization cost, so nothing here is timed.

func createChainIssues(t *testing.T, store *embeddeddolt.EmbeddedDoltStore, prefix string, n int) []string {
	t.Helper()
	ctx := t.Context()

	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s-%d", prefix, i)
		issue := &types.Issue{
			ID:        ids[i],
			Title:     fmt.Sprintf("Chain issue %d", i),
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		}
		if err := store.CreateIssue(ctx, issue, "tester"); err != nil {
			t.Fatalf("create issue %s: %v", ids[i], err)
		}
	}
	return ids
}

func TestAddDependencyCycleProbeTraversesLongBlocksChain(t *testing.T) {
	skipUnlessEmbeddedDolt(t)
	store := newTestEnv(t, "cyc").store

	const chainLen = 60
	ids := createChainIssues(t, store, "cyc", chainLen+1)
	chain, fresh := ids[:chainLen], ids[chainLen]
	head, tail := chain[0], chain[chainLen-1]

	ctx := t.Context()

	// head blocks chain[1] blocks ... blocks tail: each issue depends on the
	// one before it, so the probe from tail must walk all 59 edges to reach head.
	for i := 1; i < chainLen; i++ {
		dep := &types.Dependency{IssueID: chain[i], DependsOnID: chain[i-1], Type: types.DepBlocks}
		if err := store.AddDependency(ctx, dep, "tester"); err != nil {
			t.Fatalf("add chain edge %s -> %s: %v", chain[i], chain[i-1], err)
		}
	}

	if err := store.AddDependency(ctx, &types.Dependency{
		IssueID: tail, DependsOnID: fresh, Type: types.DepBlocks,
	}, "tester"); err != nil {
		t.Fatalf("extending the chain from %s onto %s: %v", tail, fresh, err)
	}

	err := store.AddDependency(ctx, &types.Dependency{
		IssueID: head, DependsOnID: tail, Type: types.DepBlocks,
	}, "tester")
	if !errors.Is(err, domain.ErrDependencyCycle) {
		t.Fatalf("closing the chain (%s blocked by %s) error = %v, want errors.Is(domain.ErrDependencyCycle)", head, tail, err)
	}

	deps, err := store.GetDependencyRecords(ctx, head)
	if err != nil {
		t.Fatalf("GetDependencyRecords(%s): %v", head, err)
	}
	if len(deps) != 0 {
		t.Fatalf("refused closing edge was stored: %s has dependencies %v", head, deps)
	}
}

func TestBlockingHierarchyProbeTraversesDeepParentChain(t *testing.T) {
	skipUnlessEmbeddedDolt(t)
	store := newTestEnv(t, "hier").store

	const depth = 30
	ids := createChainIssues(t, store, "hier", depth+1)
	root, leaf := ids[0], ids[depth]

	ctx := t.Context()

	// ids[i] is the child of ids[i-1], so leaf sits 30 parent-child edges
	// below root.
	for i := 1; i <= depth; i++ {
		dep := &types.Dependency{IssueID: ids[i], DependsOnID: ids[i-1], Type: types.DepParentChild}
		if err := store.AddDependency(ctx, dep, "tester"); err != nil {
			t.Fatalf("add parent-child edge %s -> %s: %v", ids[i], ids[i-1], err)
		}
	}

	// root does not reach leaf along any edge, so only the ancestry probe can
	// refuse this one.
	err := store.AddDependency(ctx, &types.Dependency{
		IssueID: leaf, DependsOnID: root, Type: types.DepBlocks,
	}, "tester")
	var conflict *domain.DependencyHierarchyConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("%s blocked by its ancestor %s: error = %v, want *domain.DependencyHierarchyConflictError", leaf, root, err)
	}
	if !conflict.BlockerIsAncestor {
		t.Fatalf("hierarchy conflict = %+v, want BlockerIsAncestor", conflict)
	}
}
