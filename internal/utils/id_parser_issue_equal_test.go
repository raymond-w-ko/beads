//go:build cgo

package utils_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage/dolt"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/utils"
)

// Comparison test: the issue ResolvePartialIDWithIssue hands back (its
// GetIssue of the input, kept when the stored id is the input byte for byte)
// against the oracle, GetIssue of the resolved id, and its resolved id and
// error against ResolvePartialID's. resolveAndGetFromStore (cmd/bd/routed.go)
// returns that issue instead of a second GetIssue read, so every mutating
// command that resolves through it sees it; the relation is equality in every
// field of types.Issue, labels in the same order.
//
// Bounds: equalSeeds fixed seeds (1-3 the writer's, 19, 97 and 31337 the
// reviewer's), equalIssuesPerSeed generated issues each, plus the fixed
// shapes; equalBudget for the whole test. A failure names the seed (or the
// fixed shape) and the input. The overlap, case-insensitive and other-case
// tests below add the review shapes of bd-v3jqq.
var equalSeeds = []int64{1, 2, 3, 19, 97, 31337}

const (
	equalIssuesPerSeed = 16
	equalBudget        = 6 * time.Minute
)

// equalShape is one issue to create and how to dress it after creation.
type equalShape struct {
	issue  *types.Issue
	parent string // a parent-child dependency on this id
	claim  bool   // claim it, which grants a lease row (durable issues only)
}

// Tokens that stress the boundary between the two reads.
var (
	equalStatuses = []types.Status{types.StatusOpen, types.StatusInProgress, types.StatusBlocked,
		types.StatusDeferred, types.StatusClosed, types.StatusPinned, types.StatusHooked}
	equalLabels    = []string{"a", "Z", "class:implementer", "tier:3", "with-dash", "ü", "zz-last", "0"}
	equalMetadata  = []string{"", `{}`, `{"k":"v","n":[1,2]}`, `{"everturn":{"worktree":"wt/x"}}`}
	equalRefs      = []string{"", "gh-9", "jira-ABC"}
	equalAssignees = []string{"", "alice", "Bob Smith"}
)

func equalFixedShapes() []equalShape {
	closedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	deferUntil := time.Date(2027, 6, 7, 8, 9, 10, 0, time.UTC)
	ref := "gh-42"
	return []equalShape{
		{issue: &types.Issue{ID: "bd-plain", Title: "no label"}},
		{issue: &types.Issue{ID: "bd-one", Title: "one label", Labels: []string{"only"}}},
		{issue: &types.Issue{ID: "bd-many", Title: "many labels", Labels: []string{"zeta", "alpha", "Mid", "class:x"}}},
		{issue: &types.Issue{ID: "bd-closed", Title: "closed", Status: types.StatusClosed, ClosedAt: &closedAt, CloseReason: "done"}},
		{issue: &types.Issue{ID: "bd-deferred", Title: "deferred", Status: types.StatusDeferred, DeferUntil: &deferUntil}},
		{issue: &types.Issue{ID: "bd-leased", Title: "held lease", Labels: []string{"l"}}, claim: true},
		{issue: &types.Issue{ID: "bd-meta", Title: "metadata", Metadata: json.RawMessage(`{"a":1,"b":["x"]}`),
			ExternalRef: &ref, Assignee: "carol"}},
		{issue: &types.Issue{ID: "bd-kid", Title: "has parent"}, parent: "bd-plain"},
		{issue: &types.Issue{ID: "bd-ab", Title: "prefix of another id"}},
		{issue: &types.Issue{ID: "bd-abc", Title: "extends bd-ab"}},
		{issue: &types.Issue{ID: "bd-UPCASE", Title: "upper-case id", Labels: []string{"U"}}},
		{issue: &types.Issue{ID: "bd-wisp-one", Title: "wisp, labels", Ephemeral: true, Labels: []string{"w2", "w1"}}},
		{issue: &types.Issue{ID: "bd-wisp-closed", Title: "closed wisp", Ephemeral: true,
			Status: types.StatusClosed, ClosedAt: &closedAt}},
		{issue: &types.Issue{ID: "bd-wisp-kid", Title: "wisp with parent", Ephemeral: true}, parent: "bd-wisp-one"},
	}
}

