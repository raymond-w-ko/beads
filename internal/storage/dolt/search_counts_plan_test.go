package dolt

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage/sqlbuild"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/workapi/storereader"
	"github.com/steveyegge/beads/issueops"
)

// TestSearchCountsByIDsPlanNeverScansADependencyTable (AC-6, GH#4370) pins the
// reverse-blocker (rdep_count) subquery of the by-IDs counts form on Dolt.
//
// Semantics: the by-IDs form splits the COALESCE(depends_on_issue_id,
// depends_on_wisp_id, depends_on_external) target into three disjoint UNION ALL
// legs per dependency table (sqlbuild.reverseBlockersByIDs, #7635). Over a graph whose blocks edges reach issue, wisp
// and external targets from both dependencies and wisp_dependencies, every
// by-IDs row (rdep_count included) must equal the predicate form's row, whose
// rc join still keys on the COALESCE.
//
// Plan: each leg filters one target column with IN (…), which the planner can
// seek through the target indexes of migrations 0041/0043 (dependencies) and
// 0021/0047 (wisp_dependencies). The old by-IDs rc filtered the COALESCE
// expression, which no index covers, so it read the whole dependency table on
// every bounded page. The EXPLAIN assertion requires an IndexedTableAccess on
// every target column of every dependency table the statement reads.
func TestSearchCountsByIDsPlanNeverScansADependencyTable(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	issues := []*types.Issue{
		{ID: "rb-a", Title: "issue blocker", Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask},
		{ID: "rb-b", Title: "issue blocked twice", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask},
		{ID: "rb-c", Title: "issue without edges", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask},
		{ID: "rb-w1", Title: "wisp blocker", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask, Ephemeral: true},
		{ID: "rb-w2", Title: "wisp blocked", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask, Ephemeral: true},
	}
	for _, iss := range issues {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("CreateIssue %s: %v", iss.ID, err)
		}
	}
	// blocks edges reaching every target column from both dependency tables,
	// plus a parent-child edge the reverse-blocker legs must not count.
	for _, d := range []*types.Dependency{
		{IssueID: "rb-b", DependsOnID: "rb-a", Type: types.DepBlocks},                // dependencies → issue
		{IssueID: "rb-b", DependsOnID: "rb-w1", Type: types.DepBlocks},               // dependencies → wisp
		{IssueID: "rb-c", DependsOnID: "external:other:cap", Type: types.DepBlocks},  // dependencies → external
		{IssueID: "rb-w2", DependsOnID: "rb-a", Type: types.DepBlocks},               // wisp_dependencies → issue
		{IssueID: "rb-w2", DependsOnID: "rb-w1", Type: types.DepBlocks},              // wisp_dependencies → wisp
		{IssueID: "rb-w2", DependsOnID: "external:other:cap", Type: types.DepBlocks}, // wisp_dependencies → external
		{IssueID: "rb-c", DependsOnID: "rb-a", Type: types.DepParentChild},           // not a blocker
	} {
		if err := store.AddDependency(ctx, d, "tester"); err != nil {
			t.Fatalf("AddDependency %s->%s: %v", d.IssueID, d.DependsOnID, err)
		}
	}

	hyd := sqlbuild.CountsHydration{}
	for _, fam := range []struct {
		name   string
		tables sqlbuild.FilterTables
		ids    []string
		rdeps  map[string]int
	}{
		{"issues", sqlbuild.IssuesFilterTables, []string{"rb-a", "rb-b", "rb-c"}, map[string]int{"rb-a": 2, "rb-b": 0, "rb-c": 0}},
		{"wisps", sqlbuild.WispsFilterTables, []string{"rb-w1", "rb-w2"}, map[string]int{"rb-w1": 2, "rb-w2": 0}},
	} {
		t.Run(fam.name, func(t *testing.T) {
			byIDsSQL, args := sqlbuild.SearchCountsSQL(fam.tables, fam.ids, "", "", "", true, hyd)
			predSQL, _ := sqlbuild.SearchCountsSQL(fam.tables, nil, "", "", "", true, hyd)

			want := make(map[string]countsRow)
			for _, r := range runCountsSQL(ctx, t, store.db, predSQL, nil, hyd) {
				want[r.id] = r
			}
			got := runCountsSQL(ctx, t, store.db, byIDsSQL, args, hyd)
			if len(got) != len(fam.ids) {
				t.Fatalf("by-IDs form returned %d rows, want %d", len(got), len(fam.ids))
			}
			for _, r := range got {
				if r != want[r.id] {
					t.Fatalf("by-IDs row differs from the COALESCE predicate form:\n by-IDs=%+v\n pred  =%+v", r, want[r.id])
				}
				if r.rdepCount != fam.rdeps[r.id] {
					t.Fatalf("%s rdep_count = %d, want %d", r.id, r.rdepCount, fam.rdeps[r.id])
				}
			}

			lits := make([]string, len(args))
			for i, a := range args {
				lits[i] = "'" + a.(string) + "'"
			}
			plan := explainPlan(t, ctx, store.db, literalizeParams(byIDsSQL, lits...))
			if !looksLikeDoltPlan(plan) {
				t.Skipf("EXPLAIN output not in a recognized Dolt plan format, skipping index assertion; plan=\n%s", plan)
			}
			for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
				for _, col := range []string{"depends_on_issue_id", "depends_on_wisp_id", "depends_on_external"} {
					if !indexedAccessOn(plan, depTable, col) {
						t.Errorf("no IndexedTableAccess on %s.%s: the reverse-blocker leg for that target scans the table.\nplan:\n%s", depTable, col, plan)
					}
				}
			}
		})
	}
}

