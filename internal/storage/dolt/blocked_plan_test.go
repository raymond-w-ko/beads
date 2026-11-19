package dolt

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/steveyegge/beads/internal/storage/issueops"
)

// recordedBlockedStmt is one mark or unmark statement the recompute path sent,
// with its bound arguments.
type recordedBlockedStmt struct {
	sql  string
	args []any
}

// blockedStmtRecorder is an issueops.DBTX that records the batched is_blocked
// mark/unmark statements RecomputeIsBlockedInTxWithResult sends and forwards
// every other statement. With dryRun set it does not run the recorded
// statements and reports 0 rows changed, so the path stops after one pass and
// the database is left as it was: the test reads the exact production text
// without its side effects.
type blockedStmtRecorder struct {
	issueops.DBTX
	dryRun bool
	stmts  []recordedBlockedStmt
}

func isBlockedRecomputeStmt(query string) bool {
	q := strings.TrimSpace(query)
	return (strings.HasPrefix(q, "UPDATE issues i SET i.is_blocked = ") ||
		strings.HasPrefix(q, "UPDATE wisps w SET w.is_blocked = ")) &&
		strings.Contains(q, "d.issue_id IN (")
}

func (r *blockedStmtRecorder) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if isBlockedRecomputeStmt(query) {
		r.stmts = append(r.stmts, recordedBlockedStmt{sql: query, args: append([]any(nil), args...)})
		if r.dryRun {
			return driver.RowsAffected(0), nil
		}
	}
	return r.DBTX.ExecContext(ctx, query, args...)
}

// capturedBlockedStmts runs the production recompute for one batch in dry-run
// mode and returns the mark and unmark statements it sends for table
// ("issues" or "wisps"). The path's own batching applies, so ids must fit one
// batch (at most 200).
func capturedBlockedStmts(t *testing.T, ctx context.Context, tx issueops.DBTX, table string, ids []string) (mark, unmark recordedBlockedStmt) {
	t.Helper()
	rec := &blockedStmtRecorder{DBTX: tx, dryRun: true}
	var issueIDs, wispIDs []string
	if table == "wisps" {
		wispIDs = ids
	} else {
		issueIDs = ids
	}
	if _, err := issueops.RecomputeIsBlockedInTxWithResult(ctx, rec, issueIDs, wispIDs); err != nil {
		t.Fatalf("recompute (recording) %s %v: %v", table, ids, err)
	}
	if len(rec.stmts) != 2 {
		t.Fatalf("recompute for %d %s ids sent %d mark/unmark statements, want 2 (one batch, one pass)", len(ids), table, len(rec.stmts))
	}
	mark, unmark = rec.stmts[0], rec.stmts[1]
	if !strings.Contains(mark.sql, ".is_blocked = 1,") || !strings.Contains(unmark.sql, ".is_blocked = 0,") {
		t.Fatalf("recorded statements are not mark then unmark:\n%s\n---\n%s", mark.sql, unmark.sql)
	}
	return mark, unmark
}

// planDepth is the column of a plan-tree line's first character after the
// tree drawing (spaces, │ ├ └ ─): a node's children sit deeper than it.
func planDepth(line string) int {
	n := 0
	for _, r := range line {
		if r != ' ' && r != '│' && r != '├' && r != '└' && r != '─' {
			return n
		}
		n++
	}
	return utf8.RuneCountInString(line)
}

// enclosingJoin returns the nearest join node above plan line k, or "".
func enclosingJoin(lines []string, k int) string {
	cur := planDepth(lines[k])
	for j := k - 1; j >= 0; j-- {
		d := planDepth(lines[j])
		if d >= cur {
			continue
		}
		node := strings.TrimSpace(strings.TrimLeft(lines[j], " │├└─"))
		if strings.Contains(node, "Join") {
			return node
		}
		cur = d
	}
	return ""
}

// joinedLeg is one of the four joined legs of the scoped should-be-blocked
// union: the alias and table it looks up and the dependency column it keys on.
type joinedLeg struct {
	name, alias, table, keyCol string
}

var blockedJoinedLegs = []joinedLeg{
	{"leg 1 (blocks -> issue)", "t", "issues", "d.depends_on_issue_id"},
	{"leg 2 (blocks -> wisp)", "t", "wisps", "d.depends_on_wisp_id"},
	{"leg 3 (parent-child -> issue)", "p", "issues", "d.depends_on_issue_id"},
	{"leg 4 (parent-child -> wisp)", "p", "wisps", "d.depends_on_wisp_id"},
}

