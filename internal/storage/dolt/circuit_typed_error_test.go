package dolt

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"

	mysql "github.com/go-sql-driver/mysql"

	"github.com/steveyegge/beads/internal/storage/dbproxy/proxy"
	"github.com/steveyegge/beads/internal/storage/schema"
)

// typedQuotingUserText are error packets a healthy server sends whose text
// quotes a value a user chose, so they carry breaker-trigger words (bd-a2wbd).
var typedQuotingUserText = []struct {
	name string
	err  error
}{
	{"1062 duplicate custom type", &mysql.MySQLError{Number: 1062, Message: "duplicate primary key given: [gone away]"}},
	{"1452 fk key value", &mysql.MySQLError{Number: 1452, Message: "cannot add or update a child row - Foreign key violation on fk: `fk_labels_issue`, table: `labels`, referenced table: `issues`, key: `[bd-lost connection]`"}},
	{"1045 user name", &mysql.MySQLError{Number: 1045, Message: "Access denied for user 'lost connection'@'%'"}},
	{"wrapper quotes the text", fmt.Errorf("syncing custom_types table broken pipe: %w", &mysql.MySQLError{Number: 1146, Message: "table not found: custom_types"})},
	{"proxy number without its prefix", &mysql.MySQLError{Number: proxy.UpstreamErrorNumber, Message: "Can't connect: connection refused"}},
}

func TestTypedErrorQuotingUserTextIsNotAnOutage(t *testing.T) {
	for _, tc := range typedQuotingUserText {
		t.Run(tc.name, func(t *testing.T) {
			if isConnectionError(tc.err) {
				t.Errorf("isConnectionError(%v) = true, want false", tc.err)
			}
			if isRetryableError(tc.err) {
				t.Errorf("isRetryableError(%v) = true, want false", tc.err)
			}
		})
	}
}

func TestOutagePacketsStayConnectionErrors(t *testing.T) {
	for _, err := range []error{
		&mysql.MySQLError{Number: 1053, Message: "Server shutdown in progress"},
		fmt.Errorf("ping: %w", &mysql.MySQLError{Number: proxy.UpstreamErrorNumber, Message: proxy.UpstreamErrorPrefix + "upstream Dolt server unreachable: dial tcp 10.0.0.1:3307: connection refused"}),
	} {
		if !isConnectionError(err) {
			t.Errorf("isConnectionError(%v) = false, want true", err)
		}
		if !isRetryableError(err) {
			t.Errorf("isRetryableError(%v) = false, want true", err)
		}
	}
	for _, err := range []error{
		&mysql.MySQLError{Number: 1049, Message: "Unknown database 'beads_test'"},
		&mysql.MySQLError{Number: 1105, Message: "no root value found in session"},
		&mysql.MySQLError{Number: 1105, Message: "cannot update manifest: database is read only"},
	} {
		if !isRetryableError(err) {
			t.Errorf("isRetryableError(%v) = false, want true (kept typed recovery)", err)
		}
		if isConnectionError(err) {
			t.Errorf("isConnectionError(%v) = true, want false", err)
		}
	}
}

// TestOutagePacketsProxyConstantsMatchProxy pins the local copies of the db
// proxy's outage packet number and prefix (circuit.go) to the proxy's own,
// which this package does not import outside tests.
func TestOutagePacketsProxyConstantsMatchProxy(t *testing.T) {
	if proxyUpstreamErrorNumber != proxy.UpstreamErrorNumber || proxyUpstreamErrorPrefix != proxy.UpstreamErrorPrefix {
		t.Fatalf("local proxy outage packet (%d, %q) differs from proxy's (%d, %q)",
			proxyUpstreamErrorNumber, proxyUpstreamErrorPrefix, proxy.UpstreamErrorNumber, proxy.UpstreamErrorPrefix)
	}
	err := &mysql.MySQLError{Number: proxy.UpstreamErrorNumber, Message: proxy.UpstreamErrorPrefix + "upstream closed"}
	if isServerOutagePacket(err) != proxy.IsUpstreamOutageError(err) {
		t.Fatalf("isServerOutagePacket and proxy.IsUpstreamOutageError disagree on %v", err)
	}
}

