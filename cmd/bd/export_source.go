package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/dberrors"
	"github.com/steveyegge/beads/internal/storage/sqlbuild"
	"github.com/steveyegge/beads/internal/storage/uow"
	"github.com/steveyegge/beads/internal/types"
)

// exportSource abstracts the storage reads `bd export` performs, so the one
// export body (runExportFromSource) serves both storage stacks:
//
//   - storeExportSource reads through the classic `store` global
//     (embedded / direct / server modes), preserving the exact pre-seam
//     call pattern.
//   - uowExportSource reads through a proxied-server unit of work, whose
//     domain use cases are plane-pinned (issues vs wisps tables) where the
//     classic bulk loaders partition internally — the impl queries both
//     planes with the full ID set and merges, so both modes read the same
//     rows regardless of which table an issue lives in.
//
// Everything downstream of these reads is shared code; the acceptance bar for
// the seam is byte-identical JSONL output across modes.
type exportSource interface {
	// GetInfraTypes returns the resolved infra-type set (config, YAML, or
	// hardcoded defaults). A nil/empty result makes the caller fall back to
	// domain.DefaultInfraTypes(), mirroring the pre-seam behavior.
	GetInfraTypes(ctx context.Context) map[string]bool
	// SearchIssues returns the full, untruncated result set for the export
	// filter (Limit=0, MaxRows=0). Implementations MUST fail rather than
	// return a truncated page — export is a data-integrity path.
	SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error)
	// SearchIssueKeys returns the ids SearchIssues would return for the same
	// query and filter, in the same order. Export's first pass walks this
	// list and reads full rows a page at a time (SearchIssues with IDs).
	SearchIssueKeys(ctx context.Context, query string, filter types.IssueFilter) ([]string, error)
	// GetConfig reads one config key from the database (used for the
	// export.exclude_owners owner-filter keys).
	GetConfig(ctx context.Context, key string) (string, error)
	// GetAllConfig reads the whole config table (used to extract kv.memory.*
	// rows when memories are exported).
	GetAllConfig(ctx context.Context) (map[string]string, error)
	// LoadExportRelations bulk-loads labels, dependency records, comments,
	// comment counts, and dependency counts for the searched issues.
	LoadExportRelations(ctx context.Context, issues []*types.Issue) (exportRelations, error)
	// WispPlaneIDs reports which of ids currently live in the WISPS table.
	// Export uses it to stamp the explicit "wisp_plane" marker on records
	// whose row flags are ambiguous: a no_history=true row is either an
	// unpromoted no-history wisp (wisps table) or a promoted one (durable
	// issues-table row that may still carry the stray flag), and only table
	// membership can tell them apart — import routes by the marker, so
	// mis-stamping a durable row would re-plane it and drop its relations
	// (bd-r9uce). Implementations must classify by table membership, never
	// by flags, and both modes must agree (byte-identity oracle).
	WispPlaneIDs(ctx context.Context, ids []string) (map[string]bool, error)
}

// storeWispPartitioner is the optional store capability WispPlaneIDs uses in
// classic mode. Both real stores (DoltStore, EmbeddedDoltStore) implement it;
// a store that does not simply gets no plane markers, which degrades to the
// data-safe side (import then routes bare no_history rows to the durable
// plane, where nothing is ever excluded from export).
type storeWispPartitioner interface {
	PartitionWispIDs(ctx context.Context, ids []string) (wispIDs, permIDs []string, err error)
}

// storeKeySearcher is the optional store capability SearchIssueKeys uses in
// classic mode: an ordered id search that hydrates no row. Both real stores
// (DoltStore, EmbeddedDoltStore) implement it; a store that does not falls
// back to the whole-set SearchIssues, projected to ids (today's memory, never
// a failure).
type storeKeySearcher interface {
	SearchIssueKeys(ctx context.Context, query string, filter types.IssueFilter) ([]string, error)
}

// findStoreCapability walks the decorator-wrapped store global (telemetry,
// hook firing) down through Unwrap() to the first store implementing C.
func findStoreCapability[C any](s storage.DoltStorage) (C, bool) {
	var zero C
	for s != nil {
		if c, ok := s.(C); ok {
			return c, true
		}
		u, ok := s.(interface{ Unwrap() storage.DoltStorage })
		if !ok {
			return zero, false
		}
		s = u.Unwrap()
	}
	return zero, false
}