func equalRandomShapes(r *rand.Rand, n int) []equalShape {
	var shapes []equalShape
	var durable, wisps []string
	for i := 0; i < n; i++ {
		wisp := r.Intn(3) == 0
		id := fmt.Sprintf("bd-r%d%s", i, strings.Repeat("x", r.Intn(3)))
		if wisp {
			id = fmt.Sprintf("bd-wisp-r%d", i)
		}
		if r.Intn(5) == 0 {
			id = "bd-" + strings.ToUpper(id[3:]) // an upper-case hash under the lower-case prefix
		}
		is := &types.Issue{ID: id, Title: fmt.Sprintf("random %d", i), Ephemeral: wisp,
			Priority: r.Intn(5), Status: equalStatuses[r.Intn(len(equalStatuses))]}
		switch is.Status {
		case types.StatusClosed:
			at := time.Date(2026, time.Month(1+r.Intn(12)), 1+r.Intn(28), r.Intn(24), r.Intn(60), r.Intn(60), 0, time.UTC)
			is.ClosedAt = &at
			is.CloseReason = []string{"", "done", "dup of bd-x"}[r.Intn(3)]
		case types.StatusDeferred:
			at := time.Date(2027, time.Month(1+r.Intn(12)), 1+r.Intn(28), 0, 0, 0, 0, time.UTC)
			is.DeferUntil = &at
		}
		for k := r.Intn(4); k > 0; k-- { // 0..3 labels, 1 the most interesting
			is.Labels = append(is.Labels, equalLabels[r.Intn(len(equalLabels))])
		}
		if m := equalMetadata[r.Intn(len(equalMetadata))]; m != "" {
			is.Metadata = json.RawMessage(m)
		}
		if ref := equalRefs[r.Intn(len(equalRefs))]; ref != "" {
			is.ExternalRef = &ref
		}
		is.Assignee = equalAssignees[r.Intn(len(equalAssignees))]
		s := equalShape{issue: is}
		pool := &durable
		if wisp {
			pool = &wisps
		}
		if len(*pool) > 0 && r.Intn(2) == 0 {
			s.parent = (*pool)[r.Intn(len(*pool))]
		}
		if !wisp && is.Status == types.StatusOpen && is.Assignee == "" && r.Intn(2) == 0 {
			s.claim = true
		}
		*pool = append(*pool, id)
		shapes = append(shapes, s)
	}
	return shapes
}

func createEqualShapes(ctx context.Context, t *testing.T, store *dolt.DoltStore, shapes []equalShape) []string {
	t.Helper()
	var ids []string
	for _, s := range shapes {
		is := s.issue
		if is.IssueType == "" {
			is.IssueType = types.TypeTask
		}
		if is.Status == "" {
			is.Status = types.StatusOpen
		}
		if err := store.CreateIssue(ctx, is, "tester"); err != nil {
			t.Fatalf("create %s: %v", is.ID, err)
		}
		if s.parent != "" {
			dep := &types.Dependency{IssueID: is.ID, DependsOnID: s.parent, Type: types.DepParentChild}
			if err := store.AddDependency(ctx, dep, "tester"); err != nil {
				t.Fatalf("parent %s -> %s: %v", is.ID, s.parent, err)
			}
		}
		if s.claim {
			if err := store.ClaimIssue(ctx, is.ID, "claimer"); err != nil {
				t.Fatalf("claim %s: %v", is.ID, err)
			}
		}
		ids = append(ids, is.ID)
	}
	return ids
}

