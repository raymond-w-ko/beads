package issueops

import (
	"context"
	"errors"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// TestExpiredDefersDueSendsOneRead pins the probe in front of the defer-wake
// sweep (bd-aw3l8): exactly one query, the shared predicate over issues and
// wisps, and no GET_LOCK, START TRANSACTION or write. The mock fails on any
// statement it was not told to expect, so a probe that took the journal
// writer lock or opened a transaction fails here. Every row carries the
// session's @@autocommit (bd-tuwml): "nothing due" needs autocommit on, and a
// session with it off reports due, still in the one statement.
func TestExpiredDefersDueSendsOneRead(t *testing.T) {
	for _, c := range []struct {
		name string
		rows *sqlmock.Rows
		err  error
		want bool
	}{
		{name: "nothing due", rows: sqlmock.NewRows([]string{"due", "ac"}).AddRow(0, 1), want: false},
		{name: "a row due", rows: sqlmock.NewRows([]string{"due", "ac"}).AddRow(1, 1).AddRow(0, 1), want: true},
		{name: "a wisp due", rows: sqlmock.NewRows([]string{"due", "ac"}).AddRow(0, 1).AddRow(1, 1), want: true},
		{name: "autocommit off, nothing due", rows: sqlmock.NewRows([]string{"due", "ac"}).AddRow(0, 0), want: true},
		{name: "autocommit off, a row due", rows: sqlmock.NewRows([]string{"due", "ac"}).AddRow(1, 0).AddRow(0, 0), want: true},
		{name: "no row at all", rows: sqlmock.NewRows([]string{"due", "ac"}), want: true},
		{name: "probe error, wisps missing", err: errors.New("Error 1146 (42S02): table not found: wisps"), want: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer func() { _ = db.Close() }()
			q := mock.ExpectQuery(expiredDefersDueSQL)
			if c.err != nil {
				q.WillReturnError(c.err)
			} else {
				q.WillReturnRows(c.rows)
			}
			if got := ExpiredDefersDue(context.Background(), db); got != c.want {
				t.Errorf("ExpiredDefersDue = %v, want %v", got, c.want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("statements: %v", err)
			}
		})
	}

	upper := strings.ToUpper(expiredDefersDueSQL)
	for _, banned := range []string{"GET_LOCK", "START TRANSACTION", "BEGIN", "UPDATE", "INSERT", "DELETE"} {
		if strings.Contains(upper, banned) {
			t.Errorf("probe %q contains %s", expiredDefersDueSQL, banned)
		}
	}
	for _, table := range []string{"issues", "wisps"} {
		if want := "@@autocommit"; !strings.Contains(expiredDefersDueSQL, want+" AS ac FROM "+table) && !strings.Contains(expiredDefersDueSQL, want+" FROM "+table) {
			t.Errorf("probe %q: the %s branch does not carry %s", expiredDefersDueSQL, table, want)
		}
		if want := "FROM " + table + " WHERE " + expiredDeferPredicate + " LIMIT 1)"; !strings.Contains(expiredDefersDueSQL, want) {
			t.Errorf("probe %q lacks the %s branch %q", expiredDefersDueSQL, table, want)
		}
		if got, want := expiredDeferSelectSQL(table), "SELECT id FROM "+table+" WHERE "+expiredDeferPredicate; got != want {
			t.Errorf("sweep snapshot of %s = %q, want %q", table, got, want)
		}
	}
	if want := " UNION ALL (SELECT 0, @@autocommit)"; !strings.HasSuffix(expiredDefersDueSQL, want) {
		t.Errorf("probe %q does not end with the table-less branch %q that always answers", expiredDefersDueSQL, want)
	}
}

// TestExpiredDefersDueCancelledReportsDue: a probe that cannot run (here a
// cancelled context) reports due, so the caller keeps today's sweep.
func TestExpiredDefersDueCancelledReportsDue(t *testing.T) {
	db, _, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !ExpiredDefersDue(ctx, db) {
		t.Errorf("ExpiredDefersDue on a cancelled context = false, want true (run the sweep)")
	}
}

// TestExpiredDefersDueEndsItsImplicitTransaction (review bd-ee2ci, finding
// 2): on a pinned connection whose session has autocommit off, the probe's
// SELECT opens an implicit transaction, so the probe ends it on the same
// connection with START TRANSACTION and ROLLBACK before it returns; with
// autocommit on it sends the one SELECT and nothing more. When the ending
// fails, the connection is dropped, not returned to its pool.
func TestExpiredDefersDueEndsItsImplicitTransaction(t *testing.T) {
	for _, c := range []struct {
		name       string
		autocommit int
		beginErr   error
		wantOpen   int // pool connections left open after the conn is closed
	}{
		{name: "autocommit on: the one SELECT", autocommit: 1, wantOpen: 1},
		{name: "autocommit off: START TRANSACTION, ROLLBACK on the same conn", autocommit: 0, wantOpen: 1},
		{name: "autocommit off, the ending fails: conn dropped", autocommit: 0, beginErr: errors.New("begin refused"), wantOpen: 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer func() { _ = db.Close() }()
			mock.ExpectQuery(expiredDefersDueSQL).WillReturnRows(sqlmock.NewRows([]string{"due", "ac"}).AddRow(0, c.autocommit))
			if c.autocommit == 0 {
				if c.beginErr != nil {
					mock.ExpectBegin().WillReturnError(c.beginErr)
				} else {
					mock.ExpectBegin()
					mock.ExpectRollback()
				}
			}
			ctx := context.Background()
			conn, err := db.Conn(ctx)
			if err != nil {
				t.Fatalf("pin: %v", err)
			}
			want := c.autocommit == 0
			if got := ExpiredDefersDue(ctx, conn); got != want {
				t.Errorf("ExpiredDefersDue = %v, want %v", got, want)
			}
			_ = conn.Close()
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("statements: %v", err)
			}
			if got := db.Stats().OpenConnections; got != c.wantOpen {
				t.Errorf("open connections after the probe = %d, want %d", got, c.wantOpen)
			}
		})
	}
}
