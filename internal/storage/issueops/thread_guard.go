package issueops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/workapi"
	publicops "github.com/steveyegge/beads/issueops"
)

// threadGuardWalkLimit bounds one thread's membership walk. A thread that
// reaches past it is not a mail thread the guard can vouch for, so the walk
// aborts the request rather than reading the workspace's whole graph inside a
// write transaction.
const threadGuardWalkLimit = 10000

// ValidateThreadGuard refuses, before anything is read, a DeleteRequest whose
// ThreadGuard cannot be checked: one paired with Cascade, or one missing a
// field every check needs. A nil guard is valid.
//
// Both delete bodies call it — DeleteInTx at its top and the unit-of-work
// deleter before it opens a unit of work — so the refusal costs no database
// work on either route.
func ValidateThreadGuard(req publicops.DeleteRequest) error {
	guard := req.ThreadGuard
	if guard == nil {
		return nil
	}
	if req.Cascade {
		return fmt.Errorf("%w: a thread guard cannot be combined with cascade: a cascade deletes rows the guard never archived", publicops.ErrValidation)
	}
	if strings.TrimSpace(guard.MemberLabel) == "" {
		return fmt.Errorf("%w: thread guard needs a member label", publicops.ErrValidation)
	}
	if strings.TrimSpace(guard.Edge) == "" {
		return fmt.Errorf("%w: thread guard needs an edge type", publicops.ErrValidation)
	}
	if len(guard.Threads) == 0 {
		return fmt.Errorf("%w: thread guard lists no threads", publicops.ErrValidation)
	}
	for i, thread := range guard.Threads {
		if strings.TrimSpace(thread.Root) == "" {
			return fmt.Errorf("%w: thread guard thread %d has no root", publicops.ErrValidation, i)
		}
		if len(thread.Members) == 0 {
			return fmt.Errorf("%w: thread guard thread %s lists no members", publicops.ErrValidation, thread.Root)
		}
		for _, member := range thread.Members {
			if strings.TrimSpace(member.ID) == "" || strings.TrimSpace(member.Revision) == "" {
				return fmt.Errorf("%w: thread guard thread %s has a member with no id or no revision", publicops.ErrValidation, thread.Root)
			}
		}
	}
	return nil
}

// CheckThreadGuardInTx runs guard's four checks against tx for a delete of
// ids, and returns a *publicops.ThreadGuardMismatchError naming every failure,
// nil when all pass, or the read error that stopped it.
//
// It only reads. The caller runs it inside the deleting transaction, after the
// writer lock is held and before any row changes, so the archive is compared
// against the snapshot the deletion acts on; a failure leaves the caller
// nothing to undo.
//
// It assumes a guard ValidateThreadGuard accepted.
func CheckThreadGuardInTx(ctx context.Context, tx DBTX, guard *publicops.ThreadGuard, ids []string) error {
	var failed []publicops.ThreadGuardFailure
	guarded := make(map[string]bool)
	for _, thread := range guard.Threads {
		expected := make(map[string]string, len(thread.Members))
		var memberIDs []string
		for _, member := range thread.Members {
			if _, dup := expected[member.ID]; !dup {
				memberIDs = append(memberIDs, member.ID)
			}
			expected[member.ID] = member.Revision
			guarded[member.ID] = true
		}

		component, err := threadComponentInTx(ctx, tx, thread.Root, guard.Edge, guard.ExcludeLabel)
		if err != nil {
			return err
		}
		strays := make(map[string]bool)
		for id := range component {
			if _, ok := expected[id]; !ok {
				strays[id] = true
			}
		}
		for _, id := range memberIDs {
			if !component[id] {
				strays[id] = true
			}
		}
		if len(strays) > 0 {
			failed = append(failed, threadGuardFailure(thread.Root, publicops.ThreadGuardMembership, strays))
		}

		images, missing, err := threadImagesInTx(ctx, tx, memberIDs)
		if err != nil {
			return err
		}
		changed := make(map[string]bool, len(missing))
		for _, id := range missing {
			changed[id] = true
		}
		ineligible := make(map[string]bool)
		for _, img := range images {
			if img.Revision != expected[img.ID] {
				changed[img.ID] = true
			}
			if !threadGuardEligible(img, guard) {
				ineligible[img.ID] = true
			}
		}
		if len(changed) > 0 {
			failed = append(failed, threadGuardFailure(thread.Root, publicops.ThreadGuardRevision, changed))
		}
		if len(ineligible) > 0 {
			failed = append(failed, threadGuardFailure(thread.Root, publicops.ThreadGuardEligibility, ineligible))
		}
	}

	labelled, err := threadGuardLabelledInTx(ctx, tx, ids, guard.MemberLabel)
	if err != nil {
		return err
	}
	uncovered := make(map[string]bool)
	for id := range labelled {
		if !guarded[id] {
			uncovered[id] = true
		}
	}
	if len(uncovered) > 0 {
		failed = append(failed, threadGuardFailure("", publicops.ThreadGuardCoverage, uncovered))
	}

	if len(failed) > 0 {
		return &publicops.ThreadGuardMismatchError{Failed: failed}
	}
	return nil
}

func threadGuardFailure(root, check string, ids map[string]bool) publicops.ThreadGuardFailure {
	return publicops.ThreadGuardFailure{Root: root, Check: check, IDs: workapi.SortedDeleteIDs(ids)}
}