// indexedAccessOn reports whether plan holds an IndexedTableAccess on table
// whose index column list includes col. Dolt prints an index by its column
// list, e.g. "index: [dependencies.type,dependencies.depends_on_issue_id]".
func indexedAccessOn(plan, table, col string) bool {
	lines := strings.Split(plan, "\n")
	for i, line := range lines {
		if !strings.Contains(line, "IndexedTableAccess("+table+")") {
			continue
		}
		for _, next := range lines[i+1:] {
			if strings.Contains(next, "TableAccess(") {
				break
			}
			if strings.Contains(next, "index: [") && strings.Contains(next, table+"."+col) {
				return true
			}
		}
	}
	return false
}

// stmtCapture records the queries (text and bound arguments) a store's
// sessions send while it is on.
type stmtCapture struct {
	mu    sync.Mutex
	on    bool
	stmts []capturedQuery
}

type capturedQuery struct {
	query string
	args  []driver.NamedValue
}

func (c *stmtCapture) add(query string, args []driver.NamedValue) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.on {
		c.stmts = append(c.stmts, capturedQuery{query, append([]driver.NamedValue(nil), args...)})
	}
}

func (c *stmtCapture) take(on bool) []capturedQuery {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.stmts
	c.stmts, c.on = nil, on
	return out
}

// captureConnector opens a live server's connection behind a captureConn.
type captureConnector struct {
	dsn string
	drv driver.Driver
	cap *stmtCapture
}

func (c captureConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.drv.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return captureConn{Conn: conn, cap: c.cap}, nil
}

func (c captureConnector) Driver() driver.Driver { return c.drv }

// captureConn records every query before passing it on. A driver that answers
// a parameterized query with driver.ErrSkip gets it again through Prepare; it
// is recorded once, here, with its arguments.
type captureConn struct {
	driver.Conn
	cap *stmtCapture
}

func (c captureConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.cap.add(query, args)
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c captureConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c captureConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c captureConn) Ping(ctx context.Context) error { return c.Conn.(driver.Pinger).Ping(ctx) }

func (c captureConn) CheckNamedValue(nv *driver.NamedValue) error {
	return c.Conn.(driver.NamedValueChecker).CheckNamedValue(nv)
}

