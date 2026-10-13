package dolt

import (
	"context"
	"database/sql"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-sql-driver/mysql"

	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/storage/journalscan"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/memoryops"
)

var (
	matchGetLock     = regexp.QuoteMeta("SELECT GET_LOCK(?, ?)")
	matchReleaseLock = regexp.QuoteMeta("SELECT RELEASE_LOCK(?)")
)

// newJournalLockMockStore is a server-free store over sqlmock. sqlmock fails
// any statement it was not told to expect, so a test that sets no expectation
// proves the helper issued no SQL at all.
func newJournalLockMockStore(t *testing.T, serverMode, journal bool) (*DoltStore, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := &DoltStore{db: db, database: "proj_a", serverMode: serverMode}
	store.eventsJournalEnabled.Store(journal)
	return store, mock
}

// TestWriteTxConn_NoLockOutsideJournaledServerMode pins that the journal
// writer lock costs nothing where it is not needed: embedded mode and a
// journal-off server store pin the connection and issue no GET_LOCK.
func TestWriteTxConn_NoLockOutsideJournaledServerMode(t *testing.T) {
	for _, tc := range []struct {
		name       string
		serverMode bool
		journal    bool
	}{
		{"embedded, journal on", false, true},
		{"server, journal off", true, false},
		{"embedded, journal off", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, mock := newJournalLockMockStore(t, tc.serverMode, tc.journal)
			conn, release, err := store.writeTxConn(context.Background())
			if err != nil {
				t.Fatalf("writeTxConn: %v", err)
			}
			if conn == nil {
				t.Fatal("writeTxConn returned no connection")
			}
			release()
			release()
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unexpected SQL: %v", err)
			}
		})
	}
}

// TestWriteTxConn_JournalWriterLockCarriesDatabase pins the lock's name and
// lifecycle in server mode with the journal on: GET_LOCK on the database's
// name before the caller begins, and exactly one RELEASE_LOCK however often
// release runs.
func TestWriteTxConn_JournalWriterLockCarriesDatabase(t *testing.T) {
	store, mock := newJournalLockMockStore(t, true, true)
	mock.ExpectQuery(matchGetLock).
		WithArgs("bd_events_seq:proj_a", issueops.JournalWriterLockWait).
		WillReturnRows(sqlmock.NewRows([]string{"got"}).AddRow(1))
	mock.ExpectExec(matchReleaseLock).
		WithArgs("bd_events_seq:proj_a").
		WillReturnResult(sqlmock.NewResult(0, 0))

	_, release, err := store.writeTxConn(context.Background())
	if err != nil {
		t.Fatalf("writeTxConn: %v", err)
	}
	release()
	release()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("lock lifecycle: %v", err)
	}
	if got := store.db.Stats().Idle; got != 1 {
		t.Fatalf("a confirmed release left %d idle sessions, want the session pooled (1)", got)
	}
}

// TestWriteTxConn_FailedReleaseDiscardsSession pins that a RELEASE_LOCK the
// server refuses does not hand the session back to the pool: locks are per
// session and reentrant, so a pooled session that may still own the lock
// would keep every other journaled writer out.
func TestWriteTxConn_FailedReleaseDiscardsSession(t *testing.T) {
	store, mock := newJournalLockMockStore(t, true, true)
	mock.ExpectQuery(matchGetLock).WillReturnRows(sqlmock.NewRows([]string{"got"}).AddRow(1))
	mock.ExpectExec(matchReleaseLock).
		WillReturnError(&mysql.MySQLError{Number: 1317, Message: "Query execution was interrupted"})

	_, release, err := store.writeTxConn(context.Background())
	if err != nil {
		t.Fatalf("writeTxConn: %v", err)
	}
	release()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("lock lifecycle: %v", err)
	}
	if got := store.db.Stats().OpenConnections; got != 0 {
		t.Fatalf("failed RELEASE_LOCK returned %d live session(s) to the pool; want the session discarded", got)
	}
}

