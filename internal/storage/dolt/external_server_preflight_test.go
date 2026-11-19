package dolt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

// newServerMode dials a server and reads its greeting before opening the
// driver, to fail fast when it is down. For a remote TCP server that dial is
// a connection that never authenticates (bd-7c0u4 rank 7, GH#5273 item 2,
// ported from gastownhall/beads#5993), so it is skipped and the driver's
// connect timeout reports a dead host. These tests pin which opens dial, that
// a remote open makes no handshake-only connection, and that a dead remote
// host still gets the "Dolt server unreachable" error and one breaker failure.

func TestExternalServerPreflightNeedsPreflight(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		cfg  Config
		want bool
	}{
		{"remote IPv4 writable", Config{ServerHost: "100.122.24.50", ServerPort: 3309}, false},
		{"remote hostname read-only", Config{ServerHost: "dolt-db", ServerPort: 3307, ReadOnly: true}, false},
		{"empty host", Config{ServerHost: "", ServerPort: 3307}, true},
		{"loopback IPv4", Config{ServerHost: "127.0.0.1", ServerPort: 3307}, true},
		{"localhost any case", Config{ServerHost: "LOCALHOST", ServerPort: 3307}, true},
		{"loopback IPv6", Config{ServerHost: "::1", ServerPort: 3307}, true},
		{"bracketed IPv6", Config{ServerHost: "[::1]", ServerPort: 3307}, true},
		{"all interfaces", Config{ServerHost: "0.0.0.0", ServerPort: 3307}, true},
		{"socket on a remote host", Config{ServerHost: "100.122.24.50", ServerPort: 3309, ServerSocket: "/tmp/dolt.sock"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := serverConnectionNeedsPreflight(&tt.cfg)
			if got != tt.want {
				t.Fatalf("serverConnectionNeedsPreflight(%+v) = %t, want %t", tt.cfg, got, tt.want)
			}
			// A skipped dial must never be a local host: auto-start
			// (serverOpenCanAutoStart) keys on isLocalHost.
			if !got && isLocalHost(tt.cfg.ServerHost) {
				t.Fatalf("preflight skipped for local host %q", tt.cfg.ServerHost)
			}
		})
	}
}

func TestExternalServerPreflightDialSkipsRemoteTCP(t *testing.T) {
	t.Parallel()

	cfg := &Config{ServerHost: "100.122.24.50", ServerPort: 3309}
	conn, addr, ran, err := dialServerPreflight(cfg)
	if err != nil || ran || conn != nil {
		if conn != nil {
			_ = conn.Close()
		}
		t.Fatalf("dialServerPreflight(remote) = conn %v, ran %t, err %v; want no dial", conn, ran, err)
	}
	if want := net.JoinHostPort(cfg.ServerHost, "3309"); addr != want {
		t.Fatalf("dialServerPreflight addr = %q, want %q", addr, want)
	}
}

// handshakeCounter is a TCP proxy to the test Dolt server that records, per
// accepted connection, whether the client sent any byte before it closed. A
// connection closed with none sent is a handshake-only connection: the
// client read the greeting and never answered it.
type handshakeCounter struct {
	mu       sync.Mutex
	accepted int
	closed   int
	silent   int
}

func startHandshakeCounter(t *testing.T, upstream string) (*handshakeCounter, int) {
	t.Helper()
	hc := &handshakeCounter{}
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
			hc.mu.Lock()
			hc.accepted++
			hc.mu.Unlock()
			go hc.proxy(client, upstream)
		}
	}()
	return hc, ln.Addr().(*net.TCPAddr).Port
}

func (hc *handshakeCounter) proxy(client net.Conn, upstream string) {
	defer func() { _ = client.Close() }()
	var sent int64
	defer func() {
		hc.mu.Lock()
		hc.closed++
		if sent == 0 {
			hc.silent++
		}
		hc.mu.Unlock()
	}()
	server, err := net.Dial("tcp", upstream)
	if err != nil {
		return
	}
	defer func() { _ = server.Close() }()
	go func() {
		_, _ = io.Copy(client, server)
		_ = client.Close()
	}()
	sent, _ = io.Copy(server, client)
}

func (hc *handshakeCounter) counts() (accepted, closed, silent int) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	return hc.accepted, hc.closed, hc.silent
}