// exportRelations carries the bulk-loaded relational data for the export set,
// keyed by issue ID.
type exportRelations struct {
	labels        map[string][]string
	deps          map[string][]*types.Dependency
	comments      map[string][]*types.Comment
	commentCounts map[string]int
	depCounts     map[string]*types.DependencyCounts
}

// errExportNoStore reports a classic-source read attempted with no store
// open. Non-strict callers of exportSource.GetConfig treat any error as "key
// unset", which reproduces the pre-seam `if store == nil` early return.
var errExportNoStore = errors.New("no store available")

// errExportNoPartitioner is a strict export's refusal to stamp no plane
// markers when the classic store cannot classify wisp-plane rows.
var errExportNoPartitioner = errors.New("wisp plane membership: store has no partitioner")

// storeExportSource is the classic-mode exportSource over the `store` global.
// strict (bd export --strict) turns every read the classic path degrades
// (bulk-load errors, a store with no partitioner) into an error.
type storeExportSource struct {
	strict bool
}

func (storeExportSource) GetInfraTypes(ctx context.Context) map[string]bool {
	if store == nil {
		return nil
	}
	return store.GetInfraTypes(ctx)
}

func (storeExportSource) SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error) {
	return store.SearchIssues(ctx, query, filter)
}

func (storeExportSource) SearchIssueKeys(ctx context.Context, query string, filter types.IssueFilter) ([]string, error) {
	if ks, ok := findStoreCapability[storeKeySearcher](store); ok {
		return ks.SearchIssueKeys(ctx, query, filter)
	}
	issues, err := store.SearchIssues(ctx, query, filter)
	if err != nil {
		return nil, err
	}
	return projectIssueIDs(issues), nil
}

func (storeExportSource) GetConfig(ctx context.Context, key string) (string, error) {
	if store == nil {
		return "", errExportNoStore
	}
	return store.GetConfig(ctx, key)
}

func (storeExportSource) GetAllConfig(ctx context.Context) (map[string]string, error) {
	return store.GetAllConfig(ctx)
}

func (s storeExportSource) LoadExportRelations(ctx context.Context, issues []*types.Issue) (exportRelations, error) {
	issueIDs := make([]string, len(issues))
	for i, issue := range issues {
		issueIDs[i] = issue.ID
	}

	// Individual bulk-load failures deliberately degrade to empty maps rather
	// than aborting the export — unchanged from the pre-seam classic behavior.
	// Under --strict the first failure is the export's error instead; the
	// loaders' own tolerance for a missing optional wisp table stays.
	labelsMap, labelsErr := store.GetLabelsForIssues(ctx, issueIDs)
	allDeps, depsErr := store.GetDependencyRecordsForIssues(ctx, issueIDs)
	commentsMap, commentsErr := store.GetCommentsForIssues(ctx, issueIDs)
	commentCounts, commentCountsErr := store.GetCommentCounts(ctx, issueIDs)
	depCounts, depCountsErr := store.GetDependencyCounts(ctx, issueIDs)

	if s.strict {
		for _, load := range []struct {
			relation string
			err      error
		}{
			{"labels", labelsErr},
			{"dependency records", depsErr},
			{"comments", commentsErr},
			{"comment counts", commentCountsErr},
			{"dependency counts", depCountsErr},
		} {
			if load.err != nil {
				return exportRelations{}, fmt.Errorf("load %s: %w", load.relation, load.err)
			}
		}
	}

	return exportRelations{
		labels:        labelsMap,
		deps:          allDeps,
		comments:      commentsMap,
		commentCounts: commentCounts,
		depCounts:     depCounts,
	}, nil
}

func (src storeExportSource) WispPlaneIDs(ctx context.Context, ids []string) (map[string]bool, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	// Without --strict, a store that cannot classify yields no markers (the
	// data-safe side, see storeWispPartitioner); a strict export refuses.
	noPartitioner := func() (map[string]bool, error) {
		if src.strict {
			return nil, errExportNoPartitioner
		}
		return nil, nil
	}
	p, ok := findStoreCapability[storeWispPartitioner](store)
	if !ok {
		return noPartitioner()
	}
	wispIDs, _, err := p.PartitionWispIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(wispIDs))
	for _, id := range wispIDs {
		set[id] = true
	}
	return set, nil
}