// TestWriteTxConn_ReleaseIsBounded pins the cleanup deadline: release runs
// detached from the caller's (here already cancelled) context, but a server
// that stalls on RELEASE_LOCK must not hold the caller past
// issueops.JournalWriterUnlockTimeout, and the unconfirmed session is
// discarded.
func TestWriteTxConn_ReleaseIsBounded(t *testing.T) {
	store, mock := newJournalLockMockStore(t, true, true)
	mock.ExpectQuery(matchGetLock).WillReturnRows(sqlmock.NewRows([]string{"got"}).AddRow(1))
	mock.ExpectExec(matchReleaseLock).WillDelayFor(6 * time.Second).WillReturnResult(sqlmock.NewResult(0, 0))

	ctx, cancel := context.WithCancel(context.Background())
	_, release, err := store.writeTxConn(ctx)
	if err != nil {
		t.Fatalf("writeTxConn: %v", err)
	}
	cancel()
	start := time.Now()
	release()
	if elapsed := time.Since(start); elapsed > issueops.JournalWriterUnlockTimeout+500*time.Millisecond {
		t.Fatalf("release after cancellation took %s, beyond the %s cleanup budget", elapsed, issueops.JournalWriterUnlockTimeout)
	}
	if got := store.db.Stats().OpenConnections; got != 0 {
		t.Fatalf("a timed-out RELEASE_LOCK returned %d live session(s) to the pool; want the session discarded", got)
	}
}

// TestHeartbeatIssue_TakesNoJournalWriterLock pins that a lease heartbeat
// never queues behind journaled writers, even in server mode with the journal
// on: a heartbeat stuck behind a long create lets its lease lapse. sqlmock
// fails the GET_LOCK it was not told to expect.
func TestHeartbeatIssue_TakesNoJournalWriterLock(t *testing.T) {
	store, mock := newJournalLockMockStore(t, true, true)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT 1 FROM wisps WHERE id = ?")).
		WillReturnRows(sqlmock.NewRows([]string{"1"}))
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE leases SET lease_expires_at").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := store.HeartbeatIssue(context.Background(), "bd-hb-1", "worker"); err != nil {
		t.Fatalf("HeartbeatIssue: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("heartbeat SQL: %v", err)
	}
}

// TestWriteTxConn_JournalWriterLockRetriesThenGivesUp pins the bounded wait:
// a GET_LOCK that times out (0) or errors server-side (NULL) is retried, and
// after issueops.JournalWriterLockAttempts the helper returns an error naming the lock
// and the total wait instead of proceeding unlocked.
func TestWriteTxConn_JournalWriterLockRetriesThenGivesUp(t *testing.T) {
	t.Run("granted on a later attempt", func(t *testing.T) {
		store, mock := newJournalLockMockStore(t, true, true)
		mock.ExpectQuery(matchGetLock).WillReturnRows(sqlmock.NewRows([]string{"got"}).AddRow(0))
		mock.ExpectQuery(matchGetLock).WillReturnRows(sqlmock.NewRows([]string{"got"}).AddRow(nil))
		mock.ExpectQuery(matchGetLock).WillReturnRows(sqlmock.NewRows([]string{"got"}).AddRow(1))
		mock.ExpectExec(matchReleaseLock).WillReturnResult(sqlmock.NewResult(0, 0))

		_, release, err := store.writeTxConn(context.Background())
		if err != nil {
			t.Fatalf("writeTxConn: %v", err)
		}
		release()
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("lock retries: %v", err)
		}
	})

	t.Run("exhausted", func(t *testing.T) {
		store, mock := newJournalLockMockStore(t, true, true)
		for i := 0; i < issueops.JournalWriterLockAttempts; i++ {
			mock.ExpectQuery(matchGetLock).WillReturnRows(sqlmock.NewRows([]string{"got"}).AddRow(0))
		}

		conn, release, err := store.writeTxConn(context.Background())
		if err == nil {
			release()
			t.Fatal("writeTxConn succeeded without the lock")
		}
		if conn != nil || release != nil {
			t.Error("a failed writeTxConn must hand back neither a connection nor a release")
		}
		for _, want := range []string{"bd_events_seq:proj_a", "60s"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("lock attempts: %v", err)
		}
	})
}