// openCaptureStoreOnBranch is a second store on primary's test branch whose
// single session records its queries in capture.
func openCaptureStoreOnBranch(t *testing.T, ctx context.Context, primary *DoltStore, capture *stmtCapture) *DoltStore {
	t.Helper()
	store := openSecondStoreOnBranch(t, ctx, primary)
	db := sql.OpenDB(captureConnector{dsn: store.connStr, drv: store.db.Driver(), cap: capture})
	db.SetMaxOpenConns(1) // DOLT_CHECKOUT below is session-level
	_ = store.db.Close()
	store.db = db // closed with the store
	var branch string
	if err := primary.db.QueryRowContext(ctx, "SELECT active_branch()").Scan(&branch); err != nil {
		t.Fatalf("read test branch: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", branch); err != nil {
		t.Fatalf("check capture store out to %s: %v", branch, err)
	}
	return store
}

func namedLiterals(t *testing.T, args []driver.NamedValue) []string {
	t.Helper()
	vals := make([]any, len(args))
	for i, a := range args {
		vals[i] = a.Value
	}
	return sqlLiterals(t, vals)
}

// seedListPlanCorpus bulk-inserts the list plan corpus: n issues (70% closed,
// 20% open, 10% in_progress), three labels on half of them and nine on the
// other half (30,000 at n = 20,000) plus listLabel on exactly two, a blocks
// edge from every issue to the next (issue targets), a wisp target every
// 50th, an external target every 40th and a parent-child edge for 9 of 10
// rows (over 20,000 edges), and a comment on every 2nd issue.
func seedListPlanCorpus(t *testing.T, ctx context.Context, db *sql.DB, n int, listLabel string) map[string]int {
	t.Helper()
	id := func(i int) string { return fmt.Sprintf("lp-%06d", i) }
	var issues, labels, deps, comments, wisps []string
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		status, closedAt := "closed", "'2026-02-01 00:00:00'"
		switch i % 10 {
		case 1, 4:
			status, closedAt = "open", "NULL"
		case 7:
			status, closedAt = "in_progress", "NULL"
		}
		created := base.Add(time.Duration(i) * time.Second).Format("2006-01-02 15:04:05")
		issues = append(issues, fmt.Sprintf("('%s','list plan %d','','','','','%s',%d,'task','%s','%s',%s)", id(i), i, status, i%4, created, created, closedAt))
		per := 1
		if i%2 == 1 {
			per = 2
		}
		for k := 0; k < per; k++ {
			labels = append(labels, fmt.Sprintf("('%s','area:%d')", id(i), (i+k*7)%61))
		}
		if i == n/3 || i == 2*n/3 {
			labels = append(labels, fmt.Sprintf("('%s','%s')", id(i), listLabel))
		}
		if i+1 < n {
			deps = append(deps, fmt.Sprintf("('%036d','%s','%s',NULL,NULL,'blocks','tester')", len(deps), id(i), id(i+1)))
		}
		if i%10 != 0 {
			deps = append(deps, fmt.Sprintf("('%036d','%s','%s',NULL,NULL,'parent-child','tester')", len(deps), id(i), id(i/10*10)))
		}
		if i%50 == 3 {
			deps = append(deps, fmt.Sprintf("('%036d','%s',NULL,'lp-w%04d',NULL,'blocks','tester')", len(deps), id(i), i%500))
		}
		if i%40 == 5 {
			deps = append(deps, fmt.Sprintf("('%036d','%s',NULL,NULL,'external:lp:%d','blocks','tester')", len(deps), id(i), i))
		}
		if i%2 == 0 {
			comments = append(comments, fmt.Sprintf("('%036d','%s','tester','note %d','%s')", len(comments), id(i), i, created))
		}
	}
	for j := 0; j < 500; j++ {
		wisps = append(wisps, fmt.Sprintf("('lp-w%04d','list plan wisp %d','open')", j, j))
	}
	exec := func(q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed %.120s: %v", q, err)
		}
	}
	bulk := func(head string, rows []string) {
		t.Helper()
		for s := 0; s < len(rows); s += 1000 {
			exec(head + strings.Join(rows[s:min(s+1000, len(rows))], ","))
		}
	}
	exec("SET FOREIGN_KEY_CHECKS=0")
	defer exec("SET FOREIGN_KEY_CHECKS=1")
	bulk("INSERT INTO issues (id,title,description,design,acceptance_criteria,notes,status,priority,issue_type,created_at,updated_at,closed_at) VALUES ", issues)
	bulk("INSERT INTO wisps (id,title,status) VALUES ", wisps)
	bulk("INSERT INTO labels (issue_id,label) VALUES ", labels)
	bulk("INSERT INTO dependencies (id,issue_id,depends_on_issue_id,depends_on_wisp_id,depends_on_external,type,created_by) VALUES ", deps)
	bulk("INSERT INTO comments (id,issue_id,author,text,created_at) VALUES ", comments)
	return map[string]int{"issues": len(issues), "labels": len(labels), "dependencies": len(deps), "comments": len(comments), "wisps": len(wisps)}
}

// sideTableReads lists every read of a side table (labels, comments,
// dependencies, wisp_dependencies) in plan that is not confined to the page:
// a bare Table scan, or an IndexedTableAccess with no lookup keys whose
// ranges name none of the page's ids (a whole index or a whole type range).
func sideTableReads(plan string, pageIDs []string) []string {
	var out []string
	lines := strings.Split(plan, "\n")
	for _, table := range []string{"labels", "comments", "dependencies", "wisp_dependencies"} {
		if hasBareTableScanOf(plan, table) {
			out = append(out, "bare Table scan of "+table)
		}
		for k, l := range lines {
			if !strings.HasSuffix(strings.TrimSpace(l), "IndexedTableAccess("+table+")") {
				continue
			}
			var attrs []string
			confined := false
			for _, a := range lines[k+1:] {
				s := strings.TrimSpace(strings.TrimLeft(a, " │├└─"))
				if !(strings.HasPrefix(s, "index:") || strings.HasPrefix(s, "filters:") ||
					strings.HasPrefix(s, "columns:") || strings.HasPrefix(s, "keys:")) {
					break
				}
				attrs = append(attrs, s)
				if strings.HasPrefix(s, "keys:") {
					confined = true
				}
				if strings.HasPrefix(s, "filters:") {
					for _, id := range pageIDs {
						if strings.Contains(s, id) {
							confined = true
						}
					}
				}
			}
			if !confined {
				out = append(out, fmt.Sprintf("%s (plan line %d): %s", table, k+1, strings.Join(attrs, "; ")))
			}
		}
	}
	return out
}

