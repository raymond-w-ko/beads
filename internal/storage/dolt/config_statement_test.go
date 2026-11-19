package dolt

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/steveyegge/beads/internal/storage/doltutil"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// TestGetAllConfigSendsOneSelect pins GetAllConfig (the routing read of every
// bd ready and bd list) to its one SELECT with no transaction around it: the
// START TRANSACTION and ROLLBACK it used to send were two round trips that
// returned the same rows (bd-ypak8 Q3, bd-aw3l8). The mock fails on a BEGIN
// or ROLLBACK it was not told to expect. The SELECT carries the session's
// @@autocommit (bd-tuwml), also through its table-less row when the table is
// empty (bd-545oj, review bd-ee2ci finding 3); here it is on, so the one
// SELECT is the answer, an empty map for an empty table.
func TestGetAllConfigSendsOneSelect(t *testing.T) {
	for _, c := range []struct {
		name string
		rows *sqlmock.Rows
		want map[string]string
	}{
		{name: "rows", rows: sqlmock.NewRows([]string{"@@autocommit", "key", "value"}).
			AddRow(1, "issue_prefix", "cs").AddRow(1, "routing.mode", "auto").AddRow(1, nil, nil),
			want: map[string]string{"issue_prefix": "cs", "routing.mode": "auto"}},
		{name: "empty config", rows: sqlmock.NewRows([]string{"@@autocommit", "key", "value"}).AddRow(1, nil, nil),
			want: map[string]string{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			mock.ExpectQuery(allConfigOneStatementSQL).WillReturnRows(c.rows)

			store := &DoltStore{db: db}
			got, err := store.GetAllConfig(context.Background())
			if err != nil {
				t.Fatalf("GetAllConfig: %v", err)
			}
			if !maps.Equal(got, c.want) || got == nil {
				t.Errorf("GetAllConfig = %v, want %v", got, c.want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("sqlmock expectations: %v", err)
			}
		})
	}
}

// TestGetAllConfigWithoutAutocommitReadsInATransaction pins the fallback of
// bd-tuwml: when the one SELECT reports autocommit off, on a table with rows
// or an empty one (its table-less row), or returns no row at all,
// GetAllConfig reads again as before bd-aw3l8 (START TRANSACTION, the
// SELECT, ROLLBACK) and returns that read's rows.
func TestGetAllConfigWithoutAutocommitReadsInATransaction(t *testing.T) {
	for _, c := range []struct {
		name string
		bare *sqlmock.Rows
	}{
		{name: "autocommit off", bare: sqlmock.NewRows([]string{"@@autocommit", "key", "value"}).AddRow(0, "issue_prefix", "stale").AddRow(0, nil, nil)},
		{name: "empty config, autocommit off", bare: sqlmock.NewRows([]string{"@@autocommit", "key", "value"}).AddRow(0, nil, nil)},
		{name: "no row", bare: sqlmock.NewRows([]string{"@@autocommit", "key", "value"})},
	} {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			mock.ExpectQuery(allConfigOneStatementSQL).WillReturnRows(c.bare)
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT `key`, value FROM config").
				WillReturnRows(sqlmock.NewRows([]string{"key", "value"}).AddRow("issue_prefix", "fresh"))
			mock.ExpectRollback()

			store := &DoltStore{db: db}
			got, err := store.GetAllConfig(context.Background())
			if err != nil {
				t.Fatalf("GetAllConfig: %v", err)
			}
			if len(got) != 1 || got["issue_prefix"] != "fresh" {
				t.Errorf("GetAllConfig = %v, want the transaction's issue_prefix=fresh", got)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("sqlmock expectations: %v", err)
			}
		})
	}
}

// recordedSoFar is how many commands rec holds. A recorder started before
// the connections under test open numbers them all; recordedSince then
// gives one call's commands, where stop and start would forget the
// connections opened before them.
func recordedSoFar(rec *openRecorder) int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return len(rec.cmds)
}

// recordedSince returns the commands rec recorded after the first n.
func recordedSince(rec *openRecorder, n int) []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]string(nil), rec.cmds[n:]...)
}

