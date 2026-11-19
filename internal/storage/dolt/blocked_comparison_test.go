package dolt

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage/issueops"
)

// The oracle: the waits-for gate, the scoped should-be-blocked union and the
// batched mark/unmark templates exactly as they were before the lookup-join
// hints and the per-column gate split (fork 11349bc2f, before bd-w2vt5). The
// patched forms must decide the same ids on every graph.

const unhintedWaitsForGateSQL = `
		(
		  (
		    EXISTS (
		      SELECT 1 FROM dependencies cd JOIN issues child ON child.id = cd.issue_id
		      WHERE cd.type = 'parent-child'
		        AND ((d.depends_on_issue_id IS NOT NULL AND cd.depends_on_issue_id = d.depends_on_issue_id)
		          OR (d.depends_on_wisp_id IS NOT NULL AND cd.depends_on_wisp_id = d.depends_on_wisp_id))
		        AND child.status <> 'closed' AND child.status <> 'pinned'
		    )
		    OR EXISTS (
		      SELECT 1 FROM wisp_dependencies cd JOIN wisps child ON child.id = cd.issue_id
		      WHERE cd.type = 'parent-child'
		        AND ((d.depends_on_issue_id IS NOT NULL AND cd.depends_on_issue_id = d.depends_on_issue_id)
		          OR (d.depends_on_wisp_id IS NOT NULL AND cd.depends_on_wisp_id = d.depends_on_wisp_id))
		        AND child.status <> 'closed' AND child.status <> 'pinned'
		    )
		  )
		  AND NOT (
		    COALESCE(JSON_UNQUOTE(JSON_EXTRACT(d.metadata, '$.gate')), 'all-children') = 'any-children'
		    AND (
		      EXISTS (
		        SELECT 1 FROM dependencies cd JOIN issues child ON child.id = cd.issue_id
		        WHERE cd.type = 'parent-child'
		          AND ((d.depends_on_issue_id IS NOT NULL AND cd.depends_on_issue_id = d.depends_on_issue_id)
		            OR (d.depends_on_wisp_id IS NOT NULL AND cd.depends_on_wisp_id = d.depends_on_wisp_id))
		          AND child.status = 'closed'
		      )
		      OR EXISTS (
		        SELECT 1 FROM wisp_dependencies cd JOIN wisps child ON child.id = cd.issue_id
		        WHERE cd.type = 'parent-child'
		          AND ((d.depends_on_issue_id IS NOT NULL AND cd.depends_on_issue_id = d.depends_on_issue_id)
		            OR (d.depends_on_wisp_id IS NOT NULL AND cd.depends_on_wisp_id = d.depends_on_wisp_id))
		          AND child.status = 'closed'
		      )
		    )
		  )
		)
		OR (
		  COALESCE(JSON_UNQUOTE(JSON_EXTRACT(d.metadata, '$.also_blocks')), 'false') = 'true'
		  AND (
		    EXISTS (
		      SELECT 1 FROM issues sp
		      WHERE sp.id = d.depends_on_issue_id
		        AND sp.status <> 'closed' AND sp.status <> 'pinned'
		    )
		    OR EXISTS (
		      SELECT 1 FROM wisps sp
		      WHERE sp.id = d.depends_on_wisp_id
		        AND sp.status <> 'closed' AND sp.status <> 'pinned'
		    )
		  )
		)
`

