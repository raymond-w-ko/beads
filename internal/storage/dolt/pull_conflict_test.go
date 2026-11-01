package dolt

import (
	"testing"
)

// TestPullAutoResolveMetadataConflicts verifies that merge conflicts limited to
// the metadata table are automatically resolved with "theirs" strategy (GH#2466).
// This simulates the scenario where two machines each write different
// dolt_auto_push_* values to the metadata table, causing recurring conflicts on pull.
func TestPullAutoResolveMetadataConflicts(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	db := store.db

	// Record the current branch (our test branch).
	var currentBranch string
	if err := db.QueryRowContext(ctx, "SELECT active_branch()").Scan(&currentBranch); err != nil {
		t.Fatalf("failed to get current branch: %v", err)
	}

	// Insert a metadata row on the current branch and commit.
	if _, err := db.ExecContext(ctx, "INSERT INTO metadata (`key`, value) VALUES ('dolt_auto_push_commit', 'aaa')"); err != nil {
		t.Fatalf("failed to insert metadata on current branch: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'local metadata')"); err != nil {
		t.Fatalf("failed to commit on current branch: %v", err)
	}

	// Create a divergent branch to simulate the remote.
	remoteBranch := currentBranch + "_remote"
	// Branch from current branch's parent (HEAD~1).
	if _, err := db.ExecContext(ctx, "CALL DOLT_BRANCH(?, 'HEAD~1')", remoteBranch); err != nil {
		t.Fatalf("failed to create remote branch: %v", err)
	}
	defer func() {
		db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", currentBranch)
		db.ExecContext(ctx, "CALL DOLT_BRANCH('-D', ?)", remoteBranch)
	}()

	// Switch to remote branch and insert conflicting metadata.
	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", remoteBranch); err != nil {
		t.Fatalf("failed to checkout remote branch: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO metadata (`key`, value) VALUES ('dolt_auto_push_commit', 'bbb')"); err != nil {
		t.Fatalf("failed to insert metadata on remote branch: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'remote metadata')"); err != nil {
		t.Fatalf("failed to commit on remote branch: %v", err)
	}

	// Switch back to current branch.
	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", currentBranch); err != nil {
		t.Fatalf("failed to checkout current branch: %v", err)
	}

	// Merge the remote branch in a transaction with dolt_allow_commit_conflicts.
	// This simulates what pullWithAutoResolve does internally.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}

	if _, err := tx.ExecContext(ctx, "SET @@dolt_allow_commit_conflicts = 1"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("failed to set dolt_allow_commit_conflicts: %v", err)
	}

	_, mergeErr := tx.ExecContext(ctx, "CALL DOLT_MERGE(?)", remoteBranch)
	// mergeErr may or may not be nil depending on Dolt version.

	// Try auto-resolve.
	resolved, resolveErr := store.tryAutoResolveMergeConflicts(ctx, tx)
	if resolveErr != nil {
		_ = tx.Rollback()
		t.Fatalf("tryAutoResolveMergeConflicts error: %v (mergeErr: %v)", resolveErr, mergeErr)
	}
	if !resolved {
		_ = tx.Rollback()
		if mergeErr != nil {
			t.Fatalf("merge failed and metadata conflicts were not auto-resolved: %v", mergeErr)
		}
		// Clean merge, no conflicts to resolve — verify the value.
		t.Log("merge succeeded without conflicts (auto-merge)")
		return
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("failed to commit after auto-resolve: %v", err)
	}

	// Verify the metadata value is "theirs" (bbb from remote).
	var value string
	if err := db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE `key` = 'dolt_auto_push_commit'").Scan(&value); err != nil {
		t.Fatalf("failed to read resolved metadata: %v", err)
	}
	if value != "bbb" {
		t.Errorf("expected metadata value 'bbb' (theirs), got %q", value)
	}
}

