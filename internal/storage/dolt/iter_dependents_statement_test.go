package dolt

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/steveyegge/beads/internal/storage/sqlbuild"
)

// TestIterDependentsSendsTheIndexedStatement pins the statement
// DoltStore.IterDependentsWithMetadata (`bd show --json` with dependents)
// streams: for issues joined to dependencies and wisps joined to
// wisp_dependencies, one UNION ALL branch per target column of d, under one
// ORDER BY created_at, each with the old filter as a residual (collation,
// review bd-lwx6j). Filtering on the COALESCE target expression alone left
// Dolt no index on the edge table and read every edge (bd-o24uj site 5). The
// WHERE text is spelled out here, not taken from the builder, and the mock
// compares whole statements with the target id bound twice per branch;
// TestDependentsRowsPlanIsIndexed EXPLAINs iterDependentsQuery.
func TestIterDependentsSendsTheIndexedStatement(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var branches []string
	for _, plane := range []struct{ table, alias, depTable string }{
		{"issues", "i", "dependencies"},
		{"wisps", "w", "wisp_dependencies"},
	} {
		head := "SELECT " + prefixedIssueColumns(plane.alias) + ", d.type FROM " + plane.table + " " + plane.alias +
			" JOIN " + plane.depTable + " d ON d.issue_id = " + plane.alias + ".id " + sqlbuild.LeaseJoin(plane.alias) + " WHERE "
		const legacy = " AND COALESCE(d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external) = ?"
		branches = append(branches,
			head+"d.depends_on_issue_id = ?"+legacy,
			head+"d.depends_on_wisp_id = ? AND d.depends_on_issue_id IS NULL"+legacy,
			head+"d.depends_on_external = ? AND COALESCE(d.depends_on_issue_id, d.depends_on_wisp_id) IS NULL"+legacy)
	}
	want := strings.Join(branches, " UNION ALL ") + " ORDER BY created_at ASC"
	// The COALESCE filter appears only as a residual after a column equality.
	for i, branch := range branches {
		where := branch[strings.Index(branch, " WHERE ")+len(" WHERE "):]
		if strings.HasPrefix(where, "COALESCE(") {
			t.Fatalf("expected branch %d filters on the COALESCE target expression alone:\n%s", i, where)
		}
	}

	const id = "it-target"
	// Two per branch (column equality, residual), six branches.
	args := make([]driver.Value, 12)
	for i := range args {
		args[i] = id
	}
	mock.ExpectQuery(want).
		WithArgs(args...).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	store := &DoltStore{db: db}
	iter, err := store.IterDependentsWithMetadata(context.Background(), id)
	if err != nil {
		t.Fatalf("IterDependentsWithMetadata: %v", err)
	}
	for iter.Next(context.Background()) {
		t.Fatalf("unexpected row %+v", iter.Value())
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if err := iter.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}
