package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/metrics"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/ui"
	"github.com/steveyegge/beads/internal/utils"
)

var duplicateCmd = &cobra.Command{
	Use:     "duplicate <id> --of <canonical>",
	GroupID: "deps",
	Short:   "Mark an issue as a duplicate of another",
	Long: `Mark an issue as a duplicate of a canonical issue.

The duplicate issue is automatically closed with a reference to the canonical.
This is essential for large issue databases with many similar reports.

Examples:
  bd duplicate bd-abc --of bd-xyz    # Mark bd-abc as duplicate of bd-xyz`,
	Args: cobra.ExactArgs(1),
	RunE: runDuplicate,
}

var supersedeCmd = &cobra.Command{
	Use:     "supersede <id> --with <new>",
	GroupID: "deps",
	Short:   "Mark an issue as superseded by a replacement issue",
	Long: `Mark an issue as superseded by a different (replacement) issue.

The superseded issue is automatically closed with a reference to the replacement.
Useful for design docs, specs, and evolving artifacts.

Issues blocked by the superseded issue now block on the replacement: each
inbound blocks edge moves to the replacement, so they wait for it instead.
Use --keep-blockers to leave those edges on the superseded issue.
A blocker that already has an edge of another non-hierarchy type to the
replacement keeps its edge on the superseded issue and is listed in
kept_blockers, as is the replacement's own edge.

Examples:
  bd supersede bd-old --with bd-new    # Mark bd-old as superseded by bd-new`,
	Args: cobra.ExactArgs(1),
	RunE: runSupersede,
}

var (
	duplicateOf    string
	supersededWith string
	keepBlockers   bool
)

// supersedeBlockerPageSize bounds each page of inbound blocks edges read
// while moving them to the replacement.
const supersedeBlockerPageSize = 500

func init() {
	duplicateCmd.Flags().StringVar(&duplicateOf, "of", "", "Canonical issue ID (required)")
	_ = duplicateCmd.MarkFlagRequired("of") // Only fails if flag missing (caught in tests)
	duplicateCmd.ValidArgsFunction = issueIDCompletion
	rootCmd.AddCommand(duplicateCmd)

	supersedeCmd.Flags().StringVar(&supersededWith, "with", "", "Replacement issue ID (required)")
	_ = supersedeCmd.MarkFlagRequired("with") // Only fails if flag missing (caught in tests)
	supersedeCmd.Flags().BoolVar(&keepBlockers, "keep-blockers", false, "Leave issues blocked by the superseded issue blocked on it instead of the replacement")
	supersedeCmd.ValidArgsFunction = issueIDCompletion
	rootCmd.AddCommand(supersedeCmd)
}

func runDuplicate(cmd *cobra.Command, args []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("duplicate is not supported in proxied-server mode")
	}
	CheckReadonly("duplicate")

	evt := metrics.NewCommandEvent("duplicate")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	ctx := getRootContext()
	store := getStore()
	actor := getActor()

	// Resolve partial IDs
	var duplicateID, canonicalID string
	var err error
	duplicateID, err = utils.ResolvePartialID(ctx, store, args[0])
	if err != nil {
		return fmt.Errorf("failed to resolve %s: %w", args[0], err)
	}
	canonicalID, err = utils.ResolvePartialID(ctx, store, duplicateOf)
	if err != nil {
		return fmt.Errorf("failed to resolve %s: %w", duplicateOf, err)
	}

	if duplicateID == canonicalID {
		return fmt.Errorf("cannot mark an issue as duplicate of itself")
	}

	// Verify canonical issue exists
	var canonical *types.Issue
	canonical, err = store.GetIssue(ctx, canonicalID)
	if err != nil || canonical == nil {
		return fmt.Errorf("canonical issue not found: %s", canonicalID)
	}

	// Add a "duplicates" dependency edge (duplicate → canonical)
	dep := &types.Dependency{
		IssueID:     duplicateID,
		DependsOnID: canonicalID,
		Type:        types.DepDuplicates,
	}
	if err := store.AddDependency(ctx, dep, actor); err != nil {
		return fmt.Errorf("failed to add duplicate link: %w", err)
	}

	// Close the duplicate issue through the lifecycle operation so it records
	// the complete closure state.
	if err := store.CloseIssue(ctx, duplicateID, "", actor, ""); err != nil {
		return fmt.Errorf("failed to close duplicate: %w", err)
	}

	commandDidWrite.Store(true)

	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"duplicate": duplicateID,
			"canonical": canonicalID,
			"status":    "closed",
		})
	}

	fmt.Printf("%s Marked %s as duplicate of %s (closed)\n", ui.RenderPass("✓"), duplicateID, canonicalID)
	return nil
}

