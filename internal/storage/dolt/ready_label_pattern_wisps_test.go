package dolt

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// Live test of bd-9mr2a: the ready reads apply --label-pattern and
// --label-regex to wisps as they do to issues. Before it, the classic ready
// read's wisps half (issueops.readyWorkWispIssueFilter) dropped both fields,
// so every ready wisp came back whatever its labels, and a patterned claim
// could take one.
//
// The oracle is the list read: SearchIssues applies the same clause to labels
// and wisp_labels. For each pattern, the ids a ready read returns must be the
// ids the same read returns with no pattern, in that order, kept when
// SearchIssues returns them for the pattern. Every ready route is read: the
// store's GetReadyWork (Limit 0 and a bounded Limit, the two wisps paths),
// GetReadyWorkWithCounts and CountReadyWork, and the public Reader.Ready and
// ReadyCounter.CountReady on the store and on the uow route behind a proxy.
// Each runs with IncludeEphemeral off and on, with wisp_labels.label (and
// labels.label) under a binary and a case-insensitive collation; the labels
// carry case variants. The claim (ClaimReadyIssue and ReadyClaimer.ClaimNext
// on both routes) with a pattern that matches no wisp and no issue must claim
// nothing while a non-matching ready wisp exists.
var rlwCollations = []string{"utf8mb4_0900_bin", "utf8mb4_0900_ai_ci"}

type rlwPattern struct{ glob, regex string }

func (p rlwPattern) String() string {
	switch {
	case p.glob != "":
		return fmt.Sprintf("glob %q", p.glob)
	case p.regex != "":
		return fmt.Sprintf("regex %q", p.regex)
	}
	return "no pattern"
}

var rlwPatterns = []rlwPattern{
	{glob: "class:*"}, {glob: "CLASS:*"}, {glob: "cl?ss:i*"}, {glob: "area:*"}, {glob: "*"}, {glob: "zz-none-*"},
	{regex: "^class:"}, {regex: "^CLASS:"}, {regex: "impl$"}, {regex: "^area:"}, {regex: "zz-none"},
}

// rlwNoMatch match no label of the corpus under either collation.
var rlwNoMatch = []rlwPattern{{glob: "zz-none-*"}, {regex: "zz-none"}}

// rlwLabelSets: no label, one matching label, its case variant, one
// non-matching label, and a matching plus a non-matching one.
var rlwLabelSets = [][]string{nil, {"class:impl"}, {"CLASS:Impl"}, {"area:x"}, {"class:impl", "area:x"}}

// rlwSeed gives each label set to an issue, an ephemeral wisp and a
// no-history wisp, all open and ready, and adds a closed wisp with a matching
// label (never ready).
func rlwSeed(t *testing.T, ctx context.Context, store *DoltStore) {
	t.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	n := 0
	add := func(id string, ephemeral, noHistory bool, status types.Status, labels []string) {
		t.Helper()
		is := &types.Issue{ID: id, Title: "ready label pattern " + id, Status: status, Priority: n % 4,
			IssueType: types.TypeTask, Ephemeral: ephemeral, NoHistory: noHistory,
			CreatedAt: base.Add(time.Duration(n) * time.Minute)}
		if status == types.StatusClosed {
			closed := is.CreatedAt.Add(time.Minute)
			is.ClosedAt = &closed
		}
		n++
		if err := store.CreateIssue(ctx, is, "tester"); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		table := "labels"
		if ephemeral || noHistory {
			table = "wisp_labels"
		}
		for _, l := range labels {
			if _, err := store.db.ExecContext(ctx, "INSERT INTO "+table+" (issue_id, label) VALUES (?, ?)", id, l); err != nil {
				t.Fatalf("label %s %q: %v", id, l, err)
			}
		}
	}
	for k, labels := range rlwLabelSets {
		add(fmt.Sprintf("rlw-i%d", k), false, false, types.StatusOpen, labels)
		add(fmt.Sprintf("rlw-we%d", k), true, false, types.StatusOpen, labels)
		add(fmt.Sprintf("rlw-wn%d", k), false, true, types.StatusOpen, labels)
	}
	add("rlw-wc", true, false, types.StatusClosed, []string{"class:impl"})
}

func rlwIsWisp(id string) bool { return strings.HasPrefix(id, "rlw-w") }

func (p rlwPattern) work(eph bool, limit int) types.WorkFilter {
	return types.WorkFilter{LabelPattern: p.glob, LabelRegex: p.regex, IncludeEphemeral: eph, Limit: limit}
}

