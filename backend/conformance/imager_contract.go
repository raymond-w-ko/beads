package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// This file holds the semantic contract every implementation of
// publicops.Imager must satisfy. Each case asserts what issueops/image.go
// PROMISES rather than what any one backend happens to do; a backend that
// genuinely disagrees is parked at its own wiring site with
// skipKnownDivergence so the case still runs on the ones that agree.
//
// THERE IS ONE BODY BEHIND THE THREE WIRINGS. All three run
// internal/storage/issueops.ImageInTx — the unit-of-work provider on its
// transaction's statement runner (uow/imager.go says why: two bodies would be
// two definitions of the revision) — so the wirings are one vote on the body
// and three checks of how each reaches one read transaction. What they CAN
// disagree about is the schema and driver beneath it: the column set the image
// reads, and the value shapes it normalizes, which is exactly what the
// revision is a hash of.
//
// THE ONE-SNAPSHOT PROMISE IS COUNTED, NOT INFERRED: "every image a call
// returns is read in one transaction". A single-threaded case cannot tell one
// transaction from two by what the images say when nothing else is writing —
// an adapter that read each id in its own transaction returns the same bytes
// here — and a concurrent writer racing between two reads is a timing test.
// So the fixture counts the transactions the store BENEATH the Imager begins
// (ReadTransactions), below anything a wrapper around the role can reach, and
// RunImagerReadsOneSnapshot asserts that one multi-id call began exactly one.
//
// EVERY CASE NAMESPACES ITS SEEDS with fixture.IssuePrefix and its own tag: the
// three wirings share one database across the whole role suite.

// ImagerFixture supplies adapter-specific storage access for the bead-image
// assertions.
type ImagerFixture struct {
	// IssuePrefix namespaces the ids each assertion seeds, so several of them
	// can share one database.
	IssuePrefix string
	// Imager is the surface under test.
	Imager publicops.Imager
	// CreateIssue seeds a durable issue in the issues plane.
	CreateIssue func(context.Context, *types.Issue, string) error
	// CreateWisp seeds an ephemeral issue in the wisps plane.
	CreateWisp func(context.Context, *types.Issue, string) error
	// AddDependency seeds ONE edge, routed to the plane the edge's SOURCE
	// lives in, and records a dependency_added event.
	AddDependency func(context.Context, *types.Dependency, string) error
	// AddLabel adds one label to a DURABLE issue: (issue id, label, actor).
	// The role kit carries no label verb, so each wiring supplies its own.
	AddLabel func(context.Context, string, string, string) error
	// AddComment adds one comment to a DURABLE issue: (issue id, author,
	// text). The role kit carries no comment verb either.
	AddComment func(context.Context, string, string, string) error
	// QueryScalar runs a single-row query and scans it, and returns the error
	// rather than failing the test.
	QueryScalar func(context.Context, string, []any, ...any) error
	// CountHistory reports how many history entries the fixture's branch has.
	// A nil hook means "this backend cannot observe history", and the case
	// that needs it SKIPS with that reason rather than passing quietly.
	CountHistory func(context.Context) (int, error)
	// ReadTransactions reports how many transactions the storage the Imager
	// reads through has begun so far. It must count BELOW the role — at the
	// connection or unit-of-work seam the Imager's own body calls — so an
	// implementation that splits one request across several transactions
	// moves it more than once. Only its delta around one call is read, so
	// transactions the seed hooks begin do not matter.
	//
	// A nil hook means "this backend cannot count its transactions", and the
	// case that needs it SKIPS with that reason rather than passing quietly.
	ReadTransactions func() int
}

// RunImagerRefusesAMalformedRequest pins the request rules that need no
// database (issueops/image.go, Imager.Image: "An empty request, or a blank id,
// is ErrValidation").
//
// The blank id beside a present one is the arrangement that can fail: an
// implementation that skipped blanks would image the real bead and answer as
// though the request were clean.
func RunImagerRefusesAMalformedRequest(t *testing.T, ctx context.Context, fixture ImagerFixture) {
	t.Helper()
	present := imagerSeedIssue(t, ctx, fixture, "malformed", "real")
	for _, test := range []struct {
		name string
		ids  []string
	}{
		{"nil ids", nil},
		{"no ids", []string{}},
		{"blank id", []string{"   "}},
		{"empty id beside a present one", []string{present, ""}},
	} {
		t.Run(test.name, func(t *testing.T) {
			images, err := fixture.Imager.Image(ctx, test.ids)
			if !errors.Is(err, publicops.ErrValidation) {
				t.Fatalf("Image(%s) error = %v, want ErrValidation", test.name, err)
			}
			if images != nil {
				t.Errorf("refused image returned %d images, want none", len(images))
			}
		})
	}
}

