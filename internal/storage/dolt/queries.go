package dolt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/steveyegge/beads/internal/storage/dberrors"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// SearchIssues finds issues matching query and filters.
// Delegates to issueops.SearchIssuesInTx for shared query logic.
func (s *DoltStore) SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error) {
	var result []*types.Issue
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.SearchIssuesInTx(ctx, tx, query, filter)
		return err
	})
	return result, err
}

// SearchIssueIDs is the narrow-projection variant of SearchIssues; returns
// only matching IDs.
func (s *DoltStore) SearchIssueIDs(ctx context.Context, query string, filter types.IssueFilter) ([]string, error) {
	var result []string
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.SearchIssueIDsInTx(ctx, tx, query, filter)
		return err
	})
	return result, err
}

// SearchIssueKeys returns the ids SearchIssues would return, in the same
// order, without hydrating the rows (the first pass of a paged bd export).
func (s *DoltStore) SearchIssueKeys(ctx context.Context, query string, filter types.IssueFilter) ([]string, error) {
	var result []string
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.SearchIssueKeysInTx(ctx, tx, query, filter)
		return err
	})
	return result, err
}

func (s *DoltStore) SearchIssuesWithCounts(ctx context.Context, query string, filter types.IssueFilter) ([]*types.IssueWithCounts, error) {
	var result []*types.IssueWithCounts
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.SearchIssuesWithCountsInTx(ctx, tx, query, filter)
		return err
	})
	return result, err
}

// deferWakeSweepEligible reports whether a store opened with the given
// readOnly/classifiedRead flags may run the lazy defer-wake sweep. A
// classified-read open (GH#804: bd ready/bd list are read-only for the
// CURRENT project) is eligible despite readOnly=true because the store is
// genuinely writable underneath; strict --readonly, preview, and
// foreign-project opens are not (be-vbhpf).
//
// Only the strict --readonly and preview cases are decided here, by
// classifiedRead being false. Foreign-project and other auxiliary opens are
// excluded one layer up: they build their own dolt.Config literals and never
// set ClassifiedRead, so they reach this guard already fail-closed. Do not
// look for a foreign-project term in this predicate — there is none, and
// setting ClassifiedRead on such an open would defeat the exclusion.
func deferWakeSweepEligible(readOnly, classifiedRead bool) bool {
	return !readOnly || classifiedRead
}

// The lazy defer-wake sweep (issueops.WakeExpiredDefersInTx) runs in its own
// write transaction before a ready-work read or a claim, through one of two
// entry points:
//
//   - wakeExpiredDefersForRead (GetReadyWork, GetReadyWorkWithCounts,
//     CountReadyWork) probes first and sweeps only when the probe says due.
//   - wakeExpiredDefersForClaim (readyClaimer.ClaimNext, and the decorators'
//     WakeExpiredDefersAdvisory, whose one caller is the externaldeps ready
//     claimer) sweeps as before the probe: the journal writer lock, the
//     transaction, the recheck.
//
// Advisory by contract: a ready listing or a claim must never fail because
// the sweep could not run, so every error is swallowed here — silently for
// the expected shapes (read-only store, open write circuit, closed store),
// with a stderr warning otherwise. It runs OUTSIDE the read tx because
// withReadTx unconditionally rolls back (and may retry its body on the
// read-only justification).
//
// A classified-read open (GH#804: bd ready/bd list are read-only for the
// CURRENT project) still runs the sweep — the store is genuinely writable
// underneath, and only the command's own semantics forbid a mutating result.
// A strict --readonly, preview, or foreign-project open must never sweep,
// which is why deferWakeSweepEligible checks classifiedRead rather than the
// guard dropping the readOnly check entirely (be-vbhpf).
func (s *DoltStore) wakeExpiredDefersForRead(ctx context.Context) {
	if !deferWakeSweepEligible(s.readOnly, s.classifiedRead) {
		return
	}
	// Probe first: when nothing is due the sweep would take the journal
	// writer lock (and queue behind every bead writer on the database) and
	// a write transaction only to find no rows (bd-ypak8, bd-aw3l8).
	//
	// Contract (review bd-w6ppm, finding 1): a ready read answers for the
	// instant its probe reads, and never waits for the journal writer lock
	// to learn that nothing is due. A defer whose date passes after that
	// instant is woken by the next ready read, not this one, even when a
	// writer holds the lock past the date: the sweep used to wait for the
	// lock, so its answer was for the instant the lock came free. With the
	// lock free the two answers are the same, since the sweep then ran at
	// once and found nothing either. A due row, a probe error and a session
	// with autocommit off all report due, and the sweep (lock, transaction,
	// recheck) decides as before (TestWakeDefersProbeAnswersForItsInstant).
	//
	// The early answer is for reads only (review bd-ee2ci, finding 1). A
	// claim selects and writes after the sweep under the same journal writer
	// lock, so it waits for that lock whatever the probe says: an answer
	// from before the wait would miss an issue the lock holder's commit made
	// due, and the claim would pass over it (claimed nothing, the issue left
	// deferred) where HEAD woke and claimed it. The probe saves a claim no
	// wait, so a claim takes wakeExpiredDefersForClaim.
	if !s.expiredDefersDue(ctx) {
		return
	}
	s.sweepExpiredDefers(ctx)
}

