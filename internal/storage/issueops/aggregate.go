package issueops

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// ChangedTables records durable tables changed by one guarded operation.
type ChangedTables map[string]bool

// Add records table names, ignoring empty names and wisp-only tables.
func (tables ChangedTables) Add(names ...string) {
	for _, name := range names {
		if name != "" && name != "wisps" && !strings.HasPrefix(name, "wisp_") {
			tables[name] = true
		}
	}
}

// Merge records every table in other.
func (tables ChangedTables) Merge(other map[string]bool) {
	for table := range other {
		tables.Add(table)
	}
}

// UpdateFields converts the public scalar patch fields to storage updates.
func UpdateFields(patch publicops.IssuePatch) map[string]interface{} {
	updates := map[string]interface{}{}
	for _, field := range []struct {
		set bool
		key string
		val interface{}
	}{
		{patch.Title.Set, "title", patch.Title.Value},
		{patch.Description.Set, "description", patch.Description.Value},
		{patch.Design.Set, "design", patch.Design.Value},
		{patch.AcceptanceCriteria.Set, "acceptance_criteria", patch.AcceptanceCriteria.Value},
		{patch.Notes.Set, "notes", patch.Notes.Value},
		{patch.AppendNotes.Set, "append_notes", patch.AppendNotes.Value},
		{patch.SpecID.Set, "spec_id", patch.SpecID.Value},
		{patch.AwaitID.Set, "await_id", patch.AwaitID.Value},
		{patch.Status.Set, "status", patch.Status.Value},
		{patch.Priority.Set, "priority", patch.Priority.Value},
		{patch.IssueType.Set, "issue_type", patch.IssueType.Value},
		{patch.Assignee.Set, "assignee", patch.Assignee.Value},
		{patch.Owner.Set, "owner", patch.Owner.Value},
		{patch.ClosedBySession.Set, "closed_by_session", patch.ClosedBySession.Value},
		{patch.EstimatedMinutes.Set, "estimated_minutes", patch.EstimatedMinutes.Value},
		{patch.ExternalRef.Set, "external_ref", patch.ExternalRef.Value},
		{patch.DueAt.Set, "due_at", patch.DueAt.Value},
		{patch.DeferUntil.Set, "defer_until", patch.DeferUntil.Value},
	} {
		if field.set {
			updates[field.key] = field.val
		}
	}
	return updates
}

// ValidateUpdateRequest checks mutually exclusive guarded-update options and
// the canonical field values every backend must reject identically. Backends
// call it before touching the row so an invalid patch cannot half-apply.
func ValidateUpdateRequest(request publicops.UpdateRequest) error {
	if request.Claim && (request.ExpectedAssignee != nil || request.ExpectedStatus != nil) {
		return fmt.Errorf("%w: claim cannot use expected assignee or status", storage.ErrValidation)
	}
	if request.ForceAssigneeTransfer && (request.Claim || !request.Patch.Assignee.Set || request.ExpectedAssignee != nil) {
		return fmt.Errorf("%w: invalid forced assignee transfer", storage.ErrValidation)
	}
	patch := request.Patch
	if patch.Title.Set {
		if err := types.ValidateIssueTitle(patch.Title.Value); err != nil {
			return fmt.Errorf("%w: update title: %w", storage.ErrValidation, err)
		}
	}
	if patch.Priority.Set {
		if err := types.ValidateIssuePriority(patch.Priority.Value); err != nil {
			return fmt.Errorf("%w: update priority: %w", storage.ErrValidation, err)
		}
	}
	if patch.EstimatedMinutes.Set {
		if err := types.ValidateIssueEstimatedMinutes(patch.EstimatedMinutes.Value); err != nil {
			return fmt.Errorf("%w: update estimated_minutes: %w", storage.ErrValidation, err)
		}
	}
	if patch.Persistence.Set && !patch.Persistence.Value.IsValid() {
		return fmt.Errorf("%w: invalid persistence mode %q", storage.ErrValidation, patch.Persistence.Value)
	}
	return nil
}

