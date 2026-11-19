package sqlbuild

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

func TestOrderByKnownKeys(t *testing.T) {
	t.Parallel()

	cases := []struct {
		sortBy   string
		sortDesc bool
		table    string
		want     string
	}{
		{"", false, "", "ORDER BY priority ASC, created_at DESC, id ASC"},
		{"priority", true, "", "ORDER BY priority DESC, created_at DESC, id ASC"},
		{"created", false, "", "ORDER BY created_at DESC, id ASC"},
		{"created", true, "", "ORDER BY created_at ASC, id ASC"},
		{"title", false, "i", "ORDER BY LOWER(i.title) ASC, i.id ASC"},
		{"updated", false, "i", "ORDER BY i.updated_at DESC, i.id ASC"},
		{"bogus-key", false, "", "ORDER BY priority ASC, created_at DESC, id ASC"},
		{"id", false, "", ""}, // Go-side sort
	}
	for _, tc := range cases {
		if got := OrderBy(tc.sortBy, tc.sortDesc, tc.table); got != tc.want {
			t.Errorf("OrderBy(%q, %v, %q) = %q, want %q", tc.sortBy, tc.sortDesc, tc.table, got, tc.want)
		}
	}
}

// TestUnionSortColumnsCoverSortDefs pins that every SQL-side sort key has a
// sort_* alias in UnionSortColumnsSQL, so UNION consumers can order by any
// key OrderByForColumns may emit.
func TestUnionSortColumnsCoverSortDefs(t *testing.T) {
	t.Parallel()

	for key := range SortDefs {
		alias := "sort_" + key
		if key == "" {
			alias = "sort_priority"
		}
		if !strings.Contains(UnionSortColumnsSQL, alias) {
			t.Errorf("UnionSortColumnsSQL missing alias %q for sort key %q", alias, key)
		}
	}
}

// TestLessMirrorsOrderBy spot-checks that the Go-side comparator agrees with
// the SQL default ordering on the documented tie-break chain: priority ASC,
// then created_at DESC, then id ASC.
func TestLessMirrorsOrderBy(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	older := now.Add(-time.Hour)
	a := &types.Issue{ID: "a", Priority: 1, CreatedAt: now}
	b := &types.Issue{ID: "b", Priority: 2, CreatedAt: now}
	if !Less(a, b, "", false) || Less(b, a, "", false) {
		t.Error("default sort must order priority 1 before priority 2")
	}
	c := &types.Issue{ID: "c", Priority: 1, CreatedAt: older}
	if !Less(a, c, "", false) {
		t.Error("equal priority must order newer created_at first (created_at DESC)")
	}
	d := &types.Issue{ID: "d", Priority: 1, CreatedAt: now}
	if !Less(a, d, "", false) || Less(d, a, "", false) {
		t.Error("full tie must break by id ASC")
	}
}

// TestLessIDHonorsSortDesc pins the bd-jao3t half of the reversed-id bug:
// Less ignored sortDesc for "id" (the one Go-side sort key), so every merge
// sort that fed a reversed id page kept the byte-FIRST rows.
func TestLessIDHonorsSortDesc(t *testing.T) {
	a := &types.Issue{ID: "bd-001"}
	b := &types.Issue{ID: "bd-002"}

	if !Less(a, b, "id", false) || Less(b, a, "id", false) {
		t.Error("id ascending must be byte order")
	}
	if !Less(b, a, "id", true) || Less(a, b, "id", true) {
		t.Error("id descending must be reversed byte order, not the ascending order sortDesc used to be ignored into")
	}
}

func TestBuildReadyWorkOrderPriorityFIFO(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		policy types.SortPolicy
		want   string
	}{
		{
			name:   "priority",
			policy: types.SortPolicyPriority,
			want:   "ORDER BY priority ASC, created_at ASC, id ASC",
		},
		{
			name:   "fallback",
			policy: types.SortPolicy("unknown"),
			want:   "ORDER BY priority ASC, created_at ASC, id ASC",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := BuildReadyWorkOrder(tc.policy, "created_at", "priority")
			if got.SQL != tc.want {
				t.Fatalf("BuildReadyWorkOrder(%q).SQL = %q, want %q", tc.policy, got.SQL, tc.want)
			}
		})
	}
}