func unhintedUnionScopedSQL(depTable, scope string) string {
	return fmt.Sprintf(`
		SELECT d.issue_id FROM %[1]s d
		JOIN issues t ON t.id = d.depends_on_issue_id
		WHERE d.issue_id IS NOT NULL %[3]s
		  AND (d.type = 'blocks' OR d.type = 'conditional-blocks')
		  AND t.status <> 'closed' AND t.status <> 'pinned'
		UNION
		SELECT d.issue_id FROM %[1]s d
		JOIN wisps t ON t.id = d.depends_on_wisp_id
		WHERE d.issue_id IS NOT NULL %[3]s
		  AND (d.type = 'blocks' OR d.type = 'conditional-blocks')
		  AND t.status <> 'closed' AND t.status <> 'pinned'
		UNION
		SELECT d.issue_id FROM %[1]s d
		JOIN issues p ON p.id = d.depends_on_issue_id
		WHERE d.issue_id IS NOT NULL %[3]s
		  AND d.type = 'parent-child'
		  AND p.is_blocked = 1
		UNION
		SELECT d.issue_id FROM %[1]s d
		JOIN wisps p ON p.id = d.depends_on_wisp_id
		WHERE d.issue_id IS NOT NULL %[3]s
		  AND d.type = 'parent-child'
		  AND p.is_blocked = 1
		UNION
		SELECT d.issue_id FROM (
		  SELECT DISTINCT d.issue_id, d.depends_on_issue_id, d.depends_on_wisp_id, d.metadata
		  FROM %[1]s d
		  WHERE d.issue_id IS NOT NULL %[3]s
		    AND d.type = 'waits-for'
		) d
		WHERE (%[2]s)
	`, depTable, unhintedWaitsForGateSQL, scope)
}

const unhintedBatchScope = "AND d.issue_id IN (%s)"

func unhintedMarkTemplate(table, alias, depTable string) string {
	return fmt.Sprintf(`
		UPDATE %[1]s %[2]s SET %[2]s.is_blocked = 1, %[2]s.updated_at = %[2]s.updated_at
		WHERE %[2]s.id IN (%%s)
		  AND %[2]s.is_blocked = 0
		  AND %[2]s.status <> 'closed' AND %[2]s.status <> 'pinned'
		  AND %[2]s.id IN (%[3]s)
	`, table, alias, unhintedUnionScopedSQL(depTable, unhintedBatchScope))
}

func unhintedUnmarkTemplate(table, alias, depTable string) string {
	return fmt.Sprintf(`
		UPDATE %[1]s %[2]s SET %[2]s.is_blocked = 0, %[2]s.updated_at = %[2]s.updated_at
		WHERE %[2]s.id IN (%%s)
		  AND %[2]s.is_blocked = 1
		  AND ( %[2]s.status = 'closed' OR %[2]s.status = 'pinned'
		        OR %[2]s.id NOT IN (%[3]s) )
	`, table, alias, unhintedUnionScopedSQL(depTable, unhintedBatchScope))
}

// fillBatch fills every %s of a batched template with ids' placeholders and
// repeats the ids once per occurrence, as issueops.expandBatchTemplate does.
func fillBatch(tmpl string, ids []string) (string, []any) {
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	n := strings.Count(tmpl, "%s")
	fills := make([]any, n)
	args := make([]any, 0, n*len(ids))
	for k := range fills {
		fills[k] = ph
		for _, id := range ids {
			args = append(args, id)
		}
	}
	return fmt.Sprintf(tmpl, fills...), args
}

// unhintedRecompute is RecomputeIsBlockedInTxWithResult's loop over the
// oracle templates: mark then unmark per 200-id batch, issues then wisps,
// until a pass changes nothing.
func unhintedRecompute(ctx context.Context, tx *sql.Tx, issueIDs, wispIDs []string) error {
	if len(issueIDs) == 0 && len(wispIDs) == 0 {
		return nil
	}
	pass := func(table, alias, depTable string, ids []string) (int64, error) {
		var changed int64
		for s := 0; s < len(ids); s += 200 {
			batch := ids[s:min(s+200, len(ids))]
			for _, tmpl := range []string{unhintedMarkTemplate(table, alias, depTable), unhintedUnmarkTemplate(table, alias, depTable)} {
				q, args := fillBatch(tmpl, batch)
				res, err := tx.ExecContext(ctx, q, args...)
				if err != nil {
					return changed, err
				}
				n, err := res.RowsAffected()
				if err != nil {
					return changed, err
				}
				changed += n
			}
		}
		return changed, nil
	}
	for round := 0; round < 100; round++ {
		a, err := pass("issues", "i", "dependencies", issueIDs)
		if err != nil {
			return err
		}
		b, err := pass("wisps", "w", "wisp_dependencies", wispIDs)
		if err != nil {
			return err
		}
		if a+b == 0 {
			return nil
		}
	}
	return fmt.Errorf("oracle recompute did not converge in 100 passes")
}

