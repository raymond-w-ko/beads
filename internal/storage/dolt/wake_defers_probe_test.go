package dolt

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage/dbproxy/proxy"
	"github.com/steveyegge/beads/internal/storage/dbproxy/server"
	"github.com/steveyegge/beads/internal/storage/doltutil"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/storage/uow"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// Live tests of the probe in front of the defer-wake sweep (bd-aw3l8,
// investigator bead bd-ypak8 Q2): issueops.ExpiredDefersDue reads, outside
// the journal writer lock and any transaction, whether the sweep could find a
// row, and the store runs the sweep only when it says due.

// deferIssue creates id (in wisps when wisp) and defers it until `until`.
func deferIssue(t *testing.T, ctx context.Context, store *DoltStore, id string, wisp bool, until time.Time) {
	t.Helper()
	iss := &types.Issue{ID: id, Title: "deferred " + id, Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask, Ephemeral: wisp}
	if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	if err := store.UpdateIssue(ctx, id, map[string]interface{}{"status": "deferred", "defer_until": until}, "deferrer"); err != nil {
		t.Fatalf("defer %s: %v", id, err)
	}
}

// TestWakeDefersProbeSkipsTheJournalLockWhenNothingIsDue: with nothing due and
// another session holding the journal writer lock for 3 s, GetReadyWork and
// CountReadyWork each return in under 1 s. Before the probe each queued on
// GET_LOCK until the holder let go (bd-ypak8: 2.018 s behind a 2 s holder).
func TestWakeDefersProbeSkipsTheJournalLockWhenNothingIsDue(t *testing.T) {
	// Its own database: the lock is per database, and holding the shared
	// test database's lock would stall every parallel test's writes.
	store, cleanup := setupConcurrentTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	open := &types.Issue{ID: "test-probe-open", Title: "open", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	if err := store.CreateIssue(ctx, open, "tester"); err != nil {
		t.Fatalf("create: %v", err)
	}
	deferIssue(t, ctx, store, "test-probe-later", false, time.Now().UTC().Add(48*time.Hour))
	if issueops.ExpiredDefersDue(ctx, store.db) {
		t.Fatalf("the probe says due with only a future defer; the case under test did not arise")
	}

	holderDB, err := sql.Open("mysql", doltutil.ServerDSN{Host: "127.0.0.1", Port: testServerPort, User: "root", Database: store.database}.String())
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	defer func() { _ = holderDB.Close() }()
	holder, err := holderDB.Conn(ctx)
	if err != nil {
		t.Fatalf("holder conn: %v", err)
	}
	defer func() { _ = holder.Close() }()
	lock := issueops.JournalWriterLockName(store.database)
	var got int
	if err := holder.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", lock).Scan(&got); err != nil || got != 1 {
		t.Fatalf("hold %s: GET_LOCK = %d, %v", lock, got, err)
	}
	const hold = 3 * time.Second
	released := make(chan struct{})
	timer := time.AfterFunc(hold, func() {
		_, _ = holder.ExecContext(context.Background(), "SELECT RELEASE_LOCK(?)", lock)
		close(released)
	})
	defer func() {
		if timer.Stop() {
			_, _ = holder.ExecContext(context.Background(), "SELECT RELEASE_LOCK(?)", lock)
		} else {
			<-released
		}
	}()

	start := time.Now()
	work, err := store.GetReadyWork(ctx, types.WorkFilter{})
	readyTook := time.Since(start)
	if err != nil {
		t.Fatalf("GetReadyWork: %v", err)
	}
	if len(work) != 1 || work[0].ID != open.ID {
		t.Errorf("GetReadyWork = %d issues, want only %s", len(work), open.ID)
	}
	start = time.Now()
	n, err := store.CountReadyWork(ctx, types.WorkFilter{})
	countTook := time.Since(start)
	if err != nil {
		t.Fatalf("CountReadyWork: %v", err)
	}
	if n != 1 {
		t.Errorf("CountReadyWork = %d, want 1", n)
	}
	t.Logf("with %s held for %v: GetReadyWork took %v, CountReadyWork %v", lock, hold, readyTook, countTook)
	if readyTook >= time.Second {
		t.Errorf("GetReadyWork took %v behind the journal writer lock with nothing due, want under 1s", readyTook)
	}
	if countTook >= time.Second {
		t.Errorf("CountReadyWork took %v behind the journal writer lock with nothing due, want under 1s", countTook)
	}
}

