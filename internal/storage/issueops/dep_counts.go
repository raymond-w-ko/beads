package issueops

import (
	"context"
	"fmt"
	"strings"

	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/types"
)

func CountDependencyEdgesInTx(ctx context.Context, tx DBTX, sourceID string, dir domain.DepDirection, typeFilter []types.DependencyType) (int64, error) {
	typeClause, typeArgs := buildDepTypeClause(typeFilter)

	var total int64
	for _, table := range []string{"dependencies", "wisp_dependencies"} {
		if dir == domain.DepDirectionOut || dir == domain.DepDirectionBoth {
			n, err := countDepWhere(ctx, tx, table, "issue_id = ?", typeClause, sourceID, typeArgs)
			if err != nil {
				return 0, err
			}
			total += n
		}
		if dir == domain.DepDirectionIn || dir == domain.DepDirectionBoth {
			n, err := countDepTarget(ctx, tx, table, typeClause, sourceID, typeArgs)
			if err != nil {
				return 0, err
			}
			total += n
		}
	}
	return total, nil
}

func countDepWhere(ctx context.Context, tx DBTX, table, whereExpr, typeClause string, sourceID string, typeArgs []any) (int64, error) {
	q := fmt.Sprintf("SELECT count(*) FROM %s WHERE %s", table, whereExpr) //nolint:gosec
	if typeClause != "" {
		q += " AND " + typeClause
	}
	args := append([]any{sourceID}, typeArgs...)
	var n int64
	if err := tx.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count %s: %w", table, err)
	}
	return n, nil
}

// countDepTarget counts the rows of table whose target (DepTargetExpr) is
// targetID, with the statement DepTargetCountQuery builds. typeClause holds
// only ? placeholders, bound by typeArgs after each branch's two target ids.
func countDepTarget(ctx context.Context, tx DBTX, table, typeClause, targetID string, typeArgs []any) (int64, error) {
	q := DepTargetCountQuery(table, typeClause)
	var n int64
	if err := tx.QueryRowContext(ctx, q, DepTargetBranchArgs(targetID, typeArgs...)...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count %s: %w", table, err)
	}
	return n, nil
}

// DepTargetCountQuery returns the statement that counts the rows of table
// (dependencies or wisp_dependencies) whose target (DepTargetExpr) equals one
// id. It is one UNION ALL branch per target column, so each branch seeks
// idx_dep_{issue,wisp,external}_target (or the wisp twins). Matching the
// COALESCE expression itself, or an OR of the three columns, leaves Dolt no
// index to seek, so the count read every edge in the table (bd-o24uj,
// GH#7666). The COALESCE(<higher columns>) IS NULL guard hands each row to
// the one branch whose column COALESCE would return, so the total equals the
// COALESCE count row for row even on a row that sets more than one target
// column (which ck_dep_one_target forbids, but 0058 found wisp tables without
// it). The guard is a residual filter on the seeked rows, not a second range.
// The branches are DepTargetPredicates, the ones the dependents reads use, so
// the count cannot differ from those rows (the residual COALESCE filter
// included).
//
// Each branch has two ? for the target id, then typeClause's placeholders
// (if any); countDepTarget binds them in that order (DepTargetBranchArgs).
// Exported so the dolt plan test EXPLAINs the text production runs.
func DepTargetCountQuery(table, typeClause string) string {
	filter := ""
	if typeClause != "" {
		filter = " AND " + typeClause
	}
	preds := DepTargetPredicates("")
	return "SELECT count(*) FROM (" +
		"SELECT 1 AS x FROM " + table + " WHERE " + preds[0] + filter +
		" UNION ALL SELECT 1 FROM " + table + " WHERE " + preds[1] + filter +
		" UNION ALL SELECT 1 FROM " + table + " WHERE " + preds[2] + filter +
		") u"
}