func TestReadyWorkExcludeTypes(t *testing.T) {
	t.Parallel()

	base := ReadyWorkExcludeTypes(nil)
	seen := make(map[types.IssueType]bool, len(base))
	for _, typ := range base {
		if seen[typ] {
			t.Errorf("duplicate type %q in default exclude list", typ)
		}
		seen[typ] = true
	}
	for _, want := range []types.IssueType{"merge-request", types.TypeGate, types.TypeMolecule, "agent", "rig", "role", "message"} {
		if !seen[want] {
			t.Errorf("default exclude list missing %q", want)
		}
	}

	extended := ReadyWorkExcludeTypes([]types.IssueType{"custom", "", types.TypeGate})
	if got, want := len(extended), len(base)+1; got != want {
		t.Errorf("extras must dedupe and drop empties: len = %d, want %d", got, want)
	}
}

func TestBuildReadyWorkWhereBatchesIDSets(t *testing.T) {
	t.Parallel()

	ids := make([]string, QueryBatchSize+1)
	for i := range ids {
		ids[i] = "x-" + strings.Repeat("a", 3)
	}
	where, args, err := BuildReadyWorkWhere(types.WorkFilter{}, IssuesFilterTables, ReadyWorkWhereInputs{DeferredChildIDs: ids})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.Count(where, "id NOT IN ("); got != 2 {
		t.Errorf("expected 2 batched NOT IN clauses for %d IDs, got %d", len(ids), got)
	}
	wantArgs := len(ids) + len(ReadyWorkExcludeTypes(nil))
	if len(args) != wantArgs {
		t.Errorf("args = %d, want %d", len(args), wantArgs)
	}
}

func TestBuildReadyWorkWhereAppliesPolicyIDExclusions(t *testing.T) {
	t.Parallel()

	ids := make([]string, QueryBatchSize+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("be-external-%d", i)
	}
	where, args, err := BuildReadyWorkWhere(
		types.WorkFilter{ExcludeIDs: ids},
		IssuesFilterTables,
		ReadyWorkWhereInputs{},
	)
	if err != nil {
		t.Fatalf("BuildReadyWorkWhere: %v", err)
	}
	if got := strings.Count(where, "id NOT IN ("); got != 2 {
		t.Fatalf("external policy exclusions produced %d NOT IN clauses, want 2", got)
	}
	wantArgs := len(ids) + len(ReadyWorkExcludeTypes(nil))
	if len(args) != wantArgs {
		t.Fatalf("args = %d, want %d", len(args), wantArgs)
	}
}

