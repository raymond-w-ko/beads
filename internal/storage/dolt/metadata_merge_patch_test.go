package dolt

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/issueops"
)

// TestMetadataMergePatchConcurrentNestedKeysNoClobber is the live test for
// MetadataPatch.MergePatch: two writers, each on its own connection, set
// different keys of ONE nested object on one bead, round after round. Each
// patch is resolved against the row read inside its own transaction, so every
// key either writer set must survive, and so must the siblings neither named.
func TestMetadataMergePatchConcurrentNestedKeysNoClobber(t *testing.T) {
	store, cleanup := setupConcurrentTestStore(t)
	defer cleanup()

	ctx, cancel := concurrentTestContext(t)
	defer cancel()

	const id = "mp-concurrent"
	createPerm(t, ctx, store, id)
	ops, err := store.IssueLifecycle()
	if err != nil {
		t.Fatalf("IssueLifecycle: %v", err)
	}
	if _, err := ops.Update(ctx, issueops.UpdateRequest{Actor: "seed", IssueID: id, Patch: issueops.IssuePatch{
		Metadata: issueops.MetadataPatch{MergePatch: []json.RawMessage{json.RawMessage(`{"top":"seeded","everturn":{"keep":"seeded"}}`)}},
	}}); err != nil {
		t.Fatalf("seed merge patch: %v", err)
	}

	const rounds = 20
	writers := []string{"landing", "worktree"}
	var wg sync.WaitGroup
	errs := make(chan error, len(writers)*rounds)
	for _, writer := range writers {
		wg.Go(func() {
			for round := range rounds {
				patch := json.RawMessage(fmt.Sprintf(`{"everturn":{%q:{"round":%d},"%s_r%d":true}}`, writer, round, writer, round))
				if _, err := ops.Update(ctx, issueops.UpdateRequest{Actor: writer, IssueID: id, Patch: issueops.IssuePatch{
					Metadata: issueops.MetadataPatch{MergePatch: []json.RawMessage{patch}},
				}}); err != nil {
					errs <- fmt.Errorf("%s round %d: %w", writer, round, err)
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("merge patch error: %v", err)
	}
	if t.Failed() {
		t.FailNow()
	}

	m := getMetadataMap(t, ctx, store, id)
	if got := string(m["top"]); got != `"seeded"` {
		t.Errorf("top-level sibling = %s, want \"seeded\"", got)
	}
	var everturn map[string]json.RawMessage
	if err := json.Unmarshal(m["everturn"], &everturn); err != nil {
		t.Fatalf("everturn = %s: %v", m["everturn"], err)
	}
	if got := string(everturn["keep"]); got != `"seeded"` {
		t.Errorf("nested sibling = %s, want \"seeded\"", got)
	}
	for _, writer := range writers {
		if got, want := string(everturn[writer]), fmt.Sprintf(`{"round":%d}`, rounds-1); got != want {
			t.Errorf("everturn.%s = %s, want %s", writer, got, want)
		}
		for round := range rounds {
			key := fmt.Sprintf("%s_r%d", writer, round)
			if got := string(everturn[key]); got != "true" {
				t.Errorf("everturn.%s = %q, want true: a concurrent merge patch lost it", key, got)
			}
		}
	}
}

// doltAtLeast reports whether the server's dolt_version() is major.minor or
// later.
func doltAtLeast(t *testing.T, ctx context.Context, db *sql.DB, major, minor int) (bool, string) {
	t.Helper()
	var version string
	if err := db.QueryRowContext(ctx, "SELECT dolt_version()").Scan(&version); err != nil {
		t.Fatalf("dolt_version(): %v", err)
	}
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		t.Fatalf("dolt_version() = %q, want major.minor", version)
	}
	gotMajor, err1 := strconv.Atoi(parts[0])
	gotMinor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		t.Fatalf("dolt_version() = %q, want numeric major.minor", version)
	}
	return gotMajor > major || (gotMajor == major && gotMinor >= minor), version
}

// TestMetadataMergePatchKeepsInt64DigitsLive stores 9007199254740993 (2^53+1)
// at depth 2 beside a sibling a merge patch edits, then patches that integer
// itself to the next one up, and reads the stored column. On a Dolt that keeps
// such digits in a JSON column (2.6 and later, the fleet's server) both must
// survive: the first because the merge never decodes a member it does not
// name, the second because the update's change check compares numbers exactly
// and so does not drop the edit as a no-op. Dolt 2.2, the harness image,
// rounds the digits in the column; there the digit assertions are logged and
// skipped, and the rest of the test runs.
func TestMetadataMergePatchKeepsInt64DigitsLive(t *testing.T) {
	store, cleanup := setupConcurrentTestStore(t)
	defer cleanup()
	ctx, cancel := concurrentTestContext(t)
	defer cancel()

	keeps, version := doltAtLeast(t, ctx, store.db, 2, 6)
	t.Logf("test Dolt server: dolt_version() = %s", version)
	const id = "mp-int64"
	createPerm(t, ctx, store, id)
	ops, err := store.IssueLifecycle()
	if err != nil {
		t.Fatalf("IssueLifecycle: %v", err)
	}
	update := func(patch issueops.MetadataPatch) {
		t.Helper()
		if _, err := ops.Update(ctx, issueops.UpdateRequest{Actor: "int64", IssueID: id, Patch: issueops.IssuePatch{Metadata: patch}}); err != nil {
			t.Fatalf("update %s: %v", id, err)
		}
	}
	stored := func() string {
		t.Helper()
		var text string
		if err := store.db.QueryRowContext(ctx, "SELECT metadata FROM issues WHERE id = ?", id).Scan(&text); err != nil {
			t.Fatalf("read stored metadata: %v", err)
		}
		return text
	}
	update(issueops.MetadataPatch{Replace: issueops.Field[json.RawMessage]{Set: true,
		Value: json.RawMessage(`{"everturn":{"id":9007199254740993,"landing":{"old":1}}}`)}})
	update(issueops.MetadataPatch{MergePatch: []json.RawMessage{json.RawMessage(`{"everturn":{"landing":{"new":2}}}`)}})
	afterSibling := stored()
	t.Logf("after patching the sibling: %s", afterSibling)
	if landing := strings.ReplaceAll(afterSibling, " ", ""); !strings.Contains(landing, `"landing":{"new":2,"old":1}`) {
		t.Errorf("stored %s, want everturn.landing merged to {\"new\":2,\"old\":1}", afterSibling)
	}
	if keeps {
		if !strings.Contains(afterSibling, "9007199254740993") {
			t.Errorf("Dolt %s: stored %s, want everturn.id's digits 9007199254740993 kept beside the patched sibling", version, afterSibling)
		} else {
			t.Logf("Dolt %s: everturn.id kept as 9007199254740993 beside the patched sibling", version)
		}
	} else {
		t.Logf("Dolt %s rounds integers past 2^53 in a JSON column: digit assertion skipped (stored %s)", version, afterSibling)
	}

	update(issueops.MetadataPatch{Replace: issueops.Field[json.RawMessage]{Set: true,
		Value: json.RawMessage(`{"everturn":{"id":9007199254740992,"landing":{"new":2}}}`)}})
	update(issueops.MetadataPatch{MergePatch: []json.RawMessage{json.RawMessage(`{"everturn":{"id":9007199254740993}}`)}})
	afterEdit := stored()
	if keeps {
		if !strings.Contains(afterEdit, "9007199254740993") {
			t.Errorf("Dolt %s: stored %s after patching everturn.id from ...992 to ...993, want the edit written", version, afterEdit)
		} else {
			t.Logf("Dolt %s: everturn.id patched from 9007199254740992 to 9007199254740993 and stored: %s", version, afterEdit)
		}
	} else {
		t.Logf("Dolt %s: digit edit assertion skipped (stored %s)", version, afterEdit)
	}
}

// mpRetryConnector wraps the store's driver so that the first commit, after
// arm, of a transaction that wrote the issues table is preceded by a
// conflicting write from another connection: the update's commit then fails
// with a serialization conflict and withRetryTx replays the whole body.
type mpRetryConnector struct {
	dsn         string
	drv         driver.Driver
	armed       atomic.Bool
	conflict    func() error
	mu          sync.Mutex
	commits     []error // commit results of transactions that wrote issues, after arm
	conflictErr error
}

func (c *mpRetryConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.drv.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &mpRetryConn{Conn: conn, c: c}, nil
}

func (c *mpRetryConnector) Driver() driver.Driver { return c.drv }

type mpRetryConn struct {
	driver.Conn
	c           *mpRetryConnector
	wroteIssues bool
}

func (c *mpRetryConn) note(query string) {
	if strings.Contains(strings.ToUpper(query), "UPDATE ISSUES") {
		c.wroteIssues = true
	}
}

func (c *mpRetryConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.note(query)
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c *mpRetryConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.note(query)
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *mpRetryConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	c.note(query)
	return c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
}

func (c *mpRetryConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	c.wroteIssues = false
	return &mpRetryTx{Tx: tx, conn: c}, nil
}

func (c *mpRetryConn) Ping(ctx context.Context) error { return c.Conn.(driver.Pinger).Ping(ctx) }

func (c *mpRetryConn) CheckNamedValue(nv *driver.NamedValue) error {
	return c.Conn.(driver.NamedValueChecker).CheckNamedValue(nv)
}

type mpRetryTx struct {
	driver.Tx
	conn *mpRetryConn
}

func (tx *mpRetryTx) Commit() error {
	c := tx.conn.c
	if !tx.conn.wroteIssues || !c.armed.Load() {
		return tx.Tx.Commit()
	}
	c.mu.Lock()
	first := len(c.commits) == 0
	c.mu.Unlock()
	if first {
		c.conflictErr = c.conflict()
	}
	err := tx.Tx.Commit()
	c.mu.Lock()
	c.commits = append(c.commits, err)
	c.mu.Unlock()
	return err
}

// TestMetadataMergePatchRetryReappliesToRowReadAgain pins what a replayed
// update does with its merge patches. The update's first commit is made to
// lose a serialization conflict: just before it, another connection commits a
// change to the same row (a new sibling key, a new top-level key, a new
// updated_at, and row_lock, the cell every update rewrites so that two writes
// to one row conflict rather than cell-merge). withRetryTx then replays the whole body, which must read the
// row again and apply the two patches once, in order, to that row: the other
// writer's keys survive beside the patched ones, and the issue gains exactly
// one update event. Reusing the first attempt's merged document would drop
// the other writer's keys.
func TestMetadataMergePatchRetryReappliesToRowReadAgain(t *testing.T) {
	store, cleanup := setupConcurrentTestStore(t)
	defer cleanup()
	ctx, cancel := concurrentTestContext(t)
	defer cancel()

	const id = "mp-retry"
	createPerm(t, ctx, store, id)
	ops, err := store.IssueLifecycle()
	if err != nil {
		t.Fatalf("IssueLifecycle: %v", err)
	}
	if _, err := ops.Update(ctx, issueops.UpdateRequest{Actor: "seed", IssueID: id, Patch: issueops.IssuePatch{
		Metadata: issueops.MetadataPatch{Replace: issueops.Field[json.RawMessage]{Set: true,
			Value: json.RawMessage(`{"everturn":{"landing":{"old":1},"keep":"k"}}`)}},
	}}); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}

	other := sql.OpenDB(&mpRetryConnector{dsn: store.connStr, drv: store.db.Driver()})
	defer other.Close()
	connector := &mpRetryConnector{dsn: store.connStr, drv: store.db.Driver()}
	connector.conflict = func() error {
		_, err := other.ExecContext(ctx, "UPDATE issues SET metadata = JSON_SET(metadata, '$.everturn.winner', CAST('true' AS JSON), '$.top', 'winner'), "+
			"updated_at = ?, row_lock = ? WHERE id = ?", time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC), int64(424242), id)
		return err
	}
	wrapped := sql.OpenDB(connector)
	original := store.db
	store.db = wrapped
	defer func() {
		store.db = original
		_ = wrapped.Close()
	}()

	eventsBefore, err := store.GetEvents(ctx, id, 1000)
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	connector.armed.Store(true)
	result, err := ops.Update(ctx, issueops.UpdateRequest{Actor: "patcher", IssueID: id, Patch: issueops.IssuePatch{
		Metadata: issueops.MetadataPatch{MergePatch: []json.RawMessage{
			json.RawMessage(`{"everturn":{"landing":null}}`),
			json.RawMessage(`{"everturn":{"landing":{"new":2}}}`),
		}},
	}})
	connector.armed.Store(false)
	if connector.conflictErr != nil {
		t.Fatalf("conflicting write: %v", connector.conflictErr)
	}
	if err != nil {
		t.Fatalf("merge patch update after a lost commit: %v", err)
	}
	connector.mu.Lock()
	commits := append([]error(nil), connector.commits...)
	connector.mu.Unlock()
	t.Logf("commits of the update's issue-writing transaction: %v", commits)
	if len(commits) < 2 || commits[0] == nil || commits[len(commits)-1] != nil {
		t.Fatalf("commit results = %v, want a lost first commit and a replayed one that lands", commits)
	}
	if !isSerializationError(commits[0]) && !isDoltAutocommitRollbackError(commits[0]) {
		t.Fatalf("first commit error = %v, want a serialization conflict withRetryTx replays", commits[0])
	}
	if !result.Changed {
		t.Error("replayed merge patch reported Changed = false")
	}

	got := getMetadataMap(t, ctx, store, id)
	var everturn map[string]json.RawMessage
	if err := json.Unmarshal(got["everturn"], &everturn); err != nil {
		t.Fatalf("everturn = %s: %v", got["everturn"], err)
	}
	for _, check := range []struct{ name, got, want string }{
		{"top (the other writer's)", string(got["top"]), `"winner"`},
		{"everturn.winner (the other writer's)", string(everturn["winner"]), "true"},
		{"everturn.keep", string(everturn["keep"]), `"k"`},
		{"everturn.landing (null, then the new object)", strings.ReplaceAll(string(everturn["landing"]), " ", ""), `{"new":2}`},
	} {
		if check.got != check.want {
			t.Errorf("%s = %s, want %s", check.name, check.got, check.want)
		}
	}
	eventsAfter, err := store.GetEvents(ctx, id, 1000)
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if added := len(eventsAfter) - len(eventsBefore); added != 1 {
		t.Errorf("the replayed update added %d events, want exactly 1", added)
	}
}