// RunImagerNamesEveryMissingID pins the all-or-nothing promise (Imager.Image:
// "An id naming no row in either plane is a *NotFoundError naming EVERY
// missing id, matching ErrNotFound, and no image is returned — not even for
// the ids that resolved").
//
// Two missing ids around a present one, and the present one named twice: an
// implementation that stopped at the first miss names one id, and one that
// imaged as it resolved returns a partial result. The order is the request's
// (NotFoundError.IDs: "in the order the request spelled them").
func RunImagerNamesEveryMissingID(t *testing.T, ctx context.Context, fixture ImagerFixture) {
	t.Helper()
	present := imagerSeedIssue(t, ctx, fixture, "missing", "real")
	first := fixture.IssuePrefix + "-img-missing-absent-one"
	second := fixture.IssuePrefix + "-img-missing-absent-two"

	images, err := fixture.Imager.Image(ctx, []string{first, present, second, present})
	if !errors.Is(err, publicops.ErrNotFound) {
		t.Fatalf("Image with absent ids error = %v, want ErrNotFound", err)
	}
	var notFound *publicops.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("Image with absent ids error = %T (%v), want a *NotFoundError", err, err)
	}
	if want := []string{first, second}; !reflect.DeepEqual(notFound.IDs, want) {
		t.Errorf("NotFoundError.IDs = %v, want %v: every missing id, in request order, and not the one that resolved", notFound.IDs, want)
	}
	if images != nil {
		t.Errorf("Image with absent ids returned %d images, want none — not even for %s", len(images), present)
	}
}

// RunImagerCollapsesDuplicatesInFirstNamedOrder pins Imager.Image: "one Image
// per distinct id, in the order the ids were first named; duplicates
// collapse". The request names the beads in the reverse of their seed order,
// so an implementation that sorted, or answered in storage order, fails.
func RunImagerCollapsesDuplicatesInFirstNamedOrder(t *testing.T, ctx context.Context, fixture ImagerFixture) {
	t.Helper()
	a := imagerSeedIssue(t, ctx, fixture, "dupes", "a")
	b := imagerSeedIssue(t, ctx, fixture, "dupes", "b")
	c := imagerSeedIssue(t, ctx, fixture, "dupes", "c")

	images := imagerImage(t, ctx, fixture, c, a, c, b, a)
	if got, want := imagerIDs(images), []string{c, a, b}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Image ids = %v, want %v", got, want)
	}
	for _, img := range images {
		if img.Row["id"] != img.ID {
			t.Errorf("image %s carries row id %v", img.ID, img.Row["id"])
		}
	}
}

// RunImagerReadsEachBeadFromItsOwnPlane pins Image.Table ("the bead's own
// table: \"issues\" or \"wisps\"") and the edge rule beside it: Dependencies
// holds edges in BOTH directions, from either edge table, so a wisp's edge
// onto a durable bead shows in the durable bead's image as well as the wisp's.
func RunImagerReadsEachBeadFromItsOwnPlane(t *testing.T, ctx context.Context, fixture ImagerFixture) {
	t.Helper()
	durable := imagerSeedIssue(t, ctx, fixture, "plane", "durable")
	wisp := imagerSeedWisp(t, ctx, fixture, "plane", "wisp")
	imagerAddEdge(t, ctx, fixture, wisp, durable)

	images := imagerImage(t, ctx, fixture, wisp, durable)
	w, d := images[0], images[1]
	if w.Table != "wisps" {
		t.Errorf("wisp %s imaged from table %q, want wisps", wisp, w.Table)
	}
	if d.Table != "issues" {
		t.Errorf("durable %s imaged from table %q, want issues", durable, d.Table)
	}
	if w.Row["id"] != wisp || d.Row["id"] != durable {
		t.Errorf("row ids = %v and %v, want %s and %s", w.Row["id"], d.Row["id"], wisp, durable)
	}
	if n := imagerEdgeRows(w, wisp, durable); n != 1 {
		t.Errorf("wisp image has %d rows for its own edge %s -> %s, want 1: %v", n, wisp, durable, w.Dependencies)
	}
	if n := imagerEdgeRows(d, wisp, durable); n != 1 {
		t.Errorf("durable image has %d rows for the inbound wisp edge %s -> %s, want 1: %v", n, wisp, durable, d.Dependencies)
	}
}

