package dolt

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"sync/atomic"
	"testing"

	"github.com/steveyegge/beads/backend/conformance"
)

// TestImagerContract runs the Imager contract against the server-backed store,
// which runs internal/storage/issueops.ImageInTx in its own read transaction.
//
// The cases are subtests of one parent so the whole role suite shares one store
// and one copy-on-write branch, which is why every case namespaces its seeds
// under fixture.IssuePrefix plus its own tag. setupTestStore already marks the
// PARENT parallel; no subtest here calls t.Parallel, because WritesNothing
// takes a before/after history delta.
func TestImagerContract(t *testing.T) {
	fixture, ctx, cleanup := newDoltImagerFixture(t, "img")
	defer cleanup()

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

// newDoltImagerFixture composes the frozen role kit with this backend's
// accessor and the label and comment verbs the kit does not carry.
func newDoltImagerFixture(t *testing.T, prefix string) (conformance.ImagerFixture, context.Context, func()) {
	t.Helper()
	store, storeCleanup := setupTestStore(t)
	ctx, cancel := testContext(t)
	stop := func() {
		cancel()
		storeCleanup()
	}
	// The Imager reads through a second store on the same branch whose pool
	// counts the transactions its sessions begin: withReadTx's BeginTx is the
	// one seam every Image passes, so a split request moves the count more
	// than once. The seeds go through the primary store, which they leave
	// committed on the branch the counting store reads.
	counter := &doltImagerTxCounter{}
	reader := openDoltImagerCountingStore(t, ctx, store, counter)
	imager, err := reader.Imager()
	if err != nil {
		stop()
		t.Fatalf("Imager(): %v", err)
	}
	kit := newDoltRoleFixtureKit(store, prefix)
	return conformance.ImagerFixture{
		IssuePrefix:   kit.IssuePrefix,
		Imager:        imager,
		CreateIssue:   kit.CreateIssue,
		CreateWisp:    kit.CreateWisp,
		AddDependency: kit.AddDependency,
		AddLabel:      store.AddLabel,
		AddComment: func(ctx context.Context, issueID, author, text string) error {
			_, err := store.AddIssueComment(ctx, issueID, author, text)
			return err
		},
		QueryScalar:      kit.QueryScalar,
		CountHistory:     kit.CountHistory,
		ReadTransactions: func() int { return int(counter.begun.Load()) },
	}, ctx, stop
}

// openDoltImagerCountingStore is openSecondStoreOnBranch with the store's pool
// swapped for a single-session one whose connections count their BeginTx in
// counter.
func openDoltImagerCountingStore(t *testing.T, ctx context.Context, primary *DoltStore, counter *doltImagerTxCounter) *DoltStore {
	t.Helper()
	store := openSecondStoreOnBranch(t, ctx, primary)
	db := sql.OpenDB(doltImagerCountingConnector{dsn: store.connStr, drv: store.db.Driver(), counter: counter})
	db.SetMaxOpenConns(1) // DOLT_CHECKOUT below is session-level
	_ = store.db.Close()
	store.db = db // closed with the store
	var branch string
	if err := primary.db.QueryRowContext(ctx, "SELECT active_branch()").Scan(&branch); err != nil {
		t.Fatalf("read test branch: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", branch); err != nil {
		t.Fatalf("check counting store out to %s: %v", branch, err)
	}
	return store
}

// doltImagerTxCounter is the count the counting store's sessions share.
type doltImagerTxCounter struct{ begun atomic.Int64 }

// doltImagerCountingConnector hands database/sql the live server's connection
// for dsn behind a doltImagerCountingConn.
type doltImagerCountingConnector struct {
	dsn     string
	drv     driver.Driver
	counter *doltImagerTxCounter
}

func (c doltImagerCountingConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.drv.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return doltImagerCountingConn{Conn: conn, counter: c.counter}, nil
}

func (c doltImagerCountingConnector) Driver() driver.Driver { return c.drv }

// doltImagerCountingConn counts each transaction it begins and passes every
// statement through, keeping the optional interfaces the driver implements so
// database/sql takes the paths it takes over a bare connection.
type doltImagerCountingConn struct {
	driver.Conn
	counter *doltImagerTxCounter
}

func (c doltImagerCountingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c doltImagerCountingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c doltImagerCountingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.counter.begun.Add(1)
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}