func runSupersede(cmd *cobra.Command, args []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("supersede is not supported in proxied-server mode")
	}
	CheckReadonly("supersede")

	evt := metrics.NewCommandEvent("supersede")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	ctx := getRootContext()
	store := getStore()
	actor := getActor()

	// Resolve partial IDs
	var oldID, newID string
	var err error
	oldID, err = utils.ResolvePartialID(ctx, store, args[0])
	if err != nil {
		return fmt.Errorf("failed to resolve %s: %w", args[0], err)
	}
	newID, err = utils.ResolvePartialID(ctx, store, supersededWith)
	if err != nil {
		return fmt.Errorf("failed to resolve %s: %w", supersededWith, err)
	}

	if oldID == newID {
		return fmt.Errorf("cannot mark an issue as superseded by itself")
	}

	// Verify new issue exists
	var newIssue *types.Issue
	newIssue, err = store.GetIssue(ctx, newID)
	if err != nil || newIssue == nil {
		return fmt.Errorf("replacement issue not found: %s", newID)
	}

	// Move each inbound blocks edge of old to new, then add the "supersedes"
	// edge (old → new) and close old, all in one transaction so a failure
	// (e.g. a cycle through new) leaves old open with its edges untouched.
	moved := []string{}
	var kept []string
	commitMsg := fmt.Sprintf("bd supersede: %s -> %s", oldID, newID)
	err = store.RunInTransaction(ctx, commitMsg, func(tx storage.Transaction) error {
		// Read every page before writing, so removals cannot shift the keyset.
		var blockers []*types.Dependency
		afterID := ""
		for {
			page, err := tx.GetDependentRecords(ctx, oldID, string(types.DepBlocks), supersedeBlockerPageSize, afterID)
			if err != nil {
				return fmt.Errorf("failed to read issues blocked by %s: %w", oldID, err)
			}
			blockers = append(blockers, page...)
			if len(page) < supersedeBlockerPageSize {
				break
			}
			afterID = page[len(page)-1].ID
		}

		for _, row := range blockers {
			// Moving new's own edge would make new depend on itself.
			if keepBlockers || row.IssueID == newID {
				kept = append(kept, row.IssueID)
				continue
			}
			// An existing blocks edge to new already does the job; re-adding it
			// would overwrite its metadata.
			existing, err := tx.GetDependencyRecords(ctx, row.IssueID)
			if err != nil {
				return fmt.Errorf("failed to read dependencies of %s: %w", row.IssueID, err)
			}
			var existingType types.DependencyType
			for _, d := range existing {
				if d.DependsOnID == newID {
					existingType = d.Type
					break
				}
			}
			// A different edge type to new (relates-to, waits-for, ...) cannot
			// become blocks: leave the edge on old. Decide here rather than on
			// AddDependency's type conflict, which its cycle check would
			// preempt. Parent-child still goes to AddDependency, whose
			// hierarchy check refuses it like a cycle.
			if existingType != "" && existingType != types.DepBlocks && existingType != types.DepParentChild {
				kept = append(kept, row.IssueID)
				continue
			}
			if existingType != types.DepBlocks {
				err := tx.AddDependency(ctx, &types.Dependency{
					IssueID:     row.IssueID,
					DependsOnID: newID,
					Type:        types.DepBlocks,
					Metadata:    row.Metadata,
				}, actor)
				// Fallback for a conflicting edge the read above did not see.
				var conflict *domain.DependencyTypeConflictError
				if errors.As(err, &conflict) {
					kept = append(kept, row.IssueID)
					continue
				}
				if err != nil {
					return fmt.Errorf("failed to move blocks edge %s -> %s to %s: %w", row.IssueID, oldID, newID, err)
				}
			}
			if err := tx.RemoveDependency(ctx, row.IssueID, oldID, actor); err != nil {
				return fmt.Errorf("failed to remove blocks edge %s -> %s: %w", row.IssueID, oldID, err)
			}
			moved = append(moved, row.IssueID)
		}

		dep := &types.Dependency{
			IssueID:     oldID,
			DependsOnID: newID,
			Type:        types.DepSupersedes,
		}
		if err := tx.AddDependency(ctx, dep, actor); err != nil {
			return fmt.Errorf("failed to add supersede link: %w", err)
		}

		// Close the superseded issue through the lifecycle operation so it
		// records the complete closure state.
		if err := tx.CloseIssue(ctx, oldID, "", actor, ""); err != nil {
			return fmt.Errorf("failed to close superseded issue: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	commandDidWrite.Store(true)

	if isJSONOutput() {
		result := map[string]interface{}{
			"superseded":     oldID,
			"replacement":    newID,
			"status":         "closed",
			"moved_blockers": moved,
		}
		if len(kept) > 0 {
			result["kept_blockers"] = kept
		}
		return outputJSON(result)
	}

	fmt.Printf("%s Marked %s as superseded by %s (closed)\n", ui.RenderPass("✓"), oldID, newID)
	if len(moved) > 0 {
		fmt.Printf("  moved %d blocks edge(s) to %s\n", len(moved), newID)
	}
	if len(kept) > 0 {
		fmt.Printf("  kept %d blocks edge(s) on %s\n", len(kept), oldID)
	}
	return nil
}
