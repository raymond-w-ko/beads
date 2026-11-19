package dolt

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	sqle "github.com/dolthub/go-mysql-server"
	"github.com/dolthub/go-mysql-server/memory"
	gmssql "github.com/dolthub/go-mysql-server/sql"

	"github.com/steveyegge/beads/internal/storage/sqlbuild"
	"github.com/steveyegge/beads/internal/types"
)

// Comparison test for the --label-pattern predicate (bd-pqgvg, from bd-ypak8).
//
// The two pieces: today's clause, `label LIKE ? ESCAPE '|'` bound to
// lpoOracleLikePattern(glob) (a frozen copy of sqlbuild.globToLikePattern as it
// stood before the change), is the oracle; sqlbuild's labelPatternClause,
// which sends a bare `LIKE ?` for a valid UTF-8 glob without %, _, \ or |, is
// the candidate. The relation: the same ids in the same order from the full ready reads
// (GetReadyWork, GetReadyWorkWithCounts) and the list reads (SearchIssues,
// SearchIssuesWithCounts), over issues (labels) and wisps (wisp_labels), with
// the label column of both tables under each of lpoCollations. The ready
// reads and the list reads apply the pattern to both tables (the ready reads'
// wisps half since bd-9mr2a, proved by TestReadyLabelPatternFiltersWisps in
// ready_label_pattern_wisps_test.go), and the test requires a rewritten
// wisp_labels clause and a list read returning a wisp.
//
// The candidate is the store under test. The oracle is a second store on the
// same branch whose session rewrites every bare label clause back to today's
// text and pattern before the server sees it, so both run the same Go read
// path and differ only in that clause.
//
// Bounds: the 18 fixed globs (bd-ypak8's 14, bd-kx6uj's 4), then
// lpoRandomPerSeed random globs for each of lpoSeeds, under each collation:
// (18 + 2*12) globs x 5 collations x 4 reads, within lpoBudget. A failure names the seed ("fixed"
// for the fixed globs), the collation, the read and the glob.
var (
	lpoSeeds      = []int64{1, 2}
	lpoCollations = []string{"utf8mb4_0900_bin", "utf8mb4_bin", "utf8mb4_0900_ai_ci", "utf8mb4_general_ci", "utf8mb4_0900_as_cs"}
)

const (
	lpoRandomPerSeed = 12
	lpoBudget        = 6 * time.Minute
)