// assertJoinedLegsLookUp fails unless, for each joined leg, the plan holds
// TableAlias(<alias>) over IndexedTableAccess(<table>) keyed on the leg's
// dependency column, directly under a LookupJoin.
func assertJoinedLegsLookUp(t *testing.T, what, plan string) {
	t.Helper()
	lines := strings.Split(plan, "\n")
	for _, leg := range blockedJoinedLegs {
		found, joins := false, []string{}
		for k := 0; k+1 < len(lines); k++ {
			if !strings.HasSuffix(strings.TrimSpace(lines[k]), "TableAlias("+leg.alias+")") ||
				!strings.HasSuffix(strings.TrimSpace(lines[k+1]), "IndexedTableAccess("+leg.table+")") {
				continue
			}
			keyed := false
			for _, attr := range planAttrs(lines, k+1) {
				if strings.HasSuffix(attr, "keys: "+leg.keyCol) {
					keyed = true
				}
			}
			join := enclosingJoin(lines, k)
			joins = append(joins, fmt.Sprintf("%s (keyed=%v)", join, keyed))
			if keyed && strings.HasPrefix(join, "LookupJoin") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: %s is not a lookup join keyed on %s; joins seen over TableAlias(%s)/%s: %v\nplan:\n%s",
				what, leg.name, leg.keyCol, leg.alias, leg.table, joins, plan)
		}
	}
}

// planAttrs returns the attribute lines (index, filters, columns, keys) of the
// plan node at line k.
func planAttrs(lines []string, k int) []string {
	var attrs []string
	for _, l := range lines[k+1:] {
		s := strings.TrimSpace(strings.TrimLeft(l, " │├└─"))
		if !(strings.HasPrefix(s, "index:") || strings.HasPrefix(s, "filters:") ||
			strings.HasPrefix(s, "columns:") || strings.HasPrefix(s, "keys:")) {
			break
		}
		attrs = append(attrs, s)
	}
	return attrs
}

// fullRangeFilter matches one range set of a filters line whose leading
// column is unbounded: "{[NULL, ∞)" opens the set.
var fullRangeFilter = regexp.MustCompile(`\{\[NULL, ∞\)`)

// fullRangeScans lists every IndexedTableAccess(<table>) node in plan that
// reads its index without a bound on the leading column: no lookup keys and a
// filters range set opening with [NULL, ∞) (or no filters at all).
func fullRangeScans(plan, table string) []string {
	lines := strings.Split(plan, "\n")
	var out []string
	for k, l := range lines {
		if !strings.HasSuffix(strings.TrimSpace(l), "IndexedTableAccess("+table+")") {
			continue
		}
		attrs := planAttrs(lines, k)
		keyed, bounded := false, false
		for _, a := range attrs {
			if strings.HasPrefix(a, "keys:") {
				keyed = true
			}
			if strings.HasPrefix(a, "filters:") && !fullRangeFilter.MatchString(a) {
				bounded = true
			}
		}
		if !keyed && !bounded {
			out = append(out, fmt.Sprintf("line %d: %s", k+1, strings.Join(attrs, "; ")))
		}
	}
	if hasBareTableScanOf(plan, table) {
		out = append(out, "bare Table scan of "+table)
	}
	return out
}

// TestBlockedRecomputePlanUsesLookupJoins pins the plan of the scoped
// is_blocked recompute a create or a close runs for one id
// (RecomputeIsBlockedInTxWithResult's batched mark and unmark statements).
// With no table statistics, which is Dolt's state right after a commit until
// its background refresh lands, legs 1-4 of the scoped should-be-blocked union
// planned as merge joins over full index scans of dependencies and issues: a
// one-id recompute read the whole graph, 1.4 s per statement at 100k issues
// (bd-eb02m). The JOIN_ORDER/LOOKUP_JOIN hints (upstream ba12ffa74) pin each
// leg to drive from the batch's own dependency rows and look the target up by
// primary key.
//
// The statements are the ones the path sends, captured by a dry-run DBTX, not
// a copy of the builder's text.
func TestBlockedRecomputePlanUsesLookupJoins(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	db := store.db

	var version string
	if err := db.QueryRowContext(ctx, "SELECT dolt_version()").Scan(&version); err != nil {
		t.Fatalf("dolt_version: %v", err)
	}
	t.Logf("dolt_version() = %s", version)

	const nIssues, nWisps = 20000, 500
	start := time.Now()
	nDeps := seedBlockedPlanGraph(t, ctx, db, nIssues, nWisps)
	t.Logf("seeded %d issues, %d wisps, %d dependencies in %v", nIssues, nWisps, nDeps, time.Since(start).Round(time.Millisecond))
	if nDeps < 25000 {
		t.Fatalf("seeded %d dependencies, want at least 25000", nDeps)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'blocked plan seed')"); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	for _, q := range []string{"CALL dolt_stats_stop()", "CALL dolt_stats_purge()"} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// An open issue with a blocks edge, a parent-child parent and an edge to a
	// wisp: the shape a create with dependencies or a close fan-out recomputes.
	id := planIssueID(301)
	mark, unmark := capturedBlockedStmts(t, ctx, db, "issues", []string{id})
	for _, st := range []struct {
		name string
		stmt recordedBlockedStmt
	}{{"mark", mark}, {"unmark", unmark}} {
		plan := explainPlan(t, ctx, db, literalizeParams(st.stmt.sql, sqlLiterals(t, st.stmt.args)...))
		if !looksLikeDoltPlan(plan) {
			t.Fatalf("%s: EXPLAIN output not in a recognized Dolt plan format:\n%s", st.name, plan)
		}
		what := st.name + " " + id
		assertJoinedLegsLookUp(t, what, plan)
		for _, table := range []string{"dependencies", "issues"} {
			if scans := fullRangeScans(plan, table); len(scans) > 0 {
				t.Errorf("%s: full-range scan of %s: %v\nSQL: %s\nplan:\n%s", what, table, scans, st.stmt.sql, plan)
			}
		}
		if t.Failed() {
			return
		}
		t.Logf("%s plan: 4 joined legs are LookupJoin, no full-range scan of dependencies or issues", what)
	}
}

