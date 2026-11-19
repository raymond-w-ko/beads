package db

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/storage/sqlbuild"
	"github.com/steveyegge/beads/internal/types"
)

// TestSearchCountsSinglePlaneMatchesThePredicateFormOracle (AC-3, GH#4370) is
// the proxied backend's oracle matrix. Over a filter × order × window ×
// hydration matrix, the counted search's single-plane routes (SkipWisps and
// Ephemeral-only) must answer exactly what the pre-change route answered: the
// PREDICATE form of the counts query under the old WHERE, ORDER BY and
// searchWindowForFilter clause, then finishSearchCountsPage's Go-side sort and
// window (cap, offset, page trim, has-more). Membership, order, counts,
// parent, labels, dependencies, HasMore and the MaxRows verdict are compared.
//
// The matrix reaches both sides of the routes' ceiling
// (countsWindowGoesIDsFirst): the one-row, offset and capped windows go
// ids-first, while the unlimited and 201-row windows, the Go-side id sort and
// the label filters with an empty entry keep the predicate form. The durable
// fixture holds more than sqlbuild.QueryBatchSize matching rows, and one row
// carries a stored empty label, so an empty filter entry that the id scan's
// label JOINs dropped would change the answer.
func (s *testSuite) TestSearchCountsSinglePlaneMatchesThePredicateFormOracle() {
	const prefix = "bd-sco-"
	const durable = 210
	r := s.issueRepo()
	dep := s.depRepo()
	labelRepo := NewLabelSQLRepository(s.Runner())

	for i := 0; i < durable; i++ {
		iss := newTestIssue(fmt.Sprintf("%s%03d", prefix, i), fmt.Sprintf("oracle row %d", (i*37)%durable))
		iss.Priority = i % 4
		if i%5 == 0 {
			iss.Status = types.StatusInProgress
		}
		s.Require().NoError(r.Insert(s.Ctx(), iss, "tester", domain.InsertIssueOpts{}))
	}
	for i := 0; i < 4; i++ {
		w := newTestIssue(fmt.Sprintf("%sw%d", prefix, i), fmt.Sprintf("oracle wisp %d", i))
		w.Ephemeral = true
		w.Priority = i % 2
		s.Require().NoError(r.Insert(s.Ctx(), w, "tester", domain.InsertIssueOpts{UseWispsTable: true}))
	}
	for i := 0; i < durable; i += 7 {
		s.Require().NoError(labelRepo.Insert(s.Ctx(), fmt.Sprintf("%s%03d", prefix, i), "alpha", "tester", domain.LabelOpts{}))
	}
	s.Require().NoError(labelRepo.Insert(s.Ctx(), prefix+"w1", "alpha", "tester", domain.LabelOpts{UseWispsTable: true}))
	// A stored empty label, which the predicate form's `label = ?` bound to
	// "" matches and the id scan's label JOINs would drop.
	_, err := s.Runner().ExecContext(s.Ctx(), "INSERT INTO labels (issue_id, label) VALUES (?, '')", prefix+"015")
	s.Require().NoError(err)
	for _, d := range [][3]string{
		{"001", "000", string(types.DepBlocks)},
		{"002", "000", string(types.DepBlocks)},
		{"003", "000", string(types.DepParentChild)},
		{"205", "204", string(types.DepBlocks)},
		{"w1", "w0", string(types.DepBlocks)},
		{"w2", "000", string(types.DepBlocks)},
		{"007", "w0", string(types.DepBlocks)},
	} {
		opts := domain.DepInsertOpts{UseWispsTable: strings.HasPrefix(d[0], "w")}
		s.Require().NoError(dep.Insert(s.Ctx(), newDep(prefix+d[0], prefix+d[1], types.DependencyType(d[2])), "tester", opts))
	}
	for _, id := range []string{"000", "000", "201"} {
		_, err := s.Runner().ExecContext(s.Ctx(),
			"INSERT INTO comments (id, issue_id, author, text) VALUES (UUID(), ?, ?, ?)", prefix+id, "tester", "note")
		s.Require().NoError(err)
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
		{"labels-empty-entry", "", func(f *types.IssueFilter) { f.Labels = []string{"alpha", ""} }},
		{"labels-any-empty-entry", "", func(f *types.IssueFilter) { f.LabelsAny = []string{"alpha", ""} }},
	}
	orders := []variant{
		{"default", "", func(*types.IssueFilter) {}},
		{"created", "", func(f *types.IssueFilter) { f.SortBy = "created" }},
		{"title", "", func(f *types.IssueFilter) { f.SortBy = "title" }},
		{"priority-desc", "", func(f *types.IssueFilter) { f.SortBy, f.SortDesc = "priority", true }},
		{"id-desc", "", func(f *types.IssueFilter) { f.SortBy, f.SortDesc = "id", true }},
	}
	windows := []variant{
		{"unlimited", "", func(*types.IssueFilter) {}},
		{"limit1", "", func(f *types.IssueFilter) { f.Limit = 1 }},
		{"limit201", "", func(f *types.IssueFilter) { f.Limit = 201 }},
		{"offset", "", func(f *types.IssueFilter) { f.Limit, f.Offset = 2, 3 }},
		{"offset-capped", "", func(f *types.IssueFilter) { f.Limit, f.Offset, f.MaxRows, f.MaxRowsSource = 2, 3, 100, "test" }},
		{"capped", "", func(f *types.IssueFilter) { f.MaxRows, f.MaxRowsSource = 50, "test" }},
	}
	hydrations := []variant{
		{"full", "", func(*types.IssueFilter) {}},
		{"skip-labels", "", func(f *types.IssueFilter) { f.SkipLabels = true }},
		{"skip-counts", "", func(f *types.IssueFilter) { f.SkipCounts = true }},
		{"lite", "", func(f *types.IssueFilter) { f.Lite = true }},
	}
	yes := true
	routes := []variant{
		{"skip-wisps", "", func(f *types.IssueFilter) { f.SkipWisps = true }},
		{"ephemeral", "", func(f *types.IssueFilter) { f.Ephemeral = &yes }},
	}

	impl := &issueSQLRepositoryImpl{runner: s.Runner()}
	idsFirst, predicate, pastBatch := 0, 0, false
	n := 0
	for _, rv := range routes {
		for _, fv := range filters {
			for _, ov := range orders {
				for _, wv := range windows {
					hv := hydrations[n%len(hydrations)]
					n++
					filter := types.IssueFilter{IDPrefix: prefix}
					for _, v := range []variant{rv, fv, ov, wv, hv} {
						v.apply(&filter)
					}
					name := fmt.Sprintf("%s/%s/%s/%s/%s", rv.name, fv.name, ov.name, wv.name, hv.name)

					tables, wispRev := issuesFilterTables, true
					if filter.Ephemeral != nil {
						tables = wispsFilterTables
					}
					want, wantErr := oracleSearchCountsPage(s.Ctx(), impl, fv.query, filter, tables, wispRev)
					got, gotErr := r.SearchAcrossIssuesAndWispsWithCounts(s.Ctx(), fv.query, filter)
					var tooMany *issueops.ErrTooManyRows
					if (wantErr != nil) != (gotErr != nil) || (wantErr != nil && !errors.As(gotErr, &tooMany)) {
						s.FailNowf("error mismatch", "%s: error = %v, oracle error = %v", name, gotErr, wantErr)
					}
					if wantErr != nil {
						continue
					}
					if countsWindowGoesIDsFirst(filter, searchWindowForFilter(filter)) {
						idsFirst++
					} else {
						predicate++
					}
					if len(want.Items) > sqlbuild.QueryBatchSize {
						pastBatch = true
					}
					s.Require().Equal(flattenCountsPage(want), flattenCountsPage(got), name)
					s.Require().Equal(want.HasMore, got.HasMore, "%s: HasMore", name)
				}
			}
		}
	}
	s.Require().Positive(idsFirst, "no case went ids-first")
	s.Require().Positive(predicate, "no case kept the predicate form")
	s.Require().True(pastBatch, "no case returned more than %d rows; the matrix never reached past one by-IDs batch", sqlbuild.QueryBatchSize)
}