// checkEqualIssue resolves input both ways and compares. where names the
// seed or shape for the failure message; stored says input is an id as
// created, which must resolve.
func checkEqualIssue(ctx context.Context, t *testing.T, store *dolt.DoltStore, where, input string, stored bool) {
	t.Helper()
	wantID, wantErr := utils.ResolvePartialID(ctx, store, input)
	id, got, err := utils.ResolvePartialIDWithIssue(ctx, store, input)
	if id != wantID || (err == nil) != (wantErr == nil) {
		t.Fatalf("%s input %q: ResolvePartialIDWithIssue = (%q, err %v), ResolvePartialID = (%q, err %v)",
			where, input, id, err, wantID, wantErr)
	}
	if err != nil {
		if stored {
			t.Fatalf("%s input %q: the stored id does not resolve: %v", where, input, err)
		}
		if err.Error() != wantErr.Error() {
			t.Fatalf("%s input %q: errors differ: %v vs ResolvePartialID %v", where, input, err, wantErr)
		}
		return // both refuse it alike (an upper-cased id is not a match)
	}
	if got == nil {
		if stored {
			t.Fatalf("%s input %q: the stored id came back without its issue", where, input)
		}
		return // a fallback resolution; the caller reads it with GetIssue
	}
	if got.ID != id {
		t.Fatalf("%s input %q: issue id %q, resolved id %q", where, input, got.ID, id)
	}
	want, err := store.GetIssue(ctx, id)
	if err != nil {
		t.Fatalf("%s input %q: oracle GetIssue(%q): %v", where, input, id, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s input %q: fast-path issue differs from GetIssue(%q):\nfast path = %+v\nGetIssue  = %+v\nfields: %s",
			where, input, id, *got, *want, issueFieldDiff(got, want))
	}
}

func issueFieldDiff(a, b *types.Issue) string {
	va, vb := reflect.ValueOf(*a), reflect.ValueOf(*b)
	var diff []string
	for i := 0; i < va.NumField(); i++ {
		if !reflect.DeepEqual(va.Field(i).Interface(), vb.Field(i).Interface()) {
			diff = append(diff, fmt.Sprintf("%s: %#v vs %#v", va.Type().Field(i).Name, va.Field(i).Interface(), vb.Field(i).Interface()))
		}
	}
	return strings.Join(diff, "; ")
}

func checkEqualIDs(ctx context.Context, t *testing.T, store *dolt.DoltStore, where string, ids []string, deadline time.Time) {
	t.Helper()
	for _, id := range ids {
		if time.Now().After(deadline) {
			t.Fatalf("%s: over the %s budget", where, equalBudget)
		}
		// The id as stored, then its case variants: all upper, the hash
		// upper under the lower-case prefix, all lower.
		inputs := []string{id}
		for _, v := range []string{strings.ToUpper(id), "bd-" + strings.ToUpper(id[3:]), strings.ToLower(id)} {
			if !containsString(inputs, v) {
				inputs = append(inputs, v)
			}
		}
		for i, in := range inputs {
			checkEqualIssue(ctx, t, store, where, in, i == 0)
		}
	}
}