// waitClosed waits until every accepted connection has closed.
func (hc *handshakeCounter) waitClosed(t *testing.T) (accepted, silent int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		a, c, s := hc.counts()
		if a == c {
			return a, s
		}
		if time.Now().After(deadline) {
			t.Fatalf("proxy: %d of %d connections still open after the store closed", a-c, a)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// treatHostAsExternal makes every host count as remote for the preflight
// decision and the hint, so a loopback listener stands in for a remote
// server. It changes a package var: callers must not run in parallel.
func treatHostAsExternal(t *testing.T) {
	t.Helper()
	prev := serverHostIsExternal
	serverHostIsExternal = func(string) bool { return true }
	t.Cleanup(func() { serverHostIsExternal = prev })
}

// openThroughCounter creates the fixture's database directly, then opens it
// through a handshake counter and closes it again, and returns how many
// connections the open made and how many were handshake-only.
func openThroughCounter(t *testing.T, name string, external bool) (accepted, silent int) {
	t.Helper()
	skipIfNoDolt(t)
	acquireTestSlot()
	t.Cleanup(releaseTestSlot)
	ctx, cancel := testContext(t)
	defer cancel()

	f := newIdentityTestFixture(t, name)
	setup, err := New(ctx, f.config())
	if err != nil {
		t.Fatalf("create database: %v", err)
	}
	if err := setup.Close(); err != nil {
		t.Fatalf("close setup store: %v", err)
	}

	hc, port := startHandshakeCounter(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(testServerPort)))
	if external {
		treatHostAsExternal(t)
	}
	cfg := f.config()
	cfg.CreateIfMissing = false
	cfg.ReadOnly = true
	cfg.ServerHost = "127.0.0.1"
	cfg.ServerPort = port
	store, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("open through proxy: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	return hc.waitClosed(t)
}

// TestExternalServerPreflightRemoteOpenHasNoHandshakeOnlyConnection: an open
// of a remote server connects only through the driver, so every connection
// it makes authenticates.
func TestExternalServerPreflightRemoteOpenHasNoHandshakeOnlyConnection(t *testing.T) {
	accepted, silent := openThroughCounter(t, "preflight-remote", true)
	if accepted == 0 {
		t.Fatalf("remote open made no connection through the proxy")
	}
	if silent != 0 {
		t.Fatalf("remote open made %d handshake-only connection(s) of %d; want 0 (the preflight dial must be skipped)", silent, accepted)
	}
	t.Logf("remote open: %d connection(s), none handshake-only", accepted)
}

// TestExternalServerPreflightLocalOpenKeepsTheDial: a local server keeps its
// preflight dial, the one handshake-only connection of the open.
func TestExternalServerPreflightLocalOpenKeepsTheDial(t *testing.T) {
	accepted, silent := openThroughCounter(t, "preflight-local", false)
	if silent != 1 {
		t.Fatalf("local open made %d handshake-only connection(s) of %d; want exactly 1 (the preflight dial)", silent, accepted)
	}
	t.Logf("local open: %d connection(s), 1 handshake-only", accepted)
}

// TestExternalServerPreflightDeadRemoteHost: with no preflight dial, a remote
// host that refuses connections still fails the open with the error that
// begins "Dolt server unreachable at <addr>" and carries the external-server
// hint, and the circuit breaker records one failure.
func TestExternalServerPreflightDeadRemoteHost(t *testing.T) {
	t.Setenv("BEADS_TEST_MODE", "")
	treatHostAsExternal(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	breaker := newTestCircuitBreakerOnPort(t, port)
	prevNew, prevClean := newServerCircuitBreaker, cleanServerCircuitState
	newServerCircuitBreaker = func(string, int, string) *circuitBreaker { return breaker }
	cleanServerCircuitState = func() {}
	t.Cleanup(func() { newServerCircuitBreaker, cleanServerCircuitState = prevNew, prevClean })

	dir := t.TempDir()
	ctx, cancel := testContext(t)
	defer cancel()
	cfg := &Config{
		Path:       dir + "/dolt",
		BeadsDir:   dir,
		Database:   "beads_preflight_dead",
		ServerHost: "127.0.0.1",
		ServerPort: port,
		ReadOnly:   true,
	}
	start := time.Now()
	store, err := newServerMode(ctx, cfg)
	elapsed := time.Since(start)
	if err == nil {
		_ = store.Close()
		t.Fatalf("open of a dead remote host succeeded")
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if want := "Dolt server unreachable at " + addr; !strings.HasPrefix(err.Error(), want) {
		t.Errorf("error = %q, want prefix %q", err, want)
	}
	if want := "Verify the external server is running and reachable from this host"; !strings.Contains(err.Error(), want) {
		t.Errorf("error lacks the external-server hint %q: %v", want, err)
	}
	if got := breaker.readState().Failures; got != 1 {
		t.Errorf("breaker failures = %d, want 1", got)
	}
	t.Logf("dead remote host refused the open in %s", elapsed)
}

func TestExternalServerPreflightConnectionFailureTripsBreaker(t *testing.T) {
	t.Setenv("BEADS_TEST_MODE", "")
	breaker := newTestCircuitBreaker(t)
	for range circuitFailureThreshold - 1 {
		breaker.RecordFailure()
	}

	cfg := &Config{ServerHost: "100.122.24.50", ServerPort: 3309}
	err := recordSkippedPreflightResult(cfg, breaker, errors.New("dial tcp: i/o timeout"))
	if err == nil {
		t.Fatal("recordSkippedPreflightResult returned nil for a connection failure")
	}
	for _, want := range []string{"Dolt server unreachable at 100.122.24.50:3309", "nc -zv 100.122.24.50 3309"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("connection error missing %q: %v", want, err)
		}
	}
	if got := breaker.State(); got != circuitOpen {
		t.Fatalf("circuit state = %q, want %q", got, circuitOpen)
	}
}

func TestExternalServerPreflightSemanticFailureDoesNotTrip(t *testing.T) {
	t.Setenv("BEADS_TEST_MODE", "")
	breaker := newTestCircuitBreaker(t)
	for range circuitFailureThreshold - 1 {
		breaker.RecordFailure()
	}

	wantErr := errors.New("Error 1045: access denied")
	gotErr := recordSkippedPreflightResult(&Config{ServerHost: "100.122.24.50", ServerPort: 3309}, breaker, wantErr)
	if !errors.Is(gotErr, wantErr) {
		t.Fatalf("semantic error = %v, want original %v", gotErr, wantErr)
	}
	if strings.Contains(gotErr.Error(), "Dolt server unreachable") {
		t.Fatalf("semantic error was rewritten as a reachability failure: %v", gotErr)
	}
	if got := breaker.State(); got != circuitClosed {
		t.Fatalf("circuit state = %q, want %q", got, circuitClosed)
	}
}

func TestExternalServerPreflightSuccessResetsBreaker(t *testing.T) {
	t.Setenv("BEADS_TEST_MODE", "")
	breaker := newTestCircuitBreaker(t)
	for range circuitFailureThreshold - 1 {
		breaker.RecordFailure()
	}
	if err := recordSkippedPreflightResult(&Config{ServerHost: "100.122.24.50", ServerPort: 3309}, breaker, nil); err != nil {
		t.Fatalf("success result returned error: %v", err)
	}
	for range circuitFailureThreshold - 1 {
		breaker.RecordFailure()
	}
	if got := breaker.State(); got != circuitClosed {
		t.Fatalf("circuit state after reset and sub-threshold failures = %q, want %q", got, circuitClosed)
	}
}

// A skipped preflight dial leaves a dead remote host to the driver, and
// openServerConnection used to pay for it twice: the fast-path ping, then the
// no-database init connection, each up to the driver's 5 s connect timeout
// (bd-wy35k measured a blackholed host at 10 s). With the preflight skipped, a
// failure to reach the host now returns after the first attempt (bd-3sqfm).
// The tests below use a listener that drops every connection at once, so no
// test waits on a timeout.

// startDroppingListener accepts TCP connections and closes each one before
// any greeting, counting them. It returns the count reader and the port.
func startDroppingListener(t *testing.T) (func() int, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var mu sync.Mutex
	accepted := 0
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			accepted++
			mu.Unlock()
			_ = c.Close()
		}
	}()
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return accepted
	}, ln.Addr().(*net.TCPAddr).Port
}