// wakeExpiredDefersForClaim is the sweep without the probe, for a caller that
// selects and writes in one operation (see wakeExpiredDefersForRead).
func (s *DoltStore) wakeExpiredDefersForClaim(ctx context.Context) {
	if !deferWakeSweepEligible(s.readOnly, s.classifiedRead) {
		return
	}
	s.sweepExpiredDefers(ctx)
}

func (s *DoltStore) sweepExpiredDefers(ctx context.Context) {
	err := s.withCircuitWrite(ctx, func(ctx context.Context) error {
		return s.runIssueOperationTxWithMessage(ctx, func(tx *sql.Tx) (issueops.ChangedTables, string, error) {
			woke, err := issueops.WakeExpiredDefersInTx(ctx, tx)
			if err != nil {
				return nil, "", err
			}
			if len(woke.Issues) == 0 {
				// Wisp-only wakes persist with the SQL commit but mint no
				// version commit: wisp tables are dolt_ignored.
				return nil, "", nil
			}
			tables := issueops.ChangedTables{}
			tables.Add("issues", "events")
			return tables, issueops.WakeDefersCommitMessage(len(woke.Issues)), nil
		})
	})
	if err != nil && !errors.Is(err, ErrCircuitOpen) && !errors.Is(err, ErrStoreClosed) {
		warnDeferWakeSweepSkipped(err)
	}
}

// expiredDefersDue is issueops.ExpiredDefersDue on one connection pinned from
// the store's pool, under the read lock every other read takes (Close swaps
// s.db for nil under the write lock). Pinned, so that on a session with
// autocommit off the probe ends the implicit transaction its SELECT opened
// on the same session before the connection goes back to the pool (review
// bd-ee2ci, finding 2); with autocommit on the pin sends nothing. A pool
// that cannot give a connection reports due, so the sweep decides. A closed
// store reports false: the sweep would end at ErrStoreClosed, which
// sweepExpiredDefers drops silently anyway.
func (s *DoltStore) expiredDefersDue(ctx context.Context) bool {
	if s.closed.Load() {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return false
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return true
	}
	defer func() { _ = conn.Close() }()
	return issueops.ExpiredDefersDue(ctx, conn)
}

// deferWakeAccessDeniedOnce rate-limits the access-denied advisory to one
// warning per process: a read-only-privileged SQL user hits it on every
// ready-front read, and repeating a configuration fact on each `bd ready`
// is noise, not signal.
var deferWakeAccessDeniedOnce sync.Once

func warnDeferWakeSweepSkipped(err error) {
	if dberrors.IsAccessDenied(err) {
		deferWakeAccessDeniedOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "warning: defer-wake sweep skipped (SQL user lacks write privileges; expired defers will not auto-wake from this client): %v\n", err)
		})
		return
	}
	fmt.Fprintf(os.Stderr, "warning: defer-wake sweep skipped: %v\n", err)
}

func (s *DoltStore) GetReadyWork(ctx context.Context, filter types.WorkFilter) ([]*types.Issue, error) {
	s.wakeExpiredDefersForRead(ctx)
	var result []*types.Issue
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetReadyWorkInTx(ctx, tx, filter)
		return err
	})
	return result, err
}

func (s *DoltStore) GetReadyWorkWithCounts(ctx context.Context, filter types.WorkFilter) ([]*types.IssueWithCounts, error) {
	s.wakeExpiredDefersForRead(ctx)
	var result []*types.IssueWithCounts
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetReadyWorkWithCountsInTx(ctx, tx, filter)
		return err
	})
	return result, err
}

// CountReadyWork returns the total ready-work count for filter. It is identical
// to len(GetReadyWorkWithCounts(filter with Limit=0)) but sizes the total with
// cheap indexed COUNT(*)s instead of re-running the counts mega-query. Backs the
// storage.ReadyWorkCounter capability.
func (s *DoltStore) CountReadyWork(ctx context.Context, filter types.WorkFilter) (int, error) {
	s.wakeExpiredDefersForRead(ctx)
	var n int
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		n, err = issueops.CountReadyWorkInTx(ctx, tx, filter)
		return err
	})
	return n, err
}

