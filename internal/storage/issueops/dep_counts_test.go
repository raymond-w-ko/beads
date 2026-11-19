package issueops

import (
	"context"
	"database/sql/driver"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/types"
)

// TestCountDependencyEdgesInLegSendsTheIndexedStatement ties the In leg of
// CountDependencyEdgesInTx (the dependents count behind every
// `bd show --json`) to DepTargetCountQuery's text, the one
// TestCountDependentsPlanIsIndexed (dolt package) EXPLAINs as three index
// seeks. A count that went back to filtering on DepTargetExpr would scan the
// whole table again (bd-o24uj) and fail here, since the mock compares whole
// statements, in order, with their arguments in branch order (review bd-ymzr1).
func TestCountDependencyEdgesInLegSendsTheIndexedStatement(t *testing.T) {
	const id = "dc-target"
	for _, c := range []struct {
		name   string
		filter []types.DependencyType
	}{
		{"no type filter", nil},
		{"type filter", []types.DependencyType{types.DepBlocks, types.DepRelated}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer db.Close()

			typeClause, typeArgs := buildDepTypeClause(c.filter)
			// One target id, then the type arguments, for each of the three
			// UNION ALL branches.
			var args []driver.Value
			for i := 0; i < 3; i++ {
				// The column equality, then the residual COALESCE filter.
				args = append(args, id, id)
				for _, a := range typeArgs {
					args = append(args, a)
				}
			}

			mock.ExpectBegin()
			mock.ExpectQuery(DepTargetCountQuery("dependencies", typeClause)).
				WithArgs(args...).
				WillReturnRows(sqlmock.NewRows([]string{"count(*)"}).AddRow(3))
			mock.ExpectQuery(DepTargetCountQuery("wisp_dependencies", typeClause)).
				WithArgs(args...).
				WillReturnRows(sqlmock.NewRows([]string{"count(*)"}).AddRow(4))

			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			n, err := CountDependencyEdgesInTx(ctx, tx, id, domain.DepDirectionIn, c.filter)
			if err != nil {
				t.Fatalf("CountDependencyEdgesInTx: %v", err)
			}
			if n != 7 {
				t.Errorf("CountDependencyEdgesInTx = %d, want 7 (3 dependencies + 4 wisp_dependencies)", n)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("sqlmock expectations: %v", err)
			}
		})
	}
}

// TestDepTargetCountQueryText spells out the count's statement: the branches
// are DepTargetPredicates, the ones the dependents reads use, each with the
// old COALESCE filter as a residual after its indexed column equality and
// guard, so the count matches only rows the old count matched under any
// column collations (review bd-lwx6j) and equals the rows the reads return.
func TestDepTargetCountQueryText(t *testing.T) {
	const legacy = " AND COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external) = ?"
	for _, c := range []struct{ typeClause, filter string }{
		{"", ""},
		{"type IN (?,?)", " AND type IN (?,?)"},
	} {
		want := "SELECT count(*) FROM (" +
			"SELECT 1 AS x FROM dependencies WHERE depends_on_issue_id = ?" + legacy + c.filter +
			" UNION ALL SELECT 1 FROM dependencies WHERE depends_on_wisp_id = ? AND depends_on_issue_id IS NULL" + legacy + c.filter +
			" UNION ALL SELECT 1 FROM dependencies WHERE depends_on_external = ? AND COALESCE(depends_on_issue_id, depends_on_wisp_id) IS NULL" + legacy + c.filter +
			") u"
		if got := DepTargetCountQuery("dependencies", c.typeClause); got != want {
			t.Errorf("DepTargetCountQuery(dependencies, %q) =\n%s\nwant\n%s", c.typeClause, got, want)
		}
	}
}
