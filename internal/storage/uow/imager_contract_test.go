package uow

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/steveyegge/beads/backend/conformance"
	"github.com/steveyegge/beads/internal/storage/dbproxy/proxy"
)

// TestImagerContract runs the Imager contract against the unit-of-work
// provider. It runs the stores' internal/storage/issueops.ImageInTx on its
// transaction's statement runner (imager.go), so it is a check of how this
// backend reaches ONE read transaction, not an independent vote on the body.
//
// One provider for the whole suite (each newUOWRoleFixtureProvider boots a real
// Dolt sql-server) and NO t.Parallel: this backend has no per-test
// copy-on-write branch, so dolt_log and issues are database-global.
func TestImagerContract(t *testing.T) {
	ctx := context.Background()
	fixture := newUOWImagerFixture(t, ctx, "img")

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

func newUOWImagerFixture(t *testing.T, ctx context.Context, prefix string) conformance.ImagerFixture {
	t.Helper()
	provider := newUOWRoleFixtureProvider(t, ctx, prefix)
	// Count below the role: the provider's own pool is swapped for one whose
	// sessions count the transactions they start, so every unit of work the
	// role opens — through whatever its accessor built — moves the count once.
	begun := countUOWTransactions(t, provider)
	// The role under test is the one the capability accessor returns, not a
	// NewImager built beside it: a provider whose accessor hands back a wrong,
	// nil or split-reading role is the regression a constructor call would
	// hide.
	source, ok := provider.(ImagerSource)
	if !ok {
		t.Fatalf("provider %T does not offer the Imager accessor", provider)
	}
	imager, err := source.Imager()
	if err != nil {
		t.Fatalf("Imager(): %v", err)
	}
	if imager == nil {
		t.Fatal("Imager() returned no role and no error")
	}
	kit := newUOWRoleFixtureKit(provider, prefix)
	return conformance.ImagerFixture{
		IssuePrefix:   kit.IssuePrefix,
		Imager:        imager,
		CreateIssue:   kit.CreateIssue,
		CreateWisp:    kit.CreateWisp,
		AddDependency: kit.AddDependency,
		AddLabel: func(ctx context.Context, issueID, label, actor string) error {
			return RunTx(ctx, provider, func(ctx context.Context, uw UnitOfWork) (string, error) {
				return "label " + issueID, uw.LabelUseCase().AddLabel(ctx, issueID, label, actor)
			})
		},
		AddComment: func(ctx context.Context, issueID, author, text string) error {
			return RunTx(ctx, provider, func(ctx context.Context, uw UnitOfWork) (string, error) {
				_, err := uw.CommentUseCase().AddCommentToIssue(ctx, issueID, author, text)
				return "comment on " + issueID, err
			})
		},
		QueryScalar:  kit.QueryScalar,
		CountHistory: kit.CountHistory,
		// The seeds and probes share the counted pool; the contract reads the
		// count only around one Image call, which runs alone.
		ReadTransactions: func() int { return int(begun.Load()) },
	}
}

// countUOWTransactions swaps the pool of the Dolt provider beneath provider for
// one on the same server and database whose sessions count each START
// TRANSACTION, the statement BeginTx opens every unit of work with, and returns
// the count. The provider keeps its own type and accessors; only its sessions
// are instrumented.
func countUOWTransactions(t *testing.T, provider UnitOfWorkProvider) *atomic.Int64 {
	t.Helper()
	p, ok := UnwrapProvider(provider).(*doltSQLProvider)
	if !ok {
		t.Fatalf("provider %T is not backed by a *doltSQLProvider", provider)
	}
	host, port, err := net.SplitHostPort(strings.TrimPrefix(p.serverEndpoint, "tcp:"))
	if err != nil {
		t.Fatalf("server endpoint %q: %v", p.serverEndpoint, err)
	}
	portNum, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("server endpoint %q: %v", p.serverEndpoint, err)
	}
	begun := &atomic.Int64{}
	dsn := buildDSN(proxy.Endpoint{Host: host, Port: portNum}, p.database, "root", "", "")
	original := p.db
	p.db = sql.OpenDB(transactionCountingConnector{dsn: dsn, drv: original.Driver(), begun: begun})
	t.Cleanup(func() { _ = original.Close() })
	return begun
}

// transactionCountingConnector opens dsn behind a transactionCountingConn.
type transactionCountingConnector struct {
	dsn   string
	drv   driver.Driver
	begun *atomic.Int64
}

func (c transactionCountingConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.drv.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return transactionCountingConn{Conn: conn, begun: c.begun}, nil
}

func (c transactionCountingConnector) Driver() driver.Driver { return c.drv }

// transactionCountingConn keeps the optional interfaces the mysql connection
// implements so database/sql takes the same paths it takes over a bare one.
type transactionCountingConn struct {
	driver.Conn
	begun *atomic.Int64
}

func (c transactionCountingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(query)), "START TRANSACTION") {
		c.begun.Add(1)
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c transactionCountingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c transactionCountingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.begun.Add(1)
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c transactionCountingConn) Ping(ctx context.Context) error {
	return c.Conn.(driver.Pinger).Ping(ctx)
}

func (c transactionCountingConn) CheckNamedValue(nv *driver.NamedValue) error {
	return c.Conn.(driver.NamedValueChecker).CheckNamedValue(nv)
}
