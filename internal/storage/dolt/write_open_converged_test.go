package dolt

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage/schema"
)

// The writable server-mode open asks schema.AlreadyConverged on its main pool
// before it opens the one-off migration pool and runs the migrate gate
// (bd-lbrt4, bd-7c0u4 rank 2). These tests count the open's TCP connections
// through the budget proxy (startup_budget_test.go): a converged open makes
// two (the preflight dial and the pooled session), and any open that takes
// the migration path makes a third, the migration pool.

// writeOpenConvergedFixture is a converged database reached through a
// recording proxy, with a direct store on it for setup and inspection.
type writeOpenConvergedFixture struct {
	f      *identityTestFixture
	rec    *openRecorder
	port   int
	direct *DoltStore
}

func newWriteOpenConvergedFixture(t *testing.T, ctx context.Context, prefix string) *writeOpenConvergedFixture {
	t.Helper()
	skipIfNoDolt(t)
	acquireTestSlot()
	t.Cleanup(releaseTestSlot)

	f := newIdentityTestFixture(t, prefix)
	direct, err := New(ctx, f.config())
	if err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { _ = direct.Close() })

	rec := &openRecorder{}
	port := startBudgetProxy(t, rec, net.JoinHostPort("127.0.0.1", strconv.Itoa(testServerPort)))
	return &writeOpenConvergedFixture{f: f, rec: rec, port: port, direct: direct}
}

// openWritable opens the database writable through the proxy and returns the
// open's connection count and commands. The store is closed before return.
func (x *writeOpenConvergedFixture) openWritable(ctx context.Context) (int, []string, error) {
	cfg := x.f.config()
	cfg.CreateIfMissing = false
	cfg.ServerHost = "127.0.0.1"
	cfg.ServerPort = x.port

	x.rec.start()
	store, err := New(ctx, cfg)
	conns, cmds := x.rec.stop()
	if err == nil {
		_ = store.Close()
	}
	return conns, cmds, err
}

