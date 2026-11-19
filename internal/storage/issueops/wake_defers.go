package issueops

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"time"

	"github.com/steveyegge/beads/internal/storage/dberrors"
	"github.com/steveyegge/beads/internal/types"
)

// DeferWakeActor is the actor recorded on the status_changed event when the
// wake sweep returns an expired dated defer to open. A constant rather than
// the invoking session's actor: the wake is the system honoring the defer
// date, not something the reader who happened to trigger it did.
const DeferWakeActor = "bd-defer-wake"

// WakeDefersResult reports what one sweep woke, per table.
type WakeDefersResult struct {
	// Issues are the permanent-table issue ids returned to open; only these
	// affect Dolt-versioned tables, so only these decide whether the caller
	// mints a dolt commit.
	Issues []string
	// Wisps are the woken wisp ids. Wisp tables are dolt_ignored, so a
	// wisp-only wake never needs a version commit.
	Wisps []string
}

// WakeDefersCommitMessage names a sweep's dolt commit. n is the number of
// permanent issues woken (len(result.Issues)); callers with n == 0 should not
// commit at all.
func WakeDefersCommitMessage(n int) string {
	return fmt.Sprintf("bd: wake %d expired defer(s)", n)
}

// WakeExpiredDefersInTx returns every DATED defer whose date has passed to
// open: status='deferred' AND defer_until <= now flips to status='open',
// defer_until=NULL — byte-identical to what `bd undefer` writes, so a later
// dateless `bd defer` cannot inherit a stale past date and instantly re-wake.
// A DATELESS defer (defer_until IS NULL) is the indefinite icebox and is
// deliberately never touched: `bd undefer` stays its only exit.
//
// This is the lazy half of the defer contract. `bd defer --until` and
// `bd update --defer` promise "hidden until <date>", but nothing ever flipped
// the status back, so an expired defer stayed invisible to the ready front
// forever. Callers run this sweep at the top of ready-work reads and claims;
// the snapshot-then-recheck shape mirrors ReclaimExpiredLeasesInTx, so a bead
// re-deferred or claimed between the snapshot and its UPDATE is simply skipped.
//
// The caller owns Dolt versioning (commit iff len(result.Issues) > 0) and must
// treat sweep failure as advisory — a ready listing never fails because the
// wake could not run.
func WakeExpiredDefersInTx(ctx context.Context, tx DBTX) (WakeDefersResult, error) {
	var result WakeDefersResult
	issues, err := wakeExpiredDefersInTable(ctx, tx, "issues", "events")
	if err != nil {
		return result, err
	}
	result.Issues = issues
	// Wisps carry the same status/defer_until columns and `bd defer` reaches
	// them through the same UpdateIssue routing. The table is tolerated absent
	// for pre-wisp databases, like every other wisp probe.
	wisps, err := wakeExpiredDefersInTable(ctx, tx, "wisps", "wisp_events")
	if err != nil {
		if dberrors.IsTableNotExist(err) {
			return result, nil
		}
		return result, err
	}
	result.Wisps = wisps
	return result, nil
}

// expiredDeferPredicate is the row the sweep wakes: a dated defer whose date
// has passed. The sweep's snapshot SELECTs (expiredDeferSelectSQL) and the
// probe (expiredDefersDueSQL) are both built from it, so the probe says due
// exactly when a snapshot would find a row (bd-aw3l8).
const expiredDeferPredicate = "status = 'deferred' AND defer_until IS NOT NULL AND defer_until <= UTC_TIMESTAMP()"

// expiredDeferSelectSQL is the sweep's snapshot of table.
func expiredDeferSelectSQL(table string) string {
	return "SELECT id FROM " + table + " WHERE " + expiredDeferPredicate
}

// expiredDefersDueSQL asks whether either table holds a row the sweep would
// wake, and what the session's autocommit is. The first two branches are the
// sweep's own snapshot predicate, one row each at most: each keeps its LIMIT
// 1, so each seeks its status index as the snapshot does (Dolt 2.2.0:
// IndexedTableAccess on [status, closed_at] for issues and [status] for
// wisps) and stops at its first row. The third branch reads no table, so the
// answer always has a row that carries @@autocommit: the probe learns the
// session's autocommit from the statement it sends anyway (bd-tuwml).
const expiredDefersDueSQL = "(SELECT 1 AS due, @@autocommit AS ac FROM issues WHERE " + expiredDeferPredicate + " LIMIT 1)" +
	" UNION ALL (SELECT 1, @@autocommit FROM wisps WHERE " + expiredDeferPredicate + " LIMIT 1)" +
	" UNION ALL (SELECT 0, @@autocommit)"

