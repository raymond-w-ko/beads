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
// only ? placeholders, bound by typeArgs after each branch's target.
func countDepTarget(ctx context.Context, tx DBTX, table, typeClause, targetID string, typeArgs []any) (int64, error) {
	q := DepTargetCountQuery(table, typeClause)
	args := make([]any, 0, 3*(1+len(typeArgs)))
	for i := 0; i < 3; i++ {
		args = append(args, targetID)
		args = append(args, typeArgs...)
	}
	var n int64
	if err := tx.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
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
//
// Each branch has one ? for the target id, then typeClause's placeholders (if
// any); countDepTarget binds them in that order. Exported so the dolt plan
// test EXPLAINs the text production runs.
func DepTargetCountQuery(table, typeClause string) string {
	filter := ""
	if typeClause != "" {
		filter = " AND " + typeClause
	}
	//nolint:gosec // G201: table is hardcoded by the caller and filter holds only ? placeholders.
	return fmt.Sprintf("SELECT count(*) FROM ("+
		"SELECT 1 AS x FROM %[1]s WHERE depends_on_issue_id = ?%[2]s"+
		" UNION ALL SELECT 1 FROM %[1]s WHERE depends_on_wisp_id = ? AND depends_on_issue_id IS NULL%[2]s"+
		" UNION ALL SELECT 1 FROM %[1]s WHERE depends_on_external = ? AND COALESCE(depends_on_issue_id, depends_on_wisp_id) IS NULL%[2]s"+
		") u", table, filter)
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
