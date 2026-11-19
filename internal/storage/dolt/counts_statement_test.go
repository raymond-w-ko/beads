package dolt

import (
	"context"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/steveyegge/beads/internal/storage/issueops"
)

// TestCountDependentsSendsTheIndexedStatement ties DoltStore.CountDependents
// (every `bd show --json` on the direct route) to the statement
// TestCountDependentsPlanIsIndexed EXPLAINs: issueops.DepTargetCountQuery with
// no type filter, once per dependency table, inside the one read transaction
// CountDependents opens. A count that went back to filtering on the COALESCE
// target expression would scan the whole table again (bd-o24uj) and fail
// here, since the mock compares whole statements (review bd-ymzr1).
func TestCountDependentsSendsTheIndexedStatement(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const id = "cs-target"
	mock.ExpectBegin()
	mock.ExpectQuery(issueops.DepTargetCountQuery("dependencies", "")).
		WithArgs(id, id, id).
		WillReturnRows(sqlmock.NewRows([]string{"count(*)"}).AddRow(2))
	mock.ExpectQuery(issueops.DepTargetCountQuery("wisp_dependencies", "")).
		WithArgs(id, id, id).
		WillReturnRows(sqlmock.NewRows([]string{"count(*)"}).AddRow(5))
	mock.ExpectRollback()

	store := &DoltStore{db: db}
	n, err := store.CountDependents(context.Background(), id)
	if err != nil {
		t.Fatalf("CountDependents: %v", err)
	}
	if n != 7 {
		t.Errorf("CountDependents = %d, want 7 (2 dependencies + 5 wisp_dependencies)", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}