// stubServerBreaker makes newServerMode use a test breaker for port, with no
// sweep of real breaker files.
func stubServerBreaker(t *testing.T, port int) *circuitBreaker {
	t.Helper()
	breaker := newTestCircuitBreakerOnPort(t, port)
	prevNew, prevClean := newServerCircuitBreaker, cleanServerCircuitState
	newServerCircuitBreaker = func(string, int, string) *circuitBreaker { return breaker }
	cleanServerCircuitState = func() {}
	t.Cleanup(func() { newServerCircuitBreaker, cleanServerCircuitState = prevNew, prevClean })
	return breaker
}

func openDeadServer(t *testing.T, host string, port int, database string) error {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := testContext(t)
	defer cancel()
	store, err := newServerMode(ctx, &Config{
		Path:       dir + "/dolt",
		BeadsDir:   dir,
		Database:   database,
		ServerHost: host,
		ServerPort: port,
		ReadOnly:   true,
	})
	if err == nil {
		_ = store.Close()
		t.Fatalf("open of a dead server at %s:%d succeeded", host, port)
	}
	return err
}

func assertUnreachable(t *testing.T, err error, addr string, breaker *circuitBreaker) {
	t.Helper()
	if want := "Dolt server unreachable at " + addr; !strings.HasPrefix(err.Error(), want) {
		t.Errorf("error = %q, want prefix %q", err, want)
	}
	if want := "Verify the external server is running and reachable from this host"; !strings.Contains(err.Error(), want) {
		t.Errorf("error lacks the external-server hint %q: %v", want, err)
	}
	if got := breaker.readState().Failures; got != 1 {
		t.Errorf("breaker failures = %d, want 1", got)
	}
}

