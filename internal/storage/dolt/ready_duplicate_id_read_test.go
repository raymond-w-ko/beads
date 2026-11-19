package dolt

import (
	"fmt"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// TestReadyDuplicateIDReadsMatchedRow (bd-63vumq, finding 1 of review
// bd-m03gt): an id held by an open issue labelled class:impl and an open
// no-history wisp labelled only area:other. With a pattern only the issue
// matches, and every ready read returns the issue: the issues half hydrates
// from issues alone, so the classic GetReadyWork no longer hands back the
// wisp the wisps half excluded. With no pattern both halves select the id and
// every read returns the wisp (be-iabdi). A ready claim on such an id keeps
// upstream's routing (bd-qdjinw) and is not asserted here.
//
// The no-pattern case where only the issue is ready (its wisp copy is closed)
// returns the issue too, as the counted and uow routes already did.
func TestReadyDuplicateIDReadsMatchedRow(t *testing.T) {
	store, cleanup := setupConcurrentTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	const id = "rdl-dup"
	if err := store.CreateIssue(ctx, &types.Issue{
		ID: id, Title: "durable match", Status: types.StatusOpen, IssueType: types.TypeTask,
	}, "tester"); err != nil {
		t.Fatal(err)
	}
	execAll(t, ctx, store.db,
		"INSERT INTO wisps (id, title, status, issue_type, priority, no_history, ephemeral, row_lock) SELECT id, 'nonmatching wisp', 'open', issue_type, priority, 1, 0, row_lock FROM issues WHERE id = 'rdl-dup'",
		"INSERT INTO labels (issue_id, label) VALUES ('rdl-dup', 'class:impl')",
		"INSERT INTO wisp_labels (issue_id, label) VALUES ('rdl-dup', 'area:other')")

	uowRoles, ok := proxiedUOWProvider(t, ctx, store).(rlwRoles)
	if !ok {
		t.Fatal("the proxied uow provider lacks IssueReader, ReadyCounter or ReadyClaimer")
	}
	routes := []rlwRoute{rlwRouteOf(t, "store", store), rlwRouteOf(t, "uow", uowRoles)}

	one := func(name string, rows []*types.Issue, err error, title string) {
		t.Helper()
		if err != nil || len(rows) != 1 || rows[0].Title != title {
			var got []string
			for _, r := range rows {
				got = append(got, fmt.Sprintf("%q %v", r.Title, r.Labels))
			}
			t.Errorf("%s = %v err=%v, want the one row %q", name, got, err, title)
		}
	}
	reads := func(where string, p rlwPattern, title string) {
		t.Helper()
		for _, limit := range []int{0, 1} {
			rows, err := store.GetReadyWork(ctx, p.work(false, limit))
			one(fmt.Sprintf("%s GetReadyWork limit %d %s", where, limit, p), rows, err, title)
		}
		counts, err := store.GetReadyWorkWithCounts(ctx, p.work(false, 0))
		var rows []*types.Issue
		for _, row := range counts {
			rows = append(rows, row.Issue)
		}
		one(where+" GetReadyWorkWithCounts "+p.String(), rows, err, title)
		if n, err := store.CountReadyWork(ctx, p.work(false, 0)); err != nil || n != 1 {
			t.Errorf("%s CountReadyWork %s = %d err=%v, want 1", where, p, n, err)
		}
		for _, route := range routes {
			page, err := route.reader.Ready(ctx, p.request(false))
			rows = nil
			for _, row := range page.Items {
				rows = append(rows, row.Issue)
			}
			one(where+" "+route.name+" Reader.Ready "+p.String(), rows, err, title)
		}
	}

	reads("both open", rlwPattern{glob: "class:*"}, "durable match")
	reads("both open", rlwPattern{regex: "^class:"}, "durable match")
	reads("both open", rlwPattern{}, "nonmatching wisp")

	// The wisp copy is closed: only the issue is ready, with no pattern.
	execAll(t, ctx, store.db, "UPDATE wisps SET status = 'closed', closed_at = UTC_TIMESTAMP() WHERE id = 'rdl-dup'")
	reads("wisp closed", rlwPattern{}, "durable match")
}