// ExpiredDefersDue reports whether WakeExpiredDefersInTx could find a row to
// wake. It is one read, outside any transaction and lock, so a ready READ can
// skip the sweep's journal writer lock and write transaction (GET_LOCK,
// START, two SELECTs, COMMIT, RELEASE_LOCK) when nothing is due, which is
// almost every ready read. It is only a hint: a row that comes due or is
// woken after it reads is the same race the sweep has with the ready read
// that follows it, and the sweep re-checks every row under the lock. Any
// error, a database without wisps included, reports true, so the caller
// runs the sweep exactly as before.
//
// Reads only, never a claim (review bd-ee2ci, finding 1): its answer is for
// the instant it reads, and a claim waits for the journal writer lock after
// it, so a claim that took "nothing due" here would miss an issue the lock
// holder's commit made due while it waited. Claims sweep without it
// (dolt.wakeExpiredDefersForClaim, uow.WakeExpiredDefersForClaimAdvisory).
//
// It answers "nothing due" only from a session with autocommit on. With
// autocommit off (a server run with --no-auto-commit) a bare SELECT joins
// the implicit transaction an earlier bare statement on that pooled session
// left open, so it can read a snapshot older than a row deferred since
// (review bd-w6ppm, finding 2). There it reports due, and the sweep, whose
// START TRANSACTION reads the latest commit, decides as before the probe.
//
// On such a session the probe's own SELECT leaves an implicit transaction
// open, and a later bare read on the session would keep its snapshot (review
// bd-ee2ci, finding 2). So when q is a pinned *sql.Conn and the answer says
// autocommit is off, the probe ends that transaction on the same connection
// before it returns: START TRANSACTION, then ROLLBACK of the empty
// transaction it began. START TRANSACTION, not a bare ROLLBACK, because it
// commits whatever an earlier statement left pending on the session, as the
// START TRANSACTION of HEAD's sweep (and of any later transaction there)
// did; the ROLLBACK then discards only the probe's empty transaction, so
// nothing HEAD would have committed is rolled back. If that cannot run, the
// connection is dropped rather than returned to its pool with the snapshot.
// A caller on a pool must pin a connection for the probe (dolt's
// expiredDefersDue does; uow's RunNonTx hands it one); with autocommit on
// nothing more is sent.
func ExpiredDefersDue(ctx context.Context, q DBTX) bool {
	due, autocommitOff := expiredDefersDue(ctx, q)
	if conn, ok := q.(*sql.Conn); ok && autocommitOff {
		endImplicitTransaction(ctx, conn)
	}
	return due
}

// expiredDefersDue sends the probe and reports its answer, and whether any
// row said the session's autocommit is off.
func expiredDefersDue(ctx context.Context, q DBTX) (due, autocommitOff bool) {
	rows, err := q.QueryContext(ctx, expiredDefersDueSQL)
	if err != nil {
		return true, false
	}
	defer func() { _ = rows.Close() }()
	answered := false
	due = false
	for rows.Next() {
		var d, autocommit int64
		if err := rows.Scan(&d, &autocommit); err != nil {
			return true, autocommitOff
		}
		if autocommit == 0 {
			autocommitOff = true
		}
		if d != 0 || autocommit == 0 {
			due = true
		}
		answered = true
	}
	if rows.Err() != nil || !answered {
		return true, autocommitOff
	}
	return due, autocommitOff
}

// endImplicitTransaction ends, on conn, the implicit transaction a bare
// statement opened on a session with autocommit off (see ExpiredDefersDue):
// START TRANSACTION, which commits what was pending, then ROLLBACK of the
// empty transaction it began. On failure the connection is dropped from its
// pool, so no later read inherits the snapshot.
func endImplicitTransaction(ctx context.Context, conn *sql.Conn) {
	tx, err := conn.BeginTx(ctx, nil)
	if err == nil {
		err = tx.Rollback()
	}
	if err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
}

func wakeExpiredDefersInTable(ctx context.Context, tx DBTX, table, eventsTable string) ([]string, error) {
	// Snapshot first so each genuinely-woken row gets its own event. The
	// UPDATE below repeats the whole predicate, so a row rescued between the
	// SELECT and its UPDATE (re-deferred further out, claimed, closed) matches
	// nothing and is skipped rather than clobbered.
	//nolint:gosec // G202: table is a hardcoded constant from the caller above.
	rows, err := tx.QueryContext(ctx, expiredDeferSelectSQL(table))
	if err != nil {
		return nil, fmt.Errorf("wake expired defers: scan %s: %w", table, err)
	}
	var expired []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("wake expired defers: scan %s row: %w", table, err)
		}
		expired = append(expired, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("wake expired defers: iterate %s: %w", table, err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("wake expired defers: close %s rows: %w", table, err)
	}
	if len(expired) == 0 {
		return nil, nil
	}

	var woken []string
	now := time.Now().UTC()
	for _, id := range expired {
		// row_lock is rewritten so a concurrent claim/update conflicts at
		// commit time instead of cell-merging with this write — the same
		// invariant the lease scheme depends on.
		//nolint:gosec // G201: table is a hardcoded constant from the caller above.
		res, err := tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE %s
			SET status = 'open', defer_until = NULL, updated_at = ?, row_lock = ?
			WHERE id = ? AND status = 'deferred' AND defer_until IS NOT NULL
			  AND defer_until <= UTC_TIMESTAMP()
		`, table), now, freshRowLock(), id)
		if err != nil {
			return woken, fmt.Errorf("wake expired defer %s: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return woken, fmt.Errorf("wake expired defer %s rows affected: %w", id, err)
		}
		if n == 0 {
			continue // rescued concurrently — leave it be
		}
		if err := RecordFullEventInTable(ctx, tx, eventsTable, id, types.EventStatusChanged,
			DeferWakeActor, string(types.StatusDeferred), string(types.StatusOpen)); err != nil {
			return woken, fmt.Errorf("record wake event for %s: %w", id, err)
		}
		// A wake is a status change, so it journals as an update. Emitted past
		// the rows-affected re-check, so a concurrently-rescued bead records
		// nothing.
		if err := RecordEventInTx(ctx, tx, EventUpdate, id, DeferWakeActor); err != nil {
			return woken, err
		}
		woken = append(woken, id)
	}
	return woken, nil
}