// patchedUnionFromMark cuts the scoped should-be-blocked union out of a
// recorded production mark statement (`... AND <alias>.id IN (<union>)`) with
// its bound arguments: every group after the outer batch filter.
func patchedUnionFromMark(t *testing.T, mark recordedBlockedStmt, alias string, batch int) (string, []any) {
	t.Helper()
	marker := alias + ".id IN ("
	i := strings.LastIndex(mark.sql, marker)
	if i < 0 {
		t.Fatalf("mark statement has no %s<union>):\n%s", marker, mark.sql)
	}
	body := strings.TrimSpace(mark.sql[i+len(marker):])
	if !strings.HasSuffix(body, ")") {
		t.Fatalf("mark statement does not end in %s<union>):\n%s", marker, mark.sql)
	}
	union := strings.TrimSuffix(body, ")")
	args := mark.args[batch:]
	if got := strings.Count(union, "?"); got != len(args) {
		t.Fatalf("cut union binds %d placeholders, %d args remain:\n%s", got, len(args), union)
	}
	return union, args
}

// rowQuerier is the read half of *sql.DB and *sql.Tx.
type rowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// cmpGraph is one generated graph: its ids per plane.
type cmpGraph struct {
	issues, wisps []string
	edges         int
}

var cmpStatuses = []string{"open", "open", "in_progress", "closed", "closed", "closed", "pinned", "deferred", "review"}

