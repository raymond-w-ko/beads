//go:build cgo

package embeddeddolt_test

import (
	"context"
	"testing"

	"github.com/steveyegge/beads/backend/conformance"
	"github.com/steveyegge/beads/internal/storage/embeddeddolt"
)

// TestImagerContract runs the Imager contract against the embedded store,
// which runs the SAME body the server-backed store does
// (internal/storage/issueops.ImageInTx) and differs only in how it reaches a
// read transaction and in the driver beneath it. Value shapes are what that
// driver can change, and the revision is a hash of them.
//
// One environment for the whole suite: booting an embedded engine per case
// would dominate the runtime, every case namespaces its seeds, and the history
// delta needs the subtests sequential anyway.
func TestImagerContract(t *testing.T) {
	skipUnlessEmbeddedDolt(t)
	te := newTestEnv(t, "img")
	ctx := t.Context()
	fixture := newEmbeddedImagerFixture(t, te, "img")

	t.Run("RefusesAMalformedRequest", func(t *testing.T) {
		conformance.RunImagerRefusesAMalformedRequest(t, ctx, fixture)
	})
	t.Run("NamesEveryMissingID", func(t *testing.T) {
		conformance.RunImagerNamesEveryMissingID(t, ctx, fixture)
	})
	t.Run("CollapsesDuplicatesInFirstNamedOrder", func(t *testing.T) {
		conformance.RunImagerCollapsesDuplicatesInFirstNamedOrder(t, ctx, fixture)
	})
	t.Run("ReadsEachBeadFromItsOwnPlane", func(t *testing.T) {
		conformance.RunImagerReadsEachBeadFromItsOwnPlane(t, ctx, fixture)
	})
	t.Run("LeavesGeneratedColumnsOut", func(t *testing.T) {
		conformance.RunImagerLeavesGeneratedColumnsOut(t, ctx, fixture)
	})
	t.Run("RevisionMovesOnEveryAssociatedWrite", func(t *testing.T) {
		conformance.RunImagerRevisionMovesOnEveryAssociatedWrite(t, ctx, fixture)
	})
	t.Run("ReadsOneSnapshot", func(t *testing.T) {
		conformance.RunImagerReadsOneSnapshot(t, ctx, fixture)
	})
	t.Run("WritesNothing", func(t *testing.T) {
		conformance.RunImagerWritesNothing(t, ctx, fixture)
	})
}

func newEmbeddedImagerFixture(t *testing.T, te *testEnv, prefix string) conformance.ImagerFixture {
	t.Helper()
	imager, err := te.store.Imager()
	if err != nil {
		t.Fatalf("Imager(): %v", err)
	}
	kit := newEmbeddedRoleFixtureKit(te, prefix)
	return conformance.ImagerFixture{
		IssuePrefix:   kit.IssuePrefix,
		Imager:        imager,
		CreateIssue:   kit.CreateIssue,
		CreateWisp:    kit.CreateWisp,
		AddDependency: kit.AddDependency,
		AddLabel:      te.store.AddLabel,
		AddComment: func(ctx context.Context, issueID, author, text string) error {
			_, err := te.store.AddIssueComment(ctx, issueID, author, text)
			return err
		},
		QueryScalar:  kit.QueryScalar,
		CountHistory: kit.CountHistory,
		// Every withConn transaction opens its own engine, so counting the
		// opens on this store's data directory counts its transactions.
		ReadTransactions: embeddeddolt.CountTransactionsForTest(t, te.store),
	}
}