// oracleSearchCountsPage replays the pre-GH#4370 single-plane route:
// runFilterSearchQuery's predicate-form counts query, then
// finishSearchCountsPage.
func oracleSearchCountsPage(ctx context.Context, r *issueSQLRepositoryImpl, query string, filter types.IssueFilter, tables filterTables, wispRev bool) (domain.SearchCountsPage, error) {
	clauses, args, err := buildIssueFilterClauses(query, filter, tables)
	if err != nil {
		return domain.SearchCountsPage{}, err
	}
	whereSQL := ""
	if len(clauses) > 0 {
		whereSQL = "WHERE " + strings.Join(clauses, " AND ")
	}
	window := searchWindowForFilter(filter)
	hyd := hydrationFor(filter)
	q, _ := sqlbuild.SearchCountsSQL(tables, nil, whereSQL, orderBySQL(filter.SortBy, filter.SortDesc, "i"), window.sql, wispRev, hyd)
	items, err := r.scanCountsQuery(ctx, tables, q, args, hyd)
	if err != nil {
		return domain.SearchCountsPage{}, err
	}
	sortRowsGoSide(items, func(iwc *types.IssueWithCounts) string { return iwc.Issue.ID }, filter.SortBy, filter.SortDesc)
	trimmed, hasMore, err := finishWindow(items, window)
	if err != nil {
		return domain.SearchCountsPage{}, err
	}
	return domain.SearchCountsPage{Items: trimmed, HasMore: hasMore}, nil
}

