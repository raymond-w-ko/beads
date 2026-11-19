package issueops

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/steveyegge/beads/internal/storage/sqlbuild"
	"github.com/steveyegge/beads/internal/types"
)

func deferredParentProbeRegex(issueTable string) string {
	return `SELECT 1 FROM ` + issueTable + `\s+WHERE defer_until IS NOT NULL\s+AND defer_until > UTC_TIMESTAMP\(\)\s+LIMIT 1`
}

func deferredChildrenQueryRegex(depTable, issueTable string) string {
	targetCol := "depends_on_issue_id"
	if issueTable == "wisps" {
		targetCol = "depends_on_wisp_id"
	}
	return `SELECT dep\.issue_id\s+FROM ` + depTable + ` dep\s+JOIN ` + issueTable + ` parent ON parent\.id = dep\.` + targetCol + `\s+WHERE dep\.type = 'parent-child'\s+AND parent\.defer_until IS NOT NULL\s+AND parent\.defer_until > UTC_TIMESTAMP\(\)`
}

func beginMockTx(t *testing.T) (*sql.DB, sqlmock.Sqlmock, *sql.Tx) {
	t.Helper()

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	return db, mock, tx
}

func TestBuildSQLInClause(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		ids              []string
		wantPlaceholders string
		wantArgs         []interface{}
	}{
		{
			name:             "single ID",
			ids:              []string{"42"},
			wantPlaceholders: "?",
			wantArgs:         []interface{}{"42"},
		},
		{
			name:             "multiple IDs",
			ids:              []string{"1", "2", "3"},
			wantPlaceholders: "?,?,?",
			wantArgs:         []interface{}{"1", "2", "3"},
		},
		{
			name:             "empty slice",
			ids:              []string{},
			wantPlaceholders: "",
			wantArgs:         []interface{}{},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotPlaceholders, gotArgs := buildSQLInClause(tt.ids)

			if gotPlaceholders != tt.wantPlaceholders {
				t.Errorf("placeholders = %q, want %q", gotPlaceholders, tt.wantPlaceholders)
			}

			if len(gotArgs) != len(tt.wantArgs) {
				t.Fatalf("args length = %d, want %d", len(gotArgs), len(tt.wantArgs))
			}

			for i := range gotArgs {
				if gotArgs[i] != tt.wantArgs[i] {
					t.Errorf("args[%d] = %v, want %v", i, gotArgs[i], tt.wantArgs[i])
				}
			}
		})
	}
}