// ValidateMetadataPatch checks mutually exclusive metadata edits.
func ValidateMetadataPatch(patch publicops.MetadataPatch) error {
	if patch.Replace.Set && (patch.Merge.Set || len(patch.Set) > 0 || len(patch.Unset) > 0) {
		return fmt.Errorf("%w: cannot combine metadata replacement with incremental metadata edits", storage.ErrValidation)
	}
	if len(patch.MergePatch) > 0 && (patch.Replace.Set || patch.Merge.Set || len(patch.Set) > 0 || len(patch.Unset) > 0) {
		return fmt.Errorf("%w: cannot combine a metadata merge patch with other metadata edits", storage.ErrValidation)
	}
	for _, mergePatch := range patch.MergePatch {
		if err := storage.ValidateMetadataMergePatch(mergePatch); err != nil {
			return fmt.Errorf("%w: %w", storage.ErrValidation, err)
		}
	}
	return nil
}

// ValidateScalarUpdates checks typed scalar values before they reach SQL.
func ValidateScalarUpdates(ctx context.Context, tx DBTX, updates map[string]interface{}) error {
	if rawType, ok := updates["issue_type"]; ok {
		var issueType types.IssueType
		switch value := rawType.(type) {
		case types.IssueType:
			issueType = value
		case string:
			issueType = types.IssueType(value)
		default:
			return fmt.Errorf("%w: invalid issue type %v", storage.ErrValidation, rawType)
		}
		customTypes, err := ResolveCustomTypesInTx(ctx, tx)
		if err != nil {
			return fmt.Errorf("resolve custom issue types: %w", err)
		}
		if !issueType.IsValidWithCustom(customTypes) {
			return fmt.Errorf("%w: invalid issue type %s", storage.ErrValidation, issueType)
		}
	}
	for _, field := range []string{"assignee", "owner"} {
		if raw, ok := updates[field]; ok {
			if value, ok := raw.(string); ok {
				if err := types.CheckFieldLen(field, value); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// AuthorizeAssigneeTransferWithPools is the assignee-transfer fence itself,
// with the pool aliases supplied rather than read. It is the single source of
// the predicate: the DBTX path below and the unit-of-work backend, which
// reaches config through a use case rather than a transaction, both evaluate
// it, so the two cannot drift into disagreeing about which transfers are
// fenced.
//
// The no-op and self-transfer exits (request.Patch.Assignee.Value against
// before.Assignee, and before.Assignee against request.Actor) are judged
// under actorMatches, not verbatim equality (ga-5ksp5): the same Gas Town
// identity arrives here spelled differently depending on which layer
// produced the string (a dotted alias vs its session-name form — see
// canonicalActor), and a byte-for-byte comparison would wrongly fence a
// holder editing its own claim, or reject an idempotent re-assert, just
// because the caller named the holder under a different layer's spelling.
//
// Passing nil pools answers every question except pool membership, so a caller
// that wants the config read only when it matters calls with nil first and
// re-evaluates with the loaded aliases on refusal.
func AuthorizeAssigneeTransferWithPools(before *types.Issue, request publicops.UpdateRequest, pools []string) error {
	if !request.Patch.Assignee.Set || actorMatches(request.Patch.Assignee.Value, before.Assignee) || request.ExpectedAssignee != nil || request.ForceAssigneeTransfer || before.Status != types.StatusInProgress || before.Assignee == "" || actorMatches(before.Assignee, request.Actor) {
		return nil
	}
	// Exact-string membership, deliberately not actorMatches (ga-v2k49, same
	// reason as claim.go's identical pool checks): a pool alias is a literal
	// claim.pools config value, not a Gas Town identity that gets respelled
	// per layer, so there is no cross-spelling variant to reconcile.
	for _, pool := range pools {
		if pool == before.Assignee {
			return nil
		}
	}
	return fmt.Errorf("%w: issue %s is assigned to %q", storage.ErrAlreadyClaimed, before.ID, before.Assignee)
}

// AuthorizeAssigneeTransfer protects an active assignment from unguarded transfer.
func AuthorizeAssigneeTransfer(ctx context.Context, tx DBTX, before *types.Issue, request publicops.UpdateRequest) error {
	if err := AuthorizeAssigneeTransferWithPools(before, request, nil); err == nil {
		return nil
	}
	pools, err := ClaimPoolAliasesInTx(ctx, tx)
	if err != nil {
		return err
	}
	return AuthorizeAssigneeTransferWithPools(before, request, pools)
}

// ApplyMetadataPatch returns the canonical metadata value and whether it changes.
func ApplyMetadataPatch(current json.RawMessage, patch publicops.MetadataPatch) (json.RawMessage, bool, error) {
	if !patch.Replace.Set && !patch.Merge.Set && len(patch.Set) == 0 && len(patch.Unset) == 0 && len(patch.MergePatch) == 0 {
		return current, false, nil
	}
	if err := ValidateMetadataPatch(patch); err != nil {
		return nil, false, err
	}
	setKeys := make([]string, 0, len(patch.Set))
	for key := range patch.Set {
		setKeys = append(setKeys, key)
	}
	sort.Strings(setKeys)
	for _, key := range setKeys {
		if err := storage.ValidateMetadataKey(key); err != nil {
			return nil, false, fmt.Errorf("%w: %w", storage.ErrValidation, err)
		}
	}
	for _, key := range patch.Unset {
		if err := storage.ValidateMetadataKey(key); err != nil {
			return nil, false, fmt.Errorf("%w: %w", storage.ErrValidation, err)
		}
	}
	var next json.RawMessage
	if patch.Replace.Set {
		next = append(json.RawMessage(nil), patch.Replace.Value...)
		if len(next) == 0 {
			next = json.RawMessage(`{}`)
		}
		if !json.Valid(next) {
			return nil, false, fmt.Errorf("%w: metadata replacement is not valid JSON", storage.ErrValidation)
		}
	} else if len(patch.MergePatch) > 0 {
		next = append(json.RawMessage(nil), current...)
		for _, mergePatch := range patch.MergePatch {
			merged, err := storage.MergePatchMetadataJSON(next, mergePatch)
			if err != nil {
				return nil, false, fmt.Errorf("%w: metadata merge patch: %v", storage.ErrValidation, err)
			}
			next = merged
		}
	} else {
		next = append(json.RawMessage(nil), current...)
		if patch.Merge.Set {
			// A JSON null unmarshals into a nil overlay map, so the merge
			// below would silently accept it as "change nothing".
			if strings.TrimSpace(string(patch.Merge.Value)) == "null" {
				return nil, false, fmt.Errorf("%w: metadata merge must be a JSON object", storage.ErrValidation)
			}
			merged, err := storage.MergeMetadataJSON(next, patch.Merge.Value)
			if err != nil {
				return nil, false, fmt.Errorf("%w: metadata merge: %v", storage.ErrValidation, err)
			}
			next = merged
		}
		if len(patch.Set) > 0 || len(patch.Unset) > 0 {
			values := make(map[string]json.RawMessage)
			if len(next) > 0 && string(next) != "null" {
				if err := json.Unmarshal(next, &values); err != nil {
					return nil, false, fmt.Errorf("%w: metadata edits require an object: %v", storage.ErrValidation, err)
				}
			}
			for _, key := range setKeys {
				value := patch.Set[key]
				if !json.Valid(value) {
					return nil, false, fmt.Errorf("%w: metadata value for key %q is not valid JSON", storage.ErrValidation, key)
				}
				values[key] = append(json.RawMessage(nil), value...)
			}
			for _, key := range patch.Unset {
				delete(values, key)
			}
			encoded, err := json.Marshal(values)
			if err != nil {
				return nil, false, fmt.Errorf("%w: encode metadata edits: %v", storage.ErrValidation, err)
			}
			next = encoded
		}
	}
	if err := ValidateMetadataIfConfigured(next); err != nil {
		return nil, false, err
	}
	changed, err := metadataChanged(current, next)
	if err != nil {
		return nil, false, fmt.Errorf("%w: compare metadata: %v", storage.ErrValidation, err)
	}
	return next, changed, nil
}

// metadataChanged reports whether next differs from current as a JSON value,
// with numbers compared as the server keeps them (review bd-ldmiy finding 3):
//
//   - two integer literals (an optional '-' and digits, no fraction or
//     exponent; -0 aside) inside the signed or unsigned 64-bit range are equal
//     only when the integers are: Dolt 2.6 keeps such an integer's digits, so
//     9007199254740993 over a stored 9007199254740992 is a change;
//   - an integer literal in that range against any other spelling is equal only
//     when the other's float64 is integral and the literal is either exactly
//     that integer or that float64's shortest decimal spelling, which is how
//     the server prints a stored float64 (2^63 as 9223372036854776000): 1 and
//     1.0, 1000 and 1e3 are equal, 9007199254740993 and 9007199254740993.0
//     (stored as ...992) are not;
//   - any other pair is equal when the two float64 values are, as the float64
//     round trip of the old form decided: 0.1 and 0.10000000000000001 are one
//     number, -0 and 0 are two. A number a float64 cannot hold (1e400) anywhere
//     in either document is an error, as it was.
//
// Each number costs time and memory linear in its text; no exponent is
// expanded into digits.
func metadataChanged(current, next json.RawMessage) (bool, error) {
	decode := func(raw json.RawMessage) (any, error) {
		if len(raw) == 0 || string(raw) == "null" {
			return map[string]any{}, nil
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if err := checkJSONNumbers(value); err != nil {
			return nil, err
		}
		return value, nil
	}
	left, err := decode(current)
	if err != nil {
		return false, err
	}
	right, err := decode(next)
	if err != nil {
		return false, err
	}
	return !jsonValuesEqual(left, right), nil
}

// checkJSONNumbers fails on a number a float64 cannot hold, where the old
// float64 decode failed.
func checkJSONNumbers(value any) error {
	switch typed := value.(type) {
	case json.Number:
		if _, err := strconv.ParseFloat(typed.String(), 64); err != nil {
			return fmt.Errorf("metadata number %.40s: %w", typed.String(), err)
		}
	case map[string]any:
		for _, member := range typed {
			if err := checkJSONNumbers(member); err != nil {
				return err
			}
		}
	case []any:
		for _, member := range typed {
			if err := checkJSONNumbers(member); err != nil {
				return err
			}
		}
	}
	return nil
}

// jsonValuesEqual compares two documents decoded with UseNumber: objects by
// their member sets, arrays in order, numbers by jsonNumbersEqual, every other
// value as itself. A number never equals a string of the same digits.
func jsonValuesEqual(left, right any) bool {
	switch l := left.(type) {
	case map[string]any:
		r, ok := right.(map[string]any)
		if !ok || len(l) != len(r) {
			return false
		}
		for key, member := range l {
			other, present := r[key]
			if !present || !jsonValuesEqual(member, other) {
				return false
			}
		}
		return true
	case []any:
		r, ok := right.([]any)
		if !ok || len(l) != len(r) {
			return false
		}
		for i := range l {
			if !jsonValuesEqual(l[i], r[i]) {
				return false
			}
		}
		return true
	case json.Number:
		r, ok := right.(json.Number)
		return ok && jsonNumbersEqual(l.String(), r.String())
	default:
		return left == right
	}
}

// jsonNumbersEqual is metadataChanged's number rule for two valid JSON
// numbers a float64 can hold (checkJSONNumbers ran first).
func jsonNumbersEqual(a, b string) bool {
	aInt, bInt := isJSON64BitIntegerLiteral(a), isJSON64BitIntegerLiteral(b)
	if aInt && bInt {
		return a == b
	}
	fa, _ := strconv.ParseFloat(a, 64)
	fb, _ := strconv.ParseFloat(b, 64)
	if aInt || bInt {
		literal, other := a, fb
		if bInt {
			literal, other = b, fa
		}
		if other != math.Trunc(other) || (other == 0 && math.Signbit(other)) {
			return false
		}
		return literal == strconv.FormatFloat(other, 'f', 0, 64) ||
			literal == strconv.FormatFloat(other, 'f', -1, 64)
	}
	return math.Float64bits(fa) == math.Float64bits(fb)
}

// isJSON64BitIntegerLiteral reports whether number, a valid JSON number, has
// no fraction and no exponent, is not -0, and fits an int64 or a uint64.
func isJSON64BitIntegerLiteral(number string) bool {
	if strings.ContainsAny(number, ".eE") || number == "-0" {
		return false
	}
	if _, err := strconv.ParseInt(number, 10, 64); err == nil {
		return true
	}
	_, err := strconv.ParseUint(number, 10, 64)
	return err == nil
}

// ApplyLabelPatch applies ordered label edits and reports whether rows changed.
func ApplyLabelPatch(ctx context.Context, tx DBTX, current *types.Issue, patch publicops.LabelPatch, actor string) (bool, error) {
	if !patch.Replace.Set && len(patch.Add) == 0 && len(patch.Remove) == 0 {
		return false, nil
	}
	existing := stringSet(current.Labels)
	target := make(map[string]struct{}, len(existing)+len(patch.Add))
	if !patch.Replace.Set {
		for label := range existing {
			target[label] = struct{}{}
		}
	}
	// An empty-string entry is DROPPED rather than written or refused
	// (bd-yby99.29). A label row carrying '' is junk that renders as nothing
	// and matches nothing, so the two useful answers were dropping it and
	// refusing it; dropping keeps a stray empty entry from failing an
	// otherwise-good multi-label edit.
	//
	// Skipping here rather than at the insert is what makes it a no-op instead
	// of a silent partial write: an Add of only '' leaves target equal to
	// existing, so the sameStringSet check below reports Changed false and
	// writes nothing at all.
	//
	// Entries already in existing are deliberately untouched. A legacy '' row
	// survives an Add/Remove patch and is cleared by a Replace that omits it,
	// which is the ordinary set semantics rather than a migration.
	for _, label := range patch.Replace.Value {
		if label == "" {
			continue
		}
		target[label] = struct{}{}
	}
	for _, label := range patch.Add {
		if label == "" {
			continue
		}
		target[label] = struct{}{}
	}
	for _, label := range patch.Remove {
		delete(target, label)
	}
	if sameStringSet(existing, target) {
		return false, nil
	}
	sqlTx, ok := tx.(*sql.Tx)
	if !ok {
		return false, fmt.Errorf("apply labels: transaction must be *sql.Tx")
	}
	for _, label := range stringSetDifference(existing, target) {
		if err := RemoveLabelInTx(ctx, sqlTx, "", "", current.ID, label, actor); err != nil {
			return false, err
		}
	}
	for _, label := range stringSetDifference(target, existing) {
		if err := AddLabelInTx(ctx, sqlTx, "", "", current.ID, label, actor); err != nil {
			return false, err
		}
	}
	return true, nil
}

// ParentPatchResult reports the concrete changes made by ApplyParentPatch.
type ParentPatchResult struct {
	Changed          bool
	IssueRowsChanged bool
	WispRowsChanged  bool
}

// ApplyParentPatch replaces the parent-child targets and reports concrete changes.
func ApplyParentPatch(ctx context.Context, tx DBTX, current *types.Issue, parent publicops.Field[string], actor string) (ParentPatchResult, error) {
	if !parent.Set {
		return ParentPatchResult{}, nil
	}
	dependencies, err := GetDependencyRecordsForIssuesInTx(ctx, tx, []string{current.ID})
	if err != nil {
		return ParentPatchResult{}, err
	}
	existing := map[string]struct{}{}
	for _, dependency := range dependencies[current.ID] {
		if dependency.Type == types.DepParentChild {
			existing[dependency.DependsOnID] = struct{}{}
		}
	}
	target := map[string]struct{}{}
	if parent.Value != "" {
		target[parent.Value] = struct{}{}
	}
	if sameStringSet(existing, target) {
		return ParentPatchResult{}, nil
	}
	sqlTx, ok := tx.(*sql.Tx)
	if !ok {
		return ParentPatchResult{}, fmt.Errorf("apply parent: transaction must be *sql.Tx")
	}
	var recomputed RecomputeIsBlockedResult
	for _, parentID := range stringSetDifference(existing, target) {
		if _, err := removeDependencyInTx(ctx, sqlTx, current.ID, parentID, actor, false, &recomputed); err != nil {
			return ParentPatchResult{}, err
		}
	}
	for _, parentID := range stringSetDifference(target, existing) {
		if _, err := addDependencyInTx(ctx, sqlTx, &types.Dependency{IssueID: current.ID, DependsOnID: parentID, Type: types.DepParentChild}, actor, AddDependencyOpts{}, &recomputed); err != nil {
			return ParentPatchResult{}, err
		}
	}
	return ParentPatchResult{Changed: true, IssueRowsChanged: recomputed.IssueRowsChanged, WispRowsChanged: recomputed.WispRowsChanged}, nil
}

func stringSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

func sameStringSet(left, right map[string]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for value := range left {
		if _, ok := right[value]; !ok {
			return false
		}
	}
	return true
}

func stringSetDifference(left, right map[string]struct{}) []string {
	values := make([]string, 0)
	for value := range left {
		if _, ok := right[value]; !ok {
			values = append(values, value)
		}
	}
	sort.Strings(values)
	return values
}