// genBlockedGraph replaces the four tables' rows with a random graph built
// from the tokens the should-be-blocked rules read: blocks and
// conditional-blocks edges to open and closed issues and wisps, external and
// dangling targets, parent-child trees across both planes with a cycle on
// each, waits-for gates (all-children, any-children, no gate key,
// also_blocks) on spawners in either column with issue and wisp children,
// statuses closed, pinned, deferred and a custom one, and a random starting
// is_blocked so both mark and unmark have work.
func genBlockedGraph(t *testing.T, ctx context.Context, db *sql.DB, rng *rand.Rand) cmpGraph {
	t.Helper()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("generate %.120s: %v", q, err)
		}
	}
	exec("SET FOREIGN_KEY_CHECKS=0")
	defer exec("SET FOREIGN_KEY_CHECKS=1")
	for _, tb := range []string{"wisp_dependencies", "dependencies", "wisps", "issues"} {
		exec("DELETE FROM " + tb)
	}

	var g cmpGraph
	nI, nW := 205+rng.Intn(60), 30+rng.Intn(40)
	var issueRows, wispRows []string
	for i := 0; i < nI; i++ {
		id := fmt.Sprintf("cq-i%03d", i)
		g.issues = append(g.issues, id)
		issueRows = append(issueRows, fmt.Sprintf("('%s','c','','','','','%s',%d)", id, cmpStatuses[rng.Intn(len(cmpStatuses))], rng.Intn(3)/2))
	}
	for j := 0; j < nW; j++ {
		id := fmt.Sprintf("cq-w%03d", j)
		g.wisps = append(g.wisps, id)
		wispRows = append(wispRows, fmt.Sprintf("('%s','c','%s',%d)", id, cmpStatuses[rng.Intn(len(cmpStatuses))], rng.Intn(3)/2))
	}
	exec("INSERT INTO issues (id,title,description,design,acceptance_criteria,notes,status,is_blocked) VALUES " + strings.Join(issueRows, ","))
	exec("INSERT INTO wisps (id,title,status,is_blocked) VALUES " + strings.Join(wispRows, ","))

	// A node is a plane ("i" or "w") and an id.
	type node struct{ plane, id string }
	pickIssue := func() node { return node{"i", g.issues[rng.Intn(nI)]} }
	pickWisp := func() node { return node{"w", g.wisps[rng.Intn(nW)]} }
	pickAny := func() node {
		if rng.Intn(4) == 0 {
			return pickWisp()
		}
		return pickIssue()
	}
	seen := map[string]bool{}
	var depRows, wdepRows []string
	children := map[node][]node{}
	add := func(src node, tgtKind, tgt, typ, meta string) bool {
		key := src.plane + "|" + src.id + "|" + tgtKind + "|" + tgt
		if seen[key] {
			return false
		}
		seen[key] = true
		it, wt, ext := "NULL", "NULL", "NULL"
		switch tgtKind {
		case "i":
			it = "'" + tgt + "'"
		case "w":
			wt = "'" + tgt + "'"
		default:
			ext = "'" + tgt + "'"
		}
		row := fmt.Sprintf("('%036d','%s',%s,%s,%s,'%s','tester','%s')", g.edges, src.id, it, wt, ext, typ, meta)
		g.edges++
		if src.plane == "i" {
			depRows = append(depRows, row)
		} else {
			wdepRows = append(wdepRows, row)
		}
		return true
	}
	// target picks a blocks target: an issue, a wisp, external, or dangling.
	target := func() (string, string) {
		switch r := rng.Intn(20); {
		case r < 12:
			return "i", pickIssue().id
		case r < 16:
			return "w", pickWisp().id
		case r < 18:
			return "x", fmt.Sprintf("external:cq:%d", rng.Intn(5))
		case r < 19:
			return "i", fmt.Sprintf("cq-i-gone-%d", rng.Intn(5))
		default:
			return "w", fmt.Sprintf("cq-w-gone-%d", rng.Intn(5))
		}
	}
	blockTypes := []string{"blocks", "blocks", "conditional-blocks", "related", "discovered-from"}
	for k := 0; k < nI/2+nW/2; k++ {
		kind, tgt := target()
		add(pickAny(), kind, tgt, blockTypes[rng.Intn(len(blockTypes))], "{}")
	}
	// Blocks cycle.
	a, b := pickIssue(), pickIssue()
	add(a, "i", b.id, "blocks", "{}")
	add(b, "i", a.id, "blocks", "{}")

	// Parent-child: each node takes, with probability 1/2, a parent from
	// either plane (chains form through earlier picks), sometimes dangling.
	parent := func(child node) {
		var p node
		switch r := rng.Intn(10); {
		case r < 6:
			p = pickIssue()
		case r < 9:
			p = pickWisp()
		default:
			kind := []string{"i", "w"}[rng.Intn(2)]
			add(child, kind, fmt.Sprintf("cq-%s-gone-p%d", kind, rng.Intn(3)), "parent-child", "{}")
			return
		}
		if p == child {
			return
		}
		if add(child, p.plane, p.id, "parent-child", "{}") {
			children[p] = append(children[p], child)
		}
	}
	for _, id := range g.issues {
		if rng.Intn(2) == 0 {
			parent(node{"i", id})
		}
	}
	for _, id := range g.wisps {
		if rng.Intn(2) == 0 {
			parent(node{"w", id})
		}
	}
	// Parent-child cycles: three issues, and an issue with a wisp.
	c1, c2, c3, cw := pickIssue(), pickIssue(), pickIssue(), pickWisp()
	add(c1, "i", c2.id, "parent-child", "{}")
	add(c2, "i", c3.id, "parent-child", "{}")
	add(c3, "i", c1.id, "parent-child", "{}")
	add(c1, "w", cw.id, "parent-child", "{}")
	add(cw, "i", c1.id, "parent-child", "{}")

	// Spawner families: a spawner in either plane with issue and wisp
	// children of random status, and waiters from either plane on it.
	gates := []string{`{}`, `{"gate":"all-children"}`, `{"gate":"any-children"}`, `{"gate":"any-children","also_blocks":true}`, `{"also_blocks":true}`}
	for f := 0; f < 8+rng.Intn(6); f++ {
		sp := pickAny()
		for c := 0; c < rng.Intn(4); c++ {
			child := pickAny()
			if child != sp && add(child, sp.plane, sp.id, "parent-child", "{}") {
				children[sp] = append(children[sp], child)
			}
		}
		for w := 0; w < 1+rng.Intn(3); w++ {
			add(pickAny(), sp.plane, sp.id, "waits-for", gates[rng.Intn(len(gates))])
		}
	}
	// Waiters on spawners that already have children, a childless spawner,
	// and a dangling spawner.
	spawners := make([]node, 0, len(children))
	for sp := range children {
		spawners = append(spawners, sp)
	}
	// Map order is random; sort so a seed reproduces its graph.
	sort.Slice(spawners, func(a, b int) bool {
		return spawners[a].plane+spawners[a].id < spawners[b].plane+spawners[b].id
	})
	for _, sp := range spawners {
		if rng.Intn(4) == 0 {
			add(pickAny(), sp.plane, sp.id, "waits-for", gates[rng.Intn(len(gates))])
		}
	}
	add(pickAny(), "i", fmt.Sprintf("cq-i-gone-s%d", rng.Intn(3)), "waits-for", gates[rng.Intn(len(gates))])
	add(pickAny(), "w", fmt.Sprintf("cq-w-gone-s%d", rng.Intn(3)), "waits-for", gates[rng.Intn(len(gates))])

	const cols = "(id,issue_id,depends_on_issue_id,depends_on_wisp_id,depends_on_external,type,created_by,metadata) VALUES "
	if len(depRows) > 0 {
		exec("INSERT INTO dependencies " + cols + strings.Join(depRows, ","))
	}
	if len(wdepRows) > 0 {
		exec("INSERT INTO wisp_dependencies " + cols + strings.Join(wdepRows, ","))
	}
	return g
}