func TestWithRetryTypedQuotedTextDoesNotRetryOrTripCircuit(t *testing.T) {
	t.Setenv("BEADS_TEST_MODE", "")
	breaker := newTestCircuitBreaker(t)
	for range circuitFailureThreshold - 1 {
		breaker.RecordFailure()
	}
	store := &DoltStore{breaker: breaker}
	err := &mysql.MySQLError{Number: 1062, Message: "duplicate primary key given: [gone away]"}
	calls := 0
	got := store.withRetry(context.Background(), func() error {
		calls++
		return err
	})
	if !errors.Is(got, err) || calls != 1 {
		t.Fatalf("withRetry() = %v after %d calls, want %v after 1", got, calls, err)
	}
	if state := breaker.State(); state != circuitClosed {
		t.Fatalf("circuit state = %q, want %q", state, circuitClosed)
	}
}

// TestTypedErrorDuplicateKeyWriteDoesNotTripCircuit is bd-a2wbd's run-time
// fault on a real server, through the write wrapper SetConfig uses
// (withRetryTxNoJournal): a duplicate-key INSERT whose key holds the words
// "gone away" fails with the server's typed 1062 quoting the key. Before the
// fix retryWriteTx replayed it five times and opened the breaker file every
// bd process for this host:port:database shares. A test store has no breaker
// in test mode (initializeServerCircuitBreaker), so the test attaches one
// built by the production constructor, on its own database name.
func TestTypedErrorDuplicateKeyWriteDoesNotTripCircuit(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	if store.breaker != nil {
		t.Fatalf("test store came with a breaker; this test attaches its own")
	}
	breaker := newCircuitBreaker("127.0.0.1", testServerPort, "bd_lgsq9_typed_error")
	breaker.Reset()
	t.Cleanup(func() {
		breaker.Reset()
		_ = os.Remove(breaker.filePath)
	})
	store.breaker = breaker
	t.Cleanup(func() { store.breaker = nil })

	calls := 0
	err := store.withRetryTxNoJournal(context.Background(), func(tx *sql.Tx) error {
		calls++
		for range 2 {
			if _, err := tx.ExecContext(context.Background(), "INSERT INTO custom_types (name) VALUES (?)", "gone away"); err != nil {
				return err
			}
		}
		return nil
	})
	var mysqlErr *mysql.MySQLError
	if !errors.As(err, &mysqlErr) || mysqlErr.Number != 1062 {
		t.Fatalf("withRetryTxNoJournal error = %v, want the server's typed 1062", err)
	}
	t.Logf("server error: %v", err)
	if calls != 1 {
		t.Errorf("callback ran %d times, want 1 (a typed 1062 is not retried)", calls)
	}
	if state := breaker.State(); state != circuitClosed {
		t.Errorf("circuit state = %q after one rejected write, want %q", state, circuitClosed)
	}
}

// multiErr is a multi-error that, unlike errors.Join, keeps nil elements.
type multiErr []error

func (m multiErr) Error() string {
	parts := make([]string, 0, len(m))
	for _, e := range m {
		if e == nil {
			parts = append(parts, "<nil>")
			continue
		}
		parts = append(parts, e.Error())
	}
	return strings.Join(parts, "; ")
}

func (m multiErr) Unwrap() []error { return m }