// TestWakeDefersProbeWakesADueIssueAndADueWisp: a due issue, and a due wisp
// alone, are woken as before the probe (status open, defer_until cleared, the
// status_changed event and the journal row under the wake actor), and the
// ready read returns them.
func TestWakeDefersProbeWakesADueIssueAndADueWisp(t *testing.T) {
	for _, c := range []struct {
		name, id, events string
		wisp             bool
	}{
		{name: "issue", id: "test-probe-due-issue", events: "events"},
		{name: "wisp alone", id: "test-probe-due-wisp", events: "wisp_events", wisp: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			store, cleanup := setupTestStore(t)
			defer cleanup()
			ctx, cancel := testContext(t)
			defer cancel()
			enableJournalForTest(t, store)

			deferIssue(t, ctx, store, c.id, c.wisp, time.Now().UTC().Add(-time.Hour))
			if !issueops.ExpiredDefersDue(ctx, store.db) {
				t.Fatalf("the probe says nothing is due with %s past its date", c.id)
			}
			clearJournal(t, store)
			work, err := store.GetReadyWork(ctx, types.WorkFilter{IncludeEphemeral: c.wisp})
			if err != nil {
				t.Fatalf("GetReadyWork: %v", err)
			}
			found := false
			for _, w := range work {
				found = found || w.ID == c.id
			}
			if !found {
				t.Errorf("ready read does not return the woken %s", c.id)
			}
			got, err := store.GetIssue(ctx, c.id)
			if err != nil {
				t.Fatalf("get %s: %v", c.id, err)
			}
			if got.Status != types.StatusOpen || got.DeferUntil != nil {
				t.Errorf("%s after the ready read: status %q, defer_until %v; want open, nil", c.id, got.Status, got.DeferUntil)
			}
			var events int
			//nolint:gosec // test-only identifiers
			if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+c.events+
				" WHERE issue_id = ? AND event_type = 'status_changed' AND actor = ? AND old_value = 'deferred' AND new_value = 'open'",
				c.id, issueops.DeferWakeActor).Scan(&events); err != nil {
				t.Fatalf("count wake events: %v", err)
			}
			if events != 1 {
				t.Errorf("%s has %d wake events in %s under %s, want 1", c.id, events, c.events, issueops.DeferWakeActor)
			}
			if actor, ok := journalActorFor(t, store, "update", c.id); !ok || actor != issueops.DeferWakeActor {
				t.Errorf("wake journal row actor = %q, %v; want %q", actor, ok, issueops.DeferWakeActor)
			}
			if issueops.ExpiredDefersDue(ctx, store.db) {
				t.Errorf("the probe still says due after the wake")
			}
		})
	}
}

