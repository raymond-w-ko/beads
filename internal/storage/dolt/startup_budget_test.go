package dolt

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TestServerOpenStatementBudget counts what a server-mode open sends to the
// Dolt server before it returns: TCP connections, and client commands
// (COM_QUERY texts, COM_PING, ...). Every start-up statement is a round trip
// and every connection a handshake on each bd invocation, which on the fleet
// costs milliseconds per statement and 30-90 ms per connection, thousands of
// times an hour (bd-7c0u4). The numbers are exact on purpose: a new start-up
// statement fails this test with its text, and a cut lowers them here.
//
// A logging TCP proxy sits between the store and the test Dolt server. The
// database is created and converged first over a direct connection; the
// measured opens are of that existing database, with a metadata.json project
// id so the identity check runs as it does for a real workspace.
//
// Current budget (bd-rlkn9 cut the per-connection SELECT @@max_allowed_packet
// probe by pinning MaxAllowedPacket in the DSN, and the second PING in
// newServerMode; bd-kgx8s read the identity check's _project_id without
// START TRANSACTION / ROLLBACK; bd-lbrt4 asks the convergence question on the
// main pool before the migration pool and the gate):
//   - read-only: 2 connections (the fail-fast preflight dial, which never
//     authenticates, and the pooled session), 4 commands.
//   - writable: 2 connections, the same two, whose commands are the read-only
//     four plus the convergence pass on the pooled session. A converged open
//     no longer opens the one-off migration pool (3 connections, 34 commands
//     before bd-lbrt4), so five commands went with it: the migrate gate's
//     four (SHOW TABLES LIKE 'schema_migrations' and
//     SELECT COALESCE(MAX(version), 0) FROM schema_migrations, once for
//     CurrentVersion and once for PendingVersions) and initSchemaOnDB's
//     SELECT DATABASE(). The convergence pass's own SELECT DATABASE() stays.
//     bd-iqmdg's mail key indexes on wisps added one probe to it, 29 to 30:
//     SHOW INDEX FROM `wisps` (the fork step's columns on wisps were already
//     read by its SHOW COLUMNS FROM `wisps`). bd-7bqv6 (review bd-313ml
//     finding 1) replaced SHOW COLUMNS FROM `issues` and SHOW INDEX FROM
//     `issues` with SHOW CREATE TABLE `issues` and SHOW CREATE TABLE `issues`
//     AS OF 'HEAD' (the gate reads HEAD too, so a lost schema commit is
//     seen), so the count stays 30. bd-hapoo's idx_wisp_dep_external_target
//     (the dependents count's external branch on wisp_dependencies) added one
//     probe, 30 to 31: SHOW INDEX FROM `wisp_dependencies` (no SHOW COLUMNS:
//     the table has no fork column).
func TestServerOpenStatementBudget(t *testing.T) {
	skipIfNoDolt(t)
	acquireTestSlot()
	t.Cleanup(releaseTestSlot)
	ctx, cancel := testContext(t)
	defer cancel()

	f := newIdentityTestFixture(t, "budget")
	const projectID = "77777777-7777-7777-7777-777777777777"
	f.saveLocalProjectID(t, projectID)

	// Create and converge the database directly, not through the proxy.
	setup, err := New(ctx, f.config())
	if err != nil {
		t.Fatalf("create database: %v", err)
	}
	if err := setup.SetMetadata(ctx, "_project_id", projectID); err != nil {
		_ = setup.Close()
		t.Fatalf("seed _project_id: %v", err)
	}
	if err := setup.Close(); err != nil {
		t.Fatalf("close setup store: %v", err)
	}

	rec := &openRecorder{}
	proxyPort := startBudgetProxy(t, rec, net.JoinHostPort("127.0.0.1", strconv.Itoa(testServerPort)))

	for _, c := range []struct {
		name      string
		readOnly  bool
		wantConns int
		wantCmds  int
	}{
		{name: "read-only", readOnly: true, wantConns: 2, wantCmds: 4},
		{name: "writable", readOnly: false, wantConns: 2, wantCmds: 31},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := f.config()
			cfg.CreateIfMissing = false
			cfg.ReadOnly = c.readOnly
			cfg.ServerHost = "127.0.0.1"
			cfg.ServerPort = proxyPort

			rec.start()
			store, err := New(ctx, cfg)
			conns, cmds := rec.stop()
			if err != nil {
				t.Fatalf("open through proxy: %v", err)
			}
			defer func() { _ = store.Close() }()

			list := strings.Join(cmds, "\n")
			if conns != c.wantConns || len(cmds) != c.wantCmds {
				t.Errorf("%s open: %d connections and %d commands, want %d and %d. Commands in order:\n%s",
					c.name, conns, len(cmds), c.wantConns, c.wantCmds, list)
				return
			}
			t.Logf("%s open: %d connections, %d commands:\n%s", c.name, conns, len(cmds), list)
		})
	}
}

// openRecorder collects what the proxy sees while an open is in flight.
type openRecorder struct {
	mu     sync.Mutex
	active bool
	conns  int
	cmds   []string
}

func (r *openRecorder) start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active, r.conns, r.cmds = true, 0, nil
}

func (r *openRecorder) stop() (int, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active = false
	return r.conns, append([]string(nil), r.cmds...)
}

// accept numbers a new connection, or returns 0 outside an open.
func (r *openRecorder) accept() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active {
		return 0
	}
	r.conns++
	return r.conns
}

func (r *openRecorder) command(conn int, desc string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active && conn != 0 {
		r.cmds = append(r.cmds, fmt.Sprintf("c%d %s", conn, desc))
	}
}

var budgetCommandNames = map[byte]string{
	0x01: "QUIT", 0x02: "INIT_DB", 0x03: "QUERY", 0x0e: "PING", 0x11: "CHANGE_USER",
	0x16: "STMT_PREPARE", 0x17: "STMT_EXECUTE", 0x19: "STMT_CLOSE", 0x1a: "STMT_RESET",
	0x1f: "RESET_CONNECTION",
}

// startBudgetProxy listens on a loopback port and forwards each connection to
// upstream, recording each client command (a packet with sequence id 0) in
// rec. The DSN has TLS and compression off, so packets are plain MySQL
// frames. It returns the listening port.
//
// A command is recorded when the proxy reads it, before forwarding it, so
// every command the client waited on is recorded before the open returns.
// COM_QUIT is not recorded: the client waits for no reply, so whether the
// proxy reads it before the open returns is a race.
func startBudgetProxy(t *testing.T, rec *openRecorder, upstream string) int {
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
			go proxyBudgetConn(client, rec.accept(), rec, upstream)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func proxyBudgetConn(client net.Conn, id int, rec *openRecorder, upstream string) {
	defer func() { _ = client.Close() }()
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
			rec.command(id, desc)
		}
		if _, err := server.Write(raw); err != nil {
			return
		}
	}
}

// readBudgetPacket reads one MySQL packet: a 3-byte little-endian payload
// length, a sequence id, then the payload. raw is the whole frame.
func readBudgetPacket(r io.Reader) (seq byte, payload, raw []byte, err error) {
	hdr := make([]byte, 4)
	if _, err = io.ReadFull(r, hdr); err != nil {
		return 0, nil, nil, err
	}
	n := int(uint32(hdr[0]) | uint32(hdr[1])<<8 | uint32(hdr[2])<<16)
	raw = make([]byte, 4+n)
	copy(raw, hdr)
	if _, err = io.ReadFull(r, raw[4:]); err != nil {
		return 0, nil, nil, err
	}
	return hdr[3], raw[4:], raw, nil
}