// TestJournalWriterLock_EveryJournalScopeTakesTheLock is the structural guard
// for the lock's coverage. The lock queues journaled writers only if EVERY
// transaction that can write the journal takes it, and a site that forgets
// still works — it just goes back to aborting its peers with 1213. So every
// function that binds journal activation to a transaction it began must take
// the lock in the same body, through writeTxConn or, on a connection it
// pinned itself, lockJournalWriter.
func TestJournalWriterLock_EveryJournalScopeTakesTheLock(t *testing.T) {
	fns, err := journalscan.ParsePackage(".")
	if err != nil {
		t.Fatalf("parse dolt package: %v", err)
	}
	lockCalls := map[string]bool{"writeTxConn": true, "lockJournalWriter": true}
	var checked int
	for key, f := range fns {
		// scopeEventsJournalTransaction is the scoping helper itself; its
		// callers are what this checks.
		if key == "DoltStore.scopeEventsJournalTransaction" || !f.CallsAnyOf(scopeCalls) {
			continue
		}
		checked++
		if !f.CallsAnyOf(lockCalls) {
			t.Errorf("%s scopes the events journal on a transaction but never takes the journal writer lock — "+
				"its journaled writes abort overlapping writers with 1213 again. Begin it on s.writeTxConn (see wisps.go).", key)
		}
	}
	if checked == 0 {
		t.Fatal("guard found no journal-scoping functions — scope detection changed; the guard is not actually running")
	}
}

// journalingIssueopsMutators derives, from the issueops source, every
// exported function that reaches a journal emit helper — the same derivation
// TestEveryRawTxJournalScopeIsScopedOrExempt makes.
func journalingIssueopsMutators(t *testing.T) map[string]bool {
	t.Helper()
	issueFns, err := journalscan.ParsePackage("../issueops")
	if err != nil {
		t.Fatalf("parse issueops package: %v", err)
	}
	emitHelpers := map[string]bool{
		"RecordEventInTx":        true,
		"RecordDeleteInTx":       true,
		"RecordDepEventInTx":     true,
		"RecordCommentEventInTx": true,
		"insertEventRow":         true,
	}
	emits := journalscan.Fixpoint(issueFns,
		func(f *journalscan.FuncInfo) bool { return f.CallsAnyOf(emitHelpers) },
		func(f *journalscan.FuncInfo) []string { return f.AllCallNames() })
	mutators := map[string]bool{}
	for key, f := range issueFns {
		if f.Exported && emits[key] {
			mutators[f.Name] = true
		}
	}
	if len(mutators) == 0 {
		t.Fatal("derived no journaling issueops mutators — the emit analysis changed and this guard is not actually running")
	}
	return mutators
}

// journalingMemoryopsMutators extends mutators with every exported function of
// the storage memoryops package that reaches one: memories.Remember and
// Forget reach issueops only through it.
func journalingMemoryopsMutators(t *testing.T, mutators map[string]bool) {
	t.Helper()
	memFns, err := journalscan.ParsePackage("../memoryops")
	if err != nil {
		t.Fatalf("parse memoryops package: %v", err)
	}
	emits := journalscan.Fixpoint(memFns,
		func(f *journalscan.FuncInfo) bool { return f.CallsAnyOf(mutators) },
		func(f *journalscan.FuncInfo) []string { return f.AllCallNames() })
	for key, f := range memFns {
		if f.Exported && emits[key] {
			mutators[f.Name] = true
		}
	}
}

// noJournalCallers are the callers the audit put on the NoJournal helpers
// (bd-i3bv, bd-zxtk). Each must stay where the converse guard looks: one that
// drops off the list has either moved back to the locked route or been
// renamed past the guard.
var noJournalCallers = []string{
	"DoltStore.HeartbeatIssue",
	"DoltStore.SetConfig",
	"DoltStore.DeleteConfig",
	"DoltStore.SetMetadata",
	"DoltStore.SetLocalMetadata",
	"DoltStore.PruneEventsJournal",
	"DoltStore.RunEventsMaintenanceTx",
	"DoltStore.SetRepoMtime",
	"DoltStore.ClearRepoMtime",
	"DoltStore.GetNextChildID",
	"DoltStore.ApplyCompaction",
	"DoltStore.SnapshotIssue",
	"DoltStore.RestoreFromSnapshot",
	"DoltStore.RecordProvenanceEvent",
	"memories.Remember",
	"memories.Forget",
	"bootstrapper.Bootstrap",
}