// TestExternalServerPreflightDeadRemoteHostConnectsOnce: a remote host that
// drops every connection costs one connect attempt per open, not two, and
// still fails with the unreachable error, its hint and one breaker failure.
func TestExternalServerPreflightDeadRemoteHostConnectsOnce(t *testing.T) {
	t.Setenv("BEADS_TEST_MODE", "")
	treatHostAsExternal(t)
	accepted, port := startDroppingListener(t)
	breaker := stubServerBreaker(t, port)

	err := openDeadServer(t, "127.0.0.1", port, "beads_preflight_drop")
	if got := accepted(); got != 1 {
		t.Errorf("remote open of a dropping host made %d connection(s), want 1 (no init connection after an unreachable fast-path ping)", got)
	}
	assertUnreachable(t, err, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), breaker)
}

// TestExternalServerPreflightDeadLocalHostKeepsTheSecondAttempt: the same
// dropping listener as a local host keeps today's attempts: the preflight
// dial, the fast-path ping and the init connection.
func TestExternalServerPreflightDeadLocalHostKeepsTheSecondAttempt(t *testing.T) {
	t.Setenv("BEADS_TEST_MODE", "")
	accepted, port := startDroppingListener(t)
	_ = stubServerBreaker(t, port)

	_ = openDeadServer(t, "127.0.0.1", port, "beads_preflight_drop_local")
	if got := accepted(); got != 3 {
		t.Errorf("local open of a dropping host made %d connection(s), want 3 (preflight dial, fast-path ping, init connection)", got)
	}
}

// TestExternalServerPreflightUnresolvableRemoteHost: a remote host name that
// does not resolve is an unreachable server, as the preflight dial reported
// it: the unreachable error with the hint and one breaker failure, not the
// init connection's "failed to check if database exists".
func TestExternalServerPreflightUnresolvableRemoteHost(t *testing.T) {
	t.Setenv("BEADS_TEST_MODE", "")
	const host, port = "bad..host", 3399
	breaker := stubServerBreaker(t, port)

	err := openDeadServer(t, host, port, "beads_preflight_dns")
	assertUnreachable(t, err, net.JoinHostPort(host, strconv.Itoa(port)), breaker)
	if strings.Contains(err.Error(), "failed to check if database") {
		t.Errorf("unresolvable host reached the init connection: %v", err)
	}
}