// mpOracleNumbers are the number groups of TestMetadataNumberEditsMatchTheServer:
// integers around 2^53, 2^63 and 2^64, fractions that share a float64, and
// exponent and decimal spellings of one value. A pair is drawn from one group.
var mpOracleNumbers = [][]string{
	{"9007199254740991", "9007199254740992", "9007199254740993", "9007199254740993.0", "9.007199254740993e15", "9007199254740994"},
	{"9223372036854775807", "9223372036854775806", "9223372036854775808", "9223372036854775809", "9.223372036854775807e18", "-9223372036854775808", "-9223372036854775807"},
	{"18446744073709551615", "18446744073709551614", "18446744073709551616", "1.8446744073709552e19", "18446744073709551615.0"},
	{"0.1", "0.10000000000000001", "1e-1", "0.1000000000000000055511151231257827", "0.2", "0.30000000000000004", "0.3"},
	{"1000", "1e3", "1E3", "1000.0", "10e2", "1", "1.0", "1e0", "-150", "-1.5e2", "0", "0.0"},
}

// mpRowState is what a write moves: the stored metadata text, the row version
// (row_lock), updated_at and the issue's event count.
type mpRowState struct {
	metadata, lock string
	updated        time.Time
	events         int
}

func mpReadRow(t *testing.T, ctx context.Context, db *sql.DB, id string) mpRowState {
	t.Helper()
	var s mpRowState
	if err := db.QueryRowContext(ctx, "SELECT metadata, HEX(row_lock), updated_at, (SELECT COUNT(*) FROM events WHERE issue_id = ?) FROM issues WHERE id = ?", id, id).
		Scan(&s.metadata, &s.lock, &s.updated, &s.events); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return s
}

