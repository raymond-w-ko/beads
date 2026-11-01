package versioncontrolops

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestSplitConflictColumn(t *testing.T) {
	cases := []struct {
		col   string
		side  string
		field string
		ok    bool
	}{
		{"base_id", "base", "id", true},
		{"our_updated_at", "our", "updated_at", true},
		{"their_diff_type", "their", "diff_type", true},
		{"our_their_thing", "our", "their_thing", true},
		{"dolt_row_hash", "", "", false},
	}
	for _, c := range cases {
		side, field, ok := splitConflictColumn(c.col)
		if side != c.side || field != c.field || ok != c.ok {
			t.Errorf("splitConflictColumn(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.col, side, field, ok, c.side, c.field, c.ok)
		}
	}
}

func TestFormatConflictValue(t *testing.T) {
	if got := formatConflictValue(nil); got != nil {
		t.Errorf("NULL should format as a nil pointer, got %q", *got)
	}
	if got := formatConflictValue([]byte("")); got == nil || *got != "" {
		t.Errorf("empty string must stay distinct from NULL, got %v", got)
	}
	ts := time.Date(2026, 7, 25, 1, 2, 3, 0, time.UTC)
	if got := formatConflictValue(ts); got == nil || *got != "2026-07-25T01:02:03Z" {
		t.Errorf("time formatting = %v, want RFC3339 UTC", got)
	}
	if got := formatConflictValue(int64(7)); got == nil || *got != "7" {
		t.Errorf("int formatting = %v, want \"7\"", got)
	}
}

// buildConflictRow is the whole presentation contract: fold base_/our_/their_
// columns into one field each, keep the metadata out of the field list, and
// classify which sides have the row.
func TestBuildConflictRowModifyModify(t *testing.T) {
	cols := []string{
		"base_id", "our_id", "their_id",
		"base_status", "our_status", "their_status",
		"base_title", "our_title", "their_title",
		"our_diff_type", "their_diff_type",
	}
	vals := []any{
		[]byte("bd-1"), []byte("bd-1"), []byte("bd-1"),
		[]byte("open"), []byte("in_progress"), []byte("closed"),
		[]byte("t"), []byte("t"), []byte("t"),
		[]byte("modified"), []byte("modified"),
	}
	row := buildConflictRow("issues", cols, vals)

	if row.Key != "bd-1" {
		t.Errorf("Key = %q, want bd-1", row.Key)
	}
	if !row.BaseExists || !row.OurExists || !row.TheirExists {
		t.Errorf("modify/modify should exist on all sides, got base=%v our=%v their=%v",
			row.BaseExists, row.OurExists, row.TheirExists)
	}
	if row.OurDiffType != "modified" || row.TheirDiffType != "modified" {
		t.Errorf("diff types = %q/%q, want modified/modified", row.OurDiffType, row.TheirDiffType)
	}
	if len(row.Fields) != 3 {
		t.Fatalf("want 3 fields (id,status,title), got %d: %+v", len(row.Fields), row.Fields)
	}
	byName := map[string]int{}
	for i, f := range row.Fields {
		byName[f.Name] = i
	}
	for _, meta := range []string{"diff_type", "cardinality"} {
		if _, ok := byName[meta]; ok {
			t.Errorf("%s is conflict metadata and must not appear as a field", meta)
		}
	}
	status := row.Fields[byName["status"]]
	if status.Base == nil || *status.Base != "open" || status.Ours == nil || *status.Ours != "in_progress" || status.Theirs == nil || *status.Theirs != "closed" {
		t.Errorf("status field = %+v, want base=open ours=in_progress theirs=closed", status)
	}
	if !status.Differs() {
		t.Error("status diverged and must report Differs")
	}
	if row.Fields[byName["title"]].Differs() {
		t.Error("title agrees on both sides and must not report Differs")
	}
}

func TestBuildConflictRowDeleteModify(t *testing.T) {
	cols := []string{
		"base_id", "our_id", "their_id",
		"base_status", "our_status", "their_status",
		"our_diff_type", "their_diff_type",
	}
	// They deleted the row: every their_* column is NULL.
	vals := []any{
		[]byte("bd-2"), []byte("bd-2"), nil,
		[]byte("open"), []byte("closed"), nil,
		[]byte("modified"), []byte("removed"),
	}
	row := buildConflictRow("issues", cols, vals)

	if row.Key != "bd-2" {
		t.Errorf("Key = %q, want bd-2 (our side still has it)", row.Key)
	}
	if !row.OurExists || row.TheirExists {
		t.Errorf("want our=true their=false, got our=%v their=%v", row.OurExists, row.TheirExists)
	}
	status := row.Fields[1]
	if status.Theirs != nil {
		t.Errorf("their status should be nil on a deleted side, got %q", *status.Theirs)
	}
	if !status.Differs() {
		t.Error("a present-vs-absent value must count as differing")
	}
}

// A table with no registered key column still has to render, so `bd conflicts
// show` works for dependencies/config rows the resolver could not settle.
func TestConflictRowKeyFallbackForUnkeyedTable(t *testing.T) {
	cols := []string{"base_key", "our_key", "their_key", "base_value", "our_value", "their_value"}
	vals := []any{nil, nil, []byte("kv.memory.x"), nil, nil, []byte("v")}
	row := buildConflictRow("config", cols, vals)
	if row.Key != "kv.memory.x" {
		t.Errorf("Key = %q, want the first field carrying a value", row.Key)
	}
	if row.BaseExists || row.OurExists || !row.TheirExists {
		t.Errorf("their-side-only add misclassified: base=%v our=%v their=%v",
			row.BaseExists, row.OurExists, row.TheirExists)
	}
}

func TestTheirFieldsExcludesKeyAndMetadata(t *testing.T) {
	raw := rawConflictRow{
		cols: []string{"base_id", "our_id", "their_id", "our_status", "their_status", "their_diff_type", "their_cardinality"},
		vals: []any{[]byte("bd-1"), []byte("bd-1"), []byte("bd-1"), []byte("open"), []byte("closed"), []byte("modified"), int64(1)},
	}
	names, vals := raw.theirFields("id")
	if len(names) != 1 || names[0] != "status" {
		t.Fatalf("theirFields = %v, want only [status] (key and metadata excluded)", names)
	}
	if s, ok := vals[0].([]byte); !ok || string(s) != "closed" {
		t.Errorf("their status value = %v, want closed", vals[0])
	}
}

func TestRawConflictRowValue(t *testing.T) {
	raw := rawConflictRow{cols: []string{"our_id", "their_id"}, vals: []any{[]byte("bd-1"), nil}}
	if v, ok := raw.value("our", "id"); !ok || string(v.([]byte)) != "bd-1" {
		t.Errorf("our_id = %v (ok=%v), want bd-1", v, ok)
	}
	if v, ok := raw.value("their", "id"); !ok || v != nil {
		t.Errorf("their_id = %v (ok=%v), want a present-but-NULL column", v, ok)
	}
	if _, ok := raw.value("base", "id"); ok {
		t.Error("base_id is absent and must report ok=false")
	}
}

func TestValidateConflictTableRejectsInjection(t *testing.T) {
	// Table names are interpolated (MySQL cannot bind an identifier), so the
	// gate is the only thing between an operator's --table and the query.
	for _, bad := range []string{"", "issues; DROP TABLE issues", "issues`", "iss ues", "../etc"} {
		if err := ValidateConflictTable(bad); err == nil {
			t.Errorf("ValidateConflictTable(%q) = nil, want an error", bad)
		}
	}
	for _, good := range []string{"issues", "dependencies", "schema_migrations"} {
		if err := ValidateConflictTable(good); err != nil {
			t.Errorf("ValidateConflictTable(%q) = %v, want nil", good, err)
		}
	}
}

func TestValidateConflictStrategy(t *testing.T) {
	if err := ValidateConflictStrategy("ours"); err != nil {
		t.Errorf("ours rejected: %v", err)
	}
	if err := ValidateConflictStrategy("theirs"); err != nil {
		t.Errorf("theirs rejected: %v", err)
	}
	for _, bad := range []string{"", "OURS", "mine", "--ours"} {
		if err := ValidateConflictStrategy(bad); err == nil {
			t.Errorf("ValidateConflictStrategy(%q) = nil, want an error", bad)
		}
	}
}

func TestSupportsRowResolve(t *testing.T) {
	if !SupportsRowResolve("issues") {
		t.Error("issues must support per-row resolution")
	}
	if SupportsRowResolve("dependencies") {
		t.Error("only tables with a registered key column may claim per-row resolution")
	}
}

// TestResolveOneConflictRowZeroAffectedRows pins the operator path's half of
// the matched-rows fix: `bd conflicts resolve --theirs <id>` writes their
// values, and an UPDATE that changes nothing because the stored bytes already
// match must not be reported to the operator as a concurrent deletion of the
// very row they named. A row that really is gone is still refused, with no
// conflict cleared.
func TestResolveOneConflictRowZeroAffectedRows(t *testing.T) {
	ctx := context.Background()
	row := rawConflictRow{
		cols: []string{"base_id", "our_id", "their_id", "base_status", "our_status", "their_status"},
		vals: []any{"bd-1", "bd-1", "bd-1", "open", "open", "closed"},
	}
	const updateSQL = "UPDATE `issues` SET `status` = ? WHERE `id` = ?"
	const existsSQL = "SELECT COUNT(*) FROM `issues` WHERE `id` = ?"
	const deleteSQL = "DELETE FROM `dolt_conflicts_issues` WHERE `our_id` = ?"

	t.Run("a live row resolves", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectExec(regexp.QuoteMeta(updateSQL)).WithArgs("closed", "bd-1").
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery(regexp.QuoteMeta(existsSQL)).WithArgs("bd-1").
			WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(1))
		mock.ExpectExec(regexp.QuoteMeta(deleteSQL)).WithArgs("bd-1").
			WillReturnResult(sqlmock.NewResult(0, 1))

		if err := resolveOneConflictRow(ctx, db, "issues", "id", "bd-1", ConflictStrategyTheirs, row, nil); err != nil {
			t.Fatalf("a write the backend normalized to a no-op must not abort the resolve: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("a vanished row is still refused", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectExec(regexp.QuoteMeta(updateSQL)).WithArgs("closed", "bd-1").
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery(regexp.QuoteMeta(existsSQL)).WithArgs("bd-1").
			WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(0))
		// No delete: their side must not be discarded silently.

		err := resolveOneConflictRow(ctx, db, "issues", "id", "bd-1", ConflictStrategyTheirs, row, nil)
		if err == nil || !strings.Contains(err.Error(), "deleted concurrently") {
			t.Fatalf("err = %v, want the concurrent-deletion refusal", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})
}

// TestResolveConflictRowsTheirsSkipsGeneratedColumns pins the --theirs write
// against a table with a STORED generated column (the fork's
// issues.everturn_action): dolt refuses any value assigned to one ("The value
// specified for generated column ... is not allowed"), so the UPDATE writes
// every other their_* column and leaves the generated one for dolt to
// recompute. The generated columns are read from SHOW CREATE TABLE, since Dolt
// 2.2.0 leaves SHOW COLUMNS Extra empty for them.
func TestResolveConflictRowsTheirsSkipsGeneratedColumns(t *testing.T) {
	ctx := context.Background()
	const showCreateSQL = "SHOW CREATE TABLE `issues`"
	const conflictSQL = "SELECT * FROM `dolt_conflicts_issues` WHERE `our_id` = ? OR `their_id` = ?"
	const deleteSQL = "DELETE FROM `dolt_conflicts_issues` WHERE `our_id` = ?"
	const ddl = "CREATE TABLE `issues` (\n" +
		"  `id` varchar(255) NOT NULL,\n" +
		"  `title` varchar(500) NOT NULL,\n" +
		"  `metadata` json DEFAULT (json_object()),\n" +
		"  `everturn_action` varchar(255) GENERATED ALWAYS AS (left(json_unquote(json_extract(`metadata`, _utf8mb4'$.everturn.action')), 255)) STORED,\n" +
		"  `odd``name` int GENERATED ALWAYS AS (1) VIRTUAL,\n" +
		"  PRIMARY KEY (`id`)\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"
	conflictCols := []string{
		"from_root_ish", "base_id", "base_title", "base_metadata", "base_everturn_action",
		"our_id", "our_title", "our_metadata", "our_everturn_action", "our_diff_type",
		"their_id", "their_title", "their_metadata", "their_everturn_action", "their_diff_type",
		"dolt_conflict_id",
	}
	expectPrelude := func(mock sqlmock.Sqlmock) {
		mock.ExpectExec(regexp.QuoteMeta("SET @@dolt_allow_commit_conflicts = 1")).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(regexp.QuoteMeta("SET @@dolt_force_transaction_commit = 1")).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery(regexp.QuoteMeta(showCreateSQL)).
			WillReturnRows(sqlmock.NewRows([]string{"Table", "Create Table"}).AddRow("issues", ddl))
	}

	t.Run("the UPDATE omits the generated column", func(t *testing.T) {
		db, mock := newMockDB(t)
		expectPrelude(mock)
		mock.ExpectQuery(regexp.QuoteMeta(conflictSQL)).WithArgs("bd-1", "bd-1").
			WillReturnRows(sqlmock.NewRows(conflictCols).AddRow(
				"r", "bd-1", "base", "{}", nil,
				"bd-1", "ours", "{}", nil, "modified",
				"bd-1", "theirs", `{"everturn":{"action":"wake"}}`, "wake", "modified",
				"c1"))
		mock.ExpectExec("^"+regexp.QuoteMeta("UPDATE `issues` SET `title` = ?, `metadata` = ? WHERE `id` = ?")+"$").
			WithArgs("theirs", `{"everturn":{"action":"wake"}}`, "bd-1").
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(regexp.QuoteMeta(deleteSQL)).WithArgs("bd-1").
			WillReturnResult(sqlmock.NewResult(0, 1))

		n, err := ResolveConflictRows(ctx, db, "issues", []string{"bd-1"}, ConflictStrategyTheirs)
		if err != nil {
			t.Fatalf("ResolveConflictRows --theirs: %v", err)
		}
		if n != 1 {
			t.Fatalf("resolved %d rows, want 1", n)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("a row differing only in generated columns is cleared without a write", func(t *testing.T) {
		db, mock := newMockDB(t)
		expectPrelude(mock)
		mock.ExpectQuery(regexp.QuoteMeta(conflictSQL)).WithArgs("bd-1", "bd-1").
			WillReturnRows(sqlmock.NewRows([]string{"base_id", "our_id", "their_id", "base_everturn_action", "our_everturn_action", "their_everturn_action"}).
				AddRow("bd-1", "bd-1", "bd-1", nil, nil, "wake"))
		// No UPDATE: a SET list of only generated columns has nothing dolt accepts.
		mock.ExpectExec(regexp.QuoteMeta(deleteSQL)).WithArgs("bd-1").
			WillReturnResult(sqlmock.NewResult(0, 1))

		if _, err := ResolveConflictRows(ctx, db, "issues", []string{"bd-1"}, ConflictStrategyTheirs); err != nil {
			t.Fatalf("ResolveConflictRows --theirs: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})
}

func TestConflictGeneratedColumnsParsesShowCreateTable(t *testing.T) {
	db, mock := newMockDB(t)
	ddl := "CREATE TABLE `issues` (\n" +
		"  `id` varchar(255) NOT NULL,\n" +
		"  `created_at` datetime(6) NOT NULL DEFAULT (CURRENT_TIMESTAMP(6)),\n" +
		"  `everturn_action` varchar(255) GENERATED ALWAYS AS (left(`metadata`, 255)) STORED,\n" +
		"  `odd``name` int generated always as (1) VIRTUAL,\n" +
		"  KEY `idx_generated_always_as` (`everturn_action`)\n" +
		")"
	mock.ExpectQuery(regexp.QuoteMeta("SHOW CREATE TABLE `issues`")).
		WillReturnRows(sqlmock.NewRows([]string{"Table", "Create Table"}).AddRow("issues", ddl))

	got, err := conflictGeneratedColumns(context.Background(), db, "issues")
	if err != nil {
		t.Fatalf("conflictGeneratedColumns: %v", err)
	}
	want := map[string]bool{"everturn_action": true, "odd`name": true}
	if len(got) != len(want) {
		t.Fatalf("generated = %v, want %v", got, want)
	}
	for col := range want {
		if !got[col] {
			t.Errorf("generated = %v, missing %q", got, col)
		}
	}
}