// TestExternalServerPreflightRemoteMissingDatabaseReachesInit: a remote
// server that answers but lacks the database still gets the init connection:
// without CreateIfMissing the open returns today's missing-database error,
// and with it the database is created.
func TestExternalServerPreflightRemoteMissingDatabaseReachesInit(t *testing.T) {
	skipIfNoDolt(t)
	acquireTestSlot()
	t.Cleanup(releaseTestSlot)
	ctx, cancel := testContext(t)
	defer cancel()
	treatHostAsExternal(t)

	f := newIdentityTestFixture(t, "preflight-missing")
	hc, port := startHandshakeCounter(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(testServerPort)))
	cfg := f.config()
	cfg.CreateIfMissing = false
	cfg.ServerHost = "127.0.0.1"
	cfg.ServerPort = port
	store, err := New(ctx, cfg)
	if err == nil {
		_ = store.Close()
		t.Fatalf("open of a missing database without CreateIfMissing succeeded")
	}
	if want := "database \"" + f.dbName + "\" not found on Dolt server"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want it to contain %q", err, want)
	}
	if strings.Contains(err.Error(), "Dolt server unreachable") {
		t.Errorf("missing database reported as an unreachable server: %v", err)
	}
	if accepted, _ := hc.waitClosed(t); accepted != 2 {
		t.Errorf("missing-database open made %d connection(s), want 2 (fast-path ping, init connection)", accepted)
	}

	cfg = f.config()
	cfg.ServerHost = "127.0.0.1"
	cfg.ServerPort = port
	store, err = New(ctx, cfg)
	if err != nil {
		t.Fatalf("open with CreateIfMissing: %v", err)
	}
	_ = store.Close()
}

// A server that answers with an error packet is reachable, even when the
// message quotes a phrase isConnectionError matches: Access denied for user
// 'lost connection' is a wrong login, not an outage (review bd-wi3uv). On a
// skipped preflight such an error stays semantic: no "Dolt server
// unreachable", no breaker failure, and the init connection still runs.

// TestExternalServerPreflightTypedErrorStaysSemantic: a typed MySQL error,
// bare or wrapped, passes through recordSkippedPreflightResult unchanged and
// leaves a breaker one failure short of its threshold closed, whichever
// isConnectionError phrase its message holds.
func TestExternalServerPreflightTypedErrorStaysSemantic(t *testing.T) {
	t.Setenv("BEADS_TEST_MODE", "")
	cfg := &Config{ServerHost: "100.122.24.50", ServerPort: 3309}
	messages := []string{"Access denied for user 'lost connection'@'%'"}
	for _, phrase := range []string{"connection refused", "connection reset", "broken pipe", "i/o timeout", "bad connection", "invalid connection", "gone away"} {
		messages = append(messages, "Access denied for user '"+phrase+"'@'%'")
	}
	for _, msg := range messages {
		typed := &mysql.MySQLError{Number: 1045, SQLState: [5]byte{'2', '8', '0', '0', '0'}, Message: msg}
		for _, c := range []struct {
			name string
			err  error
		}{
			{"bare", typed},
			{"wrapped", fmt.Errorf("failed to check if database exists: %w", typed)},
		} {
			t.Run(msg+"/"+c.name, func(t *testing.T) {
				breaker := newTestCircuitBreaker(t)
				for range circuitFailureThreshold - 1 {
					breaker.RecordFailure()
				}
				if skippedPreflightUnreachable(c.err) {
					t.Errorf("skippedPreflightUnreachable(%v) = true, want false for a typed server error", c.err)
				}
				got := recordSkippedPreflightResult(cfg, breaker, c.err)
				if got != c.err {
					t.Errorf("recordSkippedPreflightResult(%v) = %v, want the error unchanged", c.err, got)
				}
				if got := breaker.State(); got != circuitClosed {
					t.Errorf("circuit state = %q, want %q", got, circuitClosed)
				}
				if got := breaker.readState().Failures; got != circuitFailureThreshold-1 {
					t.Errorf("breaker failures = %d, want %d (none recorded)", got, circuitFailureThreshold-1)
				}
			})
		}
	}
}

