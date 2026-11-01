package dolt

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/steveyegge/beads/internal/storage/doltutil"
)

// Test databases created per test (testdb_<hex>) used to be left on the
// suite's Dolt server because back-to-back DROP DATABASE breaks concurrent
// sessions on other databases ("Error 1105: no root value found in session").
// Leaving them is worse: every schema init walks every database, so a
// branch-local init grows ~0.2s and a fresh init ~0.9s per leaked database,
// and past ~20 the branch-local init outlasts the 5s migration-lock wait
// (bd-5oq1r). Drops serialised and spaced a second apart are harmless.
var (
	testDBDropMu   sync.Mutex
	lastTestDBDrop time.Time
)

const testDBDropSpacing = time.Second

// dropTestDBs drops per-test databases on the test server at port, one at a
// time and at least testDBDropSpacing after the previous drop. Call it after
// the test's own connections to the databases are closed. Best effort.
func dropTestDBs(port int, names ...string) {
	if port == 0 {
		return
	}
	testDBDropMu.Lock()
	defer testDBDropMu.Unlock()
	admin, err := sql.Open("mysql", doltutil.ServerDSN{
		Host: "127.0.0.1", Port: port, User: "root", Timeout: 10 * time.Second,
	}.String())
	if err != nil {
		return
	}
	defer admin.Close()
	for _, name := range names {
		if name == "" {
			continue
		}
		if wait := testDBDropSpacing - time.Since(lastTestDBDrop); wait > 0 {
			time.Sleep(wait)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, _ = admin.ExecContext(ctx, "DROP DATABASE IF EXISTS `"+name+"`") //nolint:gosec // generated test name
		cancel()
		lastTestDBDrop = time.Now()
	}
}