func planIssueID(i int) string { return fmt.Sprintf("pl-i%06d", i) }
func planWispID(i int) string  { return fmt.Sprintf("pl-w%05d", i) }

// seedBlockedPlanGraph inserts a fleet-shaped graph by bulk SQL: n issues,
// 70% closed; a blocks edge from every 2nd issue to the next, a parent-child
// edge for 9 of 10 issues (an epic every 10th), a waits-for gate every 500th;
// nw wisps with wisp->wisp parent-child and wisp->issue blocks edges, and an
// issue->wisp blocks edge every 40th issue. It returns the number of
// dependency rows (both tables).
func seedBlockedPlanGraph(t *testing.T, ctx context.Context, db *sql.DB, n, nw int) int {
	t.Helper()
	var issues, wisps, deps, wdeps []string
	edge := func(list *[]string, src, issueTarget, wispTarget, typ, meta string) {
		it, wt := "NULL", "NULL"
		if issueTarget != "" {
			it = "'" + issueTarget + "'"
		}
		if wispTarget != "" {
			wt = "'" + wispTarget + "'"
		}
		*list = append(*list, fmt.Sprintf("('%036d','%s',%s,%s,'%s','tester','%s')",
			len(deps)+len(wdeps), src, it, wt, typ, meta))
	}
	for i := 0; i < n; i++ {
		status, closedAt := "closed", "'2026-01-01 00:00:00'"
		switch i % 10 {
		case 1, 7, 8:
			status, closedAt = "open", "NULL"
		case 9:
			status, closedAt = "in_progress", "NULL"
		}
		issues = append(issues, fmt.Sprintf("('%s','plan fixture %d','','','','','%s',%s)", planIssueID(i), i, status, closedAt))
		if i%2 == 1 && i+1 < n {
			edge(&deps, planIssueID(i), planIssueID(i+1), "", "blocks", "{}")
		}
		if i%10 != 0 {
			edge(&deps, planIssueID(i), planIssueID(i/10*10), "", "parent-child", "{}")
		}
		if i%500 == 3 && i+20 < n {
			edge(&deps, planIssueID(i), planIssueID((i+20)/10*10), "", "waits-for", `{"gate":"all-children"}`)
		}
		if i%40 == 21 {
			edge(&deps, planIssueID(i), "", planWispID(i%nw), "blocks", "{}")
		}
	}
	for j := 0; j < nw; j++ {
		status := "closed"
		if j%4 == 0 {
			status = "open"
		}
		wisps = append(wisps, fmt.Sprintf("('%s','plan wisp %d','%s')", planWispID(j), j, status))
		if j%10 != 0 {
			edge(&wdeps, planWispID(j), "", planWispID(j/10*10), "parent-child", "{}")
		}
		if j%5 == 1 {
			edge(&wdeps, planWispID(j), planIssueID((j*37+5)%n), "", "blocks", "{}")
		}
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
	bulk("INSERT INTO issues (id,title,description,design,acceptance_criteria,notes,status,closed_at) VALUES ", issues)
	bulk("INSERT INTO wisps (id,title,status) VALUES ", wisps)
	bulk("INSERT INTO dependencies (id,issue_id,depends_on_issue_id,depends_on_wisp_id,type,created_by,metadata) VALUES ", deps)
	bulk("INSERT INTO wisp_dependencies (id,issue_id,depends_on_issue_id,depends_on_wisp_id,type,created_by,metadata) VALUES ", wdeps)
	return len(deps) + len(wdeps)
}
