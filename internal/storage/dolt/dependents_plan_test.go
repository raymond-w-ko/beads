package dolt

import (
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// TestDependentsRowsPlanIsIndexed pins the plans of the two reads that return
// an issue's dependents: issueops.DepTargetRowsQuery (the slice path behind
// `bd show`, dep tree and relations, bd-o24uj site 4) and iterDependentsQuery
// (IterDependentsWithMetadata, `bd show --json` with dependents, site 5).
// Filtering on the COALESCE target expression read every edge: a full Table
// scan of `dependencies`, 112k rows on a fleet database. Both statements are
// one UNION ALL branch per target column, so each branch must seek the index
// on its own column.
//
// As in TestCountDependentsPlanIsIndexed, `wisp_dependencies` as migration
// 0021 creates it has no index led by depends_on_external, so its external
// branch may scan; on that table only the issue and wisp branches are
// asserted. A later batch adds the index and tightens this.
func TestDependentsRowsPlanIsIndexed(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	// A few edges of each target shape on both planes, so the planner has rows
	// to cost.
	createPerm(t, ctx, store, "dp-target")
	createWisp(t, ctx, store, "dp-wtarget")
	createPerm(t, ctx, store, "dp-src")
	createWisp(t, ctx, store, "dp-wsrc")
	for _, src := range []string{"dp-src", "dp-wsrc"} {
		for _, tgt := range []string{"dp-target", "dp-wtarget", "external:dp:x"} {
			if err := store.AddDependency(ctx, &types.Dependency{IssueID: src, DependsOnID: tgt, Type: types.DepBlocks}, "tester"); err != nil {
				t.Fatalf("add dependency %s -> %s: %v", src, tgt, err)
			}
		}
	}

	allTargets := []string{"depends_on_issue_id", "depends_on_wisp_id", "depends_on_external"}
	wispTargets := []string{"depends_on_issue_id", "depends_on_wisp_id"}

	// Site 4: one statement per dependency table, the target id at each ?
	// (three today, one per branch).
	for _, c := range []struct {
		table   string
		columns []string
	}{
		{"dependencies", allTargets},
		{"wisp_dependencies", wispTargets},
	} {
		prod := issueops.DepTargetRowsQuery(c.table)
		plan := explainPlan(t, ctx, store.db, literalizeParams(prod, targetLiterals(prod, "'dp-target'")...))
		if !looksLikeDoltPlan(plan) {
			t.Skipf("EXPLAIN output not in a recognized Dolt plan format, skipping index assertion; plan=\n%s", plan)
		}
		assertSeeksTargets(t, "dependents rows", c.table, c.columns, prod, plan)
		if c.table == "dependencies" && hasBareTableScan(plan) {
			t.Errorf("dependencies dependents rows still have a bare Table scan.\nSQL: %s\nplan:\n%s", prod, plan)
		}
	}

	// Site 5: both planes in one statement (six ? today, three per plane).
	prod := iterDependentsQuery()
	plan := explainPlan(t, ctx, store.db, literalizeParams(prod, targetLiterals(prod, "'dp-target'")...))
	if !looksLikeDoltPlan(plan) {
		t.Skipf("EXPLAIN output not in a recognized Dolt plan format, skipping index assertion; plan=\n%s", plan)
	}
	assertSeeksTargets(t, "dependents iter", "dependencies", allTargets, prod, plan)
	assertSeeksTargets(t, "dependents iter", "wisp_dependencies", wispTargets, prod, plan)
	if hasBareTableScanOf(plan, "dependencies") {
		t.Errorf("dependents iter still has a bare Table scan of dependencies.\nSQL: %s\nplan:\n%s", prod, plan)
	}
}

// targetLiterals repeats lit once per ? in query: every placeholder of the
// dependents statements binds the same target id.
func targetLiterals(query, lit string) []string {
	out := make([]string, strings.Count(query, "?"))
	for i := range out {
		out[i] = lit
	}
	return out
}

// assertSeeksTargets fails unless plan seeks an index led by each of columns
// on table. Dolt's EXPLAIN names an index by its column list, so a seek on
// idx_dep_issue_target shows as index: [dependencies.depends_on_issue_id].
func assertSeeksTargets(t *testing.T, what, table string, columns []string, sql, plan string) {
	t.Helper()
	for _, col := range columns {
		want := "index: [" + table + "." + col
		if !strings.Contains(plan, want) {
			t.Errorf("%s on %s do not seek an index on %s (want %q in an IndexedTableAccess).\nSQL: %s\nplan:\n%s", what, table, col, want, sql, plan)
		}
	}
}

// hasBareTableScanOf reports whether plan holds a bare Table node (a full
// scan) whose name is table. In FORMAT=TREE a Table node is followed by its
// "name: <table>" line.
func hasBareTableScanOf(plan, table string) bool {
	lines := strings.Split(plan, "\n")
	for i, line := range lines {
		if !bareTableNode.MatchString(line) {
			continue
		}
		for _, next := range lines[i+1 : min(i+4, len(lines))] {
			if strings.Contains(next, "name: "+table) && !strings.Contains(next, "name: "+table+"_") {
				return strings.HasSuffix(strings.TrimSpace(next), "name: "+table)
			}
		}
	}
	return false
}