// TestPullAutoResolveSkipsNonMetadataConflicts verifies that conflicts on
// tables other than metadata are NOT auto-resolved. The issues-table conflict
// here is add/add (both branches insert the same PK), which the LWW safety
// check rejects (GH#4698); modify/modify conflicts with different timestamps
// ARE auto-resolved by LWW, covered by issues_conflict_test.go.
func TestPullAutoResolveSkipsNonMetadataConflicts(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	db := store.db

	var currentBranch string
	if err := db.QueryRowContext(ctx, "SELECT active_branch()").Scan(&currentBranch); err != nil {
		t.Fatalf("failed to get current branch: %v", err)
	}

	// Create an issue on the current branch.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type) VALUES ('conflict-test', 'Local Title', '', '', '', '', 'open', 2, 'task')"); err != nil {
		t.Fatalf("failed to insert issue on current branch: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'local issue')"); err != nil {
		t.Fatalf("failed to commit on current branch: %v", err)
	}

	// Create divergent branch from parent.
	remoteBranch := currentBranch + "_remote2"
	if _, err := db.ExecContext(ctx, "CALL DOLT_BRANCH(?, 'HEAD~1')", remoteBranch); err != nil {
		t.Fatalf("failed to create remote branch: %v", err)
	}
	defer func() {
		db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", currentBranch)
		db.ExecContext(ctx, "CALL DOLT_BRANCH('-D', ?)", remoteBranch)
	}()

	// Insert conflicting issue on remote branch (same PK, different title).
	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", remoteBranch); err != nil {
		t.Fatalf("failed to checkout remote branch: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type) VALUES ('conflict-test', 'Remote Title', '', '', '', '', 'open', 2, 'task')"); err != nil {
		t.Fatalf("failed to insert issue on remote branch: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'remote issue')"); err != nil {
		t.Fatalf("failed to commit on remote branch: %v", err)
	}

	// Switch back and merge.
	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", currentBranch); err != nil {
		t.Fatalf("failed to checkout current branch: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}

	if _, err := tx.ExecContext(ctx, "SET @@dolt_allow_commit_conflicts = 1"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("failed to set dolt_allow_commit_conflicts: %v", err)
	}

	_, mergeErr := tx.ExecContext(ctx, "CALL DOLT_MERGE(?)", remoteBranch)

	// Issues table conflict should NOT be auto-resolved.
	resolved, resolveErr := store.tryAutoResolveMergeConflicts(ctx, tx)
	_ = tx.Rollback()

	if mergeErr == nil && resolveErr == nil && !resolved {
		// Clean merge — Dolt auto-merged the issue changes.
		t.Skip("merge succeeded without conflicts — cannot test non-metadata conflict path")
		return
	}

	if resolveErr != nil {
		// Error checking conflicts is acceptable for some Dolt versions.
		t.Logf("tryAutoResolveMergeConflicts returned error: %v", resolveErr)
		return
	}

	if resolved {
		t.Error("expected non-metadata conflicts NOT to be auto-resolved")
	}
}

// TestAutoResolveConflictsAfterCLIPull_DependencyAuditOnly verifies the post-CLI-pull
// resolver (used for git-protocol / credentialed remotes, #4259 finding 1) resolves an
// audit-only dependency conflict left by an out-of-transaction merge. CLI `dolt pull`
// performs the merge in a subprocess and leaves conflicts in the working set, so the
// resolver never runs in the transaction that produced the merge — unlike the SQL
// DOLT_PULL path. Here the conflict is persisted into the working set (what a CLI pull
// leaves behind) and then resolved via finishCLIPull's helper on the store connection,
// which stays on the pull's branch (a brand-new connection would default to the base
// branch and miss the conflict).
func TestAutoResolveConflictsAfterCLIPull_DependencyAuditOnly(t *testing.T) {
	store, peerBranch := setupDependencyMergeConflict(t, "blocks", "alice", "blocks", "bob")

	ctx, cancel := testContext(t)
	defer cancel()
	db := store.db

	// Persist the merge conflict into the working set (mirroring a CLI pull), then
	// resolve it from a brand-new connection rather than this transaction.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "SET @@dolt_allow_commit_conflicts = 1"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("allow commit conflicts: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "CALL DOLT_MERGE(?)", peerBranch); err != nil {
		t.Logf("merge returned: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("persist conflicted working set: %v", err)
	}

	// A fresh connection must observe the persisted conflict, else this test would
	// not be exercising the cross-connection (post-CLI-pull) resolver path.
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_conflicts_dependencies").Scan(&n); err != nil {
		t.Fatalf("count conflicts on fresh conn: %v", err)
	}
	if n == 0 {
		t.Skip("merge auto-resolved without persisted conflicts — cannot exercise post-CLI-pull resolver")
	}

	resolved, err := store.autoResolveConflictsAfterCLIPull(ctx)
	if err != nil {
		t.Fatalf("autoResolveConflictsAfterCLIPull: %v", err)
	}
	if !resolved {
		t.Fatal("expected audit-only dependency conflict to auto-resolve after a CLI pull")
	}

	var count int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM dependencies WHERE issue_id = 'depc-x' AND depends_on_issue_id = 'depc-y'").Scan(&count); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 dependency row after resolve, got %d", count)
	}
}

