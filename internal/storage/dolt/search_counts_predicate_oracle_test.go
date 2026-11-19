package dolt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/storage/sqlbuild"
	"github.com/steveyegge/beads/internal/types"
)

// TestSearchIssuesWithCountsMatchesThePredicateFormOracle (AC-3, GH#4370) runs
// a filter × order × limit × hydration matrix through the store-backed counted
// search and compares every answer — membership, order, counts, parent,
// labels, dependencies, and the MaxRows verdict — with an oracle that replays
// the pre-change path: the PREDICATE form of the counts query per plane under
// the old WHERE/ORDER BY/LIMIT, the Go-side id sort and trim, the
// wisp-preferring merge, and the terminal sort, Limit trim and MaxRows cap.
//
// The matrix reaches every route the counted search takes (#7635): bounded
// pages within one by-IDs batch go ids-first; an unbounded or above-ceiling
// read probes for at most one batch of ids past it and goes ids-first when the
// probe holds every match (the label, ids and text filters); a probe that
// overflows (the fixture holds more than sqlbuild.QueryBatchSize matching
// rows), a Go-side id sort and a label filter with an empty entry keep the
// predicate form. One row carries a stored empty label, so an empty filter
// entry that the id scan's label JOINs dropped would change the answer.
func TestSearchIssuesWithCountsMatchesThePredicateFormOracle(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	// The 672-case matrix outlives testContext's per-test budget on a loaded
	// host (fork harness); bound it on its own.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	const durable = 210
	issues := make([]*types.Issue, 0, durable)
	for i := 0; i < durable; i++ {
		status := types.StatusOpen
		if i%5 == 0 {
			status = types.StatusInProgress
		}
		issues = append(issues, &types.Issue{
			ID: fmt.Sprintf("test-o%03d", i), Title: fmt.Sprintf("oracle row %d", (i*37)%durable),
			Status: status, Priority: i % 4, IssueType: types.TypeTask,
		})
	}
	if err := store.CreateIssues(ctx, issues, "tester"); err != nil {
		t.Fatalf("CreateIssues: %v", err)
	}
	for _, w := range []string{"test-ow0", "test-ow1", "test-ow2", "test-ow3"} {
		wisp := &types.Issue{ID: w, Title: "oracle wisp " + w, Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask, Ephemeral: true}
		if err := store.CreateIssue(ctx, wisp, "tester"); err != nil {
			t.Fatalf("CreateIssue %s: %v", w, err)
		}
	}
	for i := 0; i < durable; i += 7 {
		if err := store.AddLabel(ctx, fmt.Sprintf("test-o%03d", i), "alpha", "tester"); err != nil {
			t.Fatalf("AddLabel: %v", err)
		}
	}
	for _, d := range []*types.Dependency{
		{IssueID: "test-o001", DependsOnID: "test-o000", Type: types.DepBlocks},
		{IssueID: "test-o002", DependsOnID: "test-o000", Type: types.DepBlocks},
		{IssueID: "test-o003", DependsOnID: "test-o000", Type: types.DepParentChild},
		{IssueID: "test-o205", DependsOnID: "test-o204", Type: types.DepBlocks},
		{IssueID: "test-o206", DependsOnID: "external:other:cap", Type: types.DepBlocks},
		{IssueID: "test-ow1", DependsOnID: "test-ow0", Type: types.DepBlocks},
		{IssueID: "test-ow2", DependsOnID: "test-o000", Type: types.DepBlocks},
		{IssueID: "test-o007", DependsOnID: "test-ow0", Type: types.DepBlocks},
	} {
		if err := store.AddDependency(ctx, d, "tester"); err != nil {
			t.Fatalf("AddDependency %s->%s: %v", d.IssueID, d.DependsOnID, err)
		}
	}
	for _, id := range []string{"test-o000", "test-o000", "test-o201", "test-ow0"} {
		if err := store.AddComment(ctx, id, "tester", "note"); err != nil {
			t.Fatalf("AddComment %s: %v", id, err)
		}
	}
	// A stored empty label, which the predicate form's `label = ?` bound to ""
	// matches and the id scan's label JOINs would drop.
	if _, err := store.db.ExecContext(ctx, "INSERT INTO labels (issue_id, label) VALUES (?, '')", "test-o015"); err != nil {
		t.Fatalf("seed an empty label: %v", err)
	}

	type variant struct {
		name  string
		query string
		apply func(*types.IssueFilter)
	}
	filters := []variant{
		{"all", "", func(*types.IssueFilter) {}},
		{"open", "", func(f *types.IssueFilter) { f.Statuses = []types.Status{types.StatusOpen} }},
		{"label", "", func(f *types.IssueFilter) { f.Labels = []string{"alpha"} }},
		{"text", "oracle row 1", func(*types.IssueFilter) {}},
		{"labels-any", "", func(f *types.IssueFilter) { f.LabelsAny = []string{"alpha", "nonexistent"} }},
		{"labels-empty-entry", "", func(f *types.IssueFilter) { f.Labels = []string{"alpha", ""} }},
		{"labels-any-empty-entry", "", func(f *types.IssueFilter) { f.LabelsAny = []string{"alpha", ""} }},
		{"exclude-label", "", func(f *types.IssueFilter) { f.ExcludeLabels = []string{"alpha"} }},
		{"ids", "", func(f *types.IssueFilter) { f.IDs = []string{"test-o000", "test-o007", "test-o201", "test-ow1"} }},
		{"id-prefix", "", func(f *types.IssueFilter) { f.IDPrefix = "test-o1" }},
		{"priority-min", "", func(f *types.IssueFilter) { f.PriorityMin = ptr(2) }},
		{"not-pinned", "", func(f *types.IssueFilter) { f.Pinned = ptr(false) }},
	}
	orders := []variant{
		{"default", "", func(*types.IssueFilter) {}},
		{"created", "", func(f *types.IssueFilter) { f.SortBy = "created" }},
		{"title", "", func(f *types.IssueFilter) { f.SortBy = "title" }},
		{"priority-desc", "", func(f *types.IssueFilter) { f.SortBy, f.SortDesc = "priority", true }},
		{"id-desc", "", func(f *types.IssueFilter) { f.SortBy, f.SortDesc = "id", true }},
		{"status", "", func(f *types.IssueFilter) { f.SortBy = "status" }},
		{"status-desc", "", func(f *types.IssueFilter) { f.SortBy, f.SortDesc = "status", true }},
	}
	windows := []variant{
		{"unlimited", "", func(*types.IssueFilter) {}},
		{"limit1", "", func(f *types.IssueFilter) { f.Limit = 1 }},
		{"limit201", "", func(f *types.IssueFilter) { f.Limit = 201 }},
		{"capped", "", func(f *types.IssueFilter) { f.MaxRows, f.MaxRowsSource = 50, "test" }},
	}
	hydrations := []variant{
		{"full", "", func(*types.IssueFilter) {}},
		{"skip-labels", "", func(f *types.IssueFilter) { f.SkipLabels = true }},
		{"skip-counts", "", func(f *types.IssueFilter) { f.SkipCounts = true }},
		{"lite", "", func(f *types.IssueFilter) { f.Lite = true }},
	}

	pastBatch := false
	n := 0
	for _, fv := range filters {
		for _, ov := range orders {
			for _, wv := range windows {
				for _, skipWisps := range []bool{true, false} {
					hv := hydrations[n%len(hydrations)]
					n++
					var filter types.IssueFilter
					fv.apply(&filter)
					ov.apply(&filter)
					wv.apply(&filter)
					hv.apply(&filter)
					filter.SkipWisps = skipWisps
					name := fmt.Sprintf("%s/%s/%s/%s/skipWisps=%v", fv.name, ov.name, wv.name, hv.name, skipWisps)

					want, wantErr := oracleSearchCounts(ctx, t, store.db, fv.query, filter)
					got, gotErr := store.SearchIssuesWithCounts(ctx, fv.query, filter)
					var tooMany *issueops.ErrTooManyRows
					if (wantErr != nil) != (gotErr != nil) || (wantErr != nil && !errors.As(gotErr, &tooMany)) {
						t.Fatalf("%s: error = %v, oracle error = %v", name, gotErr, wantErr)
					}
					if wantErr != nil {
						continue
					}
					if len(want) > sqlbuild.QueryBatchSize {
						pastBatch = true
					}
					gotRows := make([]countsRow, 0, len(got))
					for _, iwc := range got {
						gotRows = append(gotRows, toCountsRow(iwc))
					}
					if len(gotRows) != len(want) {
						t.Fatalf("%s: %d rows, oracle %d", name, len(gotRows), len(want))
					}
					for i := range want {
						if gotRows[i] != want[i] {
							t.Fatalf("%s: row %d differs from the oracle:\n got   =%+v\n oracle=%+v", name, i, gotRows[i], want[i])
						}
					}
				}
			}
		}
	}
	if !pastBatch {
		t.Fatalf("no case returned more than %d rows; the matrix never overflowed the ids-first probe", sqlbuild.QueryBatchSize)
	}
}