func TestResolvePartialIDWithIssueMatchesGetIssue(t *testing.T) {
	ctx := context.Background()
	deadline := time.Now().Add(equalBudget)

	t.Run("fixed", func(t *testing.T) {
		store := newTestStore(t)
		ids := createEqualShapes(ctx, t, store, equalFixedShapes())
		checkEqualIDs(ctx, t, store, "fixed shapes", ids, deadline)
	})
	for _, seed := range equalSeeds {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			store := newTestStore(t)
			shapes := equalRandomShapes(rand.New(rand.NewSource(seed)), equalIssuesPerSeed)
			ids := createEqualShapes(ctx, t, store, shapes)
			checkEqualIDs(ctx, t, store, fmt.Sprintf("seed %d", seed), ids, deadline)
		})
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// equalExec runs one fixture statement on the store's database.
func equalExec(ctx context.Context, t *testing.T, store *dolt.DoltStore, stmt string, args ...any) {
	t.Helper()
	if _, err := store.DB().ExecContext(ctx, stmt, args...); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

// TestResolvePartialIDWithIssueOverlap: an id held by a durable issue and a
// wisp at once. SearchIssues picks the wisp and GetIssue the durable row, so
// the handed-back issue must be the durable row in every field, labels too.
func TestResolvePartialIDWithIssueOverlap(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	closedAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	for _, c := range []struct {
		name          string
		durable       *types.Issue
		wispStatus    string
		wispPinned    int
		wispClosedAt  any
		wispLabels    []string
		durableTitle  string
		durableLabels []string
	}{
		{name: "differ", durable: &types.Issue{ID: "bd-ovl", Title: "durable title", Labels: []string{"d2", "d1"}},
			wispStatus: "in_progress", wispPinned: 1, wispLabels: []string{"wisp label"}},
		{name: "closed-wisp-over-open", durable: &types.Issue{ID: "bd-ovlwc", Title: "durable title", Labels: []string{"d"}},
			wispStatus: "closed", wispClosedAt: closedAt, wispLabels: []string{"w"}},
		{name: "open-wisp-over-closed", durable: &types.Issue{ID: "bd-ovldc", Title: "durable title",
			Status: types.StatusClosed, ClosedAt: &closedAt, CloseReason: "done"},
			wispStatus: "open", wispPinned: 1, wispLabels: []string{"w1", "w2"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.durable.Status == "" {
				c.durable.Status = types.StatusOpen
			}
			c.durable.IssueType = types.TypeTask
			if err := store.CreateIssue(ctx, c.durable, "tester"); err != nil {
				t.Fatal(err)
			}
			equalExec(ctx, t, store, `INSERT INTO wisps (id,title,description,design,acceptance_criteria,notes,status,priority,issue_type,no_history,pinned,closed_at) VALUES (?,'wisp title','','','','',?,1,'bug',1,?,?)`,
				c.durable.ID, c.wispStatus, c.wispPinned, c.wispClosedAt)
			for _, l := range c.wispLabels {
				equalExec(ctx, t, store, `INSERT INTO wisp_labels (issue_id,label) VALUES (?,?)`, c.durable.ID, l)
			}
			hits, err := store.SearchIssues(ctx, "", types.IssueFilter{IDs: []string{c.durable.ID}})
			if err != nil || len(hits) != 1 || hits[0].Title != "wisp title" {
				t.Fatalf("fixture: SearchIssues(%s) = %v, %v; want the wisp alone", c.durable.ID, hits, err)
			}
			checkEqualIDs(ctx, t, store, "overlap "+c.name, []string{c.durable.ID}, time.Now().Add(equalBudget))
			_, got, err := utils.ResolvePartialIDWithIssue(ctx, store, c.durable.ID)
			if err != nil || got == nil || got.Title != "durable title" {
				t.Fatalf("overlap %s: issue %+v, err %v; want the durable row", c.name, got, err)
			}
		})
	}
}

// TestResolvePartialIDWithIssueCaseInsensitiveDB: a database whose id
// columns use a case-insensitive collation, with label, dependency and
// comment rows stored under another spelling of the id. GetIssue's label
// read matches them by collation; the search's label hydration keys by the
// stored spelling and drops them (internal/storage/issueops/search.go), so
// the handed-back issue must carry GetIssue's labels. Inputs in another
// case resolve as ResolvePartialID resolves them, with no issue handed back.
func TestResolvePartialIDWithIssueCaseInsensitiveDB(t *testing.T) {
	ctx := context.Background()
	admin := newTestStore(t)
	dbName := uniqueTestDBName(t)
	equalExec(ctx, t, admin, "CREATE DATABASE "+dbName+" COLLATE utf8mb4_0900_ai_ci")
	store, err := dolt.New(ctx, &dolt.Config{Path: t.TempDir(), Database: dbName,
		ServerPort: utils.DoltTestServerPort, CreateIfMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.SetConfig(ctx, "issue_prefix", "bd"); err != nil {
		t.Fatal(err)
	}
	for _, is := range []*types.Issue{
		{ID: "bd-caselabel", Title: "case label"}, // every label under another spelling
		{ID: "bd-caseother", Title: "dependency target"},
		{ID: "bd-wisp-casel", Title: "case wisp", Ephemeral: true, Labels: []string{"wown"}},
	} {
		is.Status, is.IssueType = types.StatusOpen, types.TypeTask
		if err := store.CreateIssue(ctx, is, "tester"); err != nil {
			t.Fatalf("create %s: %v", is.ID, err)
		}
	}
	if err := store.AddLabel(ctx, "BD-CASELABEL", "human", "tester"); err != nil {
		t.Fatal(err)
	}
	equalExec(ctx, t, store, `INSERT INTO labels (issue_id,label) VALUES ('Bd-CaseLabel','mixed')`)
	equalExec(ctx, t, store, `INSERT INTO wisp_labels (issue_id,label) VALUES ('BD-WISP-CASEL','wisp human')`)
	if err := store.AddDependency(ctx, &types.Dependency{IssueID: "BD-CASELABEL", DependsOnID: "bd-caseother",
		Type: types.DepBlocks}, "tester"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddIssueComment(ctx, "BD-CASELABEL", "tester", "comment under another spelling"); err != nil {
		t.Fatal(err)
	}
	var owner, collation string
	if err := store.DB().QueryRowContext(ctx, "SELECT issue_id, COLLATION(issue_id) FROM labels WHERE label = 'human'").Scan(&owner, &collation); err != nil {
		t.Fatal(err)
	}
	if owner != "BD-CASELABEL" || !strings.Contains(collation, "_ci") {
		t.Fatalf("fixture: label owner %q collation %s, want BD-CASELABEL under a _ci collation", owner, collation)
	}
	ids := []string{"bd-caselabel", "bd-caseother", "bd-wisp-casel"}
	checkEqualIDs(ctx, t, store, "case-insensitive", ids, time.Now().Add(equalBudget))
	for _, in := range []string{"Bd-CaseLabel", "BD-WISP-CASEL"} {
		checkEqualIssue(ctx, t, store, "case-insensitive other spelling", in, false)
		if id, got, err := utils.ResolvePartialIDWithIssue(ctx, store, in); err != nil || got != nil || id != strings.ToLower(in) {
			t.Errorf("input %q: (%q, %v, %v), want the stored id and no issue", in, id, got, err)
		}
	}
	_, got, err := utils.ResolvePartialIDWithIssue(ctx, store, "bd-caselabel")
	if err != nil || got == nil || !reflect.DeepEqual(got.Labels, []string{"human", "mixed"}) {
		t.Fatalf("bd-caselabel: issue %+v, err %v; want labels human, mixed", got, err)
	}
}

// TestResolvePartialIDWithIssueOtherCaseRefused: on a binary-collation
// database an id typed in another case names no issue; both resolvers refuse
// it with the same error and no issue is handed back.
func TestResolvePartialIDWithIssueOtherCaseRefused(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if err := store.CreateIssue(ctx, &types.Issue{ID: "bd-plaincase", Title: "plain", Status: types.StatusOpen,
		IssueType: types.TypeTask}, "tester"); err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{"BD-PLAINCASE", "bd-PLAINCASE", "Bd-plaincase"} {
		checkEqualIssue(ctx, t, store, "binary collation", in, false)
		_, wantErr := utils.ResolvePartialID(ctx, store, in)
		id, got, err := utils.ResolvePartialIDWithIssue(ctx, store, in)
		if wantErr == nil || err == nil || got != nil || id != "" {
			t.Errorf("input %q: (%q, %v, %v), ResolvePartialID err %v; want both refused", in, id, got, err, wantErr)
		}
	}
}