// RunImagerLeavesGeneratedColumnsOut pins Image: "GENERATED COLUMNS ARE LEFT
// OUT", for the one the leaf names (everturn_action, generated from
// metadata.everturn.action).
//
// The seed gives the generated column a VALUE first, because a column that is
// NULL for the bead would be left out by an implementation that dropped NULLs
// just as well. And the column it is generated from must still be there, as
// the stored text: leaving out the source along with the derivative would pass
// the first half and lose data.
func RunImagerLeavesGeneratedColumnsOut(t *testing.T, ctx context.Context, fixture ImagerFixture) {
	t.Helper()
	issue := imagerIssue(fixture, "generated", "bead", false)
	issue.Metadata = json.RawMessage(`{"everturn":{"action":"imager-probe"}}`)
	id := imagerSeed(t, ctx, fixture, issue)

	var generated int
	if err := fixture.QueryScalar(ctx,
		"SELECT COUNT(*) FROM issues WHERE id = ? AND everturn_action = ?",
		[]any{id, "imager-probe"}, &generated); err != nil {
		t.Skipf("this backend's schema has no issues.everturn_action, so the exclusion is untested here: %v", err)
	}
	if generated != 1 {
		t.Fatalf("issues.everturn_action for %s does not read the seeded action; the probe would be vacuous", id)
	}

	img := imagerImage(t, ctx, fixture, id)[0]
	if value, ok := img.Row["everturn_action"]; ok {
		t.Errorf("generated column everturn_action imaged as %v; generated columns are left out", value)
	}
	metadata, ok := img.Row["metadata"].(string)
	if !ok || !strings.Contains(metadata, `"imager-probe"`) {
		t.Errorf("metadata = %#v, want the stored JSON text the generated column reads", img.Row["metadata"])
	}
}