// configFreshnessReader is a store over one pooled session of store's
// database, reached through a recording proxy, with autocommit as the DSN
// sets it: GetAllConfig needs only db (and the zero breaker).
func configFreshnessReader(t *testing.T, store *DoltStore, rec *openRecorder, autocommit string) (*DoltStore, *sql.DB) {
	t.Helper()
	port := startBudgetProxy(t, rec, net.JoinHostPort("127.0.0.1", strconv.Itoa(testServerPort)))
	db, err := sql.Open("mysql", doltutil.ServerDSN{Host: "127.0.0.1", Port: port, User: "root", Database: store.database}.String()+"&autocommit="+autocommit)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return &DoltStore{db: db}, db
}

// runConfigFreshness is the reviewer's sequence (review bd-w6ppm, finding
// 2) on a reader whose one session has autocommit as given: the writer, its
// own autocommit session, commits A; the reader reads; the writer commits B;
// the reader reads again, under the recorder. It returns the second read's
// value and the commands it sent.
func runConfigFreshness(t *testing.T, autocommit string) (second string, cmds []string) {
	t.Helper()
	store, cleanup := setupConcurrentTestStore(t)
	t.Cleanup(cleanup)
	ctx, cancel := testContext(t)
	t.Cleanup(cancel)

	rec := &openRecorder{}
	rec.start()
	reader, readerDB := configFreshnessReader(t, store, rec, autocommit)
	writer, err := sql.Open("mysql", doltutil.ServerDSN{Host: "127.0.0.1", Port: testServerPort, User: "root", Database: store.database}.String()+"&autocommit=1")
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })

	const key = "test.config.freshness"
	if err := issueops.SetConfigInTx(ctx, writer, key, "A"); err != nil {
		t.Fatalf("writer sets A: %v", err)
	}
	var got int
	if err := readerDB.QueryRowContext(ctx, "SELECT @@session.autocommit").Scan(&got); err != nil || strconv.Itoa(got) != autocommit {
		t.Fatalf("reader autocommit = %d, %v; want %s (the case under test did not arise)", got, err, autocommit)
	}
	// End whatever implicit transaction the check above opened, so the
	// first read below starts the session's snapshot, as a pooled
	// session's earlier bare read would have.
	if _, err := readerDB.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatalf("reader ROLLBACK: %v", err)
	}
	first, err := reader.GetAllConfig(ctx)
	if err != nil || first[key] != "A" {
		t.Fatalf("first read = %q, %v; want A", first[key], err)
	}
	if err := issueops.SetConfigInTx(ctx, writer, key, "B"); err != nil {
		t.Fatalf("writer sets B: %v", err)
	}
	latest, err := issueops.GetConfigInTx(ctx, writer, key)
	if err != nil || latest != "B" {
		t.Fatalf("writer reads %q, %v; want B", latest, err)
	}
	n := recordedSoFar(rec)
	again, err := reader.GetAllConfig(ctx)
	cmds = recordedSince(rec, n)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	t.Logf("reader autocommit=%s, first=%q, second=%q, independent writer=%q; second read sent %d commands:\n%s",
		autocommit, first[key], again[key], latest, len(cmds), strings.Join(cmds, "\n"))
	return again[key], cmds
}

// TestGetAllConfigWithoutAutocommitSeesTheLatestCommit: on a session with
// autocommit off, a second GetAllConfig after another session commits B
// returns B, as HEAD's transaction did (review bd-w6ppm, finding 2: the bare
// SELECT alone returned the retained snapshot's A).
func TestGetAllConfigWithoutAutocommitSeesTheLatestCommit(t *testing.T) {
	second, _ := runConfigFreshness(t, "0")
	if second != "B" {
		t.Errorf("autocommit off: second=%q, want \"B\" (the latest commit)", second)
	}
}