// TestSearchCountsListPagesReadOnlyThePage pins the plans of the two list
// reads bd-53atb measured, at 20,000 issues, 30,000 labels, over 20,000
// dependencies and 10,000 comments: `bd list -n 0 -l <label> --all --json`
// with two matches, and the 51-row page of `bd list --status open,in_progress
// --json --brief`. Each request is built the way bd list builds it
// (storereader.List, so workapi.BuildListFilter and the has-more probe row)
// and the store's statements are captured on its session, not rebuilt here.
//
// Before ids-first (bd-c43b7, upstream #8535), both reads ran the predicate
// form, whose six aggregate LEFT JOINs each grouped a whole side table:
// labels, comments and dependencies by bare Table scans, the counts by whole
// type ranges. Now each counts statement must read only the page: no side
// table read whose range is not the page's ids, and every reverse-blocker
// branch seeking its own target column.
func TestSearchCountsListPagesReadOnlyThePage(t *testing.T) {
	primary, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	var version string
	if err := primary.db.QueryRowContext(ctx, "SELECT dolt_version()").Scan(&version); err != nil {
		t.Fatalf("dolt_version: %v", err)
	}
	const listLabel = "lp:project"
	start := time.Now()
	counts := seedListPlanCorpus(t, ctx, primary.db, 20000, listLabel)
	t.Logf("dolt_version() = %s; seeded %v in %v", version, counts, time.Since(start).Round(time.Millisecond))
	for table, min := range map[string]int{"issues": 20000, "labels": 30000, "dependencies": 20000, "comments": 10000} {
		if counts[table] < min {
			t.Fatalf("seeded %d %s, want at least %d", counts[table], table, min)
		}
	}
	if _, err := primary.db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'list plan seed')"); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	capture := &stmtCapture{}
	store := openCaptureStoreOnBranch(t, ctx, primary, capture)
	reader, err := storereader.New(store)
	if err != nil {
		t.Fatalf("storereader.New: %v", err)
	}
	zero, fifty := 0, 50
	for _, c := range []struct {
		name     string
		req      issueops.ListRequest
		wantRows int
	}{
		{"bd list -n 0 -l " + listLabel + " --all --json", issueops.ListRequest{Labels: []string{listLabel}, AllFlag: true, Limit: &zero}, 2},
		{"bd list --status open,in_progress --json --brief", issueops.ListRequest{Status: "open,in_progress", Brief: true, Limit: &fifty}, 50},
	} {
		t.Run(c.name, func(t *testing.T) {
			capture.take(true)
			page, err := reader.List(ctx, c.req)
			stmts := capture.take(false)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(page.Items) != c.wantRows {
				t.Fatalf("List returned %d rows, want %d", len(page.Items), c.wantRows)
			}
			var countsStmts []capturedQuery
			for _, st := range stmts {
				if strings.Contains(st.query, "AS rdep_count") {
					countsStmts = append(countsStmts, st)
				}
			}
			if len(countsStmts) == 0 {
				t.Fatalf("captured no counts statement among %d queries", len(stmts))
			}
			var pageIDs []string
			for _, it := range page.Items {
				pageIDs = append(pageIDs, it.Issue.ID)
			}
			for i, st := range countsStmts {
				plan := explainPlan(t, ctx, store.db, literalizeParams(st.query, namedLiterals(t, st.args)...))
				if !looksLikeDoltPlan(plan) {
					t.Fatalf("EXPLAIN output not in a recognized Dolt plan format:\n%s", plan)
				}
				if reads := sideTableReads(plan, pageIDs); len(reads) > 0 {
					t.Errorf("counts statement %d of %d reads side tables past the page: %v\nSQL: %s\nplan:\n%s", i+1, len(countsStmts), reads, st.query, plan)
					continue
				}
				for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
					if !strings.Contains(st.query, "FROM "+depTable+" WHERE type = 'blocks'") {
						continue
					}
					for _, col := range []string{"depends_on_issue_id", "depends_on_wisp_id", "depends_on_external"} {
						if !indexedAccessOn(plan, depTable, col) {
							t.Errorf("counts statement %d: no IndexedTableAccess on %s.%s for its reverse-blocker branch.\nplan:\n%s", i+1, depTable, col, plan)
						}
					}
				}
			}
			if !t.Failed() {
				t.Logf("%d queries, %d counts statement(s): every side-table read confined to the %d page ids, every rc branch seeks its column", len(stmts), len(countsStmts), len(pageIDs))
			}
		})
	}
}
