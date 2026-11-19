package uow

import (
	"database/sql"
	"fmt"
	"os"
	"sync"

	"context"

	"github.com/steveyegge/beads/internal/storage/dberrors"
	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
)

// WakeExpiredDefers runs the lazy defer-wake sweep in its own unit of work,
// committing with a wake message iff any permanent issue woke — the same
// commit-iff-changed contract every RunTxResult writer keeps, so the steady
// state (nothing expired) costs one UPDATE on a transaction that never calls
// DOLT_COMMIT.
//
// It runs in a transaction of its own, not the caller's, because every
// ready-work READ in this stack runs under RunTxRead or a caller-owned UOW
// that rolls back — a sweep inside those spans would be silently discarded.
// Sequencing is what matters: sweep first, then read, and the read sees the
// woken rows.
//
// A WISP-only wake takes a third path: wisp tables are dolt_ignored, so the
// wake-message form (DOLT_COMMIT) would find nothing to commit and the
// transaction would roll back, silently discarding the wisp wakes — every
// subsequent ready read would then redo and re-discard the same writes
// forever. Those wakes persist via uw.Commit(ctx, "") — the ephemeral
// plain-COMMIT form RunTxEphemeral keeps for dolt_ignored state (bd-lrgn1) —
// issued inside the work func; a serialization failure from it surfaces as
// the closure's error and retries against a fresh unit of work, exactly like
// a commit issued by RunTxResult itself.
func WakeExpiredDefers(ctx context.Context, p UnitOfWorkProvider) (int, error) {
	// Probe first, outside any unit of work, where the provider can run a
	// bare read: when nothing is due the sweep's lock and write transaction
	// would find no rows (see storageissueops.ExpiredDefersDue). The probe
	// answers for the instant it reads, without waiting for the journal
	// writer lock, and reports due on a session with autocommit off; the
	// contract is the store's, at its call site in
	// dolt.wakeExpiredDefersForRead. RunNonTx's connection is pinned, so on
	// a session with autocommit off the probe ends the implicit transaction
	// it opened there before the connection goes back (review bd-ee2ci,
	// finding 2). For reads only: a claim takes sweepExpiredDefers.
	if mp, ok := p.(MaintenanceProvider); ok {
		due := true
		err := mp.RunNonTx(ctx, func(ctx context.Context, conn *sql.Conn) error {
			due = storageissueops.ExpiredDefersDue(ctx, conn)
			return nil
		})
		if err == nil && !due {
			return 0, nil
		}
	}
	return sweepExpiredDefers(ctx, p)
}

// sweepExpiredDefers is the sweep without the probe: the unit of work
// WakeExpiredDefers runs when the probe says due, and all a claim runs.
func sweepExpiredDefers(ctx context.Context, p UnitOfWorkProvider) (int, error) {
	return RunTxResult(ctx, p, func(ctx context.Context, uw UnitOfWork) (int, string, error) {
		issues, wisps, err := uw.IssueUseCase().WakeExpiredDefers(ctx)
		if err != nil {
			return 0, "", err
		}
		if issues > 0 {
			return issues, storageissueops.WakeDefersCommitMessage(issues), nil
		}
		if wisps > 0 {
			if err := uw.Commit(ctx, ""); err != nil {
				return 0, "", err
			}
		}
		return 0, "", nil
	})
}

// advisoryAccessDeniedOnce rate-limits the access-denied advisory to one
// warning per process: a read-only-privileged SQL user hits it on every
// ready-front read, and repeating a configuration fact on each `bd ready`
// is noise, not signal.
var advisoryAccessDeniedOnce sync.Once

// WakeExpiredDefersAdvisory is WakeExpiredDefers, the probing entry point,
// under the read paths' contract: a ready listing must never fail because
// the sweep could not run, so errors are reduced to a stderr warning
// (warn-once for access-denied). For ready READS only (the issue reader's
// Ready and --ready List, and bd ready on the proxied route); a claim calls
// WakeExpiredDefersForClaimAdvisory.
func WakeExpiredDefersAdvisory(ctx context.Context, p UnitOfWorkProvider) {
	_, err := WakeExpiredDefers(ctx, p)
	warnWakeExpiredDefers(err)
}

// WakeExpiredDefersForClaimAdvisory is the claim's sweep, under the same
// advisory contract: no probe, the journal writer lock, the transaction and
// the recheck, as before the probe. A claim selects and writes after it and
// waits for that lock anyway, so an early "nothing due" would save it no wait
// and would miss an issue the lock holder's commit made due while it waited
// (review bd-ee2ci, finding 1).
func WakeExpiredDefersForClaimAdvisory(ctx context.Context, p UnitOfWorkProvider) {
	_, err := sweepExpiredDefers(ctx, p)
	warnWakeExpiredDefers(err)
}

func warnWakeExpiredDefers(err error) {
	if err == nil {
		return
	}
	if dberrors.IsAccessDenied(err) {
		advisoryAccessDeniedOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "warning: defer-wake sweep skipped (SQL user lacks write privileges; expired defers will not auto-wake from this client): %v\n", err)
		})
		return
	}
	fmt.Fprintf(os.Stderr, "warning: defer-wake sweep skipped: %v\n", err)
}