// TestGetAllConfigWithAutocommitIsOneStatement: on a session with autocommit
// on, the same sequence reads B in one statement, the bare SELECT, with no
// transaction around it (bd-aw3l8's saving kept).
func TestGetAllConfigWithAutocommitIsOneStatement(t *testing.T) {
	second, cmds := runConfigFreshness(t, "1")
	if second != "B" {
		t.Errorf("autocommit on: second=%q, want \"B\"", second)
	}
	if len(cmds) != 1 || !strings.HasSuffix(cmds[0], "QUERY "+allConfigOneStatementSQL) {
		t.Errorf("autocommit on: second read sent %d commands %q, want exactly the one QUERY %q", len(cmds), cmds, allConfigOneStatementSQL)
	}
}

// TestGetAllConfigEmptyTableWithAutocommitIsOneCommand (review bd-ee2ci,
// finding 3): with autocommit on and an empty config table, GetAllConfig is
// the one SELECT and an empty map: the table-less row carries the session's
// autocommit, so the empty table needs no transaction (bd-tuwml's change sent
// four commands here, bd-aw3l8's one).
func TestGetAllConfigEmptyTableWithAutocommitIsOneCommand(t *testing.T) {
	store, cleanup := setupConcurrentTestStore(t)
	t.Cleanup(cleanup)
	ctx, cancel := testContext(t)
	t.Cleanup(cancel)
	if _, err := store.db.ExecContext(ctx, "DELETE FROM config"); err != nil {
		t.Fatalf("empty config: %v", err)
	}
	rec := &openRecorder{}
	rec.start()
	reader, db := configFreshnessReader(t, store, rec, "1")
	var ac int
	if err := db.QueryRowContext(ctx, "SELECT @@autocommit").Scan(&ac); err != nil || ac != 1 {
		t.Fatalf("reader autocommit = %d, %v; want 1", ac, err)
	}
	n := recordedSoFar(rec)
	got, err := reader.GetAllConfig(ctx)
	cmds := recordedSince(rec, n)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("GetAllConfig on an empty table = %v, %v; want an empty map", got, err)
	}
	t.Logf("empty config, autocommit=%d: %d commands: %q", ac, len(cmds), cmds)
	if len(cmds) != 1 || !strings.HasSuffix(cmds[0], "QUERY "+allConfigOneStatementSQL) {
		t.Errorf("empty config with autocommit on sent %d commands %q, want the one QUERY %q", len(cmds), cmds, allConfigOneStatementSQL)
	}
}