// flattenCountsPage renders each row's id and every hydrated value, with
// labels sorted and the dependencies in the order the read emitted them:
// every form must emit primary-key order (bd-2i55u), so comparing two forms
// compares it.
func flattenCountsPage(page domain.SearchCountsPage) []string {
	out := make([]string, 0, len(page.Items))
	for _, iwc := range page.Items {
		parent := ""
		if iwc.Parent != nil {
			parent = *iwc.Parent
		}
		labels := append([]string(nil), iwc.Issue.Labels...)
		sort.Strings(labels)
		deps := make([]string, 0, len(iwc.Issue.Dependencies))
		for _, d := range iwc.Issue.Dependencies {
			deps = append(deps, string(d.Type)+":"+d.DependsOnID)
		}
		out = append(out, fmt.Sprintf("%s dep=%d rdep=%d cmt=%d parent=%s labels=%s deps=%s",
			iwc.Issue.ID, iwc.DependencyCount, iwc.DependentCount, iwc.CommentCount, parent,
			strings.Join(labels, ","), strings.Join(deps, "|")))
	}
	return out
}

// TestSearchCountsSinglePlaneEqualsPredicateFormSeeded widens the oracle
// matrix above with seeded random graphs (bd-c43b7). The two pieces: the
// single-plane routes' counted search (SkipWisps and Ephemeral-only:
// ids-first for a window of up to one batch; the proxied route has no probe,
// so an unbounded read keeps the predicate form) and oracleSearchCountsPage.
// Relation: equal rows, counts, order, HasMore and MaxRows verdict.
//
// Per seed: 205-254 durable issues and 205-234 wisps of statuses open,
// in_progress, closed, pinned, deferred and blocked; labels from a small
// vocabulary on some rows and none on others, plus page labels on exactly 1,
// 200 and 201 rows of each plane; blocks, parent-child and related edges to
// issue, wisp and external targets; comments on some rows; and one durable id
// that is a blocks target in all three target columns. Filters: --all, bd
// list's default exclusions, one and two statuses, labels matching 0, 1, 200
// and 201 rows, a label, label-any and a label pattern. Windows: unbounded,
// 1, 50, 200 and 201 rows, an offset page and a MaxRows cap. Each id carries a
// per-seed prefix (IDPrefix), so the suite's shared database is not cleared.
//
// A missing wisp_comments or leases table is not dropped here (the suite's
// database is shared): issue_search_counts_byids_test.go's empty-window case
// and issue_search_counts_missing_table_test.go pin that for this route, and
// the dolt package's seeded test drops both tables live.
//
// Bounds: the seeds below, 2 routes x 11 filters x 7 windows = 154 cases
// each, and the suite's context; a failure names its seed and case.
func (s *testSuite) TestSearchCountsSinglePlaneEqualsPredicateFormSeeded() {
	seeds := []int64{3, 17, 29}
	r := s.issueRepo()
	dep := s.depRepo()
	labelRepo := NewLabelSQLRepository(s.Runner())
	impl := &issueSQLRepositoryImpl{runner: s.Runner()}
	statuses := []types.Status{types.StatusOpen, types.StatusOpen, types.StatusInProgress, types.StatusClosed,
		types.StatusClosed, types.StatusPinned, types.StatusDeferred, types.StatusBlocked}
	vocab := []string{"alpha", "beta", "gamma-1", "gamma-2"}
	start := time.Now()
	cases, rows := 0, 0
	sizes := map[int]bool{}

	for _, seed := range seeds {
		rng := rand.New(rand.NewSource(seed))
		prefix := fmt.Sprintf("bd-scs%d-", seed)
		planes := []struct {
			wisp bool
			ids  []string
		}{{false, nil}, {true, nil}}
		for p := range planes {
			n := 205 + rng.Intn(50)
			if planes[p].wisp {
				n = 205 + rng.Intn(30)
			}
			for i := 0; i < n; i++ {
				id := fmt.Sprintf("%s%03d", prefix, i)
				if planes[p].wisp {
					id = fmt.Sprintf("%sw%03d", prefix, i)
				}
				iss := newTestIssue(id, fmt.Sprintf("%s row %d", vocab[rng.Intn(len(vocab))], rng.Intn(50)))
				iss.Priority = rng.Intn(5)
				iss.Status = statuses[rng.Intn(len(statuses))]
				if iss.Status == types.StatusClosed {
					closed := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
					iss.ClosedAt = &closed
				}
				iss.Ephemeral = planes[p].wisp
				s.Require().NoError(r.Insert(s.Ctx(), iss, "tester", domain.InsertIssueOpts{UseWispsTable: planes[p].wisp}), "seed %d", seed)
				planes[p].ids = append(planes[p].ids, id)
				for _, l := range vocab {
					if rng.Intn(5) == 0 {
						s.Require().NoError(labelRepo.Insert(s.Ctx(), id, l, "tester", domain.LabelOpts{UseWispsTable: planes[p].wisp}))
					}
				}
			}
			perm := rng.Perm(len(planes[p].ids))
			for _, pl := range []struct {
				label string
				n     int
			}{{"page:one", 1}, {"page:200", 200}, {"page:201", 201}} {
				for _, i := range perm[:pl.n] {
					s.Require().NoError(labelRepo.Insert(s.Ctx(), planes[p].ids[i], pl.label, "tester", domain.LabelOpts{UseWispsTable: planes[p].wisp}))
				}
			}
		}
		durable, wisps := planes[0].ids, planes[1].ids
		depTypes := []types.DependencyType{types.DepBlocks, types.DepBlocks, types.DepParentChild, types.DepRelated}
		seen := map[string]bool{}
		for k := 0; k < len(durable); k++ {
			wispSrc := rng.Intn(4) == 0
			src := durable[rng.Intn(len(durable))]
			if wispSrc {
				src = wisps[rng.Intn(len(wisps))]
			}
			var target string
			switch x := rng.Intn(10); {
			case x < 6:
				target = durable[rng.Intn(len(durable))]
			case x < 9:
				target = wisps[rng.Intn(len(wisps))]
			default:
				target = fmt.Sprintf("external:scs:%d", rng.Intn(4))
			}
			if target == src || seen[src+"|"+target] {
				continue
			}
			seen[src+"|"+target] = true
			typ := depTypes[rng.Intn(len(depTypes))]
			if strings.HasPrefix(target, "external:") {
				typ = types.DepBlocks // the repository refuses an external parent-child edge
			}
			s.Require().NoError(dep.Insert(s.Ctx(), newDep(src, target, typ), "tester", domain.DepInsertOpts{UseWispsTable: wispSrc}))
		}
		// One durable id that is a blocks target in all three columns of
		// dependencies (the wisp and external columns hold it without a key).
		multi := durable[rng.Intn(len(durable))]
		for i, col := range []string{"depends_on_issue_id", "depends_on_wisp_id", "depends_on_external"} {
			_, err := s.Runner().ExecContext(s.Ctx(), fmt.Sprintf(
				"INSERT INTO dependencies (id, issue_id, %s, type, created_by) VALUES (?, ?, ?, 'blocks', 'tester')", col),
				fmt.Sprintf("%08d-0000-0000-0000-%012d", seed, i), durable[i], multi)
			s.Require().NoError(err, "seed %d multi-column target", seed)
		}
		for k := 0; k < len(durable)/3; k++ {
			_, err := s.Runner().ExecContext(s.Ctx(),
				"INSERT INTO comments (id, issue_id, author, text) VALUES (UUID(), ?, 'tester', 'c')", durable[rng.Intn(len(durable))])
			s.Require().NoError(err)
		}

		type variant struct {
			name  string
			apply func(*types.IssueFilter)
		}
		yes := true
		routes := []variant{
			{"skip-wisps", func(f *types.IssueFilter) { f.SkipWisps = true }},
			{"ephemeral", func(f *types.IssueFilter) { f.Ephemeral = &yes }},
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
			{"label-any", func(f *types.IssueFilter) { f.LabelsAny = []string{"beta", "gamma-1"} }},
			{"label-pattern", func(f *types.IssueFilter) { f.LabelPattern = "gamma-*" }},
		}
		windows := []variant{
			{"unbounded", func(*types.IssueFilter) {}},
			{"limit1", func(f *types.IssueFilter) { f.Limit = 1 }},
			{"limit50", func(f *types.IssueFilter) { f.Limit = 50 }},
			{"limit200", func(f *types.IssueFilter) { f.Limit = 200 }},
			{"limit201", func(f *types.IssueFilter) { f.Limit = 201 }},
			{"offset", func(f *types.IssueFilter) { f.Limit, f.Offset = 20, 7 }},
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
		n := 0
		for _, rv := range routes {
			for _, fv := range filters {
				for _, wv := range windows {
					ov, hv := orders[n%len(orders)], hydrations[(n/len(orders))%len(hydrations)]
					n++
					filter := types.IssueFilter{IDPrefix: prefix}
					for _, v := range []variant{rv, fv, wv, ov, hv} {
						v.apply(&filter)
					}
					name := fmt.Sprintf("seed %d: %s/%s/%s/%s/%s", seed, rv.name, fv.name, wv.name, ov.name, hv.name)
					tables := issuesFilterTables
					if filter.Ephemeral != nil {
						tables = wispsFilterTables
					}
					want, wantErr := oracleSearchCountsPage(s.Ctx(), impl, "", filter, tables, true)
					got, gotErr := r.SearchAcrossIssuesAndWispsWithCounts(s.Ctx(), "", filter)
					var tooMany *issueops.ErrTooManyRows
					if (wantErr != nil) != (gotErr != nil) || (wantErr != nil && !errors.As(gotErr, &tooMany)) {
						s.FailNowf("error mismatch", "%s: error = %v, oracle error = %v", name, gotErr, wantErr)
					}
					cases++
					if wantErr != nil {
						continue
					}
					s.Require().Equal(flattenCountsPage(want), flattenCountsPage(got), name)
					s.Require().Equal(want.HasMore, got.HasMore, "%s: HasMore", name)
					rows += len(want.Items)
					sizes[len(want.Items)] = true
				}
			}
		}
	}
	for _, size := range []int{0, 1, 200, 201} {
		s.Require().True(sizes[size], "no case answered exactly %d rows; the generator no longer reaches that page", size)
	}
	s.T().Logf("seeds %v: %d cases equal (%d rows compared), in %v", seeds, cases, rows, time.Since(start).Round(time.Millisecond))
}
