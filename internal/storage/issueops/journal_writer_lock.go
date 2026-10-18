package issueops

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sync"
	"time"
)

// JournalWriterLockWait bounds one GET_LOCK call, in seconds. It stays under
// the dolt store pool's read deadline (10 s): a GET_LOCK that waits longer
// would be cut off as a dead connection rather than reported as a timeout.
const JournalWriterLockWait = 5

// JournalWriterLockAttempts bounds the whole wait for the journal writer lock.
const JournalWriterLockAttempts = 12

// JournalWriterUnlockTimeout bounds the detached RELEASE_LOCK. The release
// runs after the caller's context may be gone, and a proxied pool sets no read
// deadline, so without a bound a stalled server would hold the caller — and
// the lock — forever.
const JournalWriterUnlockTimeout = 3 * time.Second

// JournalWriterStaleHolder is how long the lock's holder may sit silent (a
// PROCESSLIST Sleep) before a waiter ends its session. A live holder sends its
// next statement within milliseconds of the last; one silent this long froze or
// vanished without closing its socket (a laptop that slept mid-write, a dropped
// tailnet path), and Dolt keeps such a session until listener
// read_timeout_millis (8 h by default): it enforces no wait_timeout. Ending it
// rolls back its uncommitted transaction and releases its locks, as a closed
// connection would have. It stays under the whole wait, so one waiter's budget
// covers the reclaim. A variable so tests can shorten it.
var JournalWriterStaleHolder = 30 * time.Second

// JournalWriterLockName is the server-wide GET_LOCK name journaled writers on
// database queue on. GET_LOCK names span every database on the server, so the
// name carries the database. The dolt store and the unit-of-work provider both
// build it here, so direct and proxied writers on one database share one lock.
func JournalWriterLockName(database string) string {
	return "bd_events_seq:" + database
}

// LockJournalWriter takes the journal writer lock for database on conn and
// returns its unlock. Call it BEFORE the transaction begins and unlock after
// the transaction has committed or rolled back.
//
// Every journaled transaction advances the one bd_events_seq row and inserts
// the journal row at the seq it read, so two that overlap always conflict and
// Dolt aborts the one that commits second with 1213; the loser replays its
// whole body, and a long body (a many-edge create) keeps losing to short
// writers until its retry budget runs out (bd-5o1b). The lock turns that
// optimistic abort into a queue in the order the counter already forces. It
// must precede BEGIN: a transaction that began before the previous holder
// committed still reads the old counter, so locking inside it would only move
// the conflict.
//
// A GET_LOCK that times out (0) or fails server-side (NULL) is retried up to
// JournalWriterLockAttempts times; then the error names the lock and the total
// wait. It never proceeds unlocked. After each timed-out attempt but the last
// it ends the holder's session when that session has been silent past
// JournalWriterStaleHolder (reclaimSilentHolder), so a lost client blocks the
// other writers for at most that long rather than for the server's idle cut.
//
// unlock is idempotent and bounded by JournalWriterUnlockTimeout, detached from
// ctx's cancellation. When RELEASE_LOCK fails or times out it discards conn's
// physical session instead of letting it return to the pool still owning the
// lock: locks are per session and reentrant, so a pooled owner would keep
// every other writer out. Closing the session releases its locks server-side.
// unlock reports nothing: the transaction it follows is already settled, and a
// cleanup failure must never read as a failed write.
func LockJournalWriter(ctx context.Context, conn *sql.Conn, database string) (func(), error) {
	name := JournalWriterLockName(database)
	for attempt := 1; ; attempt++ {
		var got sql.NullInt64
		if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", name, JournalWriterLockWait).Scan(&got); err != nil {
			return nil, fmt.Errorf("journal writer lock %q: %w", name, err)
		}
		if got.Valid && got.Int64 == 1 {
			break
		}
		if attempt >= JournalWriterLockAttempts {
			return nil, fmt.Errorf("journal writer lock %q not granted after %ds", name, JournalWriterLockWait*JournalWriterLockAttempts)
		}
		// Only after a real timeout (0), never a server failure (NULL). The
		// bound check comes first so a reclaim spends an attempt like any
		// other and the loop never runs past JournalWriterLockAttempts.
		if got.Valid {
			reclaimSilentHolder(ctx, conn, name)
		}
	}
	return sync.OnceFunc(func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), JournalWriterUnlockTimeout)
		defer cancel()
		if _, err := conn.ExecContext(unlockCtx, "SELECT RELEASE_LOCK(?)", name); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}), nil
}

// reclaimSilentHolder ends the session holding name when it has been silent
// past JournalWriterStaleHolder; the caller's next GET_LOCK finds out whether
// that freed the lock. Any failure reads as "not reclaimed": the caller goes on
// waiting its bounded budget, as before.
// PROCESSLIST TIME is the seconds since the session's last statement ended, and
// a Sleep that long is a holder sending nothing; a holder running a statement
// shows Query and is never ended.
func reclaimSilentHolder(ctx context.Context, conn *sql.Conn, name string) {
	var holder sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT IS_USED_LOCK(?)", name).Scan(&holder); err != nil || !holder.Valid {
		return
	}
	var command string
	var idle int64
	if err := conn.QueryRowContext(ctx,
		"SELECT COMMAND, TIME FROM information_schema.PROCESSLIST WHERE ID = ?", holder.Int64).Scan(&command, &idle); err != nil {
		return
	}
	if command != "Sleep" || time.Duration(idle)*time.Second < JournalWriterStaleHolder {
		return
	}
	// KILL takes no placeholder; the id is the server's own integer, so the
	// formatted statement is not injectable.
	_, _ = conn.ExecContext(ctx, fmt.Sprintf("KILL %d", holder.Int64))
}