// threadGuardEligible reports whether img is still archivable thread mail:
// labelled MemberLabel, a wisp, closed, and closed before the cutoff. It reads
// the image rather than the row so the revision and the eligibility are
// answered from one read.
func threadGuardEligible(img publicops.Image, guard *publicops.ThreadGuard) bool {
	labelled := false
	for _, row := range img.Labels {
		if row["label"] == guard.MemberLabel {
			labelled = true
			break
		}
	}
	if !labelled || img.Table != "wisps" || img.Row["status"] != string(types.StatusClosed) {
		return false
	}
	closedAt, ok := img.Row["closed_at"].(string)
	if !ok {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, closedAt)
	return err == nil && at.Before(guard.Cutoff)
}

// threadImagesInTx images ids and reports separately the ids that name no row
// in either plane, which ImageInTx refuses as a whole.
func threadImagesInTx(ctx context.Context, tx DBTX, ids []string) ([]publicops.Image, []string, error) {
	images, err := ImageInTx(ctx, tx, ids)
	var notFound *publicops.NotFoundError
	if !errors.As(err, &notFound) {
		if err != nil {
			return nil, nil, fmt.Errorf("thread guard: image members: %w", err)
		}
		return images, nil, nil
	}
	gone := make(map[string]bool, len(notFound.IDs))
	for _, id := range notFound.IDs {
		gone[id] = true
	}
	var present []string
	for _, id := range ids {
		if !gone[id] {
			present = append(present, id)
		}
	}
	if len(present) == 0 {
		return nil, notFound.IDs, nil
	}
	images, err = ImageInTx(ctx, tx, present)
	if err != nil {
		return nil, nil, fmt.Errorf("thread guard: image members: %w", err)
	}
	return images, notFound.IDs, nil
}

// threadComponentInTx walks the connected component of root over edge-typed
// dependencies in either direction, in both edge tables, and returns every id
// it reached, root included. A bead labelled exclude is neither reached nor
// walked through; an excluded root reaches nothing.
//
// The walk does not ask whether a reached id names a row: a reply edge onto a
// bead deleted since is still part of what the caller would be deleting
// around, and the revision check reports the gone member.
//
//nolint:gosec // G201: inClause contains only ? placeholders
func threadComponentInTx(ctx context.Context, tx DBTX, root, edge, exclude string) (map[string]bool, error) {
	if exclude != "" {
		excluded, err := threadGuardLabelledInTx(ctx, tx, []string{root}, exclude)
		if err != nil {
			return nil, err
		}
		if excluded[root] {
			return map[string]bool{}, nil
		}
	}
	reached := map[string]bool{root: true}
	frontier := []string{root}
	for len(frontier) > 0 {
		found := make(map[string]bool)
		for i := 0; i < len(frontier); i += deleteBatchSize {
			end := min(i+deleteBatchSize, len(frontier))
			inClause, args := buildSQLInClause(frontier[i:end])
			queryArgs := append(append([]any{edge}, args...), args...)
			for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
				rows, err := tx.QueryContext(ctx,
					fmt.Sprintf(`SELECT issue_id, %s AS depends_on_id FROM %s WHERE type = ? AND (issue_id IN (%s) OR %s)`,
						DepTargetExpr, depTable, inClause, depTargetIn("", inClause)),
					queryArgs...)
				if err != nil {
					if optionalBlockedTable(depTable) && isTableNotExistError(err) {
						continue
					}
					return nil, fmt.Errorf("thread guard: walk %s from %s: %w", depTable, root, err)
				}
				for rows.Next() {
					var source string
					var target sql.NullString
					if err := rows.Scan(&source, &target); err != nil {
						_ = rows.Close()
						return nil, fmt.Errorf("thread guard: scan %s edge: %w", depTable, err)
					}
					for _, id := range [2]string{source, target.String} {
						if id != "" && !reached[id] {
							found[id] = true
						}
					}
				}
				_ = rows.Close()
				if err := rows.Err(); err != nil {
					return nil, fmt.Errorf("thread guard: walk %s from %s: %w", depTable, root, err)
				}
			}
		}
		if exclude != "" && len(found) > 0 {
			excluded, err := threadGuardLabelledInTx(ctx, tx, workapi.SortedDeleteIDs(found), exclude)
			if err != nil {
				return nil, err
			}
			for id := range excluded {
				delete(found, id)
			}
		}
		frontier = workapi.SortedDeleteIDs(found)
		for _, id := range frontier {
			reached[id] = true
		}
		if len(reached) > threadGuardWalkLimit {
			return nil, fmt.Errorf("thread guard: thread %s reaches more than %d beads over %q edges", root, threadGuardWalkLimit, edge)
		}
	}
	return reached, nil
}

// threadGuardLabelledInTx reports which of ids carry label, in either plane.
//
//nolint:gosec // G201: inClause contains only ? placeholders
func threadGuardLabelledInTx(ctx context.Context, tx DBTX, ids []string, label string) (map[string]bool, error) {
	labelled := make(map[string]bool)
	for i := 0; i < len(ids); i += deleteBatchSize {
		end := min(i+deleteBatchSize, len(ids))
		inClause, args := buildSQLInClause(ids[i:end])
		queryArgs := append([]any{label}, args...)
		for _, labelTable := range []string{"labels", "wisp_labels"} {
			rows, err := tx.QueryContext(ctx,
				fmt.Sprintf(`SELECT issue_id FROM %s WHERE label = ? AND issue_id IN (%s)`, labelTable, inClause),
				queryArgs...)
			if err != nil {
				return nil, fmt.Errorf("thread guard: read %s: %w", labelTable, err)
			}
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					_ = rows.Close()
					return nil, fmt.Errorf("thread guard: scan %s: %w", labelTable, err)
				}
				labelled[id] = true
			}
			_ = rows.Close()
			if err := rows.Err(); err != nil {
				return nil, fmt.Errorf("thread guard: read %s: %w", labelTable, err)
			}
		}
	}
	return labelled, nil
}