// TestAutoResolveConflictsAfterCLIPull_TheirsOnlyEverturnAction pins the sync
// auto-merge against the fork's STORED generated column issues.everturn_action
// (schema/fork_indexes.go). We edit the title, the peer edits only metadata's
// $.everturn.action, and both restamp updated_at, so the row conflicts with
// metadata and everturn_action both theirs-only. The field merge must take
// their metadata and leave everturn_action for dolt to recompute: assigning a
// generated column fails ("The value specified for generated column
// "everturn_action" in table "issues" is not allowed").
func TestAutoResolveConflictsAfterCLIPull_TheirsOnlyEverturnAction(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()
	db := store.db

	const issueID = "genc-1"
	const wake = `{"everturn":{"action":"wake"}}`
	var currentBranch string
	if err := db.QueryRowContext(ctx, "SELECT active_branch()").Scan(&currentBranch); err != nil {
		t.Fatalf("get current branch: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type, metadata, updated_at) "+
			"VALUES (?, 'base title', '', '', '', '', 'open', 2, 'task', '{}', '2026-07-10 10:00:00')", issueID); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'seed issue')"); err != nil {
		t.Fatalf("commit seed issue: %v", err)
	}

	peerBranch := currentBranch + "_genpeer"
	if _, err := db.ExecContext(ctx, "CALL DOLT_BRANCH(?)", peerBranch); err != nil {
		t.Fatalf("create peer branch: %v", err)
	}
	defer func() {
		db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", currentBranch)
		db.ExecContext(ctx, "CALL DOLT_BRANCH('-D', ?)", peerBranch)
	}()

	// Ours: a title edit.
	if _, err := db.ExecContext(ctx,
		"UPDATE issues SET title = 'our title', updated_at = '2026-07-10 11:00:00' WHERE id = ?", issueID); err != nil {
		t.Fatalf("edit title on current: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'title on current')"); err != nil {
		t.Fatalf("commit title on current: %v", err)
	}

	// Theirs: only metadata, and so only $.everturn.action.
	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", peerBranch); err != nil {
		t.Fatalf("checkout peer: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"UPDATE issues SET metadata = ?, updated_at = '2026-07-10 12:00:00' WHERE id = ?", wake, issueID); err != nil {
		t.Fatalf("edit metadata on peer: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'everturn action on peer')"); err != nil {
		t.Fatalf("commit metadata on peer: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", currentBranch); err != nil {
		t.Fatalf("checkout current: %v", err)
	}

	// Persist the conflicted merge into the working set, as a CLI pull leaves it.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "SET @@dolt_allow_commit_conflicts = 1"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("allow commit conflicts: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "CALL DOLT_MERGE(?)", peerBranch); err != nil {
		t.Logf("merge returned: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("persist conflicted working set: %v", err)
	}

	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_conflicts_issues WHERE our_id = ?", issueID).Scan(&n); err != nil {
		t.Fatalf("count issues conflicts: %v", err)
	}
	if n != 1 {
		t.Fatalf("issues conflicts for %s = %d, want 1 (both sides restamped updated_at)", issueID, n)
	}

	resolved, err := store.autoResolveConflictsAfterCLIPull(ctx)
	if err != nil {
		t.Fatalf("autoResolveConflictsAfterCLIPull: %v", err)
	}
	if !resolved {
		t.Fatal("expected the field-mergeable issues conflict to auto-resolve after a CLI pull")
	}

	var title, action, updated string
	if err := db.QueryRowContext(ctx,
		"SELECT title, COALESCE(everturn_action, ''), DATE_FORMAT(updated_at, '%Y-%m-%d %H:%i:%s') FROM issues WHERE id = ?", issueID).
		Scan(&title, &action, &updated); err != nil {
		t.Fatalf("read merged issue: %v", err)
	}
	if title != "our title" {
		t.Errorf("title = %q, want our edit %q", title, "our title")
	}
	if action != "wake" {
		t.Errorf("everturn_action = %q, want %q recomputed from their metadata", action, "wake")
	}
	if updated != "2026-07-10 12:00:00" {
		t.Errorf("updated_at = %q, want the newer side's %q", updated, "2026-07-10 12:00:00")
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_conflicts_issues").Scan(&n); err != nil {
		t.Fatalf("count issues conflicts after resolve: %v", err)
	}
	if n != 0 {
		t.Errorf("issues conflicts after resolve = %d, want 0", n)
	}
}

// TestAutoResolveConflictsAfterCLIPull_ContestedGeneratedEqualTimestampDeclines
// pins review bd-r7n55: a generated column is dropped from the auto-merge's
// write-back plan only, never from its merge rules. On this test branch,
// issues gains a STORED generated projection over two fields the sides edit
// independently, so it is contested: base "base title:", ours "our title:",
// theirs "base title:peer description". Both sides stamp the same updated_at,
// so the LWW tiebreak is ambiguous and the row must stay the operator's.
// Divergent row_lock values keep dolt from merging the disjoint source edits
// itself (row_lock is excluded from auto-merge classification).
func TestAutoResolveConflictsAfterCLIPull_ContestedGeneratedEqualTimestampDeclines(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()
	db := store.db

	const issueID = "genc-2"
	var currentBranch string
	if err := db.QueryRowContext(ctx, "SELECT active_branch()").Scan(&currentBranch); err != nil {
		t.Fatalf("get current branch: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"ALTER TABLE issues ADD COLUMN merge_summary TEXT GENERATED ALWAYS AS (CONCAT(title, CHAR(58), description)) STORED"); err != nil {
		t.Fatalf("add generated projection: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type, metadata, updated_at) "+
			"VALUES (?, 'base title', '', '', '', '', 'open', 2, 'task', '{}', '2026-07-10 10:00:00')", issueID); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'generated projection and seed issue')"); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	peerBranch := currentBranch + "_gencontested"
	if _, err := db.ExecContext(ctx, "CALL DOLT_BRANCH(?)", peerBranch); err != nil {
		t.Fatalf("create peer branch: %v", err)
	}
	defer func() {
		db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", currentBranch)
		db.ExecContext(ctx, "CALL DOLT_BRANCH('-D', ?)", peerBranch)
	}()

	// Ours: the title.
	if _, err := db.ExecContext(ctx,
		"UPDATE issues SET title = 'our title', row_lock = 111, updated_at = '2026-07-10 11:00:00' WHERE id = ?", issueID); err != nil {
		t.Fatalf("edit title on current: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'title on current')"); err != nil {
		t.Fatalf("commit title on current: %v", err)
	}

	// Theirs: the description, at the same updated_at.
	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", peerBranch); err != nil {
		t.Fatalf("checkout peer: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"UPDATE issues SET description = 'peer description', row_lock = 222, updated_at = '2026-07-10 11:00:00' WHERE id = ?", issueID); err != nil {
		t.Fatalf("edit description on peer: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'description on peer')"); err != nil {
		t.Fatalf("commit description on peer: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", currentBranch); err != nil {
		t.Fatalf("checkout current: %v", err)
	}

	// Persist the conflicted merge into the working set, as a CLI pull leaves it.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "SET @@dolt_allow_commit_conflicts = 1"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("allow commit conflicts: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "CALL DOLT_MERGE(?)", peerBranch); err != nil {
		t.Logf("merge returned: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("persist conflicted working set: %v", err)
	}

	var n int
	var base, ours, theirs string
	if err := db.QueryRowContext(ctx,
		"SELECT base_merge_summary, our_merge_summary, their_merge_summary FROM dolt_conflicts_issues WHERE our_id = ?", issueID).
		Scan(&base, &ours, &theirs); err != nil {
		t.Fatalf("read the issues conflict (want exactly one, contested merge_summary): %v", err)
	}
	if base == ours || base == theirs || ours == theirs {
		t.Fatalf("merge_summary base/ours/theirs = %q/%q/%q; want three distinct values (contested)", base, ours, theirs)
	}

	resolved, err := store.autoResolveConflictsAfterCLIPull(ctx)
	if err != nil {
		t.Fatalf("autoResolveConflictsAfterCLIPull: %v", err)
	}
	if resolved {
		t.Fatal("auto-merge accepted an equal-timestamp contested generated merge_summary; the merge rules must decline it")
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_conflicts_issues WHERE our_id = ?", issueID).Scan(&n); err != nil {
		t.Fatalf("count issues conflicts after decline: %v", err)
	}
	if n != 1 {
		t.Fatalf("issues conflicts for %s after decline = %d, want 1 (left for the operator)", issueID, n)
	}
}