// sawCommand reports whether the open in flight has sent a command containing
// sub so far.
func (r *openRecorder) sawCommand(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.cmds {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

func hasCommand(cmds []string, sub string) bool {
	for _, c := range cmds {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

func tableExists(t *testing.T, ctx context.Context, db *sql.DB, table string) bool {
	t.Helper()
	rows, err := db.QueryContext(ctx, "SHOW TABLES LIKE '"+table+"'")
	if err != nil {
		t.Fatalf("probe table %s: %v", table, err)
	}
	defer rows.Close()
	return rows.Next()
}

func mainCursor(t *testing.T, ctx context.Context, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&v); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	return v
}

// TestWriteOpenConvergedSkipsMigrationPool: a writable open of a converged
// database opens no migration pool and sends no gate statement or migration
// lock; the convergence pass runs on the pooled session instead.
func TestWriteOpenConvergedSkipsMigrationPool(t *testing.T) {
	ctx, cancel := testContext(t)
	defer cancel()
	x := newWriteOpenConvergedFixture(t, ctx, "woc-skip")

	conns, cmds, err := x.openWritable(ctx)
	if err != nil {
		t.Fatalf("writable open: %v", err)
	}
	list := strings.Join(cmds, "\n")
	if conns != 2 {
		t.Errorf("converged writable open made %d connections, want 2 (no migration pool). Commands:\n%s", conns, list)
	}
	if hasCommand(cmds, "c3 ") {
		t.Errorf("converged writable open sent commands on a third connection:\n%s", list)
	}
	if hasCommand(cmds, "GET_LOCK(") {
		t.Errorf("converged writable open took the migration lock:\n%s", list)
	}
	if !hasCommand(cmds, "c2 QUERY SELECT IS_FREE_LOCK(") {
		t.Errorf("convergence pass (IS_FREE_LOCK) not on the pooled session:\n%s", list)
	}
}

// TestWriteOpenConvergedRepairsSchemaWork: a database with schema work still
// takes the migration path (a third connection and the migration lock), and
// the open repairs it.
func TestWriteOpenConvergedRepairsSchemaWork(t *testing.T) {
	t.Run("dropped fork index", func(t *testing.T) {
		ctx, cancel := testContext(t)
		defer cancel()
		x := newWriteOpenConvergedFixture(t, ctx, "woc-idx")
		db := x.direct.db
		// Committed, as fork_indexes_test.go does: the step skips a dirty
		// issues table.
		for _, q := range []string{
			"ALTER TABLE issues DROP INDEX idx_issues_closed_at",
			"CALL DOLT_ADD('issues')",
			"CALL DOLT_COMMIT('-m', 'test: drop idx_issues_closed_at')",
		} {
			if _, err := db.ExecContext(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}

		conns, cmds, err := x.openWritable(ctx)
		if err != nil {
			t.Fatalf("writable open: %v", err)
		}
		assertMigrationPath(t, conns, cmds)
		if !issuesIndexExists(t, ctx, db, "idx_issues_closed_at") {
			t.Errorf("idx_issues_closed_at not recreated by the writable open")
		}
	})

	t.Run("dropped wisp_dependencies", func(t *testing.T) {
		ctx, cancel := testContext(t)
		defer cancel()
		x := newWriteOpenConvergedFixture(t, ctx, "woc-wisp")
		db := x.direct.db
		// dolt_ignored: a drop leaves no working-set change to commit.
		if _, err := db.ExecContext(ctx, "DROP TABLE wisp_dependencies"); err != nil {
			t.Fatalf("drop wisp_dependencies: %v", err)
		}

		conns, cmds, err := x.openWritable(ctx)
		if err != nil {
			t.Fatalf("writable open: %v", err)
		}
		assertMigrationPath(t, conns, cmds)
		if !tableExists(t, ctx, db, "wisp_dependencies") {
			t.Errorf("wisp_dependencies not recreated by the writable open")
		}
	})

	t.Run("main cursor one version back", func(t *testing.T) {
		ctx, cancel := testContext(t)
		defer cancel()
		// The test server is not a bd-managed server for this workspace, so
		// the open runs the shared-store gate: with no remote it refuses a
		// pending migration until the operator consents (#5920).
		t.Setenv(schema.AllowRemoteMigrateEnv, "0")
		t.Cleanup(func() { schema.SetSharedMigrateConsent(false) })
		x := newWriteOpenConvergedFixture(t, ctx, "woc-cursor")
		db := x.direct.db
		latest := schema.LatestVersion()
		if _, err := db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version = ?", latest); err != nil {
			t.Fatalf("regress schema_migrations: %v", err)
		}

		// Without consent the gate still runs, on the migration pool, and
		// refuses.
		conns, cmds, err := x.openWritable(ctx)
		if err == nil || !schema.IsRemoteMigrateGateError(err) {
			t.Fatalf("writable open one version back without consent: err = %v, want the migrate gate's refusal", err)
		}
		if conns < 3 {
			t.Errorf("gate refusal after %d connections, want the migration pool (3). Commands:\n%s", conns, strings.Join(cmds, "\n"))
		}
		if got := mainCursor(t, ctx, db); got != latest-1 {
			t.Errorf("main cursor after the refusal = %d, want %d", got, latest-1)
		}

		// With consent the open migrates.
		schema.SetSharedMigrateConsent(true)
		conns, cmds, err = x.openWritable(ctx)
		if err != nil {
			t.Fatalf("writable open with consent: %v", err)
		}
		assertMigrationPath(t, conns, cmds)
		if got := mainCursor(t, ctx, db); got != latest {
			t.Errorf("main cursor after the consented open = %d, want %d", got, latest)
		}
	})
}

// assertMigrationPath fails unless the open opened the migration pool and
// took the migration lock.
func assertMigrationPath(t *testing.T, conns int, cmds []string) {
	t.Helper()
	list := strings.Join(cmds, "\n")
	if conns < 3 {
		t.Errorf("open with schema work made %d connections, want the migration pool (3 or more). Commands:\n%s", conns, list)
	}
	if !hasCommand(cmds, "QUERY SELECT GET_LOCK(") {
		t.Errorf("open with schema work did not take the migration lock. Commands:\n%s", list)
	}
}

// TestWriteOpenConvergedWaitsOnHeldMigrationLock: while another session holds
// the migration lock (a peer midway through a migration pass), a writable open
// of a converged database does not take the shortcut: it opens the migration
// pool and queues on the lock.
func TestWriteOpenConvergedWaitsOnHeldMigrationLock(t *testing.T) {
	ctx, cancel := testContext(t)
	defer cancel()
	x := newWriteOpenConvergedFixture(t, ctx, "woc-lock")

	holder, err := x.direct.db.Conn(ctx)
	if err != nil {
		t.Fatalf("pin holder connection: %v", err)
	}
	defer func() { _ = holder.Close() }()
	lockName := schema.MigrationLockName(x.f.dbName)
	var got sql.NullInt64
	if err := holder.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", lockName).Scan(&got); err != nil || !got.Valid || got.Int64 != 1 {
		t.Fatalf("hold %s: got %v, err %v", lockName, got, err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		var freed sql.NullInt64
		if err := holder.QueryRowContext(ctx, "SELECT RELEASE_LOCK(?)", lockName).Scan(&freed); err != nil {
			t.Errorf("release %s: %v", lockName, err)
		}
	}
	defer release()

	type result struct {
		conns int
		cmds  []string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		conns, cmds, err := x.openWritable(ctx)
		done <- result{conns, cmds, err}
	}()

	// Release the lock once the open has queued on it, or report the
	// shortcut if the open returns first.
	deadline := time.After(30 * time.Second)
	var res result
wait:
	for {
		select {
		case res = <-done:
			break wait
		case <-deadline:
			release()
			res = <-done
			t.Errorf("open never sent GET_LOCK within 30s")
			break wait
		case <-time.After(20 * time.Millisecond):
			if x.rec.sawCommand("QUERY SELECT GET_LOCK(") {
				release()
				res = <-done
				break wait
			}
		}
	}
	list := strings.Join(res.cmds, "\n")
	if res.err != nil {
		t.Fatalf("writable open: %v. Commands:\n%s", res.err, list)
	}
	if res.conns < 3 {
		t.Errorf("open with the migration lock held made %d connections, want the migration pool (3). Commands:\n%s", res.conns, list)
	}
	if !hasCommand(res.cmds, "QUERY SELECT GET_LOCK(") {
		t.Errorf("open with the migration lock held did not queue on it. Commands:\n%s", list)
	}
}

// proxyHooks numbers every connection through a hooked proxy (from 1, open or
// not), runs the test's hooks before a connection dials upstream and before a
// command is forwarded, and records which connections the client has closed.
type proxyHooks struct {
	mu            sync.Mutex
	next          int
	closed        map[int]bool
	beforeConnect func(id int)
	beforeQuery   func(id int, desc string)
}

func (h *proxyHooks) accept() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	return h.next
}

func (h *proxyHooks) setBeforeQuery(fn func(id int, desc string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.beforeQuery = fn
}

func (h *proxyHooks) hooks() (func(int), func(int, string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.beforeConnect, h.beforeQuery
}

func (h *proxyHooks) markClosed(id int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed == nil {
		h.closed = make(map[int]bool)
	}
	h.closed[id] = true
}

// waitClosed reports whether connection id is closed within d.
func (h *proxyHooks) waitClosed(id int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		h.mu.Lock()
		closed := h.closed[id]
		h.mu.Unlock()
		if closed || time.Now().After(deadline) {
			return closed
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startHookedProxy is startBudgetProxy with proxyHooks: it records into rec
// as the budget proxy does, and its hooks run in the connection's goroutine,
// so a hook that blocks holds that connection's next command.
func startHookedProxy(t *testing.T, rec *openRecorder, h *proxyHooks, upstream string) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			go hookedProxyConn(client, h.accept(), rec.accept(), rec, h, upstream)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func hookedProxyConn(client net.Conn, id, recID int, rec *openRecorder, h *proxyHooks, upstream string) {
	defer h.markClosed(id)
	defer func() { _ = client.Close() }()
	if beforeConnect, _ := h.hooks(); beforeConnect != nil {
		beforeConnect(id)
	}
	server, err := net.Dial("tcp", upstream)
	if err != nil {
		return
	}
	defer func() { _ = server.Close() }()
	go func() {
		_, _ = io.Copy(client, server)
		_ = client.Close()
	}()
	for {
		seq, payload, raw, err := readBudgetPacket(client)
		if err != nil {
			return
		}
		if seq == 0 && len(payload) > 0 && payload[0] != 0x01 {
			name, ok := budgetCommandNames[payload[0]]
			if !ok {
				name = fmt.Sprintf("CMD_%#x", payload[0])
			}
			desc := name
			if payload[0] == 0x02 || payload[0] == 0x03 || payload[0] == 0x16 {
				desc += " " + strings.Join(strings.Fields(string(payload[1:])), " ")
			}
			if _, beforeQuery := h.hooks(); beforeQuery != nil {
				beforeQuery(id, desc)
			}
			rec.command(recID, desc)
		}
		if _, err := server.Write(raw); err != nil {
			return
		}
	}
}

// TestWriteOpenConvergedDiscardsFailedProbeSession: when a convergence probe
// statement fails on the main pool's session, that session is discarded, not
// returned to the pool (review bd-zxrmq). A Dolt session that sent a failing
// statement stays pinned to its pre-statement catalog (be-bv7x), so a pooled
// survivor would fail the store's first read after a fallback open that found
// nothing to apply.
func TestWriteOpenConvergedDiscardsFailedProbeSession(t *testing.T) {
	t.Run("table restored before the fallback", func(t *testing.T) {
		// The reviewer's fixture: custom_types is renamed away while the open
		// runs its start-up reads, so the probe's SELECT COUNT(*) FROM
		// custom_types fails on the pooled session; it is renamed back before
		// the migration pool connects, which then finds nothing to do.
		ctx, cancel := testContext(t)
		defer cancel()
		x := newWriteOpenConvergedFixture(t, ctx, "woc-restore")
		if _, err := x.direct.db.ExecContext(ctx, "INSERT INTO custom_types (name) VALUES ('review-kind')"); err != nil {
			t.Fatalf("seed custom type: %v", err)
		}

		renamed := make(chan error, 1)
		restored := make(chan error, 1)
		didRename := false
		h := &proxyHooks{
			beforeQuery: func(id int, desc string) {
				if id == 2 && !didRename && desc == "QUERY SELECT COALESCE(MAX(version), 0) FROM schema_migrations" {
					didRename = true
					_, err := x.direct.db.ExecContext(ctx, "RENAME TABLE custom_types TO review_custom_types")
					renamed <- err
				}
			},
			beforeConnect: func(id int) {
				if id == 3 {
					_, err := x.direct.db.ExecContext(ctx, "RENAME TABLE review_custom_types TO custom_types")
					restored <- err
				}
			},
		}
		port := startHookedProxy(t, x.rec, h, net.JoinHostPort("127.0.0.1", strconv.Itoa(testServerPort)))

		cfg := x.f.config()
		cfg.CreateIfMissing = false
		cfg.ServerHost = "127.0.0.1"
		cfg.ServerPort = port
		cfg.MaxOpenConns = 1 // the store's next read reuses the pooled session, if one survived
		x.rec.start()
		store, err := New(ctx, cfg)
		conns, cmds := x.rec.stop()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer func() { _ = store.Close() }()
		list := strings.Join(cmds, "\n")
		for what, ch := range map[string]chan error{"rename custom_types away": renamed, "restore custom_types": restored} {
			select {
			case err := <-ch:
				if err != nil {
					t.Fatalf("%s: %v", what, err)
				}
			default:
				t.Fatalf("fixture did not %s during the open. Commands:\n%s", what, list)
			}
		}
		if conns != 3 || !hasCommand(cmds, "c2 QUERY SELECT COUNT(*) FROM custom_types") {
			t.Fatalf("want the probe's custom_types read on c2 and the migration pool (3 connections), got %d. Commands:\n%s", conns, list)
		}

		rows, err := store.QueryContext(ctx, "SELECT name FROM custom_types")
		if err != nil {
			t.Fatalf("first DoltStore.QueryContext after the open: %v", err)
		}
		var names []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatalf("scan custom type: %v", err)
			}
			names = append(names, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read custom types: %v", err)
		}
		_ = rows.Close()
		if len(names) != 1 || names[0] != "review-kind" {
			t.Errorf("first DoltStore.QueryContext read %v, want [review-kind]", names)
		}
		if !h.waitClosed(2, 5*time.Second) {
			t.Errorf("the probe's session (c2, from before the restore) is still open: it went back to the pool")
		}
	})

	t.Run("probe cut by a context deadline", func(t *testing.T) {
		ctx, cancel := testContext(t)
		defer cancel()
		x := newWriteOpenConvergedFixture(t, ctx, "woc-deadline")
		h := &proxyHooks{}
		port := startHookedProxy(t, x.rec, h, net.JoinHostPort("127.0.0.1", strconv.Itoa(testServerPort)))

		cfg := x.f.config()
		cfg.CreateIfMissing = false
		cfg.ServerHost = "127.0.0.1"
		cfg.ServerPort = port
		cfg.MaxOpenConns = 1
		store, err := New(ctx, cfg)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer func() { _ = store.Close() }()
		if open := store.db.Stats().OpenConnections; open != 1 {
			t.Fatalf("converged open left %d pooled sessions, want 1", open)
		}

		// Hold the probe's last statement past a short deadline.
		h.setBeforeQuery(func(id int, desc string) {
			if strings.HasPrefix(desc, "QUERY SELECT IS_FREE_LOCK(") {
				time.Sleep(2 * time.Second)
			}
		})
		short, cancelShort := context.WithTimeout(ctx, 300*time.Millisecond)
		converged := store.writeOpenConverged(short)
		cancelShort()
		h.setBeforeQuery(nil)
		if converged {
			t.Fatalf("writeOpenConverged under a cut deadline = true, want false")
		}
		if open := store.db.Stats().OpenConnections; open != 0 {
			t.Errorf("after the cut probe the pool holds %d sessions, want 0", open)
		}
		if !h.waitClosed(2, 5*time.Second) {
			t.Errorf("the cut probe's session (c2) is still open")
		}
		var one int
		if err := store.db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
			t.Errorf("read after the cut probe: %d, %v", one, err)
		}
	})
}
