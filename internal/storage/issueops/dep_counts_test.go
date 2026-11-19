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
				args = append(args, id)
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