// TestGetAllConfigFallbackStaysOnItsConnection (review bd-ee2ci, finding 2,
// the reviewer's two-connection sequence): a pool of two autocommit-off
// sessions behind a proxy that holds GetAllConfig's first SELECT. While it is
// held, the other session is pinned and a third checkout waits; then the
// SELECT runs. The fallback (START TRANSACTION, SELECT, ROLLBACK) must run on
// the SELECT's own connection, so the session the waiter gets has no
// transaction open: after another writer commits a comment, GetIssueComments
// on it finds the comment. With the fallback on the pool, it ran on the other
// session and the first kept its implicit snapshot (found 0).
func TestGetAllConfigFallbackStaysOnItsConnection(t *testing.T) {
	store, cleanup := setupConcurrentTestStore(t)
	t.Cleanup(cleanup)
	ctx, cancel := testContext(t)
	t.Cleanup(cancel)
	const id = "test-config-fallback-comment"
	if err := store.CreateIssue(ctx, &types.Issue{ID: id, Title: "config fallback fixture", Status: types.StatusOpen, IssueType: types.TypeTask}, "tester"); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}

	rec := &openRecorder{}
	rec.start()
	hooks := &proxyHooks{}
	atSelect := make(chan int, 1)
	allowSelect := make(chan struct{})
	var allowOnce sync.Once
	allow := func() { allowOnce.Do(func() { close(allowSelect) }) }
	defer allow()
	hooks.setBeforeQuery(func(conn int, desc string) {
		if desc == "QUERY "+allConfigOneStatementSQL {
			select {
			case atSelect <- conn:
			default:
			}
			<-allowSelect
		}
	})
	port := startHookedProxy(t, rec, hooks, net.JoinHostPort("127.0.0.1", strconv.Itoa(testServerPort)))
	pool, err := sql.Open("mysql", doltutil.ServerDSN{Host: "127.0.0.1", Port: port, User: "root", Database: store.database}.String()+"&autocommit=0")
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	pool.SetMaxOpenConns(2)
	pool.SetMaxIdleConns(2)
	reader := &DoltStore{db: pool}
	other, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("pin the other session: %v", err)
	}
	defer func() { _ = other.Close() }()
	var otherID int
	if err := other.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&otherID); err != nil {
		t.Fatalf("other session id: %v", err)
	}

	n := recordedSoFar(rec)
	configDone := make(chan error, 1)
	go func() { _, err := reader.GetAllConfig(ctx); configDone <- err }()
	var selectConn int
	select {
	case selectConn = <-atSelect:
	case err := <-configDone:
		t.Fatalf("GetAllConfig returned before its SELECT was held: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	beforeWait := pool.Stats().WaitCount
	waiter := make(chan *sql.Conn, 1)
	go func() {
		conn, err := pool.Conn(ctx)
		if err != nil {
			close(waiter)
			return
		}
		waiter <- conn
	}()
	for pool.Stats().WaitCount == beforeWait {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	allow()
	// With the fallback on its own connection the waiter gets that
	// connection when GetAllConfig is done; with it on the pool, the waiter
	// got it after the bare SELECT, and the fallback waited for the other
	// session, released only once the waiter has its connection.
	var waited *sql.Conn
	select {
	case waited = <-waiter:
		if waited == nil {
			t.Fatal("the waiting checkout failed")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := other.Close(); err != nil {
		t.Fatalf("release the other session: %v", err)
	}
	select {
	case err := <-configDone:
		if err != nil {
			t.Fatalf("GetAllConfig: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cmds := recordedSince(rec, n)
	// No statement on the waited session here: on an autocommit-off session
	// any bare read would open the snapshot this test looks for.
	// Hold the other session, so the read below runs on the session
	// GetAllConfig's SELECT used (the one the waiter got).
	hold, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("hold the other session: %v", err)
	}
	defer func() { _ = hold.Close() }()
	var holdID int
	if err := hold.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&holdID); err != nil || holdID != otherID {
		t.Fatalf("held session = %d, %v; want the other session %d", holdID, err, otherID)
	}
	if _, err := store.AddIssueComment(ctx, id, "tester", "committed after GetAllConfig"); err != nil {
		t.Fatalf("another writer comments: %v", err)
	}
	if err := waited.Close(); err != nil {
		t.Fatalf("release the waited session: %v", err)
	}
	comments, err := reader.GetIssueComments(ctx, id)
	if err != nil {
		t.Fatalf("GetIssueComments: %v", err)
	}
	t.Logf("GetAllConfig's SELECT on proxy connection c%d (the other, held, is server session %d); its commands:\n%s\nGetIssueComments on GetAllConfig's session found %d", selectConn, otherID, strings.Join(cmds, "\n"), len(comments))
	if len(comments) != 1 {
		t.Errorf("GetIssueComments after GetAllConfig found %d comments, want 1: a session kept GetAllConfig's snapshot", len(comments))
	}
}

// configCompareAlphabet is what the random config keys and values are drawn
// from: ASCII letters in both cases, punctuation SQL quotes, a backslash,
// whitespace and multi-byte runes.
var configCompareAlphabet = []rune("abcXYZ019 ._-'\"\\%\t\nÄßé€中文😀")

func randomConfigText(r *rand.Rand, maxLen int) string {
	n := r.Intn(maxLen + 1)
	out := make([]rune, n)
	for i := range out {
		out[i] = configCompareAlphabet[r.Intn(len(configCompareAlphabet))]
	}
	return string(out)
}

// TestGetAllConfigOneStatementMatchesTheTransaction is a comparison test
// (review bd-ee2ci, item 3). The oracle is HEAD's read,
// issueops.GetAllConfigInTx in a transaction; the piece is the one-statement
// read GetAllConfig sends on a session with autocommit on
// (getAllConfigOneStatement, allConfigOneStatementSQL with its table-less
// row). Relation: equal maps, and the piece says fresh. Generator: the empty
// table; fixed rows (an empty value, a multi-byte key and value, a value of
// 70,000 bytes or, when the TEXT column refuses that, the largest it holds,
// two keys that differ only in case, a value that is the text NULL); then
// seeds 1 to 40, each up to 25 random rows of keys up to 40 and values up to
// 3,000 runes. The seed is in every failure.
func TestGetAllConfigOneStatementMatchesTheTransaction(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	compare := func(t *testing.T, label string) int {
		t.Helper()
		conn, err := store.db.Conn(ctx)
		if err != nil {
			t.Fatalf("%s: pin: %v", label, err)
		}
		got, fresh, err := getAllConfigOneStatement(ctx, conn)
		_ = conn.Close()
		if err != nil {
			t.Fatalf("%s: one-statement read: %v", label, err)
		}
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("%s: begin: %v", label, err)
		}
		want, err := issueops.GetAllConfigInTx(ctx, tx)
		_ = tx.Rollback()
		if err != nil {
			t.Fatalf("%s: oracle: %v", label, err)
		}
		if !fresh {
			t.Errorf("%s: the one-statement read on an autocommit-on session says not fresh", label)
		}
		if got == nil || !maps.Equal(got, want) {
			for k, v := range want {
				if g, ok := got[k]; !ok || g != v {
					t.Errorf("%s: key %q: one statement %q (present %v), transaction %q (%d bytes)", label, k, g, ok, v, len(v))
				}
			}
			for k := range got {
				if _, ok := want[k]; !ok {
					t.Errorf("%s: key %q only in the one-statement read", label, k)
				}
			}
			t.Errorf("%s: the two reads differ (%d rows against %d)", label, len(got), len(want))
		}
		return len(want)
	}
	reset := func() {
		if _, err := store.db.ExecContext(ctx, "DELETE FROM config"); err != nil {
			t.Fatalf("empty config: %v", err)
		}
	}
	insert := func(k, v string) error {
		_, err := store.db.ExecContext(ctx, "INSERT INTO config (`key`, value) VALUES (?, ?)", k, v)
		return err
	}

	start := time.Now()
	rows := 0
	reset()
	rows += compare(t, "empty table")

	reset()
	for _, kv := range [][2]string{
		{"empty.value", ""},
		{"ключ.🔑.設定", "значение — ✓ 中文 😀"},
		{"text.null", "NULL"},
		{"Case.Key", "upper"},
	} {
		if err := insert(kv[0], kv[1]); err != nil {
			t.Fatalf("insert %q: %v", kv[0], err)
		}
	}
	if err := insert("case.key", "lower"); err != nil {
		t.Logf("the table refuses a second key differing only in case: %v", err)
	} else {
		t.Logf("the table takes Case.Key and case.key as two keys")
	}
	big := strings.Repeat("v", 70000)
	if err := insert("big.value", big); err != nil {
		t.Logf("the TEXT value column refuses 70,000 bytes (%v); using 65,535", err)
		if err := insert("big.value", strings.Repeat("v", 65535)); err != nil {
			t.Fatalf("insert a 65,535-byte value: %v", err)
		}
	} else {
		t.Logf("the TEXT value column takes 70,000 bytes")
	}
	rows += compare(t, "fixed rows")

	const seeds = 40
	for seed := int64(1); seed <= seeds; seed++ {
		reset()
		r := rand.New(rand.NewSource(seed))
		seen := map[string]bool{}
		for i, n := 0, r.Intn(26); i < n; i++ {
			k := randomConfigText(r, 40)
			if k == "" || seen[strings.ToLower(k)] {
				continue
			}
			seen[strings.ToLower(k)] = true
			if err := insert(k, randomConfigText(r, 3000)); err != nil {
				t.Fatalf("seed %d: insert %q: %v", seed, k, err)
			}
		}
		rows += compare(t, fmt.Sprintf("seed %d", seed))
	}
	t.Logf("compared the empty table, the fixed rows and seeds 1..%d: %d rows in %v", seeds, rows, time.Since(start))
}