// uowExportSource is the proxied-server exportSource over a unit of work. All
// reads run inside the single read transaction runExport opened, so the
// export is one consistent snapshot. strict (bd export --strict) narrows the
// wisp-plane tolerance in LoadExportRelations to an absent wisps plane.
type uowExportSource struct {
	uw     uow.UnitOfWork
	strict bool
}

func (s *uowExportSource) GetInfraTypes(ctx context.Context) map[string]bool {
	infra, err := s.uw.ConfigUseCase().GetInfraTypes(ctx)
	if err != nil {
		// Match the classic source's error-free signature: an unreadable
		// config degrades to the caller's DefaultInfraTypes() fallback.
		return nil
	}
	return infra
}

func (s *uowExportSource) SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error) {
	page, err := s.uw.IssueUseCase().SearchIssues(ctx, query, filter)
	if err != nil {
		return nil, err
	}
	// Export builds its filter with Limit=0/MaxRows=0, so the whole result
	// arrives as one page and HasMore can only be true if a limit creeps back
	// in. Guard anyway: an export must fail loudly, never truncate silently
	// (domain.SearchPage carries no continuation cursor to loop with).
	if page.HasMore {
		return nil, fmt.Errorf("issue search returned a truncated page (limit=%d); export must never truncate", filter.Limit)
	}
	return page.Items, nil
}

// SearchIssueKeys has no key search to call in proxied-server mode: the unit
// of work's plane-pinned use cases offer none, so pass 1 runs the whole-set
// SearchIssues inside the export's one read transaction and keeps only the
// ids (this mode's pass 1 holds every row once, as before paging). The page
// loop after it is the same as classic mode's.
func (s *uowExportSource) SearchIssueKeys(ctx context.Context, query string, filter types.IssueFilter) ([]string, error) {
	issues, err := s.SearchIssues(ctx, query, filter)
	if err != nil {
		return nil, err
	}
	return projectIssueIDs(issues), nil
}

func (s *uowExportSource) GetConfig(ctx context.Context, key string) (string, error) {
	return s.uw.ConfigUseCase().GetConfig(ctx, key)
}

func (s *uowExportSource) GetAllConfig(ctx context.Context) (map[string]string, error) {
	return s.uw.ConfigUseCase().GetAllConfig(ctx)
}