// TestWakeDefersProbeFailureKeepsTheSweep: a probe that cannot answer reports
// due, so the sweep runs as before: with a cancelled context, and with the
// wisps table gone (the probe's UNION reads it; the sweep tolerates it absent)
// a due issue is still woken.
func TestWakeDefersProbeFailureKeepsTheSweep(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	cancelled, cancelNow := context.WithCancel(ctx)
	cancelNow()
	if !issueops.ExpiredDefersDue(cancelled, store.db) {
		t.Errorf("the probe on a cancelled context says nothing is due, want due (keep the sweep)")
	}

	const id = "test-probe-nowisps"
	deferIssue(t, ctx, store, id, false, time.Now().UTC().Add(-time.Hour))
	// Renamed away rather than dropped: a dropped wisps leaves the aux wisp
	// tables' foreign keys dangling, and Dolt then refuses the sweep's UPDATE
	// of issues for that, not for anything the probe did. A rename carries the
	// foreign keys along. Renamed back before the issue is read.
	wakeExec(t, ctx, store.db, "RENAME TABLE wisps TO wisps_gone")
	renamed := true
	defer func() {
		if renamed {
			wakeExec(t, ctx, store.db, "RENAME TABLE wisps_gone TO wisps")
		}
	}()
	if !issueops.ExpiredDefersDue(ctx, store.db) {
		t.Fatalf("the probe without a wisps table says nothing is due, want due")
	}
	store.wakeExpiredDefersForRead(ctx)
	wakeExec(t, ctx, store.db, "RENAME TABLE wisps_gone TO wisps")
	renamed = false
	got, err := store.GetIssue(ctx, id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	if got.Status != types.StatusOpen || got.DeferUntil != nil {
		t.Errorf("%s after the sweep with wisps gone: status %q, defer_until %v; want open, nil", id, got.Status, got.DeferUntil)
	}
}

// wakeCompareRow is one generated row of the comparison test.
type wakeCompareRow struct {
	table   string // issues or wisps
	status  string
	deferAt string // "null", "past" or "future"
}

func (r wakeCompareRow) due() bool { return r.status == "deferred" && r.deferAt == "past" }

// TestWakeDefersProbeMatchesTheSweep is a comparison test (bd-aw3l8). The
// oracle is the sweep itself: issueops.WakeExpiredDefersInTx in a transaction
// that is rolled back, which runs its two snapshot SELECTs (issues, wisps) and
// wakes what they find. The piece under test is issueops.ExpiredDefersDue on
// the same rows. Relation: the probe says due exactly when the sweep wakes a
// row, which is exactly when the generator placed a deferred row with a past
// date. Rows: every status (deferred, open, closed, a custom one), defer_until
// NULL, past or future (one to 48 hours away, so the clock cannot flip a
// case), in issues only, wisps only, both or neither. Fixed cases first, then
// wakeCompareSeeds seeded sets of 0 to 6 rows, within wakeCompareBudget; a
// failure names its seed.
func TestWakeDefersProbeMatchesTheSweep(t *testing.T) {
	const (
		wakeCompareSeeds  = 60
		wakeCompareBudget = 2 * time.Minute
	)
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), wakeCompareBudget+time.Minute)
	defer cancel()

	statuses := []string{"deferred", "open", "closed", "review"}
	defers := []string{"null", "past", "future"}
	n := 0
	place := func(rows []wakeCompareRow) {
		t.Helper()
		for _, table := range []string{"issues", "wisps"} {
			//nolint:gosec // test-only identifiers
			wakeExec(t, ctx, store.db, "DELETE FROM "+table+" WHERE id LIKE 'cmp-%'")
		}
		now := time.Now().UTC()
		for _, r := range rows {
			n++
			until := "NULL"
			switch r.deferAt {
			case "past":
				until = "'" + now.Add(-time.Duration(1+n%48)*time.Hour).Format("2006-01-02 15:04:05") + "'"
			case "future":
				until = "'" + now.Add(time.Duration(1+n%48)*time.Hour).Format("2006-01-02 15:04:05") + "'"
			}
			closedAt := "NULL"
			if r.status == "closed" {
				closedAt = "'" + now.Format("2006-01-02 15:04:05") + "'"
			}
			//nolint:gosec // test-only identifiers and generated literals
			wakeExec(t, ctx, store.db, fmt.Sprintf("INSERT INTO %s (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type, defer_until, closed_at) "+
				"VALUES ('cmp-%d', 'comparison', '', '', '', '', '%s', 2, 'task', %s, %s)", r.table, n, r.status, until, closedAt))
		}
	}
	compare := func(label string, rows []wakeCompareRow) {
		t.Helper()
		place(rows)
		want := false
		for _, r := range rows {
			want = want || r.due()
		}
		probe := issueops.ExpiredDefersDue(ctx, store.db)
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("%s: begin: %v", label, err)
		}
		woke, err := issueops.WakeExpiredDefersInTx(ctx, tx)
		_ = tx.Rollback()
		if err != nil {
			t.Fatalf("%s: sweep: %v", label, err)
		}
		swept := len(woke.Issues)+len(woke.Wisps) > 0
		if probe != swept || swept != want {
			t.Errorf("%s: rows %+v: probe due=%v, sweep woke %d issues and %d wisps, generator due=%v",
				label, rows, probe, len(woke.Issues), len(woke.Wisps), want)
		}
	}

	for _, c := range []struct {
		name string
		rows []wakeCompareRow
	}{
		{"neither table", nil},
		{"issues only, due", []wakeCompareRow{{"issues", "deferred", "past"}}},
		{"wisps only, due", []wakeCompareRow{{"wisps", "deferred", "past"}}},
		{"both, due", []wakeCompareRow{{"issues", "deferred", "past"}, {"wisps", "deferred", "past"}}},
		{"both, none due", []wakeCompareRow{{"issues", "deferred", "future"}, {"wisps", "deferred", "null"}, {"issues", "open", "past"}, {"wisps", "review", "past"}, {"issues", "closed", "past"}}},
		{"dateless defer", []wakeCompareRow{{"issues", "deferred", "null"}}},
		{"future defer", []wakeCompareRow{{"wisps", "deferred", "future"}}},
	} {
		compare("fixed "+c.name, c.rows)
	}

	deadline := time.Now().Add(wakeCompareBudget)
	ran := 0
	for seed := int64(1); seed <= wakeCompareSeeds; seed++ {
		if time.Now().After(deadline) {
			t.Logf("budget %v spent after %d of %d seeds", wakeCompareBudget, ran, wakeCompareSeeds)
			break
		}
		rng := rand.New(rand.NewSource(seed))
		rows := make([]wakeCompareRow, rng.Intn(7))
		for i := range rows {
			rows[i] = wakeCompareRow{
				table:   []string{"issues", "wisps"}[rng.Intn(2)],
				status:  statuses[rng.Intn(len(statuses))],
				deferAt: defers[rng.Intn(len(defers))],
			}
		}
		compare(fmt.Sprintf("seed %d", seed), rows)
		ran++
	}
	t.Logf("comparison: 7 fixed cases and %d of %d seeds, %d rows in all", ran, wakeCompareSeeds, n)
}