// wy-jpd3.2: --label-any was silently dropped on the ready/claim path (with or
// without --parent). BuildReadyWorkWhere must now emit an OR-set membership
// clause for LabelsAny that AND-combines with the AND-set Labels and the parent
// filter, so an atomic claim is actually fenced by the label filter it names.
func TestBuildReadyWorkWhereLabelsAny(t *testing.T) {
	t.Parallel()

	parent := "wy-jpd3"
	filter := types.WorkFilter{
		Labels:    []string{"tier:opus"},
		LabelsAny: []string{"lane-a", "lane-c"},
		ParentID:  &parent,
	}
	where, args, err := BuildReadyWorkWhere(filter, IssuesFilterTables, ReadyWorkWhereInputs{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// OR-set: one subquery with an IN over all --label-any values.
	wantAny := "id IN (SELECT issue_id FROM " + IssuesFilterTables.Labels + " WHERE label IN (?, ?))"
	if !strings.Contains(where, wantAny) {
		t.Errorf("LabelsAny OR clause missing.\n where = %s\n want substring = %s", where, wantAny)
	}
	// AND-set still emits its own per-label subquery (equality, not IN).
	wantAnd := "id IN (SELECT issue_id FROM " + IssuesFilterTables.Labels + " WHERE label = ?)"
	if !strings.Contains(where, wantAnd) {
		t.Errorf("Labels AND clause missing.\n where = %s\n want substring = %s", where, wantAnd)
	}
	// Parent filter must survive alongside the label clauses.
	if !strings.Contains(where, "LIKE CONCAT(?, '.%')") {
		t.Errorf("parent filter clause missing when combined with labels.\n where = %s", where)
	}
	// The label + parent args land in filter order (AND labels, then LabelsAny
	// values, then the parent LIKE arg) as the tail of the arg list — the
	// default issue_type exclusion prepends its own args.
	wantTail := []interface{}{"tier:opus", "lane-a", "lane-c", parent}
	if len(args) < len(wantTail) {
		t.Fatalf("args = %v, want at least %d trailing values %v", args, len(wantTail), wantTail)
	}
	tail := args[len(args)-len(wantTail):]
	for i := range wantTail {
		if tail[i] != wantTail[i] {
			t.Errorf("tail[%d] = %v, want %v (full args: %v)", i, tail[i], wantTail[i], args)
		}
	}
}

// ga-hqchm: filter.LabelPattern was parsed from --label-pattern and stored
// on WorkFilter, but BuildReadyWorkWhere never read it — the flag was dead
// code on the ready path and silently returned unfiltered results.
// BuildIssueFilterClauses coverage for LabelPattern/LabelRegex lives with
// the list path (be402a022, #3971); this covers only the ready path's own
// clause shape, which mirrors filter.go's LIKE ... ESCAPE '|' pattern.
func TestBuildReadyWorkWhereLabelPattern(t *testing.T) {
	t.Parallel()

	where, args, err := BuildReadyWorkWhere(types.WorkFilter{LabelPattern: "tech-*"}, IssuesFilterTables, ReadyWorkWhereInputs{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantClause := "id IN (SELECT issue_id FROM " + IssuesFilterTables.Labels + " WHERE label LIKE ? ESCAPE '|')"
	if !strings.Contains(where, wantClause) {
		t.Errorf("LabelPattern clause missing.\n where = %s\n want substring = %s", where, wantClause)
	}
	if len(args) == 0 || args[len(args)-1] != "tech-%" {
		t.Errorf("args tail = %v, want last arg %q", args, "tech-%")
	}
}

// #4884 intent: filter.LabelRegex was likewise dead on the ready path.
// BuildReadyWorkWhere must emit a REGEXP subquery mirroring filter.go's
// LabelRegex clause.
func TestBuildReadyWorkWhereLabelRegex(t *testing.T) {
	t.Parallel()

	where, args, err := BuildReadyWorkWhere(types.WorkFilter{LabelRegex: "^tech-"}, IssuesFilterTables, ReadyWorkWhereInputs{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantClause := "id IN (SELECT issue_id FROM " + IssuesFilterTables.Labels + " WHERE label REGEXP ?)"
	if !strings.Contains(where, wantClause) {
		t.Errorf("LabelRegex clause missing.\n where = %s\n want substring = %s", where, wantClause)
	}
	if len(args) == 0 || args[len(args)-1] != "^tech-" {
		t.Errorf("args tail = %v, want last arg %q", args, "^tech-")
	}
}

func TestSearchCountsSQLShape(t *testing.T) {
	t.Parallel()

	sql, args := SearchCountsSQL(WispsFilterTables, nil, "WHERE x = ?", "ORDER BY y", "LIMIT 5", true, CountsHydration{})
	if args != nil {
		t.Errorf("predicate form must not return generated args, got %d", len(args))
	}
	for _, want := range []string{
		"FROM wisps i",
		"FROM wisp_dependencies",
		"FROM wisp_comments",
		"FROM wisp_labels",
		"UNION ALL", // wisp reverse deps included
		"WHERE x = ?",
		"ORDER BY y",
		"LIMIT 5",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("counts SQL missing %q", want)
		}
	}

	// Filter-before-join structure (predicate form): whereSQL is applied to the
	// main table in a derived table that closes before the first aggregate LEFT
	// JOIN. Locking this prevents a regression back to the filter-after-join
	// shape that materialized every aggregate before pruning. "SELECT i.*" is
	// the derived driver's marker — "FROM (" alone is ambiguous, since the
	// reverse-blocker subquery also nests one.
	driverEnd := strings.Index(sql, ") i")
	firstJoin := strings.Index(sql, "LEFT JOIN")
	if !strings.Contains(sql, "SELECT i.*") || driverEnd < 0 {
		t.Fatalf("counts SQL must wrap the main table in a derived subquery; got:\n%s", sql)
	}
	if firstJoin < 0 || driverEnd > firstJoin {
		t.Fatalf("derived driver must close before the first aggregate LEFT JOIN")
	}
	if idx := strings.Index(sql, "WHERE x = ?"); idx < 0 || idx > driverEnd {
		t.Errorf("WHERE filter must appear inside the derived driver (before %q)", ") i")
	}
	// ORDER BY and LIMIT must stay AFTER the joins, and appear exactly once —
	// the ready-work path passes a parameterized ORDER BY, so duplicating it
	// would desync the placeholder/arg counts.
	for _, outer := range []string{"ORDER BY y", "LIMIT 5"} {
		if strings.Count(sql, outer) != 1 {
			t.Errorf("%q must appear exactly once, got %d", outer, strings.Count(sql, outer))
		}
		if idx := strings.Index(sql, outer); idx < firstJoin {
			t.Errorf("%q must appear after the aggregate joins", outer)
		}
	}

	noWispDeps, _ := SearchCountsSQL(IssuesFilterTables, nil, "", "", "", false, CountsHydration{SkipLabels: true})
	if strings.Contains(noWispDeps, "UNION ALL") {
		t.Error("counts SQL must not union wisp reverse deps when probe says absent")
	}
	// An empty whereSQL keeps the plain driver: there is no predicate to push
	// down, so the derived-table wrapper would be pure overhead for the
	// unfiltered list/counts path.
	if strings.Contains(noWispDeps, "SELECT i.*") {
		t.Errorf("empty-where counts SQL must keep the plain driver, not a derived table:\n%s", noWispDeps)
	}
	if strings.Contains(noWispDeps, "JSON_ARRAYAGG(label)") {
		t.Error("counts SQL must skip the labels join when skipLabels is set")
	}
	if !strings.Contains(noWispDeps, "NULL AS labels_json") {
		t.Error("counts SQL must project NULL labels_json when skipLabels is set")
	}

	// By-IDs form: driver and every subquery are constrained to the ids, and the
	// arg count matches the placeholder injection points (labels, dc, three
	// rc-deps branches and three rc-wisp branches binding twice each — column
	// match, then the fork's residual COALESCE filter — cc, pc, d, driver = 18
	// for the wisp family with labels).
	byIDs, idArgs := SearchCountsSQL(WispsFilterTables, []string{"a", "b"}, "", "", "", true, CountsHydration{})
	if !strings.Contains(byIDs, "WHERE i.id IN (?,?)") {
		t.Errorf("by-IDs counts SQL missing driver id filter:\n%s", byIDs)
	}
	// The by-IDs form keeps the plain "FROM wisps i" driver: its subqueries are
	// already id-constrained, so there is no join-then-filter blowup to avoid and
	// no reason to interpose a derived table.
	if strings.Contains(byIDs, "SELECT i.*") {
		t.Errorf("by-IDs counts SQL must keep the plain driver, not a derived table:\n%s", byIDs)
	}
	if strings.Contains(byIDs, "ORDER BY") || strings.Contains(byIDs, "LIMIT") {
		t.Error("by-IDs counts SQL must not carry ORDER BY / LIMIT (order restored in Go)")
	}
	if len(idArgs) != 18*2 {
		t.Errorf("by-IDs args = %d, want %d", len(idArgs), 18*2)
	}

	// skipLabels drops the labels point and !includeWispReverseDeps drops the
	// three rc-wisp branches, leaving 11 injection points (dc, three rc-deps
	// branches binding twice each, cc, pc, d, driver).
	_, idArgsNoLabels := SearchCountsSQL(IssuesFilterTables, []string{"a", "b"}, "", "", "", false, CountsHydration{SkipLabels: true})
	if len(idArgsNoLabels) != 11*2 {
		t.Errorf("by-IDs args (skipLabels, no wisp deps) = %d, want %d", len(idArgsNoLabels), 11*2)
	}

	// SkipCounts drops the three cardinality joins and projects constants in
	// their place, so the six extra columns the scan reads positionally stay
	// six and in the same order.
	skipCounts, _ := SearchCountsSQL(IssuesFilterTables, nil, "", "", "", true, CountsHydration{SkipCounts: true})
	for _, gone := range []string{"dc ON dc.issue_id", "rc ON rc.dep_id", "cc ON cc.issue_id", "COALESCE(dc.cnt, 0)", "UNION ALL"} {
		if strings.Contains(skipCounts, gone) {
			t.Errorf("counts SQL must drop %q when SkipCounts is set:\n%s", gone, skipCounts)
		}
	}
	for _, want := range []string{"0 AS dep_count", "0 AS rdep_count", "0 AS comment_count", "pc.parent_id     AS parent_id", "d.deps_json      AS deps_json"} {
		if !strings.Contains(skipCounts, want) {
			t.Errorf("counts SQL missing %q under SkipCounts:\n%s", want, skipCounts)
		}
	}
	// Labels survive SkipCounts: the two opt-outs are independent.
	if !strings.Contains(skipCounts, "JSON_ARRAYAGG(label)") {
		t.Error("SkipCounts must not drop the labels join")
	}

	// By-IDs: SkipCounts removes the dc, rc-deps, rc-wisp and cc injection
	// points, leaving labels, pc, d and the driver.
	_, idArgsNoCounts := SearchCountsSQL(WispsFilterTables, []string{"a", "b"}, "", "", "", true, CountsHydration{SkipCounts: true})
	if len(idArgsNoCounts) != 4*2 {
		t.Errorf("by-IDs args (skipCounts) = %d, want %d", len(idArgsNoCounts), 4*2)
	}
	// Both opt-outs together leave pc, d and the driver.
	_, idArgsNeither := SearchCountsSQL(WispsFilterTables, []string{"a", "b"}, "", "", "", true, CountsHydration{SkipLabels: true, SkipCounts: true})
	if len(idArgsNeither) != 3*2 {
		t.Errorf("by-IDs args (skipLabels+skipCounts) = %d, want %d", len(idArgsNeither), 3*2)
	}
}

// TestSearchCountsSQLByIDsReverseBlockersIndexable pins the by-IDs form's
// reverse-blocker (rc) match (#7635). Matching COALESCE(depends_on_issue_id,
// depends_on_wisp_id, depends_on_external) IN (ids) can use no index, so every
// by-IDs batch re-read every blocks edge in both dependency tables. The match
// is one branch per target column instead, each an IN on that column — the
// column its own index serves — and each later branch requires the earlier
// columns NULL. That guard is what keeps the rewrite equal to COALESCE on a
// row with two targets, which ck_dep_one_target / ck_wisp_dep_one_target
// forbid but a store can still hold (schema/dep_id_backfill.go leaves such
// rows for bd doctor): COALESCE files such a row under its FIRST non-NULL
// target only, and so do the guarded branches.
//
// FORK, unlike upstream #8535: every branch also carries the old filter,
// COALESCE(...) IN (ids), as a residual after the column match, and projects
// the COALESCE rather than the bare column, so a column whose collation
// differs from the COALESCE result's cannot admit or join an id the old form
// did not (review bd-lwx6j; issueops.DepTargetPredicates). The residual rides
// on the seeked range; it is never the branch's leading match.
//
// The predicate form keeps its keyless COALESCE projection: it groups the
// whole table either way, and has no id list to push.
func TestSearchCountsSQLByIDsReverseBlockersIndexable(t *testing.T) {
	t.Parallel()

	ids := []string{"a", "b", "c"}
	branches := func(table string) []string {
		return []string{
			"SELECT " + DepTargetExpr + " AS dep_id FROM " + table + " WHERE type = 'blocks' AND depends_on_issue_id IN (?,?,?) AND " + DepTargetExpr + " IN (?,?,?)",
			"SELECT " + DepTargetExpr + " AS dep_id FROM " + table + " WHERE type = 'blocks' AND depends_on_issue_id IS NULL AND depends_on_wisp_id IN (?,?,?) AND " + DepTargetExpr + " IN (?,?,?)",
			"SELECT " + DepTargetExpr + " AS dep_id FROM " + table + " WHERE type = 'blocks' AND depends_on_issue_id IS NULL AND depends_on_wisp_id IS NULL AND depends_on_external IN (?,?,?) AND " + DepTargetExpr + " IN (?,?,?)",
		}
	}
	squash := func(s string) string { return strings.Join(strings.Fields(s), " ") }

	byIDs, _ := SearchCountsSQL(WispsFilterTables, ids, "", "", "", true, CountsHydration{})
	flat := squash(byIDs)
	for _, table := range []string{"dependencies", "wisp_dependencies"} {
		for _, want := range branches(table) {
			if !strings.Contains(flat, want) {
				t.Errorf("by-IDs rc missing branch %q:\n%s", want, byIDs)
			}
		}
	}
	// The COALESCE match is only ever the residual: never right after the
	// type filter, where it would be the branch's leading (unindexable) match.
	if strings.Contains(flat, "type = 'blocks' AND "+DepTargetExpr+" IN (") {
		t.Errorf("by-IDs rc must not lead with the unindexable %s:\n%s", DepTargetExpr, byIDs)
	}
	if got := strings.Count(flat, "AND "+DepTargetExpr+" IN (?,?,?)"); got != 6 {
		t.Errorf("by-IDs rc carries the residual %s filter %d times, want 6 (one per branch):\n%s", DepTargetExpr, got, byIDs)
	}

	// Without the wisp dependency table only the dependencies branches render.
	noWisp, _ := SearchCountsSQL(IssuesFilterTables, ids, "", "", "", false, CountsHydration{})
	if strings.Contains(noWisp, "wisp_dependencies") {
		t.Errorf("by-IDs rc must not read wisp_dependencies when the probe says absent:\n%s", noWisp)
	}
	for _, want := range branches("dependencies") {
		if !strings.Contains(squash(noWisp), want) {
			t.Errorf("by-IDs rc (no wisp deps) missing branch %q", want)
		}
	}

	// The predicate form's rc is unchanged: keyless, one COALESCE projection
	// per table.
	pred, _ := SearchCountsSQL(WispsFilterTables, nil, "", "", "", true, CountsHydration{})
	for _, table := range []string{"dependencies", "wisp_dependencies"} {
		want := "SELECT " + DepTargetExpr + " AS dep_id FROM " + table + " WHERE type = 'blocks' UNION ALL"
		if table == "wisp_dependencies" {
			want = "SELECT " + DepTargetExpr + " AS dep_id FROM " + table + " WHERE type = 'blocks' ) all_blockers"
		}
		if !strings.Contains(squash(pred), want) {
			t.Errorf("predicate rc changed; missing %q:\n%s", want, pred)
		}
	}
}

// TestSearchCountsSQLByIDsArgsMatchPlaceholders pins that the by-IDs form
// returns exactly one arg per placeholder, for every hydration and both wisp
// reverse-dependency settings. Every injection point binds the same id list,
// so the count is the whole lockstep contract: a branch added to the SQL
// without its args (or the reverse) fails here rather than as a driver error.
func TestSearchCountsSQLByIDsArgsMatchPlaceholders(t *testing.T) {
	t.Parallel()

	ids := []string{"a", "b", "c"}
	for _, tables := range []FilterTables{IssuesFilterTables, WispsFilterTables} {
		for _, wispDeps := range []bool{false, true} {
			for _, hyd := range []CountsHydration{
				{}, {SkipLabels: true}, {SkipCounts: true}, {SkipLabels: true, SkipCounts: true},
				{Lite: true}, {Lite: true, SkipLabels: true, SkipCounts: true},
			} {
				q, args := SearchCountsSQL(tables, ids, "", "", "", wispDeps, hyd)
				if got := strings.Count(q, "?"); got != len(args) {
					t.Errorf("%s wispDeps=%v %+v: %d placeholders, %d args", tables.Main, wispDeps, hyd, got, len(args))
				}
				if len(args)%len(ids) != 0 {
					t.Errorf("%s wispDeps=%v %+v: %d args is not a whole number of id lists", tables.Main, wispDeps, hyd, len(args))
				}
			}
		}
	}
}

func TestBuildReadyWorkWhereStatusFilter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		filter          types.WorkFilter
		in              ReadyWorkWhereInputs
		wantClause      string
		rejectClauses   []string // none may appear in the WHERE
		wantLeadingArgs []any
	}{
		{
			name:            "StatusesInClause",
			filter:          types.WorkFilter{Statuses: []types.Status{"open", "blocked", "pinned"}},
			wantClause:      "status IN (?,?,?)",
			wantLeadingArgs: []any{"open", "blocked", "pinned"},
		},
		{
			name:            "SingularStatusWinsOverStatuses",
			filter:          types.WorkFilter{Status: "open", Statuses: []types.Status{"blocked", "pinned"}},
			in:              ReadyWorkWhereInputs{ActiveCustomStatuses: []string{"review"}, StatusCollationMatches: true},
			wantClause:      "status IN (?,?) AND",
			rejectClauses:   []string{"custom_statuses", "status IN (?,?,?)"},
			wantLeadingArgs: []any{"open", "review"},
		},
		{
			// The ready-eligible set is bound as literals, open first, so Dolt
			// can bound the [is_blocked,status] range (bd-7c0u4 Q4); a
			// custom_statuses subquery would make it [NULL, ∞). Only when the
			// resolve proved both columns share a collation.
			name:            "OpenIncludesCustomActiveCategory",
			filter:          types.WorkFilter{Status: types.StatusOpen},
			in:              ReadyWorkWhereInputs{ActiveCustomStatuses: []string{"review", "open", "", "triage", "review"}, StatusCollationMatches: true},
			wantClause:      "WHERE status IN (?,?,?) AND",
			rejectClauses:   []string{"custom_statuses", "status IN ('open', 'in_progress')"},
			wantLeadingArgs: []any{"open", "review", "triage"},
		},
		{
			// Different collations: the literal list would compare under
			// issues.status's collation, the subquery under the name column's,
			// so the subquery stays (review bd-s9czb).
			name:            "OpenWithActiveCustomDifferentCollationKeepsSubquery",
			filter:          types.WorkFilter{Status: types.StatusOpen},
			in:              ReadyWorkWhereInputs{ActiveCustomStatuses: []string{"review"}, StatusCollationMatches: false},
			wantClause:      "WHERE (status = ? OR status IN (SELECT name FROM custom_statuses WHERE category = 'active')) AND",
			rejectClauses:   []string{"status IN (?"},
			wantLeadingArgs: []any{"open"},
		},
		{
			// Unknown collation is the zero value (the resolve could not
			// tell, e.g. an empty table): the same safe subquery.
			name:            "OpenWithActiveCustomUnknownCollationKeepsSubquery",
			filter:          types.WorkFilter{Status: types.StatusOpen},
			in:              ReadyWorkWhereInputs{ActiveCustomStatuses: []string{"review"}},
			wantClause:      "WHERE (status = ? OR status IN (SELECT name FROM custom_statuses WHERE category = 'active')) AND",
			rejectClauses:   []string{"status IN (?"},
			wantLeadingArgs: []any{"open"},
		},
		{
			name:            "OpenWithoutActiveCustomIsExact",
			filter:          types.WorkFilter{Status: types.StatusOpen},
			wantClause:      "WHERE status = ? AND",
			rejectClauses:   []string{"custom_statuses", "status IN ("},
			wantLeadingArgs: []any{"open"},
		},
		{
			name:            "NonOpenSingularStatusStaysExact",
			filter:          types.WorkFilter{Status: types.StatusInProgress},
			in:              ReadyWorkWhereInputs{ActiveCustomStatuses: []string{"review"}},
			wantClause:      "WHERE status = ? AND",
			rejectClauses:   []string{"custom_statuses"},
			wantLeadingArgs: []any{"in_progress"},
		},
		{
			name:          "EmptyFilterLegacyDefault",
			filter:        types.WorkFilter{},
			in:            ReadyWorkWhereInputs{ActiveCustomStatuses: []string{"review"}},
			wantClause:    "status IN ('open', 'in_progress')",
			rejectClauses: []string{"status = ?", "custom_statuses"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			where, args, err := BuildReadyWorkWhere(tt.filter, IssuesFilterTables, tt.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(where, tt.wantClause) {
				t.Errorf("status clause %q missing.\n where = %s", tt.wantClause, where)
			}
			for _, reject := range tt.rejectClauses {
				if strings.Contains(where, reject) {
					t.Errorf("unexpected status clause %q present.\n where = %s", reject, where)
				}
			}
			if len(args) < len(tt.wantLeadingArgs) {
				t.Fatalf("args = %v, want at least the %d leading status args %v", args, len(tt.wantLeadingArgs), tt.wantLeadingArgs)
			}
			for i, want := range tt.wantLeadingArgs {
				if args[i] != want {
					t.Errorf("args[%d] = %v, want %v (status args must lead)", i, args[i], want)
				}
			}
		})
	}
}

func TestNeedsActiveCustomStatuses(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		filter types.WorkFilter
		want   bool
	}{
		{types.WorkFilter{Status: types.StatusOpen}, true},
		{types.WorkFilter{Status: types.StatusOpen, Statuses: []types.Status{"blocked"}}, true},
		{types.WorkFilter{Status: types.StatusInProgress}, false},
		{types.WorkFilter{Statuses: []types.Status{"open"}}, false},
		{types.WorkFilter{}, false},
	} {
		if got := NeedsActiveCustomStatuses(tc.filter); got != tc.want {
			t.Errorf("NeedsActiveCustomStatuses(%+v) = %v, want %v", tc.filter, got, tc.want)
		}
	}
}

// TestOptionalWispTable pins the set a wisp query may treat as "no wisps".
// leases and wisp_labels are joined or hydrated by that query but are not the
// wisp plane being absent, so tolerating them turns a broken database into an
// empty result.
func TestOptionalWispTable(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"wisps", "wisp_dependencies", "WISPS"} {
		if !OptionalWispTable(name) {
			t.Errorf("OptionalWispTable(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"leases", "wisp_labels", "issues", "labels", "", "wisp"} {
		if OptionalWispTable(name) {
			t.Errorf("OptionalWispTable(%q) = true, want false", name)
		}
	}
}

// TestActiveCustomStatusesSQLReadsBothCollations pins the resolve statement:
// one statement returning the active names, each with the name column's
// collation and the status collation of the table the ready clause reads.
func TestActiveCustomStatusesSQLReadsBothCollations(t *testing.T) {
	t.Parallel()
	for _, table := range []string{"issues", "wisps"} {
		want := "SELECT name, COLLATION(name), (SELECT COLLATION(status) FROM " + table + " LIMIT 1)" +
			" FROM custom_statuses WHERE category = 'active' ORDER BY name"
		if got := ActiveCustomStatusesSQL(table); got != want {
			t.Errorf("ActiveCustomStatusesSQL(%q) =\n%s\nwant\n%s", table, got, want)
		}
	}
}