func (p rlwPattern) request(eph bool) issueops.ReadyRequest {
	unlimited := 0
	return issueops.ReadyRequest{LabelPattern: p.glob, LabelRegex: p.regex, IncludeEphemeral: eph, Sort: "hybrid", Limit: &unlimited}
}

type rlwRoles interface {
	IssueReader() (issueops.Reader, error)
	ReadyCounter() (issueops.ReadyCounter, error)
	ReadyClaimer() (issueops.ReadyClaimer, error)
}

type rlwRoute struct {
	name    string
	reader  issueops.Reader
	counter issueops.ReadyCounter
	claimer issueops.ReadyClaimer
}

func rlwRouteOf(t *testing.T, name string, roles rlwRoles) rlwRoute {
	t.Helper()
	r := rlwRoute{name: name}
	var err error
	if r.reader, err = roles.IssueReader(); err != nil {
		t.Fatalf("%s IssueReader: %v", name, err)
	}
	if r.counter, err = roles.ReadyCounter(); err != nil {
		t.Fatalf("%s ReadyCounter: %v", name, err)
	}
	if r.claimer, err = roles.ReadyClaimer(); err != nil {
		t.Fatalf("%s ReadyClaimer: %v", name, err)
	}
	return r
}

type rlwRead struct {
	name string
	ids  func(ctx context.Context, p rlwPattern, eph bool) ([]string, error)
	// count, when set, is the read's ready total, compared to len of ids.
	count func(ctx context.Context, p rlwPattern, eph bool) (int, error)
}

func rlwIssueIDs(in []*types.Issue) []string {
	out := []string{}
	for _, is := range in {
		out = append(out, is.ID)
	}
	return out
}

func rlwCountIDs(in []*types.IssueWithCounts) []string {
	out := []string{}
	for _, is := range in {
		out = append(out, is.Issue.ID)
	}
	return out
}

func rlwReads(store *DoltStore, routes []rlwRoute) []rlwRead {
	reads := []rlwRead{
		{name: "GetReadyWork Limit 0", ids: func(ctx context.Context, p rlwPattern, eph bool) ([]string, error) {
			got, err := store.GetReadyWork(ctx, p.work(eph, 0))
			return rlwIssueIDs(got), err
		}},
		{name: "GetReadyWork Limit 1000", ids: func(ctx context.Context, p rlwPattern, eph bool) ([]string, error) {
			got, err := store.GetReadyWork(ctx, p.work(eph, 1000))
			return rlwIssueIDs(got), err
		}},
		{name: "GetReadyWorkWithCounts", ids: func(ctx context.Context, p rlwPattern, eph bool) ([]string, error) {
			got, err := store.GetReadyWorkWithCounts(ctx, p.work(eph, 0))
			return rlwCountIDs(got), err
		}, count: func(ctx context.Context, p rlwPattern, eph bool) (int, error) {
			return store.CountReadyWork(ctx, p.work(eph, 0))
		}},
	}
	for _, r := range routes {
		r := r
		reads = append(reads, rlwRead{name: r.name + " Reader.Ready", ids: func(ctx context.Context, p rlwPattern, eph bool) ([]string, error) {
			page, err := r.reader.Ready(ctx, p.request(eph))
			return rlwCountIDs(page.Items), err
		}, count: func(ctx context.Context, p rlwPattern, eph bool) (int, error) {
			req := p.request(eph)
			req.Limit = nil // a ready count takes no limit
			got, err := r.counter.CountReady(ctx, req)
			return int(got.Total), err
		}})
	}
	return reads
}