func wakeExec(t *testing.T, ctx context.Context, db *sql.DB, queries ...string) {
	t.Helper()
	for _, q := range queries {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// serverSessionDB opens a pool on store's database with the server's own
// autocommit (on in the test container), for a second session in a test.
func serverSessionDB(t *testing.T, store *DoltStore) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", doltutil.ServerDSN{Host: "127.0.0.1", Port: testServerPort, User: "root", Database: store.database}.String())
	if err != nil {
		t.Fatalf("open second session: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestWakeDefersProbeAnswersForItsInstantUnderJournalLock is the contract of
// review bd-w6ppm, finding 1 (comment at the probe's call site in
// wakeExpiredDefers). One issue is due in about three server seconds and
// another session holds the journal writer lock until after that. The ready
// read returns before the deadline, without the issue and without waiting
// for the lock: it answers for the instant its probe read. With the lock
// free, the sweep the store ran before the probe (WakeExpiredDefersInTx in
// a transaction, rolled back here) gives the same first answer: nothing to
// wake before the deadline. After the deadline, with the lock let go, a
// second ready read wakes the issue and returns it, with the status, event
// and journal row the sweep writes.
func TestWakeDefersProbeAnswersForItsInstantUnderJournalLock(t *testing.T) {
	store, cleanup := setupConcurrentTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()
	enableJournalForTest(t, store)

	const id = "test-probe-instant"
	deferIssue(t, ctx, store, id, false, time.Now().UTC().Add(time.Hour))
	observer := serverSessionDB(t, store)
	// The deadline is read back from the row: UTC_TIMESTAMP() carries
	// microseconds and the DATETIME column rounds them to the second.
	wakeExecArgs(t, ctx, observer, "UPDATE issues SET defer_until = TIMESTAMPADD(SECOND, 3, UTC_TIMESTAMP()) WHERE id = ?", id)
	var until time.Time
	if err := observer.QueryRowContext(ctx, "SELECT defer_until FROM issues WHERE id = ?", id).Scan(&until); err != nil {
		t.Fatalf("read the deadline: %v", err)
	}
	beforeDeadline := func() bool {
		t.Helper()
		var before int
		if err := observer.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP() < ?", until).Scan(&before); err != nil {
			t.Fatalf("read the server clock: %v", err)
		}
		return before == 1
	}

	// The pre-probe path's first answer with the lock free: its sweep runs
	// at once, before the deadline, and finds nothing.
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	woke, err := issueops.WakeExpiredDefersInTx(ctx, tx)
	_ = tx.Rollback()
	if err != nil {
		t.Fatalf("sweep with the lock free: %v", err)
	}
	if !beforeDeadline() {
		t.Fatalf("the deadline passed before the lock was taken; the case under test did not arise")
	}
	t.Logf("lock free, before the deadline: the sweep wakes %d issues and %d wisps", len(woke.Issues), len(woke.Wisps))
	if len(woke.Issues)+len(woke.Wisps) != 0 {
		t.Fatalf("the sweep before the deadline woke %v, want nothing", woke)
	}

	holderConn, err := observer.Conn(ctx)
	if err != nil {
		t.Fatalf("holder conn: %v", err)
	}
	defer func() { _ = holderConn.Close() }()
	unlock, err := issueops.LockJournalWriter(ctx, holderConn, store.database)
	if err != nil {
		t.Fatalf("hold the journal writer lock: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()

	start := time.Now()
	first, err := store.GetReadyWork(ctx, types.WorkFilter{})
	took := time.Since(start)
	if err != nil {
		t.Fatalf("first GetReadyWork: %v", err)
	}
	returnedBefore := beforeDeadline()
	firstHas := false
	for _, w := range first {
		firstHas = firstHas || w.ID == id
	}
	t.Logf("lock held: first ready read took %v, returned before the deadline %v, %d issues, has %s %v", took, returnedBefore, len(first), id, firstHas)
	if !returnedBefore {
		t.Errorf("first ready read returned after the deadline %s (took %v): it waited for the journal writer lock", until, took)
	}
	if firstHas {
		t.Errorf("first ready read returned %s before its date", id)
	}

	for beforeDeadline() {
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for the deadline: %v", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	unlock()
	locked = false

	second, err := store.GetReadyWork(ctx, types.WorkFilter{})
	if err != nil {
		t.Fatalf("second GetReadyWork: %v", err)
	}
	secondHas := false
	for _, w := range second {
		secondHas = secondHas || w.ID == id
	}
	if !secondHas {
		t.Errorf("second ready read after the deadline does not return %s", id)
	}
	got, err := store.GetIssue(ctx, id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	if got.Status != types.StatusOpen || got.DeferUntil != nil {
		t.Errorf("%s after the second ready read: status %q, defer_until %v; want open, nil", id, got.Status, got.DeferUntil)
	}
	var events int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM events WHERE issue_id = ? AND event_type = 'status_changed' AND actor = ? AND old_value = 'deferred' AND new_value = 'open'",
		id, issueops.DeferWakeActor).Scan(&events); err != nil {
		t.Fatalf("count wake events: %v", err)
	}
	if events != 1 {
		t.Errorf("%s has %d wake events under %s, want 1", id, events, issueops.DeferWakeActor)
	}
	if actor, ok := journalActorFor(t, store, "update", id); !ok || actor != issueops.DeferWakeActor {
		t.Errorf("wake journal row actor = %q, %v; want %q", actor, ok, issueops.DeferWakeActor)
	}
}

// TestWakeDefersProbeWithoutAutocommitWakesARowDeferredSinceTheFirstProbe:
// the store's one pooled session has autocommit off. A first bare probe on
// it opens an implicit transaction that nothing ends. Another session then
// commits a deferred issue whose date has passed. The next ready read wakes
// it and returns it: on such a session the probe reports due and the sweep
// decides, since the probe's bare SELECT would read the retained snapshot,
// say nothing is due, and leave the issue deferred (review bd-w6ppm,
// finding 2, for the probe).
func TestWakeDefersProbeWithoutAutocommitWakesARowDeferredSinceTheFirstProbe(t *testing.T) {
	store, cleanup := setupConcurrentTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	// One session, kept across calls, as a long-lived process keeps one.
	store.db.SetMaxOpenConns(1)
	store.db.SetMaxIdleConns(1)
	store.db.SetConnMaxIdleTime(0)
	store.db.SetConnMaxLifetime(0)
	wakeExec(t, ctx, store.db, "SET autocommit = 0")
	sessionAutocommit := func() int {
		t.Helper()
		var ac int
		if err := store.db.QueryRowContext(ctx, "SELECT @@session.autocommit").Scan(&ac); err != nil {
			t.Fatalf("read autocommit: %v", err)
		}
		return ac
	}
	if ac := sessionAutocommit(); ac != 0 {
		t.Fatalf("store session autocommit = %d, want 0; the case under test did not arise", ac)
	}
	wakeExec(t, ctx, store.db, "ROLLBACK")
	firstProbe := issueops.ExpiredDefersDue(ctx, store.db)

	const id = "test-probe-noautocommit"
	writer := serverSessionDB(t, store)
	wakeExecArgs(t, ctx, writer, "INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type, defer_until) "+
		"VALUES (?, 'deferred since the probe', '', '', '', '', 'deferred', 2, 'task', TIMESTAMPADD(HOUR, -1, UTC_TIMESTAMP()))", id)

	work, err := store.GetReadyWork(ctx, types.WorkFilter{})
	if err != nil {
		t.Fatalf("GetReadyWork: %v", err)
	}
	found := false
	for _, w := range work {
		found = found || w.ID == id
	}
	got, err := store.GetIssue(ctx, id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	ac := sessionAutocommit()
	t.Logf("autocommit off: first probe due=%v; next ready read returned %s %v, status %q; session autocommit still %d", firstProbe, id, found, got.Status, ac)
	if ac != 0 {
		t.Fatalf("the store's session lost autocommit=0 (now %d): a fresh session ran the reads, so the case under test did not arise", ac)
	}
	if !found || got.Status != types.StatusOpen || got.DeferUntil != nil {
		t.Errorf("autocommit off: the ready read after another session deferred %s with a past date returned it %v, status %q, defer_until %v; want returned, open, nil",
			id, found, got.Status, got.DeferUntil)
	}
}

// TestWakeDefersIdleReadStatementCount counts, through a recording proxy,
// what a freshly opened store sends for an idle bd ready and bd list on a
// server with autocommit on (nothing due): the routing read GetAllConfig,
// then GetReadyWork, and CountReadyWork. The autocommit check rides on the
// statements already sent (bd-tuwml), so GetAllConfig is one statement and
// the wake costs one, the probe, before the read transaction.
func TestWakeDefersIdleReadStatementCount(t *testing.T) {
	skipIfNoDolt(t)
	acquireTestSlot()
	t.Cleanup(releaseTestSlot)
	ctx, cancel := testContext(t)
	defer cancel()

	f := newIdentityTestFixture(t, "idleready")
	setup, err := New(ctx, f.config())
	if err != nil {
		t.Fatalf("create database: %v", err)
	}
	if err := setup.SetConfig(ctx, "issue_prefix", "idle"); err != nil {
		_ = setup.Close()
		t.Fatalf("set prefix: %v", err)
	}
	open := &types.Issue{ID: "idle-open", Title: "open", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	if err := setup.CreateIssue(ctx, open, "tester"); err != nil {
		_ = setup.Close()
		t.Fatalf("create: %v", err)
	}
	if err := setup.Close(); err != nil {
		t.Fatalf("close setup store: %v", err)
	}

	rec := &openRecorder{}
	proxyPort := startBudgetProxy(t, rec, net.JoinHostPort("127.0.0.1", strconv.Itoa(testServerPort)))
	cfg := f.config()
	cfg.CreateIfMissing = false
	cfg.ServerHost = "127.0.0.1"
	cfg.ServerPort = proxyPort
	rec.start()
	store, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("open through proxy: %v", err)
	}
	defer func() { _ = store.Close() }()

	record := func(name string, op func() error) []string {
		t.Helper()
		n := recordedSoFar(rec)
		err := op()
		cmds := recordedSince(rec, n)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		t.Logf("idle %s: %d commands:\n%s", name, len(cmds), strings.Join(cmds, "\n"))
		return cmds
	}
	config := record("GetAllConfig", func() error { _, err := store.GetAllConfig(ctx); return err })
	ready := record("GetReadyWork", func() error { _, err := store.GetReadyWork(ctx, types.WorkFilter{}); return err })
	count := record("CountReadyWork", func() error { _, err := store.CountReadyWork(ctx, types.WorkFilter{}); return err })
	t.Logf("idle ready read: GetAllConfig %d + GetReadyWork %d = %d commands; CountReadyWork %d", len(config), len(ready), len(config)+len(ready), len(count))

	if len(config) != 1 {
		t.Errorf("idle GetAllConfig sent %d commands, want 1", len(config))
	}
	for name, cmds := range map[string][]string{"GetReadyWork": ready, "CountReadyWork": count} {
		before := 0
		for _, c := range cmds {
			if strings.Contains(c, "START TRANSACTION") {
				break
			}
			before++
		}
		if before != 1 || !strings.Contains(cmds[0], "@@autocommit AS ac FROM issues") {
			t.Errorf("idle %s sent %d commands before its read transaction, want 1, the probe", name, before)
		}
		for _, c := range cmds {
			if strings.Contains(c, "GET_LOCK") {
				t.Errorf("idle %s took a named lock: %s", name, c)
			}
		}
	}
}

func wakeExecArgs(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(ctx, query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// proxiedUOWProvider is the uow route over store's database: a test-owned
// dbproxy in front of the test server, and the proxied-server provider on it,
// as bd's --proxied-server commands build it.
func proxiedUOWProvider(t *testing.T, ctx context.Context, store *DoltStore) uow.UnitOfWorkProvider {
	t.Helper()
	root := t.TempDir()
	external := configfile.ExternalDoltConfig{Host: "127.0.0.1", Port: testServerPort}
	upstream, err := server.NewExternalDoltServer(external)
	if err != nil {
		t.Fatalf("external server: %v", err)
	}
	proxyCtx, stopProxy := context.WithCancel(context.Background())
	done := make(chan error, 1)
	p := proxy.NewProxyServer(proxy.ProxyOpts{RootDir: root, Server: upstream, IdleTimeout: proxy.IdleTimeoutNever})
	go func() { done <- p.ListenAndServe(proxyCtx) }()
	t.Cleanup(func() {
		stopProxy()
		if err := <-done; err != nil {
			t.Errorf("stop the test-owned proxy: %v", err)
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if ok, _ := proxy.IsRunning(root); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the test-owned proxy did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	provider, err := uow.NewExternalDoltServerUOWProvider(ctx, root, store.database,
		filepath.Join(root, "server.log"), external, store.cfg.ServerUser, store.cfg.ServerPassword,
		0, 0, false, "", uow.WithProxiedServerMode())
	if err != nil {
		t.Fatalf("proxied uow provider: %v", err)
	}
	t.Cleanup(func() { _ = provider.Close(context.Background()) })
	return provider
}

// TestClaimWaitingForTheJournalLockWakesTheDeferred (review bd-ee2ci,
// finding 1): a claim waits for the journal writer lock before it selects, so
// it must see what the lock holder's commit made due. The only issue is
// deferred an hour out; a holder takes the journal writer lock and, in its
// transaction, moves the date into the past; the claim starts and queues on
// GET_LOCK (seen in PROCESSLIST); the holder commits and releases. The claim
// returns that issue, stored in_progress, as HEAD did. With the read's probe
// in front of it, the claim answered "nothing due" before the wait and
// claimed nothing (claimed=<none>, the issue left deferred).
func TestClaimWaitingForTheJournalLockWakesTheDeferred(t *testing.T) {
	for _, route := range []string{"store", "uow"} {
		t.Run(route, func(t *testing.T) {
			store, cleanup := setupConcurrentTestStore(t)
			defer cleanup()
			ctx, cancel := testContext(t)
			defer cancel()

			id := "test-claim-wait-" + route
			deferIssue(t, ctx, store, id, false, time.Now().UTC().Add(time.Hour))
			var claimer publicops.ReadyClaimer
			var err error
			if route == "store" {
				claimer, err = store.ReadyClaimer()
			} else {
				claimer, err = uow.NewReadyClaimer(proxiedUOWProvider(t, ctx, store))
			}
			if err != nil {
				t.Fatalf("ready claimer: %v", err)
			}

			observer := serverSessionDB(t, store)
			holder, err := observer.Conn(ctx)
			if err != nil {
				t.Fatalf("holder conn: %v", err)
			}
			defer func() { _ = holder.Close() }()
			unlock, err := issueops.LockJournalWriter(ctx, holder, store.database)
			if err != nil {
				t.Fatalf("take the journal writer lock: %v", err)
			}
			defer unlock()
			tx, err := holder.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("holder begin: %v", err)
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.ExecContext(ctx, "UPDATE issues SET defer_until = TIMESTAMPADD(HOUR, -1, UTC_TIMESTAMP()) WHERE id = ?", id); err != nil {
				t.Fatalf("holder makes %s due: %v", id, err)
			}

			type answer struct {
				result publicops.ClaimNextResult
				err    error
			}
			done := make(chan answer, 1)
			go func() {
				got, err := claimer.ClaimNext(ctx, publicops.ClaimNextRequest{Actor: "claimer"})
				done <- answer{got, err}
			}()
			lockName := issueops.JournalWriterLockName(store.database)
			for {
				var waiting int
				// The store route sends the lock name inline; the proxied uow
				// route prepares GET_LOCK(?, ?), so INFO shows no name there.
				if err := observer.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE DB = ? AND ID <> CONNECTION_ID() AND INFO LIKE 'SELECT GET_LOCK(%' AND (INFO LIKE ? OR INFO LIKE 'SELECT GET_LOCK(?%')", store.database, "%"+lockName+"%").Scan(&waiting); err != nil {
					t.Fatalf("PROCESSLIST: %v", err)
				}
				if waiting >= 1 {
					break
				}
				select {
				case got := <-done:
					t.Fatalf("the claim returned while the journal writer lock was held: %+v, %v", got.result, got.err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(15 * time.Millisecond):
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("holder commit: %v", err)
			}
			unlock()
			got := <-done
			if got.err != nil {
				t.Fatalf("claim: %v", got.err)
			}
			claimed := "<none>"
			if got.result.Claimed != nil {
				claimed = got.result.Claimed.ID
			}
			persisted, err := store.GetIssue(ctx, id)
			if err != nil {
				t.Fatalf("get %s: %v", id, err)
			}
			t.Logf("%s route: the claim queued on GET_LOCK while the holder made %s due; claimed=%s persisted_status=%s", route, id, claimed, persisted.Status)
			if claimed != id || persisted.Status != types.StatusInProgress {
				t.Errorf("%s route: claimed=%s persisted_status=%s; want %s claimed and in_progress", route, claimed, persisted.Status, id)
			}
		})
	}
}

// TestClaimSweepSendsNoProbe records, through a proxy, what a claim on the
// store route sends with one deferred issue not yet due: the sweep's journal
// writer lock and transaction, then the claim's own, and no probe statement
// (review bd-ee2ci, finding 1: the claim's statements are HEAD's again).
func TestClaimSweepSendsNoProbe(t *testing.T) {
	store, cleanup := setupConcurrentTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	deferIssue(t, ctx, store, "test-claim-trace", false, time.Now().UTC().Add(time.Hour))
	rec := &openRecorder{}
	rec.start() // before the open, so its pooled connections are numbered
	port := startBudgetProxy(t, rec, net.JoinHostPort("127.0.0.1", strconv.Itoa(testServerPort)))
	cfg := *store.cfg
	cfg.ServerPort = port
	cfg.Path = t.TempDir()
	traced, err := New(ctx, &cfg)
	if err != nil {
		t.Fatalf("open through the proxy: %v", err)
	}
	defer func() { _ = traced.Close() }()
	claimer, err := traced.ReadyClaimer()
	if err != nil {
		t.Fatalf("ready claimer: %v", err)
	}
	n := recordedSoFar(rec)
	got, err := claimer.ClaimNext(ctx, publicops.ClaimNextRequest{Actor: "claimer"})
	cmds := recordedSince(rec, n)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	t.Logf("claim (claimed %v) sent %d commands:\n%s", got.Claimed != nil, len(cmds), strings.Join(cmds, "\n"))
	sawLock := false
	for _, c := range cmds {
		if strings.Contains(c, "UNION ALL (SELECT 0, @@autocommit)") {
			t.Errorf("the claim sent the read's probe: %s", c)
		}
		if strings.Contains(c, "GET_LOCK(") {
			sawLock = true
		}
	}
	if !sawLock {
		t.Errorf("the claim's sweep took no journal writer lock")
	}
}

// probeSessionCommentCount is how many comments of id a bare read on conn
// sees.
func probeSessionCommentCount(t *testing.T, ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) int {
	t.Helper()
	var n int
	if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM comments WHERE issue_id = ?", id).Scan(&n); err != nil {
		t.Fatalf("count comments of %s: %v", id, err)
	}
	return n
}

// TestWakeDefersProbeLeavesNoSnapshotOnItsConnection (review bd-ee2ci,
// finding 2, for the probe): on a session with autocommit off the probe's
// bare SELECT opens an implicit transaction, and the probe ends it on the
// same connection before the connection goes back. After the probe, another
// session commits a comment, and the next bare read on the probe's session
// sees it. On the store route (a pool of one session, through a recording
// proxy whose trace names each statement's connection) and on RunNonTx's
// connection of the uow route.
func TestWakeDefersProbeLeavesNoSnapshotOnItsConnection(t *testing.T) {
	t.Run("store", func(t *testing.T) {
		store, cleanup := setupConcurrentTestStore(t)
		defer cleanup()
		ctx, cancel := testContext(t)
		defer cancel()
		const id = "test-probe-snapshot-store"
		if err := store.CreateIssue(ctx, &types.Issue{ID: id, Title: "probe snapshot", Status: types.StatusOpen, IssueType: types.TypeTask}, "tester"); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		rec := &openRecorder{}
		rec.start()
		port := startBudgetProxy(t, rec, net.JoinHostPort("127.0.0.1", strconv.Itoa(testServerPort)))
		pool, err := sql.Open("mysql", doltutil.ServerDSN{Host: "127.0.0.1", Port: port, User: "root", Database: store.database}.String()+"&autocommit=0")
		if err != nil {
			t.Fatalf("open reader: %v", err)
		}
		t.Cleanup(func() { _ = pool.Close() })
		pool.SetMaxOpenConns(1)
		pool.SetMaxIdleConns(1)
		reader := &DoltStore{db: pool}
		var ac int
		if err := pool.QueryRowContext(ctx, "SELECT @@autocommit").Scan(&ac); err != nil || ac != 0 {
			t.Fatalf("reader autocommit = %d, %v; want 0 (the case under test did not arise)", ac, err)
		}
		wakeExec(t, ctx, pool, "ROLLBACK")
		n := recordedSoFar(rec)
		if !reader.expiredDefersDue(ctx) {
			t.Errorf("the probe on an autocommit-off session said nothing is due, want due")
		}
		cmds := recordedSince(rec, n)
		if _, err := store.AddIssueComment(ctx, id, "tester", "committed after the probe"); err != nil {
			t.Fatalf("another session comments: %v", err)
		}
		comments, err := reader.GetIssueComments(ctx, id)
		if err != nil {
			t.Fatalf("GetIssueComments: %v", err)
		}
		t.Logf("store route, autocommit=0: the probe sent %d commands:\n%s\nGetIssueComments on the probe's session found %d", len(cmds), strings.Join(cmds, "\n"), len(comments))
		if len(comments) != 1 {
			t.Errorf("GetIssueComments after the probe found %d comments, want 1 (the probe's session kept its snapshot)", len(comments))
		}
	})

	t.Run("uow RunNonTx", func(t *testing.T) {
		store, cleanup := setupConcurrentTestStore(t)
		defer cleanup()
		ctx, cancel := testContext(t)
		defer cancel()
		const id = "test-probe-snapshot-uow"
		if err := store.CreateIssue(ctx, &types.Issue{ID: id, Title: "probe snapshot", Status: types.StatusOpen, IssueType: types.TypeTask}, "tester"); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		mp := proxiedUOWProvider(t, ctx, store).(uow.MaintenanceProvider)
		var first, second, ac int
		due := false
		if err := mp.RunNonTx(ctx, func(ctx context.Context, conn *sql.Conn) error {
			if _, err := conn.ExecContext(ctx, "SET autocommit = 0"); err != nil {
				return err
			}
			if err := conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&first); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
				return err
			}
			due = issueops.ExpiredDefersDue(ctx, conn)
			return nil
		}); err != nil {
			t.Fatalf("RunNonTx probe: %v", err)
		}
		if !due {
			t.Errorf("the probe on an autocommit-off session said nothing is due, want due")
		}
		if _, err := store.AddIssueComment(ctx, id, "tester", "committed after the probe"); err != nil {
			t.Fatalf("another session comments: %v", err)
		}
		count := -1
		if err := mp.RunNonTx(ctx, func(ctx context.Context, conn *sql.Conn) error {
			if err := conn.QueryRowContext(ctx, "SELECT CONNECTION_ID(), @@autocommit").Scan(&second, &ac); err != nil {
				return err
			}
			count = probeSessionCommentCount(t, ctx, conn, id)
			return nil
		}); err != nil {
			t.Fatalf("RunNonTx read: %v", err)
		}
		t.Logf("uow route: session %d/%d autocommit=%d; a bare read after the probe found %d comments", first, second, ac, count)
		if first != second || ac != 0 {
			t.Fatalf("RunNonTx did not reuse the autocommit-off session (%d/%d, autocommit=%d): the case under test did not arise", first, second, ac)
		}
		if count != 1 {
			t.Errorf("a bare read on RunNonTx's session after the probe found %d comments, want 1 (the probe's session kept its snapshot)", count)
		}
	})
}