// LoadExportRelations reproduces the classic bulk loaders over the
// plane-pinned domain use cases:
//
//   - Labels, comments, and comment counts: the classic loaders partition IDs
//     by wisps-table membership (issueops.PartitionWispIDsInTx). Row flags are
//     NOT a substitute for that partition — a promoted no-history wisp is a
//     durable issues-table row that (as wild data from the pre-bd-r9uce
//     promote) still carries NoHistory=true — so membership is read from the
//     wisps table itself (WispPlaneIDs), never inferred. The durable plane is
//     then queried with the FULL ID set and the wisp plane only with that
//     membership subset (wispReaderIDs), and the two are merged. Unreadable
//     membership fails OPEN to the full id list, so a nil set is never
//     mistaken for "no wisps" (wy-237yfi). The planes are ID-disjoint
//     (GH#4455 cross-table guard), so at most one plane returns rows for any
//     id and the merge cannot collide.
//   - Dependency records: domain GetIssueDependencyRecords already partitions
//     internally (same issueops helper), so one call covers both planes.
//   - Dependency counts: classic GetDependencyCountsInTx queries BOTH dep
//     tables for EVERY id and sums (a wisp blocking a durable issue
//     contributes to the durable issue's dependent_count), so both planes are
//     queried for the full ID set and summed here too.
//
// Wisp-plane reads tolerate a rig without the wisp tables (IsTableNotExist),
// mirroring the dependency-counts leg. The three wisp-relation readers below
// keep that blanket check: each is a single-table read, so blanket and
// table-specific are the same test there. The membership probe is the
// exception — WispPlaneIDs renders FROM wisps LEFT JOIN leases, so its
// tolerance is pinned to a missing `wisps` table and a broken database
// surfaces as an error instead of an empty wisp plane. Under --strict the
// readers' tolerance narrows too (wispReadTolerated): a missing wisp_labels
// or wisp_comments while the wisps table answers is a failed read, not an
// absent plane, as it is for the classic loaders.
func (s *uowExportSource) LoadExportRelations(ctx context.Context, issues []*types.Issue) (exportRelations, error) {
	rel := exportRelations{
		labels:        map[string][]string{},
		deps:          map[string][]*types.Dependency{},
		comments:      map[string][]*types.Comment{},
		commentCounts: map[string]int{},
		depCounts:     map[string]*types.DependencyCounts{},
	}

	allIDs := make([]string, 0, len(issues))
	for _, issue := range issues {
		allIDs = append(allIDs, issue.ID)
	}
	if len(allIDs) == 0 {
		return rel, nil
	}

	// BATCH READS, and the batch is what keeps them here. Export hydrates every
	// relation of N issues at once — labels, comments, dependencies, counts,
	// both planes — and no role answers "the labels of these N ids": Reader.Get
	// is one detail view per call, so a role-routed export would be N round
	// trips over a whole workspace. A bulk relation reader is the follow-up
	// (ga-2ltro.12). Every LabelUseCase call in this file is one of these.
	labels, err := s.uw.LabelUseCase().GetLabelsForIssues(ctx, allIDs) //nolint:forbidigo // bulk relation load; no role answers labels-for-N-ids
	if err != nil {
		return exportRelations{}, fmt.Errorf("load labels: %w", err)
	}
	mergeExportMap(rel.labels, labels)
	comments, err := s.uw.CommentUseCase().GetCommentsForIssues(ctx, allIDs)
	if err != nil {
		return exportRelations{}, fmt.Errorf("load comments: %w", err)
	}
	mergeExportMap(rel.comments, comments)
	counts, err := s.uw.CommentUseCase().GetCommentCounts(ctx, allIDs)
	if err != nil {
		return exportRelations{}, fmt.Errorf("load comment counts: %w", err)
	}
	mergeExportMap(rel.commentCounts, counts)

	// The wisp_labels / wisp_comments rows of a wisp are keyed by an issue_id
	// that lives in the WISPS table (promotion moves them to the durable
	// tables, issueops.PromoteFromEphemeralInTx), so the three wisp readers
	// below only need the ids that are wisps right now. Handing them the
	// whole workspace was pure waste — on a 19k-issue rig with no wisps it
	// was three 19k-placeholder statements answering nothing (wy-237yfi).
	// CountsByWispIDs is NOT narrowed: its inbound leg counts wisp -> durable
	// edges under the durable target's id.
	// A nil set means membership could NOT be read (the wisps table is
	// absent, the tolerated case) — never "no wisps": fail open to the full
	// id list so the readers below keep their own table-not-exist tolerance.
	wispSet, err := s.WispPlaneIDs(ctx, allIDs)
	if err != nil {
		return exportRelations{}, err
	}
	wispIDs := wispReaderIDs(allIDs, wispSet)

	wispLabels, err := s.uw.LabelUseCase().GetLabelsForWisps(ctx, wispIDs) //nolint:forbidigo // bulk relation load; see GetLabelsForIssues above
	if err != nil {
		if !s.wispReadTolerated(err, wispSet) {
			return exportRelations{}, fmt.Errorf("load wisp labels: %w", err)
		}
	} else {
		mergeExportMap(rel.labels, wispLabels)
	}
	wispComments, err := s.uw.CommentUseCase().GetCommentsForWisps(ctx, wispIDs)
	if err != nil {
		if !s.wispReadTolerated(err, wispSet) {
			return exportRelations{}, fmt.Errorf("load wisp comments: %w", err)
		}
	} else {
		mergeExportMap(rel.comments, wispComments)
	}
	wispCounts, err := s.uw.CommentUseCase().GetWispCommentCounts(ctx, wispIDs)
	if err != nil {
		if !s.wispReadTolerated(err, wispSet) {
			return exportRelations{}, fmt.Errorf("load wisp comment counts: %w", err)
		}
	} else {
		mergeExportMap(rel.commentCounts, wispCounts)
	}

	deps, err := s.uw.DependencyUseCase().GetIssueDependencyRecords(ctx, allIDs)
	if err != nil {
		return exportRelations{}, fmt.Errorf("load dependency records: %w", err)
	}
	rel.deps = deps

	depCounts, err := s.uw.DependencyUseCase().CountsByIssueIDs(ctx, allIDs)
	if err != nil {
		return exportRelations{}, fmt.Errorf("load dependency counts: %w", err)
	}
	rel.depCounts = depCounts
	wispDepCounts, err := s.uw.DependencyUseCase().CountsByWispIDs(ctx, allIDs)
	if err != nil {
		// A rig without the wisp tables simply has no wisp-plane edges.
		if !s.wispDepCountsTolerated(err) {
			return exportRelations{}, fmt.Errorf("load wisp dependency counts: %w", err)
		}
	} else {
		for id, wc := range wispDepCounts {
			if wc == nil || (wc.DependencyCount == 0 && wc.DependentCount == 0) {
				continue
			}
			c := rel.depCounts[id]
			if c == nil {
				c = &types.DependencyCounts{}
				rel.depCounts[id] = c
			}
			c.DependencyCount += wc.DependencyCount
			c.DependentCount += wc.DependentCount
		}
	}

	return rel, nil
}

