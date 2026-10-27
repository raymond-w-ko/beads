//go:build cgo

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/dolt"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// setupPagedExportStore extends setupStrictExportStore's store with 12
// durable issues and 6 wisps-table rows: mixed priorities (so the issues and
// wisps legs interleave in the merged order), distinct created_at values,
// ephemeral and no-history wisps, a promoted no-history durable row, an infra
// type and a template, each with a label, a dependency and a comment.
func setupPagedExportStore(t *testing.T) *dolt.DoltStore {
	t.Helper()
	s, _ := setupStrictExportStore(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.DB().ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	for i := 1; i <= 12; i++ {
		id := fmt.Sprintf("pg-%02d", i)
		issueType, isTemplate, noHistory := "task", 0, 0
		switch i {
		case 5:
			noHistory = 1 // promoted no-history row: durable, so no wisp_plane marker
		case 11:
			issueType = "agent"
		case 12:
			isTemplate = 1
		}
		exec(`INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type, created_at, updated_at, is_template, no_history) VALUES (?, ?, '', '', '', '', 'open', ?, ?, ?, ?, ?, ?)`,
			id, "Paged "+id, i%4, issueType, base.Add(time.Duration(i)*time.Minute), base, isTemplate, noHistory)
		exec(`INSERT INTO labels (issue_id, label) VALUES (?, ?)`, id, "label-"+id)
		exec(`INSERT INTO dependencies (id, issue_id, depends_on_issue_id, type, created_at, created_by) VALUES (UUID(), ?, ?, 'related', NOW(), 'test')`, id, "strict-1")
		exec(`INSERT INTO comments (id, issue_id, author, text, created_at) VALUES (UUID(), ?, 'test', ?, NOW())`, id, "comment on "+id)
	}
	for i := 1; i <= 6; i++ {
		id := fmt.Sprintf("pgw-%02d", i)
		ephemeral, noHistory := 1, 0
		if i > 3 {
			ephemeral, noHistory = 0, 1
		}
		exec(`INSERT INTO wisps (id, title, status, priority, issue_type, created_at, updated_at, ephemeral, no_history) VALUES (?, ?, 'open', ?, 'task', ?, ?, ?, ?)`,
			id, "Paged wisp "+id, (i+1)%4, base.Add(time.Duration(i)*time.Minute+30*time.Second), base, ephemeral, noHistory)
		exec(`INSERT INTO wisp_labels (issue_id, label) VALUES (?, ?)`, id, "label-"+id)
		exec(`INSERT INTO wisp_dependencies (id, issue_id, depends_on_issue_id, type, created_at, created_by) VALUES (UUID(), ?, ?, 'related', NOW(), 'test')`, id, fmt.Sprintf("pg-%02d", i))
		exec(`INSERT INTO wisp_comments (id, issue_id, author, text, created_at) VALUES (UUID(), ?, 'test', ?, NOW())`, id, "comment on "+id)
	}
	oldPageSize := exportPageSize
	t.Cleanup(func() { exportPageSize = oldPageSize })
	exportPageSize = 4
	return s
}

// referenceExportLines is the whole-set export path before paging: one
// search, one bulk load of every relation, one plane classification, then the
// same record shaping. It is the oracle the paged export must equal.
func referenceExportLines(t *testing.T, filter types.IssueFilter) []string {
	t.Helper()
	ctx := context.Background()
	src := storeExportSource{}
	issues, err := store.SearchIssues(ctx, "", filter)
	if err != nil {
		t.Fatalf("reference search: %v", err)
	}
	rel, err := src.LoadExportRelations(ctx, issues)
	if err != nil {
		t.Fatalf("reference relations: %v", err)
	}
	var noHistoryIDs []string
	for _, issue := range issues {
		if issue.NoHistory && !issue.Ephemeral {
			noHistoryIDs = append(noHistoryIDs, issue.ID)
		}
	}
	wispPlane, err := src.WispPlaneIDs(ctx, noHistoryIDs)
	if err != nil {
		t.Fatalf("reference plane: %v", err)
	}
	lines := make([]string, 0, len(issues))
	for _, issue := range issues {
		issue.Labels = rel.labels[issue.ID]
		issue.Dependencies = rel.deps[issue.ID]
		issue.Comments = rel.comments[issue.ID]
		counts := rel.depCounts[issue.ID]
		if counts == nil {
			counts = &types.DependencyCounts{}
		}
		sanitizeZeroTime(issue)
		data, err := json.Marshal(&exportIssueRecord{
			RecordType: "issue",
			IssueWithCounts: &types.IssueWithCounts{
				Issue:           issue,
				DependencyCount: counts.DependencyCount,
				DependentCount:  counts.DependentCount,
				CommentCount:    rel.commentCounts[issue.ID],
			},
			WispPlane: wispPlane[issue.ID],
		})
		if err != nil {
			t.Fatalf("reference marshal %s: %v", issue.ID, err)
		}
		lines = append(lines, string(data))
	}
	return lines
}

// issueIDsInOrder returns the ids of issues, in order.
func issueIDsInOrder(t *testing.T, filter types.IssueFilter) []string {
	t.Helper()
	issues, err := store.SearchIssues(context.Background(), "", filter)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	return projectIssueIDs(issues)
}

// exportedIssueLines splits export stdout into its issue lines and their ids.
func exportedIssueLines(t *testing.T, out string) (lines, ids []string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var rec struct {
			Type string `json:"_type"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("parse line %q: %v", line, err)
		}
		if rec.Type == "issue" {
			lines = append(lines, line)
			ids = append(ids, rec.ID)
		}
	}
	return lines, ids
}

func TestExportPagedOrderMatchesSearch(t *testing.T) {
	setupPagedExportStore(t)
	exportAll = true

	filter := buildExportFilter(context.Background(), storeExportSource{})
	wantIDs := issueIDsInOrder(t, filter)
	wantLines := referenceExportLines(t, filter)
	if len(wantIDs) <= exportPageSize {
		t.Fatalf("seed spans %d issues, want more than one page of %d", len(wantIDs), exportPageSize)
	}

	stdout, stderr, err := runExportCaptured(t)
	if err != nil {
		t.Fatalf("export: %v (stderr %q)", err, stderr)
	}
	gotLines, gotIDs := exportedIssueLines(t, stdout)
	if !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("paged export order\n got %v\nwant %v", gotIDs, wantIDs)
	}
	for i := range wantLines {
		if gotLines[i] != wantLines[i] {
			t.Errorf("line %d (%s) differs from the whole-set export\n got %s\nwant %s", i, gotIDs[i], gotLines[i], wantLines[i])
		}
	}
	// The seed must exercise the merge: wisps interleave with durable rows
	// and carry the plane marker where only membership can tell.
	if !strings.Contains(stdout, `"id":"pgw-04"`) || !strings.Contains(stdout, `"wisp_plane":true`) {
		t.Errorf("export lacks the no-history wisps or their wisp_plane marker:\n%s", stdout)
	}
}

func TestExportPagedWithoutAllKeepsFilters(t *testing.T) {
	setupPagedExportStore(t)
	exportAll = false

	filter := buildExportFilter(context.Background(), storeExportSource{})
	wantIDs := issueIDsInOrder(t, filter)
	wantLines := referenceExportLines(t, filter)

	stdout, stderr, err := runExportCaptured(t)
	if err != nil {
		t.Fatalf("export: %v (stderr %q)", err, stderr)
	}
	gotLines, gotIDs := exportedIssueLines(t, stdout)
	if !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("paged export order\n got %v\nwant %v", gotIDs, wantIDs)
	}
	if !slices.Equal(gotLines, wantLines) {
		t.Errorf("paged export lines differ from the whole-set export\n got %v\nwant %v", gotLines, wantLines)
	}
	for _, excluded := range []string{"pgw-01", "pgw-02", "pgw-03", "pg-11", "pg-12"} {
		if slices.Contains(gotIDs, excluded) {
			t.Errorf("export without --all printed %s (ephemeral, infra or template)", excluded)
		}
	}
	if !slices.Contains(gotIDs, "pgw-05") {
		t.Errorf("export without --all dropped the no-history wisp pgw-05: %v", gotIDs)
	}
}

// midStreamLabelsStore fails the export's bulk label load from its failFrom-th
// call on, so a paged export fails after earlier pages were written. A labels
// table broken by DDL would fail the page search's own label hydration too,
// in both modes (see failingLabelsStore).
type midStreamLabelsStore struct {
	storage.DoltStorage
	calls    int
	failFrom int
}

func (m *midStreamLabelsStore) GetLabelsForIssues(ctx context.Context, ids []string) (map[string][]string, error) {
	m.calls++
	if m.calls >= m.failFrom {
		return nil, errors.New("injected mid-stream labels read failure")
	}
	return m.DoltStorage.GetLabelsForIssues(ctx, ids)
}

func (m *midStreamLabelsStore) Unwrap() storage.DoltStorage { return m.DoltStorage }

func TestExportPagedStrictFailsMidStream(t *testing.T) {
	s := setupPagedExportStore(t)
	exportPageSize = 2

	for _, strict := range []bool{true, false} {
		t.Run(fmt.Sprintf("strict=%v", strict), func(t *testing.T) {
			store = &midStreamLabelsStore{DoltStorage: s, failFrom: 2}
			exportStrict = strict
			stdout, _, err := runExportCaptured(t)
			_, ids := exportedIssueLines(t, stdout)
			if strict {
				if err == nil {
					t.Fatalf("strict export with a failed page read returned nil; printed %v", ids)
				}
				if len(ids) != exportPageSize {
					t.Errorf("strict export printed %d lines before failing, want the first page (%d)", len(ids), exportPageSize)
				}
				return
			}
			if err != nil {
				t.Fatalf("non-strict export: %v", err)
			}
			if len(ids) <= exportPageSize {
				t.Errorf("non-strict export printed %d lines, want every page", len(ids))
			}
		})
	}
}

func TestSearchIssueKeysOrder(t *testing.T) {
	s := setupPagedExportStore(t)
	ctx := context.Background()
	persistentOnly := false
	for _, tc := range []struct {
		name   string
		filter types.IssueFilter
	}{
		{"default sort", types.IssueFilter{}},
		{"id sort", types.IssueFilter{SortBy: "id"}},
		{"id sort desc", types.IssueFilter{SortBy: "id", SortDesc: true}},
		{"created sort", types.IssueFilter{SortBy: "created"}},
		{"persistent only", types.IssueFilter{Ephemeral: &persistentOnly}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var keys, want []string
			err := withSearchTx(ctx, s.DB(), func(tx *sql.Tx) error {
				var err error
				if keys, err = issueops.SearchIssueKeysInTx(ctx, tx, "", tc.filter); err != nil {
					return err
				}
				issues, err := issueops.SearchIssuesInTx(ctx, tx, "", tc.filter)
				want = projectIssueIDs(issues)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(want) < 2 || !slices.ContainsFunc(want, func(id string) bool { return strings.HasPrefix(id, "pgw-") }) {
				t.Fatalf("search returned %v, want rows from both tables", want)
			}
			if !slices.Equal(keys, want) {
				t.Errorf("SearchIssueKeysInTx order\n got %v\nwant %v", keys, want)
			}
		})
	}
}

// withSearchTx runs fn in one read transaction on db.
func withSearchTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return fn(tx)
}