// DepTargetPredicates returns the three WHERE predicates that together match
// the rows whose target (DepTargetExpr) equals one id, one per target column
// and in the order COALESCE reads them: depends_on_issue_id; then
// depends_on_wisp_id with depends_on_issue_id NULL; then depends_on_external
// with both NULL. Used as the branches of a UNION ALL, each seeks the index
// on its own column, and the guards give each row to the one branch COALESCE
// would pick (the guards' rationale is DepTargetCountQuery's).
//
// Each predicate ends with the old filter, `DepTargetExpr = ?`, as a residual
// on the seeked rows. A bare column equality compares under that column's
// collation, the COALESCE under the collation of its result; where the three
// columns differ (the migrations pin none, so they agree on every database bd
// creates), a column could match an id the old read did not (review
// bd-lwx6j). The residual keeps every branch to rows the old read matched.
// Each predicate has two ? for the target id (DepTargetBranchArgs binds them).
// alias qualifies the columns ("d" gives d.depends_on_issue_id); "" leaves
// them bare.
func DepTargetPredicates(alias string) [3]string {
	p := ""
	if alias != "" {
		p = alias + "."
	}
	legacy := " AND COALESCE(" + p + "depends_on_issue_id, " + p + "depends_on_wisp_id, " + p + "depends_on_external) = ?"
	return [3]string{
		p + "depends_on_issue_id = ?" + legacy,
		p + "depends_on_wisp_id = ? AND " + p + "depends_on_issue_id IS NULL" + legacy,
		p + "depends_on_external = ? AND COALESCE(" + p + "depends_on_issue_id, " + p + "depends_on_wisp_id) IS NULL" + legacy,
	}
}

// DepTargetBranchArgs returns the arguments of the three DepTargetPredicates
// branches, in order: for each branch the target id twice (the column
// equality, then the residual), followed by extra (the placeholders a caller
// appends to every branch, such as a type clause's).
func DepTargetBranchArgs(targetID string, extra ...any) []any {
	args := make([]any, 0, 3*(2+len(extra)))
	for i := 0; i < 3; i++ {
		args = append(args, targetID, targetID)
		args = append(args, extra...)
	}
	return args
}

// DepTargetRowsQuery returns the statement that reads the dependents of one
// target id from table (dependencies or wisp_dependencies): (issue_id, type)
// of every row whose target (DepTargetExpr) is that id. It is one UNION ALL
// branch per target column (DepTargetPredicates), so each branch seeks
// idx_dep_{issue,wisp,external}_target (or the wisp twins); filtering on the
// COALESCE expression read every edge in the table on each `bd show`
// (bd-o24uj, site 4). The union sits in a derived table, as in
// DepTargetCountQuery: Dolt does not push a branch's filter into an index when
// the UNION ALL of plain filtered SELECTs is the top-level statement, and
// plans each branch as a full Table scan; inside the derived table each
// branch seeks (TestDependentsRowsPlanIsIndexed).
//
// The rows come ORDER BY the primary key id, the order the old statement's
// scan gave them. One id can sit in two columns of one table (a same-prefix
// child holds it in depends_on_issue_id, a cross-prefix child in
// depends_on_external), so the branches alone would group rows by column, and
// `bd show --children` and the mol reader's current step follow this order
// (review bd-lwx6j). Bind DepTargetBranchArgs(id). Exported so the dolt plan
// test EXPLAINs the text production runs.
func DepTargetRowsQuery(table string) string {
	branches := make([]string, 0, 3)
	for _, pred := range DepTargetPredicates("") {
		branches = append(branches, "SELECT id, issue_id, type FROM "+table+" WHERE "+pred)
	}
	return "SELECT issue_id, type FROM (" + strings.Join(branches, " UNION ALL ") + ") u ORDER BY id"
}

func buildDepTypeClause(filter []types.DependencyType) (string, []any) {
	if len(filter) == 0 {
		return "", nil
	}
	placeholders := make([]string, len(filter))
	args := make([]any, len(filter))
	for i, t := range filter {
		placeholders[i] = "?"
		args[i] = string(t)
	}
	return "type IN (" + strings.Join(placeholders, ",") + ")", args
}