func TestReadyLabelPatternFiltersWisps(t *testing.T) {
	store, cleanup := setupConcurrentTestStore(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	rlwSeed(t, ctx, store)
	uowRoles, ok := proxiedUOWProvider(t, ctx, store).(rlwRoles)
	if !ok {
		t.Fatal("the proxied uow provider lacks IssueReader, ReadyCounter or ReadyClaimer")
	}
	routes := []rlwRoute{rlwRouteOf(t, "store", store), rlwRouteOf(t, "uow", uowRoles)}
	reads := rlwReads(store, routes)

	compared := 0
	for _, coll := range rlwCollations {
		for _, table := range []string{"labels", "wisp_labels"} {
			if _, err := store.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s MODIFY label VARCHAR(255) COLLATE %s NOT NULL", table, coll)); err != nil {
				t.Fatalf("%s: set %s.label collation: %v", coll, table, err)
			}
		}

		// The oracle: the list read's ids for each pattern.
		listed := make(map[rlwPattern]map[string]bool, len(rlwPatterns))
		for _, p := range rlwPatterns {
			got, err := store.SearchIssues(ctx, "", types.IssueFilter{LabelPattern: p.glob, LabelRegex: p.regex, Limit: 10000})
			if err != nil {
				t.Fatalf("%s SearchIssues %s: %v", coll, p, err)
			}
			set := make(map[string]bool, len(got))
			for _, is := range got {
				set[is.ID] = true
			}
			listed[p] = set
		}

		for _, eph := range []bool{false, true} {
			for _, read := range reads {
				where := fmt.Sprintf("%s IncludeEphemeral=%v %s", coll, eph, read.name)
				all, err := read.ids(ctx, rlwPattern{}, eph)
				if err != nil {
					t.Fatalf("%s no pattern: %v", where, err)
				}
				// Vacuity guards: the unpatterned read returns wisps of the
				// planes this mode admits, so a pattern has wisps to drop.
				var kinds []string
				for _, id := range all {
					if strings.HasPrefix(id, "rlw-wn") {
						kinds = append(kinds, "no-history")
						break
					}
				}
				if eph {
					for _, id := range all {
						if strings.HasPrefix(id, "rlw-we") {
							kinds = append(kinds, "ephemeral")
							break
						}
					}
				}
				if want := map[bool]int{false: 1, true: 2}[eph]; len(kinds) != want {
					t.Fatalf("%s no pattern returned wisp kinds %v of %v, want both admitted planes", where, kinds, all)
				}

				matchedWisp := false
				for _, p := range rlwPatterns {
					got, err := read.ids(ctx, p, eph)
					if err != nil {
						t.Fatalf("%s %s: %v", where, p, err)
					}
					want := []string{}
					for _, id := range all {
						if listed[p][id] {
							want = append(want, id)
						}
					}
					if !reflect.DeepEqual(got, want) {
						var extra []string
						wanted := make(map[string]bool, len(want))
						for _, id := range want {
							wanted[id] = true
						}
						for _, id := range got {
							if !wanted[id] {
								extra = append(extra, id)
							}
						}
						t.Errorf("%s %s: ids differ; returned rows the list read does not match: %v\nready = %v\nwant  = %v", where, p, extra, got, want)
					}
					for _, id := range want {
						if rlwIsWisp(id) {
							matchedWisp = true
						}
					}
					if read.count != nil {
						n, err := read.count(ctx, p, eph)
						if err != nil {
							t.Fatalf("%s %s count: %v", where, p, err)
						}
						if n != len(want) {
							t.Errorf("%s %s: count = %d, want %d (%v)", where, p, n, len(want), want)
						}
					}
					compared++
				}
				if !matchedWisp {
					t.Fatalf("%s: no pattern matched a ready wisp; the comparison proves nothing about wisps", where)
				}
			}

			// The claim: a pattern matching no wisp and no issue claims nothing
			// while a non-matching ready wisp exists.
			ready, err := store.GetReadyWork(ctx, rlwPattern{}.work(eph, 0))
			if err != nil {
				t.Fatalf("%s IncludeEphemeral=%v ready before claim: %v", coll, eph, err)
			}
			wispReady := false
			for _, is := range ready {
				wispReady = wispReady || rlwIsWisp(is.ID)
			}
			if !wispReady {
				t.Fatalf("%s IncludeEphemeral=%v: no ready wisp before the claim", coll, eph)
			}
			for _, p := range rlwNoMatch {
				if len(listed[p]) != 0 {
					t.Fatalf("%s %s: the list read matched %v; want a pattern that matches nothing", coll, p, listed[p])
				}
				where := fmt.Sprintf("%s IncludeEphemeral=%v %s", coll, eph, p)
				claimed, err := store.ClaimReadyIssue(ctx, p.work(eph, 0), "rlw-claimer")
				if err != nil {
					t.Fatalf("%s ClaimReadyIssue: %v", where, err)
				}
				if claimed != nil {
					t.Errorf("%s: ClaimReadyIssue claimed non-matching %s", where, claimed.ID)
				}
				for _, r := range routes {
					req := p.request(eph)
					req.Limit = nil
					got, err := r.claimer.ClaimNext(ctx, issueops.ClaimNextRequest{Actor: "rlw-claimer", Filter: req})
					if err != nil {
						t.Fatalf("%s %s ClaimNext: %v", where, r.name, err)
					}
					if got.Claimed != nil {
						t.Errorf("%s: %s ClaimNext claimed non-matching %s", where, r.name, got.Claimed.Issue.ID)
					}
				}
			}
		}
	}
	t.Logf("ready label pattern on wisps: %d patterned reads compared over %d collations, %d routes", compared, len(rlwCollations), len(routes))
}
