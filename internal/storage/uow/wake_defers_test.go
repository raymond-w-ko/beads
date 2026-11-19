package uow

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// errSweepOpened stands for "the sweep opened its unit of work": the fakes
// below fail NewUOW with it, so WakeExpiredDefers returns it exactly when
// today's path ran.
var errSweepOpened = errors.New("sweep opened a unit of work")

// probeProbeSQL matches the probe's issues branch (issueops.ExpiredDefersDue).
var probeProbeSQL = regexp.QuoteMeta("FROM issues WHERE status = 'deferred' AND defer_until IS NOT NULL AND defer_until <= UTC_TIMESTAMP() LIMIT 1)")

// wakeFakeProvider opens no real unit of work: NewUOW counts and fails.
type wakeFakeProvider struct{ newUOW int }

func (p *wakeFakeProvider) NewUOW(context.Context) (UnitOfWork, error) {
	p.newUOW++
	return nil, errSweepOpened
}

func (p *wakeFakeProvider) Close(context.Context) error { return nil }

// wakeMaintenanceProvider adds RunNonTx on a pinned connection of db, as
// doltSQLProvider does, or fails it with runErr.
type wakeMaintenanceProvider struct {
	wakeFakeProvider
	db     *sql.DB
	runErr error
}

func (p *wakeMaintenanceProvider) RunNonTx(ctx context.Context, fn func(ctx context.Context, conn *sql.Conn) error) error {
	if p.runErr != nil {
		return p.runErr
	}
	conn, err := p.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	return fn(ctx, conn)
}

// TestWakeExpiredDefersProbeFirst covers the uow route of bd-aw3l8: with
// RunNonTx, a probe that finds nothing due returns at once and opens no unit
// of work (no journal writer lock, no transaction); a probe that finds a row,
// or reads a session with autocommit off (bd-tuwml), or fails, and a
// provider without RunNonTx, take today's path.
func TestWakeExpiredDefersProbeFirst(t *testing.T) {
	newMock := func(t *testing.T, due bool, autocommit int) *sql.DB {
		t.Helper()
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock.New: %v", err)
		}
		t.Cleanup(func() {
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("statements: %v", err)
			}
			_ = db.Close()
		})
		// The probe's rows: one per table with a row due, then the
		// table-less branch, each with the session's @@autocommit.
		rows := sqlmock.NewRows([]string{"due", "ac"})
		if due {
			rows.AddRow(1, autocommit)
		}
		rows.AddRow(0, autocommit)
		mock.ExpectQuery(probeProbeSQL).WillReturnRows(rows)
		if autocommit == 0 {
			// On RunNonTx's pinned connection the probe ends the implicit
			// transaction its SELECT opened (review bd-ee2ci, finding 2).
			mock.ExpectBegin()
			mock.ExpectRollback()
		}
		return db
	}

	t.Run("RunNonTx, nothing due: no unit of work", func(t *testing.T) {
		p := &wakeMaintenanceProvider{db: newMock(t, false, 1)}
		n, err := WakeExpiredDefers(context.Background(), p)
		if err != nil || n != 0 {
			t.Errorf("WakeExpiredDefers = %d, %v; want 0, nil", n, err)
		}
		if p.newUOW != 0 {
			t.Errorf("NewUOW called %d times with nothing due, want 0", p.newUOW)
		}
	})
	t.Run("RunNonTx, a row due: today's path", func(t *testing.T) {
		p := &wakeMaintenanceProvider{db: newMock(t, true, 1)}
		if _, err := WakeExpiredDefers(context.Background(), p); !errors.Is(err, errSweepOpened) {
			t.Errorf("WakeExpiredDefers err = %v, want the sweep's unit of work", err)
		}
		if p.newUOW != 1 {
			t.Errorf("NewUOW called %d times with a row due, want 1", p.newUOW)
		}
	})
	t.Run("RunNonTx, autocommit off: today's path", func(t *testing.T) {
		// On such a session the bare probe may read a retained snapshot
		// (review bd-w6ppm), so it reports due and the sweep decides.
		p := &wakeMaintenanceProvider{db: newMock(t, false, 0)}
		if _, err := WakeExpiredDefers(context.Background(), p); !errors.Is(err, errSweepOpened) {
			t.Errorf("WakeExpiredDefers err = %v, want the sweep's unit of work", err)
		}
		if p.newUOW != 1 {
			t.Errorf("NewUOW called %d times on an autocommit-off session, want 1", p.newUOW)
		}
	})
	t.Run("claim: no probe, the sweep's unit of work", func(t *testing.T) {
		// review bd-ee2ci, finding 1: a claim sweeps as before the probe.
		// The mock expects no statement, so a probe would fail it.
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock.New: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		p := &wakeMaintenanceProvider{db: db}
		WakeExpiredDefersForClaimAdvisory(context.Background(), p)
		if p.newUOW != 1 {
			t.Errorf("NewUOW called %d times by the claim's sweep, want 1", p.newUOW)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("statements: %v", err)
		}
	})
	t.Run("RunNonTx fails: today's path", func(t *testing.T) {
		p := &wakeMaintenanceProvider{runErr: errors.New("uow: provider does not support non-transactional maintenance")}
		if _, err := WakeExpiredDefers(context.Background(), p); !errors.Is(err, errSweepOpened) {
			t.Errorf("WakeExpiredDefers err = %v, want the sweep's unit of work", err)
		}
		if p.newUOW != 1 {
			t.Errorf("NewUOW called %d times after a failed RunNonTx, want 1", p.newUOW)
		}
	})
	t.Run("no RunNonTx: today's path", func(t *testing.T) {
		p := &wakeFakeProvider{}
		if _, err := WakeExpiredDefers(context.Background(), p); !errors.Is(err, errSweepOpened) {
			t.Errorf("WakeExpiredDefers err = %v, want the sweep's unit of work", err)
		}
		if p.newUOW != 1 {
			t.Errorf("NewUOW called %d times without RunNonTx, want 1", p.newUOW)
		}
	})
}