// TestTypedErrorJoinedTreeTable pins classifyErrorTree's rule (bd-dcwku) on
// error trees that hold a server packet beside other errors: a lost
// connection anywhere in the tree wins over the packet, a packet's text and
// the text of a wrapper above it are never matched.
func TestTypedErrorJoinedTreeTable(t *testing.T) {
	dup := &mysql.MySQLError{Number: 1062, Message: "duplicate primary key given: [gone away]"}
	fk := &mysql.MySQLError{Number: 1452, Message: "cannot add or update a child row: key [bd-broken pipe]"}
	denied := &mysql.MySQLError{Number: 1045, Message: "Access denied for user 'lost connection'@'%'"}
	shutdown := &mysql.MySQLError{Number: 1053, Message: "Server shutdown in progress"}
	readOnly := &mysql.MySQLError{Number: 1105, Message: "cannot update manifest: database is read only"}
	unknownDB := &mysql.MySQLError{Number: 1049, Message: "Unknown database 'beads_test'"}
	netErr := &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
	cases := []struct {
		name          string
		err           error
		wantConn      bool
		wantRetryable bool
	}{
		{"join 1062 quoting gone away with nil", errors.Join(dup, nil), false, false},
		{"join 1062 with invalid connection", errors.Join(dup, mysql.ErrInvalidConn), true, true},
		{"join 1062 with 1053 shutdown", errors.Join(dup, shutdown), true, true},
		{"join 1062 with driver bad connection", errors.Join(dup, driver.ErrBadConn), true, true},
		{"join 1062 with plain broken pipe", errors.Join(dup, errors.New("rollback: write tcp 127.0.0.1:3307: broken pipe")), true, true},
		{"join 1062 with net op error", errors.Join(dup, netErr), true, true},
		{"wrapper around a join", fmt.Errorf("save config: %w", errors.Join(dup, mysql.ErrInvalidConn)), true, true},
		{"two %w in one Errorf", fmt.Errorf("insert: %w; rollback: %w", dup, mysql.ErrInvalidConn), true, true},
		{"two %w, both packets quoting words", fmt.Errorf("insert: %w; rollback: %w", dup, denied), false, false},
		{"lost connection wrapper above join of 1062 and plain", fmt.Errorf("lost connection while saving: %w", errors.Join(dup, errors.New("rollback refused"))), false, false},
		{"join inside a join, loss outside", errors.Join(errors.Join(dup, fk), mysql.ErrInvalidConn), true, true},
		{"join inside a join, loss inside", errors.Join(fk, errors.Join(dup, driver.ErrBadConn)), true, true},
		{"join inside a join, packets only", errors.Join(errors.Join(dup, fk), denied), false, false},
		{"nil element beside 1062", multiErr{dup, nil}, false, false},
		{"nil element beside 1062 and invalid connection", multiErr{nil, dup, mysql.ErrInvalidConn}, true, true},
		{"join 1062 with kept 1105 read only", errors.Join(dup, readOnly), false, true},
		{"join 1062 with kept 1049 unknown database", errors.Join(unknownDB, dup), false, true},
		{"join 1062 with migration lock", errors.Join(dup, fmt.Errorf("release: %w", schema.ErrMigrationLockRelease)), false, true},
		{"permanent wrapper around a join", fmt.Errorf("%w (circuit breaker tripped)", errors.Join(dup, mysql.ErrInvalidConn)), true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isConnectionError(tc.err); got != tc.wantConn {
				t.Errorf("isConnectionError(%v) = %t, want %t", tc.err, got, tc.wantConn)
			}
			if got := isRetryableError(tc.err); got != tc.wantRetryable {
				t.Errorf("isRetryableError(%v) = %t, want %t", tc.err, got, tc.wantRetryable)
			}
		})
	}
}

// headIsConnectionError is a frozen copy of isConnectionError at cbcb704ca,
// before bd-lgsq9: the oracle for an error tree that holds no server packet.
func headIsConnectionError(err error) bool {
	if err == nil {
		return false
	}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1105 {
		return false
	}
	errStr := strings.ToLower(err.Error())
	for _, word := range []string{"connection refused", "connection reset", "broken pipe", "i/o timeout",
		"bad connection", "invalid connection", "lost connection", "gone away"} {
		if strings.Contains(errStr, word) {
			return true
		}
	}
	return false
}

// headIsRetryableError is a frozen copy of isRetryableError at cbcb704ca,
// before bd-lgsq9.
func headIsRetryableError(err error) bool {
	if err == nil {
		return false
	}
	if schema.IsMigrationLockError(err) {
		return true
	}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1105 {
		message := strings.ToLower(mysqlErr.Message)
		if strings.Contains(message, "no root value found") ||
			strings.Contains(message, "database is read only") {
			return true
		}
		return false
	}
	errStr := strings.ToLower(err.Error())
	for _, word := range []string{"driver: bad connection", "invalid connection", "broken pipe", "connection reset",
		"connection refused", "database is read only", "lost connection", "gone away", "i/o timeout",
		"unknown database", "no root value found"} {
		if strings.Contains(errStr, word) {
			return true
		}
	}
	return false
}

// specTypedConnection and specTypedRetryable are bd-lgsq9's typed answers for
// one server packet, written from its spec rather than from circuit.go.
func specTypedConnection(p *mysql.MySQLError) bool {
	return p.Number == 1053 ||
		(p.Number == proxy.UpstreamErrorNumber && strings.HasPrefix(p.Message, proxy.UpstreamErrorPrefix))
}

func specTypedRetryable(p *mysql.MySQLError) bool {
	message := strings.ToLower(p.Message)
	switch p.Number {
	case 1105:
		return strings.Contains(message, "no root value found") || strings.Contains(message, "database is read only")
	case 1049:
		return strings.Contains(message, "unknown database")
	}
	return specTypedConnection(p)
}

// errTreeNode is one node of a generated error tree. The oracle walks this
// structure, not the built error's Unwrap chain.
type errTreeNode struct {
	typed *mysql.MySQLError // a packet leaf
	leaf  error             // an untyped leaf (nil with no kids and no typed: a nil element)
	kids  []*errTreeNode
	build func(kids []error) error
	err   error // the built error
}