func (s mpRowState) wrote(after mpRowState) bool {
	return s.lock != after.lock || !s.updated.Equal(after.updated) || s.events != after.events
}

// mpRoutes are the two Lifecycle routes bd update takes against a server: the
// store (server mode) and the unit of work behind a dbproxy (proxied server).
func mpRoutes(t *testing.T, ctx context.Context, store *DoltStore) []struct {
	name string
	ops  issueops.Lifecycle
} {
	t.Helper()
	direct, err := store.IssueLifecycle()
	if err != nil {
		t.Fatalf("IssueLifecycle: %v", err)
	}
	provider, ok := proxiedUOWProvider(t, ctx, store).(interface {
		IssueLifecycle() (issueops.Lifecycle, error)
	})
	if !ok {
		t.Fatal("the proxied uow provider has no IssueLifecycle")
	}
	proxied, err := provider.IssueLifecycle()
	if err != nil {
		t.Fatalf("uow IssueLifecycle: %v", err)
	}
	return []struct {
		name string
		ops  issueops.Lifecycle
	}{{"store", direct}, {"uow", proxied}}
}

// TestMetadataNumberEditsMatchTheServer is the live comparison test of review
// bd-ldmiy finding 3, with the server as the oracle. For seeds 1 to
// mpOracleSeeds a pair of numbers a, b from one group of mpOracleNumbers: the
// row is set to {"k":a,"keep":"x"} by SQL, its stored text read, then b is
// applied by --set-metadata k=b and by --merge-metadata {"k":b}, on the store
// route and on the uow route, and the row read again. Relation on Dolt 2.6 and
// later: the row version, updated_at and the event count move if and only if
// the stored text moved. Dolt 2.2 (the harness image) rounds integers past
// 2^53 in the column, so there the relation is one-sided: a moved text must
// come with a write; a write that left the text as it was is logged, not
// failed. A failure prints the seed.
func TestMetadataNumberEditsMatchTheServer(t *testing.T) {
	store, cleanup := setupConcurrentTestStore(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	db := serverSessionDB(t, store)
	twoSided, version := doltAtLeast(t, ctx, db, 2, 6)
	t.Logf("test Dolt server: dolt_version() = %s (relation %s)", version, map[bool]string{true: "two-sided", false: "one-sided"}[twoSided])

	const mpOracleSeeds = 60
	start := time.Now()
	writes, stays, loose, skipped := 0, 0, 0, 0
	for _, route := range mpRoutes(t, ctx, store) {
		id := "mp-oracle-" + route.name
		createPerm(t, ctx, store, id)
		for seed := int64(1); seed <= mpOracleSeeds; seed++ {
			rng := rand.New(rand.NewSource(seed))
			group := mpOracleNumbers[rng.Intn(len(mpOracleNumbers))]
			a, b := group[rng.Intn(len(group))], group[rng.Intn(len(group))]
			for _, flag := range []struct {
				name  string
				patch issueops.MetadataPatch
			}{
				{"--set-metadata", issueops.MetadataPatch{Set: map[string]json.RawMessage{"k": storage.MetadataEditValue(b)}}},
				{"--merge-metadata", issueops.MetadataPatch{MergePatch: []json.RawMessage{json.RawMessage(`{"k":` + b + `}`)}}},
			} {
				label := fmt.Sprintf("seed %d %s %s k=%s over %s", seed, route.name, flag.name, b, a)
				if _, err := db.ExecContext(ctx, "UPDATE issues SET metadata = ?, updated_at = '2001-01-01 00:00:00' WHERE id = ?", `{"k":`+a+`,"keep":"x"}`, id); err != nil {
					t.Logf("%s: the server refused the seed row: %v", label, err)
					skipped++
					continue
				}
				before := mpReadRow(t, ctx, db, id)
				if _, err := route.ops.Update(ctx, issueops.UpdateRequest{Actor: "oracle", IssueID: id, Patch: issueops.IssuePatch{Metadata: flag.patch}}); err != nil {
					t.Errorf("%s: %v", label, err)
					continue
				}
				after := mpReadRow(t, ctx, db, id)
				wrote, moved := before.wrote(after), before.metadata != after.metadata
				switch {
				case wrote == moved:
					if wrote {
						writes++
					} else {
						stays++
					}
				case moved:
					t.Errorf("%s: stored %s -> %s with no write (row version, updated_at, events unchanged)", label, before.metadata, after.metadata)
				case twoSided:
					t.Errorf("%s: wrote (version %s -> %s, events %+d) but the stored text stayed %s", label, before.lock, after.lock, after.events-before.events, after.metadata)
				default:
					loose++
					t.Logf("%s: Dolt %s: wrote, stored text stayed %s (one-sided relation, not a failure)", label, version, after.metadata)
				}
			}
		}
	}
	t.Logf("Dolt %s: %d seeds x 2 flags x 2 routes: %d writes moved the stored text, %d no-ops left it, %d writes left it (one-sided only), %d seed rows refused; %s relation held in %v",
		version, mpOracleSeeds, writes, stays, loose, skipped, map[bool]string{true: "two-sided", false: "one-sided"}[twoSided], time.Since(start).Round(time.Millisecond))
}

// TestMetadataReviewCasesLive commits review bd-ldmiy's live cases on the
// store and uow routes: a patch naming the raw byte e9 is refused with the
// U+FFFD member it would have removed kept and no event (finding 1);
// {"n":1.0} over {"n":1} and {"n":1e3} over {"n":1000} write nothing (finding
// 2); --set-metadata jira.sprint=0.10000000000000001 over 0.1 writes nothing
// (finding 3).
func TestMetadataReviewCasesLive(t *testing.T) {
	store, cleanup := setupConcurrentTestStore(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := serverSessionDB(t, store)
	for _, route := range mpRoutes(t, ctx, store) {
		t.Run(route.name, func(t *testing.T) {
			id := "mp-review-" + route.name
			createPerm(t, ctx, store, id)
			seed := func(metadata string) mpRowState {
				t.Helper()
				if _, err := db.ExecContext(ctx, "UPDATE issues SET metadata = ?, updated_at = '2001-01-01 00:00:00' WHERE id = ?", metadata, id); err != nil {
					t.Fatalf("seed %s: %v", metadata, err)
				}
				return mpReadRow(t, ctx, db, id)
			}
			t.Run("e9_name_refused", func(t *testing.T) {
				before := seed(`{"everturn":{"` + "�" + `":"keep","other":"keep"}}`)
				_, err := route.ops.Update(ctx, issueops.UpdateRequest{Actor: "review", IssueID: id, Patch: issueops.IssuePatch{
					Metadata: issueops.MetadataPatch{MergePatch: []json.RawMessage{json.RawMessage(`{"everturn":{"` + string([]byte{0xe9}) + `":null}}`)}},
				}})
				after := mpReadRow(t, ctx, db, id)
				if err == nil || !strings.Contains(err.Error(), storage.ErrMetadataMergePatchText.Error()) {
					t.Errorf("err = %v, want %q", err, storage.ErrMetadataMergePatchText)
				}
				if before != after {
					t.Errorf("row %+v -> %+v, want it unchanged with the U+FFFD member kept", before, after)
				}
				if !strings.Contains(after.metadata, "�") {
					t.Errorf("stored %s, want the U+FFFD member kept", after.metadata)
				}
			})
			for _, noop := range []struct {
				name, stored string
				patch        issueops.MetadataPatch
			}{
				{"1.0_over_1", `{"n":1}`, issueops.MetadataPatch{MergePatch: []json.RawMessage{json.RawMessage(`{"n":1.0}`)}}},
				{"1e3_over_1000", `{"n":1000}`, issueops.MetadataPatch{MergePatch: []json.RawMessage{json.RawMessage(`{"n":1e3}`)}}},
				{"legacy_set_0.10000000000000001_over_0.1", `{"jira.sprint":0.1}`, issueops.MetadataPatch{Set: map[string]json.RawMessage{"jira.sprint": storage.MetadataEditValue("0.10000000000000001")}}},
			} {
				t.Run(noop.name, func(t *testing.T) {
					before := seed(noop.stored)
					result, err := route.ops.Update(ctx, issueops.UpdateRequest{Actor: "review", IssueID: id, Patch: issueops.IssuePatch{Metadata: noop.patch}})
					after := mpReadRow(t, ctx, db, id)
					if err != nil {
						t.Fatalf("update: %v", err)
					}
					if result.Changed || before != after {
						t.Errorf("Changed = %v, row %+v -> %+v; want nothing written", result.Changed, before, after)
					}
				})
			}
		})
	}
}