func (s *DoltStore) GetBlockedIssues(ctx context.Context, filter types.WorkFilter) ([]*types.BlockedIssue, error) {
	var result []*types.BlockedIssue
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetBlockedIssuesInTx(ctx, tx, filter)
		return err
	})
	return result, err
}

// GetEpicsEligibleForClosure returns epics whose children are all closed
func (s *DoltStore) GetEpicsEligibleForClosure(ctx context.Context) ([]*types.EpicStatus, error) {
	var result []*types.EpicStatus
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetEpicsEligibleForClosureInTx(ctx, tx)
		return err
	})
	return result, err
}

// GetStaleIssues returns issues that haven't been updated recently
func (s *DoltStore) GetStaleIssues(ctx context.Context, filter types.StaleFilter) ([]*types.Issue, error) {
	var result []*types.Issue
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetStaleIssuesInTx(ctx, tx, filter)
		return err
	})
	return result, err
}

// GetStatistics returns summary statistics
func (s *DoltStore) GetStatistics(ctx context.Context) (*types.Statistics, error) {
	stats := &types.Statistics{}

	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		return issueops.ScanIssueCountsInTx(ctx, tx, stats)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get statistics: %w", err)
	}

	var blockedCount int
	if err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM issues
			WHERE is_blocked = 1 AND status <> 'closed' AND status <> 'pinned'
		`).Scan(&blockedCount)
	}); err != nil {
		return nil, fmt.Errorf("failed to count blocked issues: %w", err)
	}
	stats.BlockedIssues = &blockedCount

	ready := stats.OpenIssues - blockedCount
	if ready < 0 {
		ready = 0
	}
	stats.ReadyIssues = &ready

	return stats, nil
}

// GetStatisticsNoBlocked returns aggregate counts without the blocked-set traversal.
// BlockedIssues and ReadyIssues are nil in the result (readiness needs the blocked
// set). Use for bd stats --no-blocked fast path.
func (s *DoltStore) GetStatisticsNoBlocked(ctx context.Context) (*types.Statistics, error) {
	stats := &types.Statistics{}
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		return issueops.ScanIssueCountsInTx(ctx, tx, stats)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get statistics: %w", err)
	}
	// BlockedIssues stays nil; ReadyIssues not computable without blocked set.
	return stats, nil
}

// getChildrenOfDeferredParents returns IDs of issues whose parent has a future
// defer_until date. Uses separate single-table queries to avoid correlated
// cross-table JOIN subqueries that trigger Dolt joinIter hangs (GH#1190).
// Caller must hold s.mu (at least RLock).
func (s *DoltStore) getChildrenOfDeferredParents(ctx context.Context) ([]string, error) {
	// Step 1: Get IDs of issues with future defer_until
	deferredRows, err := s.queryContext(ctx, `
		SELECT id FROM issues
		WHERE defer_until IS NOT NULL AND defer_until > UTC_TIMESTAMP()
	`)
	if err != nil {
		return nil, wrapQueryError("deferred parents: get deferred issues", err)
	}
	var deferredIDs []string
	for deferredRows.Next() {
		var id string
		if err := deferredRows.Scan(&id); err != nil {
			_ = deferredRows.Close()
			return nil, wrapScanError("deferred parents: scan deferred issue", err)
		}
		deferredIDs = append(deferredIDs, id)
	}
	_ = deferredRows.Close()
	if err := deferredRows.Err(); err != nil {
		return nil, wrapQueryError("deferred parents: deferred rows", err)
	}
	if len(deferredIDs) == 0 {
		return nil, nil
	}

	// Step 2: Get children of those deferred parents
	return s.getChildrenOfIssues(ctx, deferredIDs)
}

// getChildrenOfIssues returns IDs of direct children (parent-child deps) of the given issue IDs.
func (s *DoltStore) getChildrenOfIssues(ctx context.Context, parentIDs []string) ([]string, error) {
	var result []string
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetChildrenOfIssuesInTx(ctx, tx, parentIDs)
		return err
	})
	return result, err
}

// getChildrenWithParents returns a map of childID -> parentID for direct children
// (parent-child deps) of the given parent IDs.
func (s *DoltStore) getChildrenWithParents(ctx context.Context, parentIDs []string) (map[string]string, error) {
	var result map[string]string
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetChildrenWithParentsInTx(ctx, tx, parentIDs)
		return err
	})
	return result, err
}

func (s *DoltStore) getDescendantIDs(ctx context.Context, rootID string) ([]string, error) {
	var result []string
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetDescendantIDsInTx(ctx, tx, rootID, 0)
		return err
	})
	return result, err
}

// GetMoleculeProgress returns progress stats for a molecule
func (s *DoltStore) GetMoleculeProgress(ctx context.Context, moleculeID string) (*types.MoleculeProgressStats, error) {
	stats := &types.MoleculeProgressStats{
		MoleculeID: moleculeID,
	}

	// Route to correct table based on whether molecule is a wisp (bd-w2w)
	issueTable := "issues"
	depTable := "dependencies"
	parentCol := "depends_on_issue_id"
	if s.isActiveWisp(ctx, moleculeID) {
		issueTable = "wisps"
		depTable = "wisp_dependencies"
		parentCol = "depends_on_wisp_id"
	}

	// Get molecule title
	var title sql.NullString
	//nolint:gosec // G201: issueTable is hardcoded to "issues" or "wisps"
	err := s.db.QueryRowContext(ctx, fmt.Sprintf("SELECT title FROM %s WHERE id = ?", issueTable), moleculeID).Scan(&title)
	if err == nil && title.Valid {
		stats.MoleculeTitle = title.String
	}

	// Step 1: Get child issue IDs from dependencies table (single-table scan)
	//nolint:gosec // G201: depTable and parentCol are hardcoded
	depRows, err := s.queryContext(ctx, fmt.Sprintf(`
		SELECT issue_id FROM %s
		WHERE %s = ? AND type = 'parent-child'
	`, depTable, parentCol), moleculeID)
	if err != nil {
		return nil, fmt.Errorf("failed to get molecule children: %w", err)
	}
	var childIDs []string
	for depRows.Next() {
		var id string
		if err := depRows.Scan(&id); err != nil {
			_ = depRows.Close() // Best effort cleanup on error path
			return nil, wrapScanError("get molecule progress: scan child", err)
		}
		childIDs = append(childIDs, id)
	}
	_ = depRows.Close() // Redundant close for safety (rows already iterated)

	// Step 2: Batch-fetch status for all children (batched IN clauses to avoid full table scans).
	// Children of a wisp molecule are also wisps, so use the same table.
	if len(childIDs) > 0 {
		type childInfo struct {
			status string
		}
		childMap := make(map[string]childInfo)
		for start := 0; start < len(childIDs); start += queryBatchSize {
			end := start + queryBatchSize
			if end > len(childIDs) {
				end = len(childIDs)
			}
			batch := childIDs[start:end]
			placeholders, args := doltBuildSQLInClause(batch)
			// nolint:gosec // G201: issueTable is hardcoded, placeholders contains only ? markers
			query := fmt.Sprintf("SELECT id, status FROM %s WHERE id IN (%s)", issueTable, placeholders)
			statusRows, err := s.queryContext(ctx, query, args...)
			if err != nil {
				return nil, fmt.Errorf("failed to batch-fetch child statuses: %w", err)
			}
			for statusRows.Next() {
				var id, status string
				if err := statusRows.Scan(&id, &status); err != nil {
					_ = statusRows.Close()
					return nil, wrapScanError("get molecule progress: scan status", err)
				}
				childMap[id] = childInfo{status: status}
			}
			_ = statusRows.Close()
		}

		for _, childID := range childIDs {
			info, ok := childMap[childID]
			if !ok {
				continue
			}
			stats.Total++
			switch types.Status(info.status) {
			case types.StatusClosed:
				stats.Completed++
			case types.StatusInProgress:
				stats.InProgress++
				if stats.CurrentStepID == "" {
					stats.CurrentStepID = childID
				}
			}
		}
	}

	return stats, nil
}

// GetMoleculeLastActivity returns the most recent activity timestamp for a molecule.
func (s *DoltStore) GetMoleculeLastActivity(ctx context.Context, moleculeID string) (*types.MoleculeLastActivity, error) {
	var result *types.MoleculeLastActivity
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetMoleculeLastActivityInTx(ctx, tx, moleculeID)
		return err
	})
	return result, err
}

// GetNextChildID returns the next available child ID for a parent.
// Delegates SQL work to issueops.GetNextChildIDTx. The allocation writes only
// the child counter table and records no journal event, so it takes no journal
// writer lock.
func (s *DoltStore) GetNextChildID(ctx context.Context, parentID string) (string, error) {
	var childID string
	err := s.withRetryTxNoJournal(ctx, func(tx *sql.Tx) error {
		var err error
		childID, err = issueops.GetNextChildIDTx(ctx, tx, parentID)
		return err
	})
	return childID, err
}