func (n *errTreeNode) holdsPacket() bool {
	if n.typed != nil {
		return true
	}
	for _, k := range n.kids {
		if k.holdsPacket() {
			return true
		}
	}
	return false
}

func (n *errTreeNode) materialize() error {
	switch {
	case n.typed != nil:
		n.err = n.typed
	case n.build == nil:
		n.err = n.leaf
	default:
		kids := make([]error, len(n.kids))
		for i, k := range n.kids {
			kids[i] = k.materialize()
		}
		n.err = n.build(kids)
	}
	return n.err
}

// oracle: a tree with no packet gets HEAD's answer; one with a packet is true
// when any maximal packet-free part is true by HEAD's copy or any packet leaf
// is true by the typed answer.
func (n *errTreeNode) oracle(head func(error) bool, typed func(*mysql.MySQLError) bool) bool {
	if !n.holdsPacket() {
		return n.err != nil && head(n.err)
	}
	if n.typed != nil {
		return typed(n.typed)
	}
	for _, k := range n.kids {
		if k.oracle(head, typed) {
			return true
		}
	}
	return false
}

var (
	randomTypedLeaves = []*mysql.MySQLError{
		{Number: 1062, Message: "duplicate primary key given: [bd-1]"},
		{Number: 1062, Message: "duplicate primary key given: [gone away]"},
		{Number: 1452, Message: "foreign key violation, key: [bd-lost connection]"},
		{Number: 1045, Message: "Access denied for user 'broken pipe'@'%'"},
		{Number: 1146, Message: "table not found: custom_types"},
		{Number: 1146, Message: "table not found: connection refused"},
		{Number: 1105, Message: "no root value found in session"},
		{Number: 1105, Message: "cannot update manifest: database is read only"},
		{Number: 1105, Message: "invalid connection state in session"},
		{Number: 1049, Message: "Unknown database 'beads_x'"},
		{Number: 1049, Message: "database not found: beads_x"},
		{Number: 1053, Message: "Server shutdown in progress"},
		{Number: proxy.UpstreamErrorNumber, Message: proxy.UpstreamErrorPrefix + "upstream Dolt server unreachable: connection refused"},
		{Number: proxy.UpstreamErrorNumber, Message: "Can't connect: i/o timeout"},
	}
	randomUntypedLeaves = []error{
		mysql.ErrInvalidConn,
		driver.ErrBadConn,
		&net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET},
		&net.OpError{Op: "dial", Net: "tcp", Err: syscall.EHOSTUNREACH},
		errors.New("rollback: write tcp 127.0.0.1:3307: broken pipe"),
		errors.New("i/o timeout"),
		errors.New("rollback refused"),
		errors.New("context deadline exceeded"),
		errors.New("unknown database beads_x"),
		errors.New("cannot update manifest: database is read only"),
	}
	randomWrapperTexts = []string{"save config", "insert issue", "lost connection while saving", "server gone away", "begin write tx"}
)

func randomErrTree(r *rand.Rand, depth int) *errTreeNode {
	if depth == 0 || r.Intn(3) == 0 {
		switch r.Intn(10) {
		case 0:
			return &errTreeNode{} // nil element
		case 1, 2, 3, 4, 5:
			p := *randomTypedLeaves[r.Intn(len(randomTypedLeaves))]
			return &errTreeNode{typed: &p}
		default:
			return &errTreeNode{leaf: randomUntypedLeaves[r.Intn(len(randomUntypedLeaves))]}
		}
	}
	text := randomWrapperTexts[r.Intn(len(randomWrapperTexts))]
	switch r.Intn(4) {
	case 0:
		return &errTreeNode{kids: []*errTreeNode{randomErrTree(r, depth-1)}, build: func(k []error) error {
			return fmt.Errorf("%s: %w", text, k[0])
		}}
	case 1:
		kids := make([]*errTreeNode, 1+r.Intn(3))
		for i := range kids {
			kids[i] = randomErrTree(r, depth-1)
		}
		return &errTreeNode{kids: kids, build: func(k []error) error { return errors.Join(k...) }}
	case 2:
		return &errTreeNode{kids: []*errTreeNode{randomErrTree(r, depth-1), randomErrTree(r, depth-1)}, build: func(k []error) error {
			return fmt.Errorf("%s: %w; %w", text, k[0], k[1])
		}}
	default:
		kids := make([]*errTreeNode, 2+r.Intn(2))
		for i := range kids {
			kids[i] = randomErrTree(r, depth-1)
		}
		return &errTreeNode{kids: kids, build: func(k []error) error { return multiErr(k) }}
	}
}