// oracleSearchCounts replays the pre-GH#4370 SearchIssuesWithCountsInTx for a
// store whose wisp tables exist and hold rows: each plane through the
// predicate-form counts query, then the merge and terminal finish.
func oracleSearchCounts(ctx context.Context, t *testing.T, db *sql.DB, query string, filter types.IssueFilter) ([]countsRow, error) {
	t.Helper()
	hyd := sqlbuild.CountsHydration{SkipLabels: filter.SkipLabels, SkipCounts: filter.SkipCounts, Lite: filter.Lite}
	plane := func(tables sqlbuild.FilterTables) []*types.IssueWithCounts {
		clauses, args, err := sqlbuild.BuildIssueFilterClauses(query, filter, tables)
		if err != nil {
			t.Fatalf("BuildIssueFilterClauses: %v", err)
		}
		whereSQL := ""
		if len(clauses) > 0 {
			whereSQL = "WHERE " + strings.Join(clauses, " AND ")
		}
		goSide := sqlbuild.IsGoSideSort(filter.SortBy)
		eff := issueops.EffectiveSearchLimit(filter.Limit, filter.MaxRows)
		limitSQL := ""
		if eff > 0 && !goSide {
			limitSQL = fmt.Sprintf("LIMIT %d", eff)
		}
		q, _ := sqlbuild.SearchCountsSQL(tables, nil, whereSQL, sqlbuild.OrderBy(filter.SortBy, filter.SortDesc, "i"), limitSQL, true, hyd)
		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			t.Fatalf("oracle query: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var out []*types.IssueWithCounts
		seen := map[string]bool{}
		for rows.Next() {
			iwc, err := issueops.ScanReadyWorkRowWithCounts(rows, hyd)
			if err != nil {
				t.Fatalf("oracle scan: %v", err)
			}
			if iwc == nil || iwc.Issue == nil || seen[iwc.Issue.ID] {
				continue
			}
			seen[iwc.Issue.ID] = true
			out = append(out, iwc)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("oracle rows: %v", err)
		}
		if goSide {
			sort.SliceStable(out, func(i, j int) bool { return sqlbuild.LessID(out[i].Issue.ID, out[j].Issue.ID, filter.SortDesc) })
			if eff > 0 && len(out) > eff {
				out = out[:eff]
			}
		}
		return out
	}

	merged := plane(sqlbuild.IssuesFilterTables)
	if !filter.SkipWisps {
		wisps := plane(sqlbuild.WispsFilterTables)
		inWisps := map[string]bool{}
		for _, w := range wisps {
			inWisps[w.Issue.ID] = true
		}
		kept := merged[:0:0]
		for _, iwc := range merged {
			if !inWisps[iwc.Issue.ID] {
				kept = append(kept, iwc)
			}
		}
		merged = append(kept, wisps...)
	}
	sort.SliceStable(merged, func(i, j int) bool {
		return sqlbuild.Less(merged[i].Issue, merged[j].Issue, filter.SortBy, filter.SortDesc)
	})
	if filter.Limit > 0 && len(merged) > filter.Limit {
		merged = merged[:filter.Limit]
	}
	if err := issueops.EnforceMaxRowsCap(len(merged), filter.MaxRows, filter.MaxRowsSource); err != nil {
		return nil, err
	}
	out := make([]countsRow, 0, len(merged))
	for _, iwc := range merged {
		out = append(out, toCountsRow(iwc))
	}
	return out, nil
}

// toCountsRow flattens one IssueWithCounts the way runCountsSQL does, with
// labels in sorted order and the dependencies in the order the read emitted
// them: every form must emit primary-key order (bd-2i55u), so a comparison
// of two forms compares it.
func toCountsRow(iwc *types.IssueWithCounts) countsRow {
	labels := append([]string(nil), iwc.Issue.Labels...)
	sort.Strings(labels)
	parent := ""
	if iwc.Parent != nil {
		parent = *iwc.Parent
	}
	deps := make([]string, 0, len(iwc.Issue.Dependencies))
	for _, d := range iwc.Issue.Dependencies {
		deps = append(deps, string(d.Type)+":"+d.DependsOnID)
	}
	return countsRow{
		id:           iwc.Issue.ID,
		depCount:     iwc.DependencyCount,
		rdepCount:    iwc.DependentCount,
		commentCount: iwc.CommentCount,
		parent:       parent,
		labels:       strings.Join(labels, ","),
		deps:         strings.Join(deps, "|"),
	}
}

// seededCountsGraph replaces the rows of every table the counted search reads
// with a random graph for one seed and returns the durable issue ids. Tokens:
// statuses open, in_progress, closed, pinned, deferred and blocked; labels
// from a small vocabulary on some rows and none on others, plus page labels
// on exactly 1, 200 and 201 durable issues; blocks, parent-child and related
// edges to issue, wisp, external and dangling targets from both planes;
// comments on some rows of both planes. One durable issue is a target in all
// three target columns of dependencies and in two of wisp_dependencies.
// vocab is the label vocabulary (seededLabelVocab unless a case widens it).
func seededCountsGraph(t *testing.T, ctx context.Context, db *sql.DB, rng *rand.Rand, vocab []string) (durable, wisps []string) {
	t.Helper()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("generate %.120s: %v", q, err)
		}
	}
	exec("SET FOREIGN_KEY_CHECKS=0")
	defer exec("SET FOREIGN_KEY_CHECKS=1")
	for _, tb := range []string{"wisp_comments", "comments", "wisp_labels", "labels", "wisp_dependencies", "dependencies", "wisps", "issues"} {
		exec("DELETE FROM " + tb)
	}
	statuses := []string{"open", "open", "in_progress", "closed", "closed", "pinned", "deferred", "blocked"}
	words := []string{"alpha", "beta", "gamma", "delta", "omega"}
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	nI, nW := 205+rng.Intn(50), 10+rng.Intn(30)
	row := func(id string) string {
		created := base.Add(time.Duration(rng.Intn(400)) * time.Minute).Format("2006-01-02 15:04:05")
		return fmt.Sprintf("('%s','%s %s','','','','','%s',%d,'task','%s','%s')", id, words[rng.Intn(len(words))], id,
			statuses[rng.Intn(len(statuses))], rng.Intn(5), created, created)
	}
	var issueRows, wispRows []string
	for i := 0; i < nI; i++ {
		durable = append(durable, fmt.Sprintf("sc-%03d", i))
		issueRows = append(issueRows, row(durable[i]))
	}
	for j := 0; j < nW; j++ {
		wisps = append(wisps, fmt.Sprintf("sc-w%02d", j))
		wispRows = append(wispRows, row(wisps[j]))
	}
	const cols = "(id,title,description,design,acceptance_criteria,notes,status,priority,issue_type,created_at,updated_at) VALUES "
	exec("INSERT INTO issues " + cols + strings.Join(issueRows, ","))
	exec("INSERT INTO wisps " + cols + strings.Join(wispRows, ","))

	var labels, wlabels []string
	for _, id := range durable {
		for _, l := range vocab {
			if rng.Intn(5) == 0 {
				labels = append(labels, fmt.Sprintf("('%s','%s')", id, l))
			}
		}
	}
	for _, id := range wisps {
		for _, l := range vocab {
			if rng.Intn(4) == 0 {
				wlabels = append(wlabels, fmt.Sprintf("('%s','%s')", id, l))
			}
		}
	}
	perm := rng.Perm(nI)
	for k, n := range map[string]int{"page:one": 1, "page:200": 200, "page:201": 201} {
		_ = k
		for _, i := range perm[:n] {
			labels = append(labels, fmt.Sprintf("('%s','%s')", durable[i], k))
		}
	}
	exec("INSERT INTO labels (issue_id,label) VALUES " + strings.Join(labels, ","))
	if len(wlabels) > 0 {
		exec("INSERT INTO wisp_labels (issue_id,label) VALUES " + strings.Join(wlabels, ","))
	}

	var deps, wdeps []string
	seen := map[string]bool{}
	edge := func(wispSrc bool, src, col, target, typ string) {
		key := fmt.Sprint(wispSrc, src, col, target)
		if seen[key] {
			return
		}
		seen[key] = true
		vals := map[string]string{"depends_on_issue_id": "NULL", "depends_on_wisp_id": "NULL", "depends_on_external": "NULL"}
		vals[col] = "'" + target + "'"
		r := fmt.Sprintf("('%036d','%s',%s,%s,%s,'%s','tester')", len(deps)+len(wdeps), src,
			vals["depends_on_issue_id"], vals["depends_on_wisp_id"], vals["depends_on_external"], typ)
		if wispSrc {
			wdeps = append(wdeps, r)
		} else {
			deps = append(deps, r)
		}
	}
	depTypes := []string{"blocks", "blocks", "parent-child", "related"}
	for k := 0; k < nI+nW; k++ {
		wispSrc := rng.Intn(5) == 0
		src := durable[rng.Intn(nI)]
		if wispSrc {
			src = wisps[rng.Intn(nW)]
		}
		col, target := "depends_on_issue_id", durable[rng.Intn(nI)]
		switch r := rng.Intn(10); {
		case r < 2:
			col, target = "depends_on_wisp_id", wisps[rng.Intn(nW)]
		case r < 3:
			col, target = "depends_on_external", fmt.Sprintf("external:sc:%d", rng.Intn(4))
		case r < 4:
			target = fmt.Sprintf("sc-gone-%d", rng.Intn(4))
		}
		edge(wispSrc, src, col, target, depTypes[rng.Intn(len(depTypes))])
	}
	// One durable id that is a blocks target in every column.
	multi := durable[rng.Intn(nI)]
	edge(false, durable[rng.Intn(nI)], "depends_on_issue_id", multi, "blocks")
	edge(false, durable[rng.Intn(nI)], "depends_on_wisp_id", multi, "blocks")
	edge(false, durable[rng.Intn(nI)], "depends_on_external", multi, "blocks")
	edge(true, wisps[rng.Intn(nW)], "depends_on_issue_id", multi, "blocks")
	edge(true, wisps[rng.Intn(nW)], "depends_on_wisp_id", multi, "blocks")
	const depCols = "(id,issue_id,depends_on_issue_id,depends_on_wisp_id,depends_on_external,type,created_by) VALUES "
	exec("INSERT INTO dependencies " + depCols + strings.Join(deps, ","))
	exec("INSERT INTO wisp_dependencies " + depCols + strings.Join(wdeps, ","))

	var comments, wcomments []string
	for k := 0; k < nI/3; k++ {
		comments = append(comments, fmt.Sprintf("('%036d','%s','tester','c')", k, durable[rng.Intn(nI)]))
	}
	for k := 0; k < nW/2+1; k++ {
		wcomments = append(wcomments, fmt.Sprintf("('%036d','%s','tester','c')", k, wisps[rng.Intn(nW)]))
	}
	exec("INSERT INTO comments (id,issue_id,author,text) VALUES " + strings.Join(comments, ","))
	exec("INSERT INTO wisp_comments (id,issue_id,author,text) VALUES " + strings.Join(wcomments, ","))
	return durable, wisps
}