// lpoEdgeLabels are bd-ypak8's edge labels: case and accent variants, literal
// LIKE metacharacters and the '|' escape, prefixes ending at U+10FFFF and
// U+D7FF (the code points next to a range bound), trailing and inner spaces.
var lpoEdgeLabels = []string{
	"class:investigator", "class:lead", "class:", "CLASS:upper", "Class:mixed", "clàss:accent", "class;semi", "class", "classx", "class9", "clas", "class: space",
	"p%q1", "pXq1", "p_q1", "pYq1", "p%", "a|b1", "a|c1", "ab1", `a\b1`, "ab", "a|", `a\`,
	"z\U0010FFFF", "z\U0010FFFFx", "z\U0010FFFE", "{", "z", "y퟿a", "y", "y퟿",
	"pad", "pad x", "pad  ", "pad\tx", "padx", "PAD x", "q?mark", "s*star",
	// bd-kx6uj: the labels a non-UTF-8 glob finds through the old clause,
	// whose rune walk turns each invalid byte into U+FFFD.
	"tag-\uFFFD", "tag-\uFFFDxyz",
}

// lpoFixedGlobs are bd-ypak8's 14 globs, then bd-kx6uj's: a glob that is not
// valid UTF-8 (byte FF, as `--label-pattern "$(printf 'tag-\377*')"` sends
// it) beside its valid U+FFFD controls.
var lpoFixedGlobs = []string{"class:*", "CLASS:*", "p%q*", "p_q*", "a|b*", `a\b*`, "a|*", "z\U0010FFFF*", "y퟿*", "pad *", "pad*", "cl?ss:*", "*", "class:",
	"tag-\xff", "tag-\xff*", "tag-\uFFFD", "tag-\uFFFD*"}

// lpoGlobTokens build the random globs: the edge labels' pieces, the glob
// wildcards and every LIKE metacharacter.
var lpoGlobTokens = []string{"class", ":", "CLASS", "clàss", "cl", "%", "_", "|", `\`, "*", "?", " ", "p", "q", "a", "b", "1", "z\U0010FFFF", "y퟿", "pad", "x", ";"}

func lpoRandomGlobs(r *rand.Rand, n int) []string {
	out := make([]string, n)
	for i := range out {
		var b strings.Builder
		for k := 1 + r.Intn(4); k > 0; k-- {
			b.WriteString(lpoGlobTokens[r.Intn(len(lpoGlobTokens))])
		}
		out[i] = b.String()
	}
	return out
}

// lpoOracleLikePattern is sqlbuild.globToLikePattern frozen as the oracle's
// pattern: %, _ and | escaped with |, * and ? to % and _.
func lpoOracleLikePattern(glob string) string {
	var b strings.Builder
	for _, c := range glob {
		switch c {
		case '%', '_', '|':
			b.WriteByte('|')
			b.WriteRune(c)
		case '*':
			b.WriteByte('%')
		case '?':
			b.WriteByte('_')
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

const (
	lpoBareClause   = "WHERE label LIKE ?)"
	lpoOracleClause = "WHERE label LIKE ? ESCAPE '|')"
)

// lpoRewriter turns the candidate's bare label clause back into the oracle's.
type lpoRewriter struct {
	mu       sync.Mutex
	glob     string
	rewrites int
	bad      []string // a bare clause bound to something other than the glob's bare pattern
	// wispRewrites counts, over the whole test, rewritten statements that
	// read wisp_labels, proving the wisps half was compared.
	wispRewrites int
}

func (o *lpoRewriter) set(glob string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.glob, o.rewrites, o.bad = glob, 0, nil
}

func (o *lpoRewriter) stats() (int, []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.rewrites, o.bad
}

// rewriteQuery returns the oracle text and the ordinals (0-based) of the
// placeholders that bind a rewritten clause.
func (o *lpoRewriter) rewriteQuery(query string) (string, []int) {
	if !strings.Contains(query, lpoBareClause) {
		return query, nil
	}
	var b strings.Builder
	var idxs []int
	before := 0
	rest := query
	for {
		i := strings.Index(rest, lpoBareClause)
		if i < 0 {
			b.WriteString(rest)
			break
		}
		n := before + strings.Count(rest[:i], "?")
		idxs = append(idxs, n)
		b.WriteString(rest[:i])
		b.WriteString(lpoOracleClause)
		before = n + 1
		rest = rest[i+len(lpoBareClause):]
	}
	o.mu.Lock()
	o.rewrites++
	if strings.Contains(query, "FROM wisp_labels "+lpoBareClause) {
		o.wispRewrites++
	}
	o.mu.Unlock()
	return b.String(), idxs
}

func (o *lpoRewriter) rewriteArgs(args []driver.NamedValue, idxs []int) []driver.NamedValue {
	if len(idxs) == 0 {
		return args
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	out := append([]driver.NamedValue(nil), args...)
	for _, n := range idxs {
		if n >= len(out) {
			o.bad = append(o.bad, fmt.Sprintf("placeholder %d of %d", n, len(out)))
			continue
		}
		if s, ok := out[n].Value.(string); !ok || s != strings.NewReplacer("*", "%", "?", "_").Replace(o.glob) {
			o.bad = append(o.bad, fmt.Sprintf("bare clause bound to %#v for glob %q", out[n].Value, o.glob))
		}
		out[n].Value = lpoOracleLikePattern(o.glob)
	}
	return out
}

type lpoConnector struct {
	dsn string
	drv driver.Driver
	o   *lpoRewriter
}

func (c lpoConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.drv.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return lpoConn{Conn: conn, o: c.o}, nil
}

func (c lpoConnector) Driver() driver.Driver { return c.drv }

// lpoConn rewrites on both of database/sql's query paths: QueryContext, and
// PrepareContext for a driver that answers a parameterized query with
// driver.ErrSkip (the statement then binds the rewritten arguments).
type lpoConn struct {
	driver.Conn
	o *lpoRewriter
}

func (c lpoConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q, idxs := c.o.rewriteQuery(query)
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, c.o.rewriteArgs(args, idxs))
}

func (c lpoConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c lpoConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	q, idxs := c.o.rewriteQuery(query)
	st, err := c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, q)
	if err != nil {
		return nil, err
	}
	return lpoStmt{Stmt: st, idxs: idxs, o: c.o}, nil
}

func (c lpoConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c lpoConn) Ping(ctx context.Context) error { return c.Conn.(driver.Pinger).Ping(ctx) }

func (c lpoConn) CheckNamedValue(nv *driver.NamedValue) error {
	return c.Conn.(driver.NamedValueChecker).CheckNamedValue(nv)
}

type lpoStmt struct {
	driver.Stmt
	idxs []int
	o    *lpoRewriter
}

func (s lpoStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.Stmt.(driver.StmtQueryContext).QueryContext(ctx, s.o.rewriteArgs(args, s.idxs))
}

func (s lpoStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.Stmt.(driver.StmtExecContext).ExecContext(ctx, s.o.rewriteArgs(args, s.idxs))
}

func (s lpoStmt) CheckNamedValue(nv *driver.NamedValue) error {
	if c, ok := s.Stmt.(driver.NamedValueChecker); ok {
		return c.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

// openOracleStoreOnBranch is a second store on primary's test branch whose
// session sends the oracle's label clause.
func openOracleStoreOnBranch(t *testing.T, ctx context.Context, primary *DoltStore, o *lpoRewriter) *DoltStore {
	t.Helper()
	store := openSecondStoreOnBranch(t, ctx, primary)
	db := sql.OpenDB(lpoConnector{dsn: store.connStr, drv: store.db.Driver(), o: o})
	db.SetMaxOpenConns(1) // DOLT_CHECKOUT below is session-level
	_ = store.db.Close()
	store.db = db // closed with the store
	var branch string
	if err := primary.db.QueryRowContext(ctx, "SELECT active_branch()").Scan(&branch); err != nil {
		t.Fatalf("read test branch: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", branch); err != nil {
		t.Fatalf("check oracle store out to %s: %v", branch, err)
	}
	return store
}

type lpoRead struct {
	name string
	run  func(ctx context.Context, s *DoltStore, glob string) ([]string, error)
}

var lpoReads = []lpoRead{
	{"GetReadyWork", func(ctx context.Context, s *DoltStore, glob string) ([]string, error) {
		got, err := s.GetReadyWork(ctx, types.WorkFilter{LabelPattern: glob, IncludeEphemeral: true, Limit: 1000})
		return lpoIssueIDs(got), err
	}},
	{"GetReadyWorkWithCounts", func(ctx context.Context, s *DoltStore, glob string) ([]string, error) {
		got, err := s.GetReadyWorkWithCounts(ctx, types.WorkFilter{LabelPattern: glob, IncludeEphemeral: true, Limit: 1000})
		return lpoCountIDs(got), err
	}},
	{"SearchIssues", func(ctx context.Context, s *DoltStore, glob string) ([]string, error) {
		got, err := s.SearchIssues(ctx, "", types.IssueFilter{LabelPattern: glob, Limit: 1000})
		return lpoIssueIDs(got), err
	}},
	{"SearchIssuesWithCounts", func(ctx context.Context, s *DoltStore, glob string) ([]string, error) {
		got, err := s.SearchIssuesWithCounts(ctx, "", types.IssueFilter{LabelPattern: glob, Limit: 1000})
		return lpoCountIDs(got), err
	}},
}

func lpoIssueIDs(in []*types.Issue) []string {
	out := []string{}
	for _, is := range in {
		out = append(out, is.ID)
	}
	return out
}

func lpoCountIDs(in []*types.IssueWithCounts) []string {
	out := []string{}
	for _, is := range in {
		out = append(out, is.Issue.ID)
	}
	return out
}

// lpoSeedCorpus gives every edge label to its own issue and its own wisp, and
// ordinary labels to more of each, all open so every one is ready.
func lpoSeedCorpus(t *testing.T, ctx context.Context, store *DoltStore) {
	t.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ordinary := []string{"class:implementer", "class:reviewer", "tier:3", "area:01", "area:02", "needs-pm"}
	n := 0
	add := func(id string, wisp bool, labels ...string) {
		t.Helper()
		is := &types.Issue{ID: id, Title: "label pattern " + id, Status: types.StatusOpen, Priority: n % 5,
			IssueType: types.TypeTask, Ephemeral: wisp, CreatedAt: base.Add(time.Duration(n) * time.Minute)}
		n++
		if err := store.CreateIssue(ctx, is, "tester"); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		table := "labels"
		if wisp {
			table = "wisp_labels"
		}
		for _, l := range labels {
			if _, err := store.db.ExecContext(ctx, "INSERT INTO "+table+" (issue_id, label) VALUES (?, ?)", id, l); err != nil {
				t.Fatalf("label %s %q: %v", id, l, err)
			}
		}
	}
	for i, l := range lpoEdgeLabels {
		add(fmt.Sprintf("lpo-e%02d", i), false, l)
		add(fmt.Sprintf("lpo-wisp-e%02d", i), true, l)
	}
	for i := 0; i < 12; i++ {
		add(fmt.Sprintf("lpo-o%02d", i), false, ordinary[i%len(ordinary)], ordinary[(i+1)%len(ordinary)])
		add(fmt.Sprintf("lpo-wisp-o%02d", i), true, ordinary[i%len(ordinary)])
	}
	add("lpo-none", false)
}

func TestLabelPatternOracle(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), lpoBudget+time.Minute)
	defer cancel()
	deadline := time.Now().Add(lpoBudget)
	started := time.Now()

	lpoSeedCorpus(t, ctx, store)
	rw := &lpoRewriter{}
	oracle := openOracleStoreOnBranch(t, ctx, store, rw)

	type globSet struct {
		where string
		globs []string
	}
	sets := []globSet{{"fixed", lpoFixedGlobs}}
	for _, seed := range lpoSeeds {
		sets = append(sets, globSet{fmt.Sprintf("seed %d", seed), lpoRandomGlobs(rand.New(rand.NewSource(seed)), lpoRandomPerSeed)})
	}

	var refused []string
	compared, nonEmpty, wispHits := 0, 0, 0
	for _, coll := range lpoCollations {
		var alterErr error
		for _, table := range []string{"labels", "wisp_labels"} {
			if _, err := store.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s MODIFY label VARCHAR(255) COLLATE %s NOT NULL", table, coll)); err != nil {
				alterErr = fmt.Errorf("%s: %w", table, err)
				break
			}
		}
		if alterErr != nil {
			refused = append(refused, fmt.Sprintf("%s (%v)", coll, alterErr))
			t.Logf("collation %s refused by the server: %v", coll, alterErr)
			continue
		}
		for _, set := range sets {
			for _, glob := range set.globs {
				if time.Now().After(deadline) {
					t.Fatalf("over the %s budget at %s, %s, glob %q", lpoBudget, coll, set.where, glob)
				}
				for _, read := range lpoReads {
					rw.set(glob)
					want, err := read.run(ctx, oracle, glob)
					if err != nil {
						t.Fatalf("%s %s %s glob %q: oracle: %v", set.where, coll, read.name, glob, err)
					}
					rewrites, bad := rw.stats()
					got, err := read.run(ctx, store, glob)
					if err != nil {
						t.Fatalf("%s %s %s glob %q: candidate: %v", set.where, coll, read.name, glob, err)
					}
					if len(bad) > 0 {
						t.Fatalf("%s %s %s glob %q: oracle rewrite met %v", set.where, coll, read.name, glob, bad)
					}
					if bare := !strings.ContainsAny(glob, `%_\|`) && utf8.ValidString(glob); bare && rewrites == 0 {
						t.Fatalf("%s %s %s glob %q: the read sent no bare label clause to rewrite, so the oracle ran the candidate", set.where, coll, read.name, glob)
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("%s %s %s glob %q: ids differ\ncandidate = %v\noracle    = %v", set.where, coll, read.name, glob, got, want)
					}
					compared++
					if len(want) > 0 {
						nonEmpty++
					}
					// wispHits counts the list reads' wisp rows only. The ready
					// reads filter wisps by the pattern too since bd-9mr2a; their
					// wisps half is proved by TestReadyLabelPatternFiltersWisps
					// (ready_label_pattern_wisps_test.go).
					if strings.HasPrefix(read.name, "Search") {
						for _, id := range want {
							if strings.HasPrefix(id, "lpo-wisp-") {
								wispHits++
								break
							}
						}
					}
				}
			}
		}
	}
	if len(refused) == len(lpoCollations) {
		t.Fatalf("the server refused every collation: %v", refused)
	}
	rw.mu.Lock()
	wispRewrites := rw.wispRewrites
	rw.mu.Unlock()
	if nonEmpty == 0 || wispHits == 0 || wispRewrites == 0 {
		t.Fatalf("vacuous comparison: %d of %d reads returned rows, %d list reads returned a wisp, %d wisp_labels clauses rewritten", nonEmpty, compared, wispHits, wispRewrites)
	}
	t.Logf("label pattern oracle: %d reads compared (%d with rows, %d list reads with a wisp, %d wisp_labels clauses rewritten), seeds %v x %d random globs + %d fixed, collations refused: %v, %s",
		compared, nonEmpty, wispHits, wispRewrites, lpoSeeds, lpoRandomPerSeed, len(lpoFixedGlobs), refused, time.Since(started).Round(time.Millisecond))
}

// TestLabelPatternOracleMemoryEngine runs the fixed globs on bd's pinned
// go-mysql-server memory engine, the engine whose bare-LIKE rewrite returns
// no rows for an escaped prefix (bd-ypak8): the candidate clause, taken from
// sqlbuild, against the oracle's, per collation the engine accepts.
func TestLabelPatternOracleMemoryEngine(t *testing.T) {
	t.Parallel()
	db := memory.NewDatabase("lpo")
	pro := memory.NewDBProvider(db)
	e := sqle.NewDefault(pro)
	ctx := gmssql.NewContext(context.Background(), gmssql.WithSession(memory.NewSession(gmssql.NewBaseSession(), pro)))
	ctx.SetCurrentDatabase("lpo")
	lit := func(s string) string {
		return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", "''") + "'"
	}
	query := func(q string) ([]string, error) {
		_, it, _, err := e.Query(ctx, q)
		if err != nil {
			return nil, err
		}
		rows, err := gmssql.RowIterToRows(ctx, it)
		if err != nil {
			return nil, err
		}
		out := []string{}
		for _, r := range rows {
			out = append(out, fmt.Sprint(r[0]))
		}
		return out, nil
	}
	mustQuery := func(q string) []string {
		t.Helper()
		out, err := query(q)
		if err != nil {
			t.Fatalf("%.200s: %v", q, err)
		}
		return out
	}

	var refused []string
	ran := 0
	for ci, coll := range lpoCollations {
		issues, labels := fmt.Sprintf("issues_%d", ci), fmt.Sprintf("labels_%d", ci)
		if _, err := query(fmt.Sprintf("CREATE TABLE %s (issue_id VARCHAR(255) NOT NULL, label VARCHAR(255) COLLATE %s NOT NULL, PRIMARY KEY (issue_id, label), INDEX idx_labels_label (label))", labels, coll)); err != nil {
			refused = append(refused, fmt.Sprintf("%s (%v)", coll, err))
			continue
		}
		mustQuery(fmt.Sprintf("CREATE TABLE %s (id VARCHAR(255) PRIMARY KEY)", issues))
		var ivals, lvals []string
		for i, l := range lpoEdgeLabels {
			id := fmt.Sprintf("e%02d", i)
			ivals = append(ivals, "("+lit(id)+")")
			lvals = append(lvals, "("+lit(id)+","+lit(l)+")")
		}
		mustQuery("INSERT INTO " + issues + " VALUES " + strings.Join(ivals, ","))
		mustQuery("INSERT INTO " + labels + " VALUES " + strings.Join(lvals, ","))
		tables := sqlbuild.FilterTables{Main: issues, Labels: labels}
		for _, glob := range lpoFixedGlobs {
			clauses, args, err := sqlbuild.BuildIssueFilterClauses("", types.IssueFilter{LabelPattern: glob}, tables)
			if err != nil || len(clauses) != 1 || len(args) != 1 {
				t.Fatalf("BuildIssueFilterClauses(%q) = %v, %v, %v; want one clause and one arg", glob, clauses, args, err)
			}
			cand := strings.Replace(clauses[0], "?", lit(args[0].(string)), 1)
			orac := fmt.Sprintf("id IN (SELECT issue_id FROM %s WHERE label LIKE %s ESCAPE '|')", labels, lit(lpoOracleLikePattern(glob)))
			got := mustQuery("SELECT id FROM " + issues + " WHERE " + cand + " ORDER BY id")
			want := mustQuery("SELECT id FROM " + issues + " WHERE " + orac + " ORDER BY id")
			if !reflect.DeepEqual(got, want) {
				t.Errorf("memory engine %s glob %q: candidate %s = %v, oracle %s = %v", coll, glob, cand, got, orac, want)
			}
			ran++
		}
	}
	if ran == 0 {
		t.Fatalf("the memory engine refused every collation: %v", refused)
	}
	t.Logf("memory engine: %d fixed-glob comparisons, collations refused: %v", ran, refused)
}