// wispReadTolerated reports whether a wisp_labels / wisp_comments reader's
// error is the tolerated absence of the wisp plane. Without --strict any
// table-not-exist is (unchanged). Under --strict only when membership could
// not be read because the wisps table is absent (wispSet nil): while the
// wisps table answers, its rows' labels and comments must be read.
func (s *uowExportSource) wispReadTolerated(err error, wispSet map[string]bool) bool {
	if !dberrors.IsTableNotExist(err) {
		return false
	}
	return !s.strict || wispSet == nil
}

// wispDepCountsTolerated is the same policy for CountsByWispIDs, which is not
// narrowed by membership: under --strict only a missing optional wisp table
// (sqlbuild.OptionalWispTable: wisps, wisp_dependencies) is tolerated.
func (s *uowExportSource) wispDepCountsTolerated(err error) bool {
	if !s.strict {
		return dberrors.IsTableNotExist(err)
	}
	name, ok := dberrors.MissingTableName(err)
	return ok && sqlbuild.OptionalWispTable(name)
}

// wispPlaneSubset returns, in input order, the ids that wispSet marks as
// wisp-plane; none marked yields nil, and the bulk readers answer nil with no
// statement at all. The caller decides what a nil SET means (see
// LoadExportRelations: unreadable membership fails open, never empty).
func wispPlaneSubset(ids []string, wispSet map[string]bool) []string {
	var out []string
	for _, id := range ids {
		if wispSet[id] {
			out = append(out, id)
		}
	}
	return out
}

// wispReaderIDs is the caller-side policy wispPlaneSubset defers: a nil set
// means membership was UNREADABLE (WispPlaneIDs tolerated a missing wisps
// table), so every id goes to the wisp readers and they keep their own
// table-not-exist tolerance; a non-nil set — even an empty one — is the
// answer, and only the ids in it go.
func wispReaderIDs(ids []string, wispSet map[string]bool) []string {
	if wispSet == nil {
		return ids
	}
	return wispPlaneSubset(ids, wispSet)
}

// WispPlaneIDs classifies by wisps-table membership through the plane-pinned
// GetWispsByIDs read (row flags are NOT a substitute — see the exportSource
// interface comment). A rig without the wisps table has no wisp-plane rows
// (nil set), mirroring the tolerance of the wisp-plane legs in
// LoadExportRelations. Only the WISPS table is optional: the read joins
// leases, and a blanket table-not-exist check would report a broken
// database as an empty wisp plane (the issue_search.go missingOptionalWispTable
// lesson), so the tolerance is pinned to that one table name.
func (s *uowExportSource) WispPlaneIDs(ctx context.Context, ids []string) (map[string]bool, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	wisps, err := s.uw.IssueUseCase().GetWispsByIDs(ctx, ids)
	if err != nil {
		if dberrors.IsMissingTable(err, "wisps") {
			return nil, nil
		}
		return nil, fmt.Errorf("wisp plane membership: %w", err)
	}
	set := make(map[string]bool, len(wisps))
	for _, w := range wisps {
		set[w.ID] = true
	}
	return set, nil
}

// projectIssueIDs projects issues to their ids, in order.
func projectIssueIDs(issues []*types.Issue) []string {
	ids := make([]string, len(issues))
	for i, issue := range issues {
		ids[i] = issue.ID
	}
	return ids
}

// mergeExportMap copies src entries into dst. The label/comment planes are
// disjoint by ID (an issue lives in exactly one of issues/wisps), so a plain
// overwrite-merge reassembles the classic partitioned loaders' single map.
func mergeExportMap[V any](dst, src map[string]V) {
	for k, v := range src {
		dst[k] = v
	}
}