// blockedState reads every issue's and wisp's is_blocked.
func blockedState(ctx context.Context, q rowQuerier) (map[string]int, error) {
	out := map[string]int{}
	for _, table := range []string{"issues", "wisps"} {
		rows, err := q.QueryContext(ctx, "SELECT id, is_blocked FROM "+table)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var b int
			if err := rows.Scan(&id, &b); err != nil {
				rows.Close()
				return nil, err
			}
			out[table+":"+id] = b
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// stateAfter runs recompute in a transaction, reads the resulting is_blocked
// column and rolls back.
func stateAfter(ctx context.Context, db *sql.DB, recompute func(*sql.Tx) error) (map[string]int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := recompute(tx); err != nil {
		return nil, err
	}
	return blockedState(ctx, tx)
}

func diffState(want, got map[string]int) []string {
	var out []string
	for k, w := range want {
		if g, ok := got[k]; !ok || g != w {
			out = append(out, fmt.Sprintf("%s unhinted=%d patched=%d", k, w, g))
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			out = append(out, k+" only in patched")
		}
	}
	sort.Strings(out)
	return out
}

func batchesOf(ids []string, size int) [][]string {
	var out [][]string
	for s := 0; s < len(ids); s += size {
		out = append(out, ids[s:min(s+size, len(ids))])
	}
	return out
}

// TestBlockedRecomputeEqualsUnhintedForm compares the scoped is_blocked
// recompute the path sends (with the lookup-join hints and the per-column
// waits-for gate split of upstream ba12ffa74) against the same union and gate
// as they were before (kept above as the oracle). Relation: equal. On each
// seeded random graph:
//
//   - the set of ids the scoped should-be-blocked union selects is the same for
//     every issue id and every wisp id, in shuffled batches of 200 (the
//     production batch size; the issue plane always spans two batches, one of
//     them exactly 200), and in batches of one;
//   - RecomputeIsBlockedInTxWithResult leaves the same is_blocked column on
//     every issue and wisp as the oracle loop does, for all ids, one issue,
//     one wisp, and a mixed batch of 200 issues with half the wisps;
//   - an empty batch sends no statement.
//
// Bounds: the fixed seeds below (12 graphs of 205-264 issues, 30-69 wisps)
// and a 10-minute budget; a failure names its seed.
func TestBlockedRecomputeEqualsUnhintedForm(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	const budget = 10 * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	db := store.db

	seeds := []int64{1, 2, 3, 5, 8, 13, 21, 34, 55, 89, 144, 233}
	start := time.Now()

	// Empty batch: the path sends nothing.
	rec := &blockedStmtRecorder{DBTX: db, dryRun: true}
	if _, err := issueops.RecomputeIsBlockedInTxWithResult(ctx, rec, nil, nil); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
	if len(rec.stmts) != 0 {
		t.Fatalf("empty batch sent %d statements, want 0", len(rec.stmts))
	}

	var setChecks, stateChecks, blockedSeen int
	curSeed := int64(-1)
	defer func() {
		if t.Failed() {
			t.Logf("failed on seed %d of %v", curSeed, seeds)
		}
	}()
	for _, seed := range seeds {
		curSeed = seed
		rng := rand.New(rand.NewSource(seed))
		g := genBlockedGraph(t, ctx, db, rng)
		fail := func(format string, args ...any) {
			t.Helper()
			t.Fatalf("seed %d (%d issues, %d wisps, %d edges): %s", seed, len(g.issues), len(g.wisps), g.edges, fmt.Sprintf(format, args...))
		}

		// 1. The should-be-blocked set, per batch.
		for _, plane := range []struct {
			table, alias, depTable string
			ids                    []string
		}{
			{"issues", "i", "dependencies", g.issues},
			{"wisps", "w", "wisp_dependencies", g.wisps},
		} {
			ids := append([]string(nil), plane.ids...)
			rng.Shuffle(len(ids), func(a, b int) { ids[a], ids[b] = ids[b], ids[a] })
			batches := batchesOf(ids, 200)
			for _, id := range ids[:10] {
				batches = append(batches, []string{id})
			}
			for _, batch := range batches {
				mark, _ := capturedBlockedStmts(t, ctx, db, plane.table, batch)
				patched, pargs := patchedUnionFromMark(t, mark, plane.alias, len(batch))
				oracle, oargs := fillBatch(unhintedUnionScopedSQL(plane.depTable, unhintedBatchScope), batch)
				want := queryIDSet(t, ctx, db, oracle, oargs)
				got := queryIDSet(t, ctx, db, patched, pargs)
				if !reflect.DeepEqual(want, got) {
					fail("%s batch of %d %v: should-be-blocked unhinted=%v patched=%v", plane.table, len(batch), batch, want, got)
				}
				setChecks++
				blockedSeen += len(want)
			}
		}

		// 2. The is_blocked column the recompute leaves.
		mixed := append([]string(nil), g.issues...)
		rng.Shuffle(len(mixed), func(a, b int) { mixed[a], mixed[b] = mixed[b], mixed[a] })
		cases := []struct {
			name          string
			issues, wisps []string
		}{
			{"all", g.issues, g.wisps},
			{"one issue", []string{g.issues[rng.Intn(len(g.issues))]}, nil},
			{"one wisp", nil, []string{g.wisps[rng.Intn(len(g.wisps))]}},
			{"200 issues + half the wisps", mixed[:200], g.wisps[:len(g.wisps)/2]},
		}
		for _, c := range cases {
			want, err := stateAfter(ctx, db, func(tx *sql.Tx) error { return unhintedRecompute(ctx, tx, c.issues, c.wisps) })
			if err != nil {
				fail("%s: unhinted recompute: %v", c.name, err)
			}
			got, err := stateAfter(ctx, db, func(tx *sql.Tx) error {
				_, err := issueops.RecomputeIsBlockedInTxWithResult(ctx, tx, c.issues, c.wisps)
				return err
			})
			if err != nil {
				fail("%s: patched recompute: %v", c.name, err)
			}
			if d := diffState(want, got); len(d) > 0 {
				fail("%s: is_blocked differs on %d rows: %v", c.name, len(d), d)
			}
			stateChecks++
		}
	}
	t.Logf("seeds %v: %d union batches equal (%d should-be-blocked rows), %d recompute states equal, in %v (budget %v)",
		seeds, setChecks, blockedSeen, stateChecks, time.Since(start).Round(time.Millisecond), budget)
}
