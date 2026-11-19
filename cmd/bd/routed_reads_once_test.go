//go:build cgo

package main

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// readCountingStore records the order of GetIssue and SearchIssues calls
// and passes everything through.
type readCountingStore struct {
	storage.DoltStorage
	calls []string
}

func (s *readCountingStore) GetIssue(ctx context.Context, id string) (*types.Issue, error) {
	s.calls = append(s.calls, "GetIssue")
	return s.DoltStorage.GetIssue(ctx, id)
}

func (s *readCountingStore) SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error) {
	s.calls = append(s.calls, "SearchIssues")
	return s.DoltStorage.SearchIssues(ctx, query, filter)
}

func (s *readCountingStore) count(name string) int {
	n := 0
	for _, c := range s.calls {
		if c == name {
			n++
		}
	}
	return n
}

// TestRoutedResolveReadsIssueOnce pins the two resolvers' reads. bd show's
// (resolveAndGetIssueForShow) reads an exact id once: the resolver's GetIssue
// of the input is the issue, so an exact id (an issue or a wisp) costs one
// GetIssue and no SearchIssues, and yields the RoutedResult HEAD's
// search-then-GetIssue gives; a bare hash misses that GetIssue and resolves
// through the search and the fallback, then reads the resolved id with a
// second GetIssue. Every other command's (resolveAndGetFromStore, here as
// bd close reaches it) makes HEAD's reads: the exact search, then GetIssue.
func TestRoutedResolveReadsIssueOnce(t *testing.T) {
	ctx := context.Background()
	base := newTestStore(t, filepath.Join(t.TempDir(), ".beads", "beads.db"))
	issue := &types.Issue{ID: "test-r0nce", Title: "read once", Status: types.StatusOpen,
		Priority: 2, IssueType: types.TypeTask, Labels: []string{"b", "a"}}
	wisp := &types.Issue{ID: "test-wisp-r0nce", Title: "wisp read once", Status: types.StatusOpen,
		Priority: 2, IssueType: types.TypeTask, Labels: []string{"w"}, Ephemeral: true}
	for _, is := range []*types.Issue{issue, wisp} {
		if err := base.CreateIssue(ctx, is, "tester"); err != nil {
			t.Fatal(err)
		}
	}

	type resolver func(ctx context.Context, s storage.DoltStorage, id string) (*RoutedResult, error)
	show := func(ctx context.Context, s storage.DoltStorage, id string) (*RoutedResult, error) {
		return resolveAndGetIssueForShow(ctx, s, id)
	}
	closeResolve := func(ctx context.Context, s storage.DoltStorage, id string) (*RoutedResult, error) {
		return resolveAndGetIssueForMutation(ctx, s, id)
	}
	for _, tc := range []struct {
		name, input, id string
		resolve         resolver
		wantGetIssue    int
		wantSearch      int      // -1: at least one
		wantOrder       []string // when set, the exact call order
	}{
		{name: "show exact issue", input: "test-r0nce", id: issue.ID, resolve: show, wantGetIssue: 1, wantSearch: 0},
		{name: "show exact wisp", input: "test-wisp-r0nce", id: wisp.ID, resolve: show, wantGetIssue: 1, wantSearch: 0},
		{name: "show bare hash", input: "r0nce", id: issue.ID, resolve: show, wantGetIssue: 2, wantSearch: -1},
		{name: "close exact issue", input: "test-r0nce", id: issue.ID, resolve: closeResolve, wantGetIssue: 1, wantSearch: 1,
			wantOrder: []string{"SearchIssues", "GetIssue"}},
		{name: "close exact wisp", input: "test-wisp-r0nce", id: wisp.ID, resolve: closeResolve, wantGetIssue: 1, wantSearch: 1,
			wantOrder: []string{"SearchIssues", "GetIssue"}},
	} {
		want, err := base.GetIssue(ctx, tc.id)
		if err != nil {
			t.Fatal(err)
		}
		s := &readCountingStore{DoltStorage: base}
		got, err := tc.resolve(ctx, s, tc.input)
		if err != nil {
			t.Fatalf("%s %q: %v", tc.name, tc.input, err)
		}
		if n := s.count("GetIssue"); n != tc.wantGetIssue {
			t.Errorf("%s %q: GetIssue called %d times, want %d (calls %v)", tc.name, tc.input, n, tc.wantGetIssue, s.calls)
		}
		if n := s.count("SearchIssues"); (tc.wantSearch < 0 && n == 0) || (tc.wantSearch >= 0 && n != tc.wantSearch) {
			t.Errorf("%s %q: SearchIssues called %d times, want %d (-1: at least one; calls %v)", tc.name, tc.input, n, tc.wantSearch, s.calls)
		}
		if tc.wantOrder != nil && !reflect.DeepEqual(s.calls, tc.wantOrder) {
			t.Errorf("%s %q: calls %v, want HEAD's %v", tc.name, tc.input, s.calls, tc.wantOrder)
		}
		wantResult := &RoutedResult{Issue: want, Store: s, ResolvedID: tc.id}
		if !reflect.DeepEqual(got, wantResult) {
			t.Errorf("%s %q: result = %+v (issue %+v)\nwant %+v (issue %+v)", tc.name, tc.input, got, got.Issue, wantResult, want)
		}
	}
}