// TestExternalServerPreflightUnknownUserNamedLikeAnOutage: a remote open of
// the test server as user "lost connection", which has no account, is a wrong
// login like any other: two connections (the init connection runs), no
// "unreachable" in the error, no breaker failure.
func TestExternalServerPreflightUnknownUserNamedLikeAnOutage(t *testing.T) {
	skipIfNoDolt(t)
	acquireTestSlot()
	t.Cleanup(releaseTestSlot)
	t.Setenv("BEADS_TEST_MODE", "")
	ctx, cancel := testContext(t)
	defer cancel()
	treatHostAsExternal(t)

	f := newIdentityTestFixture(t, "preflight-auth")
	hc, port := startHandshakeCounter(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(testServerPort)))
	breaker := stubServerBreaker(t, port)
	cfg := f.config()
	cfg.ReadOnly = true
	cfg.CreateIfMissing = false
	cfg.ServerHost = "127.0.0.1"
	cfg.ServerPort = port
	cfg.ServerUser = "lost connection"
	cfg.ServerPassword = "unused-test-value"
	store, err := newServerMode(ctx, cfg)
	if err == nil {
		_ = store.Close()
		t.Fatal("open as a user with no account succeeded")
	}
	accepted, _ := hc.waitClosed(t)
	if accepted != 2 {
		t.Errorf("open as an unknown user made %d connection(s), want 2 (fast-path ping, init connection)", accepted)
	}
	if strings.Contains(err.Error(), "Dolt server unreachable") {
		t.Errorf("access denied reported as an unreachable server: %v", err)
	}
	if !strings.Contains(err.Error(), "Error 1045") {
		t.Errorf("error = %v, want the server's Error 1045", err)
	}
	if got := breaker.readState().Failures; got != 0 {
		t.Errorf("breaker failures = %d, want 0", got)
	}
}

// TestExternalServerPreflightWrongPasswordsLeaveBreakerClosed: with an account
// named "lost connection", a breaker threshold's worth of wrong-password opens
// leaves the breaker closed, and the right password then opens.
func TestExternalServerPreflightWrongPasswordsLeaveBreakerClosed(t *testing.T) {
	skipIfNoDolt(t)
	acquireTestSlot()
	t.Cleanup(releaseTestSlot)
	ctx, cancel := testContext(t)
	defer cancel()

	f := newIdentityTestFixture(t, "preflight-auth-recovery")
	admin, err := New(ctx, f.config())
	if err != nil {
		t.Fatalf("create fixture database: %v", err)
	}
	defer func() { _ = admin.Close() }()
	const password = "fixture-auth-password"
	if _, err := admin.db.ExecContext(ctx, "CREATE USER 'lost connection'@'%' IDENTIFIED BY '"+password+"'"); err != nil {
		t.Fatalf("create test user: %v", err)
	}
	defer func() {
		if _, err := admin.db.ExecContext(context.Background(), "DROP USER 'lost connection'@'%'"); err != nil {
			t.Errorf("drop test user: %v", err)
		}
	}()
	if _, err := admin.db.ExecContext(ctx, fmt.Sprintf("GRANT SELECT ON `%s`.* TO 'lost connection'@'%%'", f.dbName)); err != nil {
		t.Fatalf("grant test user: %v", err)
	}

	t.Setenv("BEADS_TEST_MODE", "")
	treatHostAsExternal(t)
	_, port := startHandshakeCounter(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(testServerPort)))
	breaker := stubServerBreaker(t, port)
	cfg := f.config()
	cfg.ServerHost = "127.0.0.1"
	cfg.ServerPort = port
	cfg.ServerUser = "lost connection"
	cfg.ReadOnly = true
	cfg.CreateIfMissing = false

	cfg.ServerPassword = "intentionally-wrong"
	for i := range circuitFailureThreshold {
		store, err := newServerMode(ctx, cfg)
		if err == nil {
			_ = store.Close()
			t.Fatalf("wrong password %d opened", i+1)
		}
		if !strings.Contains(err.Error(), "Error 1045") || strings.Contains(err.Error(), "Dolt server unreachable") {
			t.Fatalf("wrong password %d: error = %v, want the server's Error 1045 and no unreachable", i+1, err)
		}
	}
	if got := breaker.State(); got != circuitClosed {
		t.Errorf("after %d wrong passwords the circuit is %q, want %q", circuitFailureThreshold, got, circuitClosed)
	}
	if got := breaker.readState().Failures; got != 0 {
		t.Errorf("after %d wrong passwords breaker failures = %d, want 0", circuitFailureThreshold, got)
	}

	cfg.ServerPassword = password
	store, err := newServerMode(ctx, cfg)
	if err != nil {
		t.Fatalf("right password after the wrong ones: %v", err)
	}
	_ = store.Close()
}