// seededLabelVocab is seededCountsGraph's default label vocabulary.
var seededLabelVocab = []string{"alpha", "beta", "gamma-1", "gamma-2", "hot"}

// TestSearchCountsIDsFirstEqualsPredicateFormSeeded is the comparison test
// of bd-c43b7. The two pieces: the store's counted search (ids-first for a
// page of up to 200, or an unbounded read whose probe holds at most 200) and
// the predicate form it replaced (oracleSearchCounts). Relation: equal rows,
// counts, parents, labels, dependencies and order, and the same MaxRows
// verdict.
//
// Dependencies are compared in the order each read emitted them, not sorted
// (review bd-r8crc): the arrays must agree element for element.
//
// Per seeded graph (seededCountsGraph) it runs filter x window x skipWisps,
// with the order and the hydration rotating: filters --all (no status),
// bd list's default exclusions, one status, two statuses, labels matching 0,
// 1, 200 and 201 issues, a label, label-any, a label pattern and a label with
// a status; windows unbounded and 1, 50, 200, 201 rows and a MaxRows cap.
// After the seeds it drops wisp_comments, then leases, and requires both
// forms to fail on an empty, a one-row and an unbounded read (#5942).
//
// Bounds: the seeds below, 144 cases each, and a 15-minute budget; a failure
// names its seed and case.
func TestSearchCountsIDsFirstEqualsPredicateFormSeeded(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	const budget = 15 * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	seeds := []int64{11, 23, 37, 41, 59}
	type variant struct {
		name  string
		apply func(*types.IssueFilter)
	}
	filters := []variant{
		{"all", func(*types.IssueFilter) {}},
		{"default-list", func(f *types.IssueFilter) { f.ExcludeStatus = []types.Status{types.StatusClosed, types.StatusPinned} }},
		{"status-open", func(f *types.IssueFilter) { f.Statuses = []types.Status{types.StatusOpen} }},
		{"status-open-in_progress", func(f *types.IssueFilter) {
			f.Statuses = []types.Status{types.StatusOpen, types.StatusInProgress}
		}},
		{"label-0", func(f *types.IssueFilter) { f.Labels = []string{"page:none"} }},
		{"label-1", func(f *types.IssueFilter) { f.Labels = []string{"page:one"} }},
		{"label-200", func(f *types.IssueFilter) { f.Labels = []string{"page:200"} }},
		{"label-201", func(f *types.IssueFilter) { f.Labels = []string{"page:201"} }},
		{"label", func(f *types.IssueFilter) { f.Labels = []string{"alpha"} }},
		{"label-any", func(f *types.IssueFilter) { f.LabelsAny = []string{"beta", "hot"} }},
		{"label-pattern", func(f *types.IssueFilter) { f.LabelPattern = "gamma-*" }},
		{"label-and-status", func(f *types.IssueFilter) {
			f.Labels, f.Statuses = []string{"page:200"}, []types.Status{types.StatusOpen}
		}},
	}
	windows := []variant{
		{"unbounded", func(*types.IssueFilter) {}},
		{"limit1", func(f *types.IssueFilter) { f.Limit = 1 }},
		{"limit50", func(f *types.IssueFilter) { f.Limit = 50 }},
		{"limit200", func(f *types.IssueFilter) { f.Limit = 200 }},
		{"limit201", func(f *types.IssueFilter) { f.Limit = 201 }},
		{"capped150", func(f *types.IssueFilter) { f.MaxRows, f.MaxRowsSource = 150, "test" }},
	}
	orders := []variant{
		{"default", func(*types.IssueFilter) {}},
		{"created", func(f *types.IssueFilter) { f.SortBy = "created" }},
		{"priority-desc", func(f *types.IssueFilter) { f.SortBy, f.SortDesc = "priority", true }},
		{"id", func(f *types.IssueFilter) { f.SortBy = "id" }},
		{"title", func(f *types.IssueFilter) { f.SortBy = "title" }},
	}
	hydrations := []variant{
		{"full", func(*types.IssueFilter) {}},
		{"skip-labels", func(f *types.IssueFilter) { f.SkipLabels = true }},
		{"skip-counts", func(f *types.IssueFilter) { f.SkipCounts = true }},
		{"lite", func(f *types.IssueFilter) { f.Lite = true }},
	}

	start := time.Now()
	where := "setup"
	defer func() {
		if t.Failed() {
			t.Logf("failed at %s (seeds %v)", where, seeds)
		}
	}()
	cases, rows, sizes := 0, 0, map[int]bool{}
	for _, seed := range seeds {
		rng := rand.New(rand.NewSource(seed))
		durable, wisps := seededCountsGraph(t, ctx, store.db, rng, seededLabelVocab)
		n := 0
		for _, fv := range filters {
			for _, wv := range windows {
				for _, skipWisps := range []bool{true, false} {
					ov, hv := orders[n%len(orders)], hydrations[(n/len(orders))%len(hydrations)]
					n++
					var filter types.IssueFilter
					fv.apply(&filter)
					wv.apply(&filter)
					ov.apply(&filter)
					hv.apply(&filter)
					filter.SkipWisps = skipWisps
					where = fmt.Sprintf("seed %d (%d issues, %d wisps) case %s/%s/%s/%s/skipWisps=%v", seed, len(durable), len(wisps), fv.name, wv.name, ov.name, hv.name, skipWisps)

					want, wantErr := oracleSearchCounts(ctx, t, store.db, "", filter)
					got, gotErr := store.SearchIssuesWithCounts(ctx, "", filter)
					var tooMany *issueops.ErrTooManyRows
					if (wantErr != nil) != (gotErr != nil) || (wantErr != nil && !errors.As(gotErr, &tooMany)) {
						t.Fatalf("%s: error = %v, oracle error = %v", where, gotErr, wantErr)
					}
					cases++
					if wantErr != nil {
						continue
					}
					gotRows := make([]countsRow, 0, len(got))
					for _, iwc := range got {
						gotRows = append(gotRows, toCountsRow(iwc))
					}
					if !reflect.DeepEqual(gotRows, want) {
						for i := 0; i < min(len(want), len(gotRows)); i++ {
							if gotRows[i] != want[i] {
								t.Fatalf("%s: %d rows (oracle %d); row %d differs:\n got   =%+v\n oracle=%+v", where, len(gotRows), len(want), i, gotRows[i], want[i])
							}
						}
						t.Fatalf("%s: %d rows, oracle %d", where, len(gotRows), len(want))
					}
					rows += len(want)
					sizes[len(want)] = true
				}
			}
		}
	}
	for _, size := range []int{0, 1, 200, 201} {
		if !sizes[size] {
			t.Errorf("no case answered exactly %d rows; the generator no longer reaches that page", size)
		}
	}

	// A missing wisp_comments, then a missing leases, fails both forms (#5942),
	// whichever route the read takes: an empty id scan still hydrates once.
	for _, drop := range []struct{ table string }{{"wisp_comments"}, {"leases"}} {
		if _, err := store.db.ExecContext(ctx, "DROP TABLE "+drop.table); err != nil {
			t.Fatalf("drop %s: %v", drop.table, err)
		}
		for _, fv := range []variant{
			{"label-0/limit50", func(f *types.IssueFilter) { f.Labels, f.Limit = []string{"page:none"}, 50 }},
			{"label-1/limit50", func(f *types.IssueFilter) { f.Labels, f.Limit = []string{"page:one"}, 50 }},
			{"all/unbounded", func(*types.IssueFilter) {}},
		} {
			var filter types.IssueFilter
			fv.apply(&filter)
			where = "missing " + drop.table + " " + fv.name
			if _, err := store.SearchIssuesWithCounts(ctx, "", filter); err == nil || !strings.Contains(err.Error(), drop.table) {
				t.Errorf("%s: ids-first error = %v, want one naming %s", where, err, drop.table)
			}
			q, _ := sqlbuild.SearchCountsSQL(sqlbuild.WispsFilterTables, nil, "", "", "", true, sqlbuild.CountsHydration{})
			if _, err := store.db.QueryContext(ctx, q); err == nil {
				t.Errorf("%s: the predicate form no longer fails, so this check proves nothing", where)
			}
			cases++
		}
	}
	t.Logf("seeds %v: %d cases equal (%d rows compared), in %v (budget %v)", seeds, cases, rows, time.Since(start).Round(time.Millisecond), budget)
}