// TestTypedErrorRandomTreesMatchHeadRule compares both classifiers on seeded
// random error trees with the rule of bd-dcwku, built from a frozen copy of
// HEAD's classifiers and the typed answers of bd-lgsq9's spec.
func TestTypedErrorRandomTreesMatchHeadRule(t *testing.T) {
	const (
		baseSeed = 20261119
		count    = 5000
		depth    = 4
	)
	compared, typedTrees := 0, 0
	for i := range count {
		seed := int64(baseSeed + i)
		tree := randomErrTree(rand.New(rand.NewSource(seed)), depth)
		err := tree.materialize()
		if err == nil {
			continue
		}
		compared++
		if tree.holdsPacket() {
			typedTrees++
		}
		if got, want := isConnectionError(err), tree.oracle(headIsConnectionError, specTypedConnection); got != want {
			t.Errorf("seed %d: isConnectionError(%q) = %t, want %t", seed, err, got, want)
		}
		if got, want := isRetryableError(err), tree.oracle(headIsRetryableError, specTypedRetryable); got != want {
			t.Errorf("seed %d: isRetryableError(%q) = %t, want %t", seed, err, got, want)
		}
	}
	t.Logf("compared %d non-nil trees of %d seeded (seeds %d..%d, depth %d), %d holding a server packet", compared, count, baseSeed, baseSeed+count-1, depth, typedTrees)
	if typedTrees < count/4 {
		t.Fatalf("only %d of %d trees hold a packet; the generator lost its typed leaves", typedTrees, count)
	}
}

// TestTypedErrorJoinedRollbackLossTripsCircuit is review bd-49oko's live case:
// a duplicate INSERT gets the server's typed 1062, and the socket then drops
// when the driver sends ROLLBACK, so commitWriteTx returns
// errors.Join(1062, mysql.ErrInvalidConn). The lost connection must win over
// the packet: both classifiers say true and a production breaker one failure
// short of its threshold opens, as at HEAD.
func TestTypedErrorJoinedRollbackLossTripsCircuit(t *testing.T) {
	original, cleanup := setupTestStore(t)
	defer cleanup()

	cfg, err := mysql.ParseDSN(original.connStr)
	if err != nil {
		t.Fatal(err)
	}
	port := startForkGateProxy(t, cfg.Addr, func(query string) bool {
		return query != "ROLLBACK"
	})
	cfg.Addr = fmt.Sprintf("127.0.0.1:%d", port)
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	breaker := newCircuitBreaker("127.0.0.1", port, "bd_dcwku_joined_rollback")
	breaker.Reset()
	t.Cleanup(func() {
		breaker.Reset()
		_ = os.Remove(breaker.filePath)
	})
	for range circuitFailureThreshold - 1 {
		breaker.RecordFailure()
	}
	if state := breaker.State(); state != circuitClosed {
		t.Fatalf("seeded breaker state = %q, want %q", state, circuitClosed)
	}
	store := &DoltStore{db: db, breaker: breaker, serverMode: true}

	ctx := context.Background()
	calls := 0
	err = store.withRetryTxNoJournal(ctx, func(tx *sql.Tx) error {
		calls++
		for range 2 {
			if _, err := tx.ExecContext(ctx, "INSERT INTO custom_types (name) VALUES (?)", "bd-dcwku-joined-rollback"); err != nil {
				return err
			}
		}
		return nil
	})
	var packet *mysql.MySQLError
	if !errors.As(err, &packet) || packet.Number != 1062 || !errors.Is(err, mysql.ErrInvalidConn) {
		t.Fatalf("withRetryTxNoJournal error = %v, want the server's typed 1062 joined with %v", err, mysql.ErrInvalidConn)
	}
	t.Logf("joined error: %v; calls = %d", err, calls)
	if !isConnectionError(err) {
		t.Errorf("isConnectionError(%v) = false, want true (the ROLLBACK lost its connection)", err)
	}
	if !isRetryableError(err) {
		t.Errorf("isRetryableError(%v) = false, want true", err)
	}
	if state := breaker.State(); state != circuitOpen {
		t.Errorf("circuit state = %q after a write that lost its connection, want %q", state, circuitOpen)
	}
	if calls != 1 {
		t.Errorf("callback ran %d times, want 1 (the tripped breaker stops the retry)", calls)
	}
}