// TestJournalWriterLock_NoJournalBodiesRecordNothing is the converse guard:
// the NoJournal transaction helpers skip the lock, which is only safe for a
// body that can never advance bd_events_seq. A journaled write slipped into
// one would go back to aborting its peers with 1213 — or being aborted. So no
// function that calls withRetryTxNoJournal/withWriteTxNoJournal may call a
// journaling issueops mutator, or a function of this package that calls one.
// Like the scope guard, reachability is direct (one hop), not a fixpoint.
func TestJournalWriterLock_NoJournalBodiesRecordNothing(t *testing.T) {
	mutators := journalingIssueopsMutators(t)
	journalingMemoryopsMutators(t, mutators)
	fns, err := journalscan.ParsePackage(".")
	if err != nil {
		t.Fatalf("parse dolt package: %v", err)
	}
	journalingHere := map[string]bool{}
	for _, f := range fns {
		if f.CallsAnyOf(mutators) {
			journalingHere[f.Name] = true
		}
	}
	noJournal := map[string]bool{"withRetryTxNoJournal": true, "withWriteTxNoJournal": true}
	checked := map[string]bool{}
	for key, f := range fns {
		if !f.CallsAnyOf(noJournal) {
			continue
		}
		checked[key] = true
		for _, c := range f.AllCallNames() {
			if mutators[c] || journalingHere[c] {
				t.Errorf("%s runs on a NoJournal transaction helper but calls %s, which can record a journal event — use withRetryTx/withWriteTx so it takes the journal writer lock", key, c)
			}
		}
	}
	for _, want := range noJournalCallers {
		if !checked[want] {
			t.Errorf("NoJournal callers = %v, want %s among them — the guard is not looking where it should", checked, want)
		}
	}
}

// TestJournalWriterLock_NonJournaledWritesSkipAHeldLock pins the audit end to
// end (bd-zxtk): while another session holds the journal writer lock, writes
// that record no journal event complete inside a deadline far shorter than one
// GET_LOCK wait, and a journaled write in the same window still waits.
func TestJournalWriterLock_NonJournaledWritesSkipAHeldLock(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx := context.Background()
	enableJournalForTest(t, store)
	if err := store.CreateIssue(ctx, &types.Issue{ID: "bd-nj-1", Title: "nj", IssueType: types.TypeTask, Status: types.StatusOpen}, "actor"); err != nil {
		t.Fatalf("create: %v", err)
	}
	mems, err := store.Memories()
	if err != nil {
		t.Fatalf("memories: %v", err)
	}

	holder := openSecondStoreOnBranch(t, ctx, store)
	conn, err := holder.db.Conn(ctx)
	if err != nil {
		t.Fatalf("holder conn: %v", err)
	}
	defer conn.Close()
	lockName := issueops.JournalWriterLockName(store.database)
	var got int
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 5)", lockName).Scan(&got); err != nil || got != 1 {
		t.Fatalf("hold journal writer lock: got=%d err=%v", got, err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, "SELECT RELEASE_LOCK(?)", lockName) }()

	const deadline = time.Second
	for _, tc := range []struct {
		name string
		call func(context.Context) error
	}{
		{"SetRepoMtime", func(ctx context.Context) error {
			return store.SetRepoMtime(ctx, "/tmp/bd-nj/repo", "/tmp/bd-nj/repo/issues.jsonl", 1)
		}},
		{"ClearRepoMtime", func(ctx context.Context) error { return store.ClearRepoMtime(ctx, "/tmp/bd-nj/repo") }},
		{"Remember", func(ctx context.Context) error {
			_, err := mems.Remember(ctx, memoryops.RememberRequest{Key: "nj", Content: "held"})
			return err
		}},
		{"Forget", func(ctx context.Context) error {
			_, err := mems.Forget(ctx, memoryops.ForgetRequest{Key: "nj"})
			return err
		}},
	} {
		callCtx, cancel := context.WithTimeout(ctx, deadline)
		start := time.Now()
		err := tc.call(callCtx)
		cancel()
		t.Logf("%s under a held journal lock: %s", tc.name, time.Since(start))
		if err != nil {
			t.Errorf("non-journaled %s waited on the held journal writer lock: %v", tc.name, err)
		}
	}

	callCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	start := time.Now()
	err = store.UpdateIssue(callCtx, "bd-nj-1", map[string]interface{}{"title": "held"}, "actor")
	t.Logf("journaled UpdateIssue under a held journal lock: %s (err=%v)", time.Since(start), err)
	if err == nil {
		t.Fatal("journaled UpdateIssue committed while another session held the journal writer lock; it must wait")
	}
}