// TestSearchCountsByIDsReverseCountMatchesPredicateFormMixedCollations: the
// by-IDs form matches the reverse blockers (rc) one target column at a time,
// and a bare column compares under its own collation where the predicate
// form's join compares the COALESCE (review bd-lwx6j). Each subtest switches
// the target columns Dolt lets it (see TestDependentsMixedCollations) to a
// case-insensitive collation, holds two durable issues whose ids differ only
// by case and, in every target column of both dependency tables, one blocks
// row targeting each, and requires the by-IDs rows, the store's ids-first
// page and the predicate form to agree on every count.
func TestSearchCountsByIDsReverseCountMatchesPredicateFormMixedCollations(t *testing.T) {
	const ci = "utf8mb4_0900_ai_ci"
	for _, c := range []struct {
		name string
		ci   []string
	}{
		{"external_ci", []string{"depends_on_external"}},
		{"wisp_ci", []string{"depends_on_wisp_id"}},
		{"all_ci", []string{"depends_on_issue_id", "depends_on_wisp_id", "depends_on_external"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			store, cleanup := setupTestStore(t)
			defer cleanup()
			ctx, cancel := testContext(t)
			defer cancel()

			targets := []string{"mc-target", "MC-TARGET"}
			for _, id := range targets {
				createPerm(t, ctx, store, id)
			}
			createPerm(t, ctx, store, "mc-src")
			createWisp(t, ctx, store, "mc-wsrc")

			conn, err := store.db.Conn(ctx)
			if err != nil {
				t.Fatalf("conn: %v", err)
			}
			defer conn.Close() //nolint:errcheck
			exec := func(q string, args ...any) {
				t.Helper()
				if _, err := conn.ExecContext(ctx, q, args...); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			changed := 0
			for _, table := range []string{"dependencies", "wisp_dependencies"} {
				for _, col := range c.ci {
					var colType string
					if err := conn.QueryRowContext(ctx,
						"SELECT COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?",
						table, col).Scan(&colType); err != nil {
						t.Fatalf("column type %s.%s: %v", table, col, err)
					}
					q := fmt.Sprintf("ALTER TABLE %s MODIFY %s %s CHARACTER SET utf8mb4 COLLATE %s NULL", table, col, colType, ci)
					if _, err := conn.ExecContext(ctx, q); err != nil {
						if strings.Contains(err.Error(), "used by foreign keys") {
							t.Logf("left out %s.%s: %v", table, col, err)
							continue
						}
						t.Fatalf("%s: %v", q, err)
					}
					changed++
				}
			}
			if changed == 0 {
				t.Fatalf("precondition: no column of the %s mix accepted a case-insensitive collation", c.name)
			}
			exec("SET FOREIGN_KEY_CHECKS = 0")
			k := 0
			for _, plane := range []struct{ table, src string }{{"dependencies", "mc-src"}, {"wisp_dependencies", "mc-wsrc"}} {
				for _, col := range []string{"depends_on_issue_id", "depends_on_wisp_id", "depends_on_external"} {
					for _, tgt := range targets {
						k++
						exec(fmt.Sprintf("INSERT INTO %s (id, issue_id, %s, type, created_at, created_by) VALUES (?, ?, ?, 'blocks', UTC_TIMESTAMP(), 'tester')", plane.table, col),
							fmt.Sprintf("00000000-0000-0000-0000-%012d", k), fmt.Sprintf("%s-%02d", plane.src, k), tgt)
					}
				}
			}
			exec("SET FOREIGN_KEY_CHECKS = 1")
			if err := conn.Close(); err != nil {
				t.Fatalf("close conn: %v", err)
			}

			hyd := sqlbuild.CountsHydration{}
			predSQL, _ := sqlbuild.SearchCountsSQL(sqlbuild.IssuesFilterTables, nil, "WHERE id IN (?,?)", "", "", true, hyd)
			pred := map[string]countsRow{}
			for _, r := range runCountsSQL(ctx, t, store.db, predSQL, []any{targets[0], targets[1]}, hyd) {
				pred[r.id] = r
			}
			byIDsSQL, args := sqlbuild.SearchCountsSQL(sqlbuild.IssuesFilterTables, targets, "", "", "", true, hyd)
			byIDs := map[string]countsRow{}
			for _, r := range runCountsSQL(ctx, t, store.db, byIDsSQL, args, hyd) {
				byIDs[r.id] = r
			}
			page, err := store.SearchIssuesWithCounts(ctx, "", types.IssueFilter{IDs: targets, SkipWisps: true, Limit: 10})
			if err != nil {
				t.Fatalf("SearchIssuesWithCounts: %v", err)
			}
			viaStore := map[string]countsRow{}
			for _, iwc := range page {
				viaStore[iwc.Issue.ID] = toCountsRow(iwc)
			}
			for _, id := range targets {
				t.Logf("%s rdep_count: predicate %d, by-IDs %d, store %d", id, pred[id].rdepCount, byIDs[id].rdepCount, viaStore[id].rdepCount)
				if byIDs[id] != pred[id] {
					t.Errorf("%s by-IDs row differs from the predicate form:\n by-IDs=%+v\n pred  =%+v", id, byIDs[id], pred[id])
				}
				if viaStore[id] != pred[id] {
					t.Errorf("%s store row differs from the predicate form:\n store=%+v\n pred =%+v", id, viaStore[id], pred[id])
				}
			}
		})
	}
}

// TestSearchCountsDependencyOrderReviewCase is review bd-r8crc's P2 input,
// fixed: seed 2 over the reviewer's wider label vocabulary, the case
// all/unbounded/created/full/skipWisps=false. The ids-first read emitted
// sc-w06's dependencies as [parent-child:sc-110, related:sc-w11,
// related:external:sc:2] where the predicate form emitted
// [related:external:sc:2, parent-child:sc-110, related:sc-w11]. Both must now
// agree element for element, on every row.
func TestSearchCountsDependencyOrderReviewCase(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	vocab := []string{"alpha", "beta", "gamma-1", "gamma-2", "hot", "éter", "ÉTER", "z%_", "zXY", "one", " ", "x", "slash/token"}
	seededCountsGraph(t, ctx, store.db, rand.New(rand.NewSource(2)), vocab)
	filter := types.IssueFilter{SortBy: "created"}

	want, err := oracleSearchCounts(ctx, t, store.db, "", filter)
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	got, err := store.SearchIssuesWithCounts(ctx, "", filter)
	if err != nil {
		t.Fatalf("SearchIssuesWithCounts: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d rows, oracle %d", len(got), len(want))
	}
	const id = "sc-w06"
	found := false
	for i, iwc := range got {
		row := toCountsRow(iwc)
		if row != want[i] {
			t.Fatalf("seed 2 all/unbounded/created/full/skipWisps=false: row %d differs from the oracle:\n got   =%+v\n oracle=%+v", i, row, want[i])
		}
		if row.id == id {
			found = true
			t.Logf("%s dependencies: [%s]", id, row.deps)
			if strings.Count(row.deps, "|") < 2 {
				t.Fatalf("%s has dependencies [%s]; the reviewer's three-element case no longer arises", id, row.deps)
			}
		}
	}
	if !found {
		t.Fatalf("%s not in the answer; the reviewer's case no longer arises", id)
	}
}

// TestSearchCountsDependenciesInPrimaryKeyOrder pins the order itself, not only
// that the forms agree: an issue and a wisp, each with four dependencies of
// mixed types and target columns inserted out of primary-key order, come back
// with their dependencies in primary-key order through the narrow (ids-first)
// path, the wide (predicate-form) path and the ready read, and without the
// primary key in the emitted dependency.
func TestSearchCountsDependenciesInPrimaryKeyOrder(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	for _, id := range []string{"po-src", "po-parent", "po-closed"} {
		createPerm(t, ctx, store, id)
	}
	for _, id := range []string{"po-wsrc", "po-w1"} {
		createWisp(t, ctx, store, id)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// A closed blocks target keeps both sources ready.
	exec("UPDATE issues SET status = 'closed', closed_at = UTC_TIMESTAMP() WHERE id = 'po-closed'")
	type edge struct{ pk, col, target, typ string }
	insert := func(table, src string, edges []edge) {
		for _, e := range edges {
			exec(fmt.Sprintf("INSERT INTO %s (id, issue_id, %s, type, created_at, created_by) VALUES (?, ?, ?, ?, UTC_TIMESTAMP(), 'tester')", table, e.col),
				"00000000-0000-0000-0000-"+e.pk, src, e.target, e.typ)
		}
	}
	// Inserted 3, 1, 4, 2: neither insertion order nor target order is the
	// primary-key order.
	insert("dependencies", "po-src", []edge{
		{"000000000003", "depends_on_external", "external:po:cap", "discovered-from"},
		{"000000000001", "depends_on_wisp_id", "po-w1", "related"},
		{"000000000004", "depends_on_issue_id", "po-closed", "blocks"},
		{"000000000002", "depends_on_issue_id", "po-parent", "parent-child"},
	})
	insert("wisp_dependencies", "po-wsrc", []edge{
		{"000000000013", "depends_on_wisp_id", "po-w1", "related"},
		{"000000000011", "depends_on_issue_id", "po-closed", "blocks"},
		{"000000000014", "depends_on_issue_id", "po-parent", "parent-child"},
		{"000000000012", "depends_on_external", "external:po:cap", "related"},
	})
	want := map[string]string{
		"po-src":  "related:po-w1|parent-child:po-parent|discovered-from:external:po:cap|blocks:po-closed",
		"po-wsrc": "blocks:po-closed|related:external:po:cap|related:po-w1|parent-child:po-parent",
	}

	check := func(path string, got []*types.IssueWithCounts) {
		t.Helper()
		seen := 0
		for _, iwc := range got {
			w, ok := want[iwc.Issue.ID]
			if !ok {
				continue
			}
			seen++
			for _, d := range iwc.Issue.Dependencies {
				if d.ID != "" {
					t.Errorf("%s: %s dependency %s carries its primary key %q into the emitted row", path, iwc.Issue.ID, d.DependsOnID, d.ID)
				}
			}
			if g := toCountsRow(iwc).deps; g != w {
				t.Errorf("%s: %s dependencies = [%s], want primary-key order [%s]", path, iwc.Issue.ID, g, w)
			}
		}
		if seen != len(want) {
			t.Errorf("%s: %d of the %d sources came back", path, seen, len(want))
		}
	}
	narrow, err := store.SearchIssuesWithCounts(ctx, "", types.IssueFilter{IDs: []string{"po-src", "po-wsrc"}, Limit: 10})
	if err != nil {
		t.Fatalf("narrow read: %v", err)
	}
	check("narrow (ids-first)", narrow)
	// A Go-side id sort keeps the predicate form.
	wide, err := store.SearchIssuesWithCounts(ctx, "", types.IssueFilter{SortBy: "id"})
	if err != nil {
		t.Fatalf("wide read: %v", err)
	}
	check("wide (predicate form)", wide)
	ready, err := store.GetReadyWorkWithCounts(ctx, types.WorkFilter{IncludeEphemeral: true})
	if err != nil {
		t.Fatalf("ready read: %v", err)
	}
	check("ready", ready)
}
