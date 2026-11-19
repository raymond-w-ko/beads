package sqlbuild

import (
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/types"
)

// ReadyWorkExcludeTypes returns the issue types excluded from ready work by
// default, plus any caller extras (deduped, empty entries dropped). Infra types
// stay hidden from ready work, and rig identity beads are also hidden even
// though they are durable issues rather than infra wisps.
func ReadyWorkExcludeTypes(extra []types.IssueType) []types.IssueType {
	out := []types.IssueType{
		types.IssueType("merge-request"),
		types.TypeGate,
		types.TypeMolecule,
		types.IssueType("rig"),
	}
	for _, t := range domain.DefaultInfraTypes() {
		out = append(out, types.IssueType(t))
	}
	seen := make(map[types.IssueType]bool, len(out)+len(extra))
	for _, t := range out {
		seen[t] = true
	}
	for _, t := range extra {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// ReadyWorkOrder is an ORDER BY fragment plus any args its CASE expressions
// need (the hybrid policy parameterizes a recency cutoff).
type ReadyWorkOrder struct {
	SQL  string
	Args []any
}

// BuildReadyWorkOrder renders the ready-work ORDER BY for a sort policy.
// createdCol/priorityCol name the sortable columns: real columns
// ("created_at"/"priority") for per-table queries, or the sort_* aliases
// ("sort_created"/"sort_priority") for UNION outer queries.
func BuildReadyWorkOrder(policy types.SortPolicy, createdCol, priorityCol string) ReadyWorkOrder {
	switch policy {
	case types.SortPolicyOldest:
		return ReadyWorkOrder{SQL: fmt.Sprintf("ORDER BY %s ASC, id ASC", createdCol)}
	case types.SortPolicyPriority:
		return ReadyWorkOrder{SQL: fmt.Sprintf("ORDER BY %s ASC, %s ASC, id ASC", priorityCol, createdCol)}
	case types.SortPolicyHybrid, "":
		recentCutoff := time.Now().UTC().Add(-48 * time.Hour)
		return ReadyWorkOrder{
			SQL: fmt.Sprintf(`ORDER BY
			CASE WHEN %s >= ? THEN 0 ELSE 1 END ASC,
			CASE WHEN %s >= ? THEN %s ELSE 999 END ASC,
			%s ASC, id ASC`, createdCol, createdCol, priorityCol, createdCol),
			Args: []any{recentCutoff, recentCutoff},
		}
	default:
		return ReadyWorkOrder{SQL: fmt.Sprintf("ORDER BY %s ASC, %s ASC, id ASC", priorityCol, createdCol)}
	}
}

// ReadyWorkWhereInputs carries the precomputed ID sets the ready-work WHERE
// clause folds in. Computing them takes queries, which is execution-context
// work each stack does its own way.
type ReadyWorkWhereInputs struct {
	// DeferredChildIDs are children of future-deferred parents; consulted
	// only when !filter.IncludeDeferred.
	DeferredChildIDs []string
	// ParentDescendantIDs are the transitive descendants of *filter.ParentID;
	// consulted only when filter.ParentID != nil.
	ParentDescendantIDs []string
	// ActiveCustomStatuses are the names of the category-active custom
	// statuses (the rows ActiveCustomStatusesSQL returns); consulted only when
	// NeedsActiveCustomStatuses(filter). The caller resolves them in the
	// transaction of the ready read, so the builder can bind the ready-eligible
	// set as literals: Dolt plans a literal status IN list as a bounded range
	// of the [is_blocked,status] index, but an OR with a custom_statuses
	// subquery as the open-ended range [NULL, ∞), reading every unblocked issue
	// of every status, closed included (bd-7c0u4, Q4).
	ActiveCustomStatuses []string
	// StatusCollationMatches reports that the resolve proved
	// custom_statuses.name and the status column of the table the clause
	// reads (FilterTables.Main) have the same collation. Only then does a
	// literal IN list compare the way the subquery did: the subquery compared
	// under the name column's collation, a literal list compares under the
	// status column's, so with a binary status and a case-insensitive name an
	// active "review" matches a stored "REVIEW" in one and not the other
	// (review bd-s9czb). False, the zero value, also means "could not tell",
	// and keeps the subquery clause.
	StatusCollationMatches bool
}

// ActiveCustomStatusesSQL returns the statement that resolves the ready
// clause's custom statuses for a read of table ("issues" or "wisps"): the
// names of the category-active custom statuses, which `bd ready` treats as
// ready-eligible next to built-in open, each with the name column's collation
// and the collation of table's status column. COLLATION() of a column reports
// the column's collation on Dolt and the embedded engine alike. The status
// collation comes from a row of table, so it is NULL when table is empty,
// which reads as "could not tell". One statement, the one the resolve already
// sent: no round trip is added for the collation check.
func ActiveCustomStatusesSQL(table string) string {
	return "SELECT name, COLLATION(name), (SELECT COLLATION(status) FROM " + table + " LIMIT 1)" +
		" FROM custom_statuses WHERE category = 'active' ORDER BY name"
}

// legacyReadyStatusClause is the ready-eligible status clause as a subquery.
// It is correct under any collations but plans as an unbounded status range;
// BuildReadyWorkWhere keeps it when the literal list could compare
// differently (see ReadyWorkWhereInputs.StatusCollationMatches).
const legacyReadyStatusClause = "(status = ? OR status IN (SELECT name FROM custom_statuses WHERE category = 'active'))"

// NeedsActiveCustomStatuses reports whether BuildReadyWorkWhere consults
// ReadyWorkWhereInputs.ActiveCustomStatuses for filter, so a caller runs
// ActiveCustomStatusesSQL only when the clause needs it.
func NeedsActiveCustomStatuses(filter types.WorkFilter) bool {
	return filter.Status == types.StatusOpen
}

// readyEligibleStatuses is built-in open followed by the active custom
// statuses, without empty names or repeats.
func readyEligibleStatuses(active []string) []string {
	out := []string{string(types.StatusOpen)}
	seen := map[string]bool{string(types.StatusOpen): true}
	for _, name := range active {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// BuildReadyWorkWhere renders the full ready-work WHERE clause for one table
// family. Both stacks must keep ready semantics identical (Seam A parity
// suite); all ready predicates live here.
//
// Invariant: every clause must reference only main-table columns or correlated
// subqueries keyed by id — never the counts mega-query's aggregate aliases
// (labels_json, dep_count, rdep_count, comment_count, parent_id, deps_json).
// SearchCountsSQL renders this WHERE inside a pre-join subquery where those
// aliases are out of scope. See the SearchCountsSQL doc comment for why a
// violation fails loud.
func BuildReadyWorkWhere(filter types.WorkFilter, tables FilterTables, in ReadyWorkWhereInputs) (string, []any, error) {
	var statusClause string
	var args []any
	switch {
	case filter.Status != "":
		// Singular StatusOpen is the `bd ready` pin and means the ready-
		// eligible set: built-in open plus category-active custom statuses,
		// matching the ready_issues view since migration 0025 (GH#5831).
		// The set is bound as literals from in.ActiveCustomStatuses, so the
		// status range stays bounded (see ReadyWorkWhereInputs), when the
		// resolve proved the name and status columns share a collation; the
		// schema pins neither, so they do on every database it creates.
		// Otherwise the subquery clause stays: it compares exactly as before.
		// Any other singular status stays exact.
		if NeedsActiveCustomStatuses(filter) {
			eligible := readyEligibleStatuses(in.ActiveCustomStatuses)
			switch {
			case len(eligible) == 1:
				statusClause = "status = ?"
				args = append(args, eligible[0])
			case !in.StatusCollationMatches:
				statusClause = legacyReadyStatusClause
				args = append(args, string(types.StatusOpen))
			default:
				ph, statusArgs := InPlaceholders(eligible)
				statusClause = fmt.Sprintf("status IN (%s)", ph)
				args = append(args, statusArgs...)
			}
		} else {
			statusClause = "status = ?"
			args = append(args, string(filter.Status))
		}
	case len(filter.Statuses) > 0:
		ph, statusArgs := InPlaceholders(filter.Statuses)
		statusClause = fmt.Sprintf("status IN (%s)", ph)
		args = append(args, statusArgs...)
	default:
		statusClause = "status IN ('open', 'in_progress')"
	}
	whereClauses := []string{
		statusClause,
		"(pinned = 0 OR pinned IS NULL)",
		"is_blocked = 0",
	}
	if !filter.IncludeEphemeral {
		whereClauses = append(whereClauses, "(ephemeral = 0 OR ephemeral IS NULL)")
	}

	if filter.Priority != nil {
		whereClauses = append(whereClauses, "priority = ?")
		args = append(args, *filter.Priority)
	}
	if filter.Type != "" {
		whereClauses = append(whereClauses, "issue_type = ?")
		args = append(args, filter.Type)
	} else {
		ph, a := InPlaceholders(ReadyWorkExcludeTypes(filter.ExcludeTypes))
		whereClauses = append(whereClauses, fmt.Sprintf("issue_type NOT IN (%s)", ph))
		args = append(args, a...)
	}
	if filter.Unassigned {
		whereClauses = append(whereClauses, "(assignee IS NULL OR assignee = '')")
	} else if filter.Assignee != nil {
		whereClauses = append(whereClauses, "assignee = ?")
		args = append(args, *filter.Assignee)
	}

	if !filter.IncludeDeferred {
		whereClauses = append(whereClauses, "(defer_until IS NULL OR defer_until <= UTC_TIMESTAMP())")
		for start := 0; start < len(in.DeferredChildIDs); start += QueryBatchSize {
			end := start + QueryBatchSize
			if end > len(in.DeferredChildIDs) {
				end = len(in.DeferredChildIDs)
			}
			placeholders, batchArgs := InPlaceholders(in.DeferredChildIDs[start:end])
			args = append(args, batchArgs...)
			whereClauses = append(whereClauses, fmt.Sprintf("id NOT IN (%s)", placeholders))
		}
	}
	for start := 0; start < len(filter.ExcludeIDs); start += QueryBatchSize {
		end := start + QueryBatchSize
		if end > len(filter.ExcludeIDs) {
			end = len(filter.ExcludeIDs)
		}
		placeholders, batchArgs := InPlaceholders(filter.ExcludeIDs[start:end])
		args = append(args, batchArgs...)
		whereClauses = append(whereClauses, fmt.Sprintf("id NOT IN (%s)", placeholders))
	}

	if len(filter.Labels) > 0 {
		for _, label := range filter.Labels {
			whereClauses = append(whereClauses, fmt.Sprintf("id IN (SELECT issue_id FROM %s WHERE label = ?)", tables.Labels))
			args = append(args, label)
		}
	}
	// LabelsAny is an OR-set: an issue qualifies if it carries AT LEAST ONE of
	// the labels. Previously this clause was absent entirely, so --label-any was
	// silently dropped on the ready/claim path (with or without --parent) — a
	// worker believed it was fenced when it was not. It AND-combines with Labels
	// (the flag help promises "Can combine with --label").
	if len(filter.LabelsAny) > 0 {
		placeholders := make([]string, len(filter.LabelsAny))
		for i, label := range filter.LabelsAny {
			placeholders[i] = "?"
			args = append(args, label)
		}
		whereClauses = append(whereClauses, fmt.Sprintf("id IN (SELECT issue_id FROM %s WHERE label IN (%s))", tables.Labels, strings.Join(placeholders, ", ")))
	}
	if len(filter.ExcludeLabels) > 0 {
		placeholders := make([]string, len(filter.ExcludeLabels))
		for i, label := range filter.ExcludeLabels {
			placeholders[i] = "?"
			args = append(args, label)
		}
		whereClauses = append(whereClauses, fmt.Sprintf("id NOT IN (SELECT issue_id FROM %s WHERE label IN (%s))", tables.Labels, strings.Join(placeholders, ", ")))
	}
	if filter.LabelPattern != "" {
		clause, pattern := labelPatternClause(tables.Labels, filter.LabelPattern)
		whereClauses = append(whereClauses, clause)
		args = append(args, pattern)
	}
	if filter.LabelRegex != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("id IN (SELECT issue_id FROM %s WHERE label REGEXP ?)", tables.Labels))
		args = append(args, filter.LabelRegex)
	}

	// Parent filtering: return all transitive descendants of parentID.
	// GH#3396: a one-hop subquery silently dropped grandchildren despite the
	// help text and WorkFilter.ParentID godoc both promising recursion.
	if filter.ParentID != nil {
		parentID := *filter.ParentID
		parentClauses := []string{fmt.Sprintf("(id LIKE CONCAT(?, '.%%') AND id NOT IN (SELECT issue_id FROM %s WHERE type = 'parent-child'))", tables.Dependencies)}
		args = append(args, parentID)
		for start := 0; start < len(in.ParentDescendantIDs); start += QueryBatchSize {
			end := start + QueryBatchSize
			if end > len(in.ParentDescendantIDs) {
				end = len(in.ParentDescendantIDs)
			}
			placeholders, batchArgs := InPlaceholders(in.ParentDescendantIDs[start:end])
			parentClauses = append(parentClauses, fmt.Sprintf("id IN (%s)", placeholders))
			args = append(args, batchArgs...)
		}
		whereClauses = append(whereClauses, "("+strings.Join(parentClauses, " OR ")+")")
	}

	if filter.MoleculeID != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("(id IN (SELECT issue_id FROM %s WHERE type = 'parent-child' AND %s = ?) OR (id LIKE CONCAT(?, '.%%') AND id NOT IN (SELECT issue_id FROM %s WHERE type = 'parent-child')))", tables.Dependencies, DepTargetExpr, tables.Dependencies))
		args = append(args, filter.MoleculeID, filter.MoleculeID)
	}

	var err error
	whereClauses, args, err = AppendMetadataClauses(whereClauses, args, filter.HasMetadataKey, filter.MetadataFields)
	if err != nil {
		return "", nil, err
	}

	return "WHERE " + strings.Join(whereClauses, " AND "), args, nil
}