// openSecondStoreOnBranch opens another store on primary's database with its
// own single-connection pool, checked out to primary's test branch. Two
// writers on two pools share nothing client-side, so only the server's named
// lock can serialize them.
func openSecondStoreOnBranch(t *testing.T, ctx context.Context, primary *DoltStore) *DoltStore {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "dolt-test-second-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })
	second, err := New(ctx, &Config{
		Path:           tmpDir,
		CommitterName:  "test",
		CommitterEmail: "test@example.com",
		Database:       primary.database,
		MaxOpenConns:   1, // DOLT_CHECKOUT below is session-level
	})
	if err != nil {
		t.Fatalf("open second store: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	var branch string
	if err := primary.db.QueryRowContext(ctx, "SELECT active_branch()").Scan(&branch); err != nil {
		t.Fatalf("read test branch: %v", err)
	}
	if _, err := second.db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", branch); err != nil {
		t.Fatalf("check second store out to %s: %v", branch, err)
	}
	return second
}

// TestEventsJournal_OverlappingServerWritersQueue pins the journal writer
// lock (bd-5o1b). The long writer and the short one run on different stores
// with their own pools on the same database and branch, so nothing but the
// server-side GET_LOCK can make the short writer wait.
func TestEventsJournal_OverlappingServerWritersQueue(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx := context.Background()
	second := openSecondStoreOnBranch(t, ctx, store)
	assertJournaledWritersQueue(t, ctx, store, second)
}

// assertJournaledWritersQueue runs a long journaled transaction on long and,
// inside its window, a short journaled update on short; both stores must be on
// one database and branch. Two journaled transactions always write the same
// bd_events_seq cell, so without the lock the short writer commits inside the
// long one's window and the long one loses with 1213 and replays its whole
// body. With the lock the short writer waits, the long body runs once, and the
// seqs stay contiguous in commit order.
func assertJournaledWritersQueue(t *testing.T, ctx context.Context, long, short *DoltStore) {
	t.Helper()
	enableJournalForTest(t, long)
	enableJournalForTest(t, short)
	for _, id := range []string{"bd-q-1", "bd-q-2"} {
		if err := long.CreateIssue(ctx, &types.Issue{ID: id, Title: id, IssueType: types.TypeTask, Status: types.StatusOpen}, "actor"); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	clearJournal(t, long)

	attempts := 0
	inside := make(chan struct{})
	release := make(chan struct{})
	// Let the long body finish however this ends, so a failure below does not
	// strand it holding the lock while cleanup closes the stores.
	releaseLong := sync.OnceFunc(func() { close(release) })
	defer releaseLong()
	longDone := make(chan error, 1)
	go func() {
		longDone <- long.withRetryTx(ctx, func(tx *sql.Tx) error {
			attempts++
			if _, err := tx.ExecContext(ctx, "UPDATE issues SET title = 'long' WHERE id = 'bd-q-1'"); err != nil {
				return err
			}
			if err := issueops.RecordEventInTx(ctx, tx, issueops.EventUpdate, "bd-q-1", "actor"); err != nil {
				return err
			}
			if attempts == 1 {
				close(inside)
				<-release
			}
			return nil
		})
	}()
	<-inside

	shortDone := make(chan error, 1)
	go func() {
		shortDone <- short.UpdateIssue(ctx, "bd-q-2", map[string]interface{}{"title": "short"}, "actor")
	}()
	select {
	case err := <-shortDone:
		t.Fatalf("short journaled write finished (err=%v) while the long one held the journal; it must queue", err)
	case <-time.After(500 * time.Millisecond):
	}
	releaseLong()
	if err := <-longDone; err != nil {
		t.Fatalf("long tx: %v", err)
	}
	if err := <-shortDone; err != nil {
		t.Fatalf("short write: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("long body ran %d times, want 1: a replay means it lost a 1213 to the short writer", attempts)
	}
	rows := readJournalRows(t, long)
	if len(rows) < 2 || rows[0].id != "bd-q-1" {
		t.Fatalf("journal rows = %+v, want the long write's row first", rows)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].seq != rows[i-1].seq+1 {
			t.Fatalf("journal seqs not contiguous: %+v", rows)
		}
	}
}
