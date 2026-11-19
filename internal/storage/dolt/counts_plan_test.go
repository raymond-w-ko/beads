package dolt

import (
	"regexp"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// TestCountDependentsPlanIsIndexed pins the plan of the dependents count that
// every `bd show --json` runs (CountDependents, via
// issueops.CountDependencyEdgesInTx). Filtering on the COALESCE target
// expression read every edge: a full Table scan of `dependencies`, 150-220 ms
// on a 112k-edge fleet database (bd-o24uj). The production statement is one
// UNION ALL branch per target column, so each branch must seek the index on
// its own column.
//
// `wisp_dependencies` as migration 0021 creates it (the shape this test
// database has) has no index leading with depends_on_external outside the
// (type, target) composites, so its external branch scans; only databases that
// ran 0047 or ignored/0003 carry idx_wisp_dep_external_target. Adding that
// index is a schema change this test does not ask for, so on
// `wisp_dependencies` it asserts the issue and wisp branches only.
func TestCountDependentsPlanIsIndexed(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	// A few edges of each target shape on both planes, so the planner has rows
	// to cost.
	createPerm(t, ctx, store, "cp-target")
	createWisp(t, ctx, store, "cp-wtarget")
	createPerm(t, ctx, store, "cp-src")
	createWisp(t, ctx, store, "cp-wsrc")
	for _, src := range []string{"cp-src", "cp-wsrc"} {
		for _, tgt := range []string{"cp-target", "cp-wtarget", "external:cp:x"} {
			if err := store.AddDependency(ctx, &types.Dependency{IssueID: src, DependsOnID: tgt, Type: types.DepBlocks}, "tester"); err != nil {
				t.Fatalf("add dependency %s -> %s: %v", src, tgt, err)
			}
		}
	}

	// Single-source: this EXPLAINs the builder's text, not a captured
	// statement. Two sqlmock tests tie the count path to that text:
	// TestCountDependentsSendsTheIndexedStatement (counts_statement_test.go)
	// pins that DoltStore.CountDependents sends exactly
	// DepTargetCountQuery(table, "") for both tables, and
	// issueops TestCountDependencyEdgesInLegSendsTheIndexedStatement
	// (dep_counts_test.go) pins the same for CountDependencyEdgesInTx's In leg,
	// with and without a type filter.
	for _, c := range []struct {
		table   string
		columns []string
	}{
		{"dependencies", []string{"depends_on_issue_id", "depends_on_wisp_id", "depends_on_external"}},
		{"wisp_dependencies", []string{"depends_on_issue_id", "depends_on_wisp_id"}},
	} {
		prod := issueops.DepTargetCountQuery(c.table, "")
		// The target id at every ? (twice per branch).
		plan := explainPlan(t, ctx, store.db, literalizeParams(prod, targetLiterals(prod, "'cp-target'")...))
		if !looksLikeDoltPlan(plan) {
			t.Skipf("EXPLAIN output not in a recognized Dolt plan format, skipping index assertion; plan=\n%s", plan)
		}
		// Dolt's EXPLAIN names an index by its column list, so a seek on
		// idx_dep_issue_target shows as index: [dependencies.depends_on_issue_id].
		for _, col := range c.columns {
			want := "index: [" + c.table + "." + col
			if !strings.Contains(plan, want) {
				t.Errorf("%s dependents count does not seek an index on %s (want %q in an IndexedTableAccess).\nSQL: %s\nplan:\n%s", c.table, col, want, prod, plan)
			}
		}
		if c.table == "dependencies" && hasBareTableScan(plan) {
			t.Errorf("dependencies dependents count still has a bare Table scan.\nSQL: %s\nplan:\n%s", prod, plan)
		}
	}
}

// bareTableNode matches a plan-tree line that is a plain Table node (a full
// scan), as opposed to IndexedTableAccess(...).
var bareTableNode = regexp.MustCompile(`(?m)^[\s│├└─]*Table\s*$`)

func hasBareTableScan(plan string) bool {
	return bareTableNode.MatchString(plan)
}