// RunImagerRevisionMovesOnEveryAssociatedWrite pins Image.Revision: "It
// changes on any write to the bead — its row, a label, a comment, an event, or
// a dependency in EITHER direction".
//
// Each step asserts two things, because either alone can pass wrongly: the
// revision moved, and the image shows the row that moved it. A revision over
// the bead's own row alone would stall on the inbound edge, whose row names
// the OTHER bead as its source; that step is the reason the case exists.
//
// It also asserts the negative a guard needs as much: two reads with no write
// between them agree, and a write to an unrelated bead does not move it.
func RunImagerRevisionMovesOnEveryAssociatedWrite(t *testing.T, ctx context.Context, fixture ImagerFixture) {
	t.Helper()
	if fixture.AddLabel == nil || fixture.AddComment == nil {
		t.Skip("this backend's fixture cannot add a label or a comment, so the revision walk is untested here")
	}
	bead := imagerSeedIssue(t, ctx, fixture, "revision", "bead")
	upstream := imagerSeedIssue(t, ctx, fixture, "revision", "upstream")
	downstream := imagerSeedIssue(t, ctx, fixture, "revision", "downstream")
	stranger := imagerSeedIssue(t, ctx, fixture, "revision", "stranger")

	prev := imagerImage(t, ctx, fixture, bead)[0]
	if prev.Revision == "" {
		t.Fatalf("image of %s carries no revision", bead)
	}
	if again := imagerImage(t, ctx, fixture, bead)[0]; again.Revision != prev.Revision {
		t.Fatalf("two reads with no write between them differ: %s vs %s", prev.Revision, again.Revision)
	}

	step := func(name string, write func() error, shows func(publicops.Image) bool) {
		t.Helper()
		if err := write(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		img := imagerImage(t, ctx, fixture, bead)[0]
		if img.Revision == prev.Revision {
			t.Errorf("%s did not change the revision %s", name, img.Revision)
		}
		if !shows(img) {
			t.Errorf("%s: the image does not show the write", name)
		}
		prev = img
	}
	step("a label", func() error {
		return fixture.AddLabel(ctx, bead, "imager-archived", "imager-contract")
	}, func(img publicops.Image) bool {
		for _, row := range img.Labels {
			if row["label"] == "imager-archived" {
				return true
			}
		}
		return false
	})
	step("a comment", func() error {
		return fixture.AddComment(ctx, bead, "imager-contract", "imager comment")
	}, func(img publicops.Image) bool {
		for _, row := range img.Comments {
			if row["text"] == "imager comment" {
				return true
			}
		}
		return false
	})
	step("an outbound dependency", func() error {
		return fixture.AddDependency(ctx, &types.Dependency{IssueID: bead, DependsOnID: upstream, Type: types.DepBlocks}, "imager-contract")
	}, func(img publicops.Image) bool { return imagerEdgeRows(img, bead, upstream) == 1 })
	step("an inbound dependency", func() error {
		return fixture.AddDependency(ctx, &types.Dependency{IssueID: downstream, DependsOnID: bead, Type: types.DepBlocks}, "imager-contract")
	}, func(img publicops.Image) bool { return imagerEdgeRows(img, downstream, bead) == 1 })

	if err := fixture.AddLabel(ctx, stranger, "imager-elsewhere", "imager-contract"); err != nil {
		t.Fatalf("label the unrelated bead: %v", err)
	}
	if again := imagerImage(t, ctx, fixture, bead)[0]; again.Revision != prev.Revision {
		t.Errorf("a write to unrelated %s moved %s's revision: %s -> %s", stranger, bead, prev.Revision, again.Revision)
	}
}

// RunImagerReadsOneSnapshot pins Imager: "READ-ONLY, AND ONE SNAPSHOT. Every
// image a call returns is read in one transaction". It counts the
// transactions one call over three ids — two durable beads joined by an edge
// and a wisp, so both planes — begins, and wants exactly one: two or more is
// an implementation that split the request, so a write landing between its
// reads would leave two images disagreeing about the edge they share; none is
// a ReadTransactions hook that does not see the Imager's reads at all.
//
// The edge rows are compared too, for the consequence the leaf names ("two
// images of beads joined by an edge agree about it"). Alone that comparison
// cannot fail with nothing writing — the file header says why — so it is the
// count that makes this case able to.
func RunImagerReadsOneSnapshot(t *testing.T, ctx context.Context, fixture ImagerFixture) {
	t.Helper()
	if fixture.ReadTransactions == nil {
		t.Skip("this backend cannot count the transactions its Imager reads in, so the one-snapshot promise is untested here")
	}
	dependent := imagerSeedIssue(t, ctx, fixture, "snapshot", "dependent")
	blocker := imagerSeedIssue(t, ctx, fixture, "snapshot", "blocker")
	wisp := imagerSeedWisp(t, ctx, fixture, "snapshot", "wisp")
	imagerAddEdge(t, ctx, fixture, dependent, blocker)

	before := fixture.ReadTransactions()
	images := imagerImage(t, ctx, fixture, dependent, blocker, wisp)
	if began := fixture.ReadTransactions() - before; began != 1 {
		t.Errorf("one Image call over %d ids began %d transactions, want exactly one: "+
			"every image a call returns is read in one transaction", len(images), began)
	}
	fromDependent := imagerEdge(t, images[0], dependent, blocker)
	fromBlocker := imagerEdge(t, images[1], dependent, blocker)
	if !reflect.DeepEqual(fromDependent, fromBlocker) {
		t.Errorf("the two images disagree about the edge they share:\n  %s: %v\n  %s: %v",
			dependent, fromDependent, blocker, fromBlocker)
	}
}

// RunImagerWritesNothing pins the role's "READ-ONLY": imaging a bead records
// no history entry and changes nothing the next image would see. An image that
// moved its own revision would make the archive guard it exists for refuse
// every restore.
func RunImagerWritesNothing(t *testing.T, ctx context.Context, fixture ImagerFixture) {
	t.Helper()
	if fixture.CountHistory == nil {
		t.Skip("this backend cannot observe history, so the role's no-entry clause is untested here")
	}
	durable := imagerSeedIssue(t, ctx, fixture, "readonly", "durable")
	wisp := imagerSeedWisp(t, ctx, fixture, "readonly", "wisp")
	imagerAddEdge(t, ctx, fixture, wisp, durable)

	before := imagerHistory(t, ctx, fixture)
	first := imagerImage(t, ctx, fixture, durable, wisp)
	second := imagerImage(t, ctx, fixture, durable, wisp)
	if after := imagerHistory(t, ctx, fixture); after != before {
		t.Errorf("history went %d -> %d across two images, want no entry", before, after)
	}
	for i := range first {
		if first[i].Revision != second[i].Revision {
			t.Errorf("imaging %s moved its revision: %s -> %s", first[i].ID, first[i].Revision, second[i].Revision)
		}
	}
	if _, err := fixture.Imager.Image(ctx, []string{durable, fixture.IssuePrefix + "-img-readonly-absent"}); !errors.Is(err, publicops.ErrNotFound) {
		t.Fatalf("Image with an absent id error = %v, want ErrNotFound", err)
	}
	if after := imagerHistory(t, ctx, fixture); after != before {
		t.Errorf("history went %d -> %d across a refused image, want no entry", before, after)
	}
}

func imagerIssue(fixture ImagerFixture, tag, name string, ephemeral bool) *types.Issue {
	id := fixture.IssuePrefix + "-img-" + tag + "-" + name
	return &types.Issue{
		ID:        id,
		Title:     id,
		Status:    types.StatusOpen,
		Priority:  2,
		IssueType: types.TypeTask,
		Ephemeral: ephemeral,
	}
}

func imagerSeed(t *testing.T, ctx context.Context, fixture ImagerFixture, issue *types.Issue) string {
	t.Helper()
	create := fixture.CreateIssue
	if issue.Ephemeral {
		create = fixture.CreateWisp
	}
	if err := create(ctx, issue, "imager-contract"); err != nil {
		t.Fatalf("seed %s: %v", issue.ID, err)
	}
	return issue.ID
}

func imagerSeedIssue(t *testing.T, ctx context.Context, fixture ImagerFixture, tag, name string) string {
	t.Helper()
	return imagerSeed(t, ctx, fixture, imagerIssue(fixture, tag, name, false))
}

func imagerSeedWisp(t *testing.T, ctx context.Context, fixture ImagerFixture, tag, name string) string {
	t.Helper()
	return imagerSeed(t, ctx, fixture, imagerIssue(fixture, tag, name, true))
}

func imagerAddEdge(t *testing.T, ctx context.Context, fixture ImagerFixture, dependent, blocker string) {
	t.Helper()
	if err := fixture.AddDependency(ctx, &types.Dependency{
		IssueID: dependent, DependsOnID: blocker, Type: types.DepBlocks,
	}, "imager-contract"); err != nil {
		t.Fatalf("seed edge %s -> %s: %v", dependent, blocker, err)
	}
}

// imagerImage images ids and fails on any error. It checks only the count, so
// a case reading images[i] reads the i-th distinct id it named.
func imagerImage(t *testing.T, ctx context.Context, fixture ImagerFixture, ids ...string) []publicops.Image {
	t.Helper()
	images, err := fixture.Imager.Image(ctx, ids)
	if err != nil {
		t.Fatalf("Image(%v): %v", ids, err)
	}
	distinct := map[string]bool{}
	for _, id := range ids {
		distinct[id] = true
	}
	if len(images) != len(distinct) {
		t.Fatalf("Image(%v) returned %d images, want %d", ids, len(images), len(distinct))
	}
	return images
}

func imagerIDs(images []publicops.Image) []string {
	ids := make([]string, len(images))
	for i, img := range images {
		ids[i] = img.ID
	}
	return ids
}

// imagerEdgeRows counts img's dependency rows for the edge dependent ->
// blocker. The target's class decides which typed column holds it, so a row
// matches on whichever depends_on_*_id column names the blocker.
func imagerEdgeRows(img publicops.Image, dependent, blocker string) int {
	var n int
	for _, row := range img.Dependencies {
		if imagerIsEdge(row, dependent, blocker) {
			n++
		}
	}
	return n
}

func imagerIsEdge(row map[string]any, dependent, blocker string) bool {
	return row["issue_id"] == dependent && (row["depends_on_issue_id"] == blocker || row["depends_on_wisp_id"] == blocker)
}

func imagerEdge(t *testing.T, img publicops.Image, dependent, blocker string) map[string]any {
	t.Helper()
	for _, row := range img.Dependencies {
		if imagerIsEdge(row, dependent, blocker) {
			return row
		}
	}
	t.Fatalf("image of %s has no row for the edge %s -> %s: %v", img.ID, dependent, blocker, img.Dependencies)
	return nil
}

func imagerHistory(t *testing.T, ctx context.Context, fixture ImagerFixture) int {
	t.Helper()
	entries, err := fixture.CountHistory(ctx)
	if err != nil {
		t.Fatalf("CountHistory: %v", err)
	}
	return entries
}