func TestGetReadyWorkInTx_PropagatesDeferredParentChildError(t *testing.T) {
	t.Parallel()

	_, mock, tx := beginMockTx(t)
	childErr := errors.New("dolt transient dependency read failure")
	mock.ExpectQuery(deferredParentProbeRegex("issues")).WillReturnError(childErr)

	_, err := GetReadyWorkInTx(
		context.Background(),
		tx,
		types.WorkFilter{},
	)
	if err == nil {
		t.Fatal("expected deferred parent child error")
	}
	if !errors.Is(err, childErr) {
		t.Fatalf("expected wrapped deferred parent child error, got %v", err)
	}
	if !strings.Contains(err.Error(), "compute deferred parent children") {
		t.Fatalf("expected deferred parent child context, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestLoadStatusByIDInTxPrefersWispOnCollision(t *testing.T) {
	t.Parallel()

	_, mock, tx := beginMockTx(t)
	mock.ExpectQuery("SELECT id, status FROM issues").
		WithArgs("dup-id").
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow("dup-id", types.StatusOpen))
	mock.ExpectQuery("SELECT id, status FROM wisps").
		WithArgs("dup-id").
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow("dup-id", types.StatusClosed))

	got, err := loadStatusByIDInTx(context.Background(), tx, []string{"dup-id"})
	if err != nil {
		t.Fatalf("loadStatusByIDInTx error = %v, want no error on cross-table dup", err)
	}
	if got["dup-id"] != types.StatusClosed {
		t.Errorf("status = %v, want %v (wisp canonical preferred over issues)", got["dup-id"], types.StatusClosed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestMergeReadyWispsPrefersWispOnCollision(t *testing.T) {
	t.Parallel()

	issuesCopy := &types.Issue{ID: "dup-id", Status: types.StatusOpen, Title: "issues copy"}
	wispCopy := &types.Issue{ID: "dup-id", Status: types.StatusClosed, Title: "wisp canonical"}

	got := mergeReadyWisps(
		[]*types.Issue{issuesCopy},
		[]*types.Issue{wispCopy},
		types.WorkFilter{},
	)
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 (deduped)", len(got))
	}
	if got[0].Title != "wisp canonical" {
		t.Errorf("title = %q, want %q (wisp preferred over issues copy)", got[0].Title, "wisp canonical")
	}
}

func TestGetChildrenOfDeferredParentsInTx_ReturnsChildrenFromBothDependencyTables(t *testing.T) {
	t.Parallel()

	_, mock, tx := beginMockTx(t)
	mock.ExpectQuery(deferredParentProbeRegex("issues")).
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	mock.ExpectQuery(deferredChildrenQueryRegex("dependencies", "issues")).
		WillReturnRows(sqlmock.NewRows([]string{"issue_id"}).AddRow("child-from-dependencies-issues"))
	mock.ExpectQuery(deferredChildrenQueryRegex("dependencies", "wisps")).
		WillReturnRows(sqlmock.NewRows([]string{"issue_id"}).AddRow("child-from-dependencies-wisps"))
	mock.ExpectQuery(deferredChildrenQueryRegex("wisp_dependencies", "issues")).
		WillReturnRows(sqlmock.NewRows([]string{"issue_id"}).AddRow("child-from-wisp-dependencies-issues"))
	mock.ExpectQuery(deferredChildrenQueryRegex("wisp_dependencies", "wisps")).
		WillReturnRows(sqlmock.NewRows([]string{"issue_id"}).AddRow("child-from-wisp-dependencies-wisps"))

	got, err := getChildrenOfDeferredParentsInTx(context.Background(), tx)
	if err != nil {
		t.Fatalf("getChildrenOfDeferredParentsInTx: %v", err)
	}
	want := []string{
		"child-from-dependencies-issues",
		"child-from-dependencies-wisps",
		"child-from-wisp-dependencies-issues",
		"child-from-wisp-dependencies-wisps",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("children = %v, want %v", got, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestGetChildrenOfDeferredParentsInTx_NoDeferredParentsExitsAfterProbe(t *testing.T) {
	t.Parallel()

	_, mock, tx := beginMockTx(t)
	mock.ExpectQuery(deferredParentProbeRegex("issues")).
		WillReturnRows(sqlmock.NewRows([]string{"1"}))
	mock.ExpectQuery(deferredParentProbeRegex("wisps")).
		WillReturnRows(sqlmock.NewRows([]string{"1"}))

	got, err := getChildrenOfDeferredParentsInTx(context.Background(), tx)
	if err != nil {
		t.Fatalf("getChildrenOfDeferredParentsInTx: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("children = %v, want empty", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestGetChildrenOfDeferredParentsInTx_IgnoresMissingWispDependenciesTable(t *testing.T) {
	t.Parallel()

	_, mock, tx := beginMockTx(t)
	mock.ExpectQuery(deferredParentProbeRegex("issues")).
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	mock.ExpectQuery(deferredChildrenQueryRegex("dependencies", "issues")).
		WillReturnRows(sqlmock.NewRows([]string{"issue_id"}).AddRow("child-from-dependencies-issues"))
	mock.ExpectQuery(deferredChildrenQueryRegex("dependencies", "wisps")).
		WillReturnRows(sqlmock.NewRows([]string{"issue_id"}).AddRow("child-from-dependencies-wisps"))
	mock.ExpectQuery(deferredChildrenQueryRegex("wisp_dependencies", "issues")).
		WillReturnError(errors.New("table wisp_dependencies does not exist"))

	got, err := getChildrenOfDeferredParentsInTx(context.Background(), tx)
	if err != nil {
		t.Fatalf("getChildrenOfDeferredParentsInTx: %v", err)
	}
	want := []string{"child-from-dependencies-issues", "child-from-dependencies-wisps"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("children = %v, want %v", got, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

// recordingQueryMatcher matches like sqlmock's default regexp matcher and
// records every statement it is shown, so a test can assert on the exact text
// the code sent rather than only on a pattern it matched.
type recordingQueryMatcher struct {
	mu   sync.Mutex
	sent []string
}

func (m *recordingQueryMatcher) Match(expectedSQL, actualSQL string) error {
	m.mu.Lock()
	m.sent = append(m.sent, actualSQL)
	m.mu.Unlock()
	return sqlmock.QueryMatcherRegexp.Match(expectedSQL, actualSQL)
}

func (m *recordingQueryMatcher) statementsContaining(substr string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, q := range m.sent {
		if strings.Contains(q, substr) {
			out = append(out, q)
		}
	}
	return out
}

// TestGetReadyWorkInTxOpenIncludesCustomActive captures the statement the
// store's ready path sends for `bd ready` (Status open). The ready-eligible
// set, built-in open plus the category-active custom statuses, arrives as
// bound literals read in the same transaction when the resolve proves
// custom_statuses.name and issues.status share a collation: an OR with a
// custom_statuses subquery makes Dolt read the [is_blocked,status] index as
// the open-ended range [NULL, ∞), every unblocked issue of every status
// (bd-7c0u4 Q4). When the collations differ or are unknown, the literal list
// would compare under the other column's collation, so the subquery clause
// stays (review bd-s9czb). The resolve is one statement in every case.
func TestGetReadyWorkInTxOpenIncludesCustomActive(t *testing.T) {
	t.Parallel()

	const legacyClause = "WHERE (status = ? OR status IN (SELECT name FROM custom_statuses WHERE category = 'active')) AND"
	type resolveRow struct {
		name            string
		nameCollation   any
		statusCollation any
	}
	for _, tc := range []struct {
		name         string
		rows         []resolveRow
		wantClause   string
		wantArgs     []string
		wantSubquery bool
	}{
		{
			name:       "ActiveCustomStatusSameCollation",
			rows:       []resolveRow{{"review", "utf8mb4_0900_bin", "utf8mb4_0900_bin"}},
			wantClause: "WHERE status IN (?,?) AND",
			wantArgs:   []string{"open", "review"},
		},
		{
			name:         "ActiveCustomStatusDifferentCollation",
			rows:         []resolveRow{{"review", "utf8mb4_0900_ai_ci", "utf8mb4_0900_bin"}},
			wantClause:   legacyClause,
			wantArgs:     []string{"open"},
			wantSubquery: true,
		},
		{
			// An empty issues table reports no status collation.
			name:         "ActiveCustomStatusUnknownCollation",
			rows:         []resolveRow{{"review", "utf8mb4_0900_bin", nil}},
			wantClause:   legacyClause,
			wantArgs:     []string{"open"},
			wantSubquery: true,
		},
		{
			name: "OneOfSeveralDiffers",
			rows: []resolveRow{
				{"review", "utf8mb4_0900_bin", "utf8mb4_0900_bin"},
				{"triage", "utf8mb4_0900_ai_ci", "utf8mb4_0900_bin"},
			},
			wantClause:   legacyClause,
			wantArgs:     []string{"open"},
			wantSubquery: true,
		},
		{name: "NoActiveCustomStatus", wantClause: "WHERE status = ? AND", wantArgs: []string{"open"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			matcher := &recordingQueryMatcher{}
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(matcher))
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			mock.ExpectBegin()
			tx, err := db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatalf("BeginTx: %v", err)
			}
			t.Cleanup(func() { _ = tx.Rollback() })

			var active []string
			for _, r := range tc.rows {
				active = append(active, r.name)
			}
			filter := types.WorkFilter{Status: types.StatusOpen}
			_, whereArgs, err := sqlbuild.BuildReadyWorkWhere(filter, IssuesFilterTables,
				sqlbuild.ReadyWorkWhereInputs{ActiveCustomStatuses: active, StatusCollationMatches: !tc.wantSubquery})
			if err != nil {
				t.Fatalf("BuildReadyWorkWhere: %v", err)
			}
			for i, want := range tc.wantArgs {
				if i >= len(whereArgs) || whereArgs[i] != want {
					t.Fatalf("builder args = %v, want leading %v", whereArgs, tc.wantArgs)
				}
			}
			// The hybrid ORDER BY binds two recency cutoffs after the WHERE args.
			queryArgs := make([]driver.Value, 0, len(whereArgs)+2)
			for _, a := range whereArgs {
				queryArgs = append(queryArgs, a)
			}
			queryArgs = append(queryArgs, sqlmock.AnyArg(), sqlmock.AnyArg())

			mock.ExpectQuery(deferredParentProbeRegex("issues")).WillReturnError(sql.ErrNoRows)
			mock.ExpectQuery(deferredParentProbeRegex("wisps")).WillReturnError(sql.ErrNoRows)
			activeRows := sqlmock.NewRows([]string{"name", "name_collation", "status_collation"})
			for _, r := range tc.rows {
				activeRows.AddRow(r.name, r.nameCollation, r.statusCollation)
			}
			mock.ExpectQuery("^" + regexp.QuoteMeta(sqlbuild.ActiveCustomStatusesSQL("issues")) + "$").
				WillReturnRows(activeRows)
			// Loose on purpose: the exact-text checks below name the clause.
			mock.ExpectQuery(`SELECT id FROM issues\s+WHERE `).
				WithArgs(queryArgs...).
				WillReturnRows(sqlmock.NewRows([]string{"id"}))
			mock.ExpectQuery(`SELECT 1 FROM wisps LIMIT 1`).WillReturnError(sql.ErrNoRows)

			got, err := GetReadyWorkInTx(context.Background(), tx, filter)
			if err != nil {
				t.Fatalf("GetReadyWorkInTx: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("expected no rows, got %d", len(got))
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unmet SQL expectations: %v", err)
			}

			if resolves := matcher.statementsContaining("FROM custom_statuses WHERE category = 'active' ORDER BY name"); len(resolves) != 1 {
				t.Fatalf("resolve statements sent = %d, want 1: %q", len(resolves), resolves)
			}
			reads := matcher.statementsContaining("SELECT id FROM issues")
			if len(reads) == 0 {
				t.Fatalf("no ready read sent; statements: %q", matcher.sent)
			}
			readyRead := reads[len(reads)-1]
			if got := strings.Contains(readyRead, "custom_statuses"); got != tc.wantSubquery {
				t.Fatalf("ready read has the custom_statuses subquery = %v, want %v:\n%s", got, tc.wantSubquery, readyRead)
			}
			if !strings.Contains(readyRead, tc.wantClause) {
				t.Fatalf("ready read lacks status clause %q:\n%s", tc.wantClause, readyRead)
			}
		})
	}
}

func TestGetReadyWorkInTxStatusesSingleQuery(t *testing.T) {
	t.Parallel()

	_, mock, tx := beginMockTx(t)
	mock.ExpectQuery(deferredParentProbeRegex("issues")).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(deferredParentProbeRegex("wisps")).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT id FROM issues\s+WHERE status IN \(\?,\?\)`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(`SELECT 1 FROM wisps LIMIT 1`).WillReturnError(sql.ErrNoRows)

	got, err := GetReadyWorkInTx(
		context.Background(),
		tx,
		types.WorkFilter{Statuses: []types.Status{"open", "blocked"}},
	)
	if err != nil {
		t.Fatalf("GetReadyWorkInTx: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no rows, got %d", len(got))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestReadyWorkWispIssueFilterPropagatesStatuses(t *testing.T) {
	t.Parallel()

	filter := readyWorkWispIssueFilter(types.WorkFilter{Statuses: []types.Status{"open", "blocked"}})
	if filter.Status != nil {
		t.Fatalf("wisp filter Status = %v, want nil", *filter.Status)
	}
	want := []types.Status{"open", "blocked"}
	if !reflect.DeepEqual(filter.Statuses, want) {
		t.Fatalf("wisp filter Statuses = %v, want %v", filter.Statuses, want)
	}
}

// bd-9mr2a: the wisps half carries the label glob and regex, so the wisps read
// filters wisp_labels as the issues half filters labels.
func TestReadyWorkWispIssueFilterPropagatesLabelPatterns(t *testing.T) {
	t.Parallel()

	filter := readyWorkWispIssueFilter(types.WorkFilter{LabelPattern: "class:*", LabelRegex: "^tier:[0-9]$"})
	if filter.LabelPattern != "class:*" {
		t.Errorf("wisp filter LabelPattern = %q, want %q", filter.LabelPattern, "class:*")
	}
	if filter.LabelRegex != "^tier:[0-9]$" {
		t.Errorf("wisp filter LabelRegex = %q, want %q", filter.LabelRegex, "^tier:[0-9]$")
	}
}
