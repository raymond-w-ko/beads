//go:build cgo

package embeddeddolt

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
)

// CountTransactionsForTest counts the transactions store begins from now until
// t ends, at the one seam every withConn transaction opens its engine through
// (openTxSQL). A role wrapped around the store cannot reach it, which is what
// lets a contract case tell one transaction from several.
//
// Only transactions on store's own data directory count, so another store open
// in the same process does not move it. t must not run in parallel with
// another test that swaps the seam.
func CountTransactionsForTest(t testing.TB, store *EmbeddedDoltStore) func() int {
	t.Helper()
	var begun atomic.Int64
	prev := openTxSQL
	dir := store.dataDir
	openTxSQL = func(ctx context.Context, dataDir, database, branch string) (*sql.DB, func() error, error) {
		if dataDir == dir {
			begun.Add(1)
		}
		return prev(ctx, dataDir, database, branch)
	}
	t.Cleanup(func() { openTxSQL = prev })
	return func() int { return int(begun.Load()) }
}
