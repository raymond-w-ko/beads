package schema

import (
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestForkIndexesSQLNamesEveryIndexWithDirectDDL pins the fresh-bundle side of
// the fork indexes without a Dolt binary: every index is named, every
// statement is direct DDL (a PREPARE'd ALTER silently does nothing on a
// pre-2.3 Dolt CLI, dolthub/dolt#11345), and AllMigrationsSQL ends with the
// statements so the CLI bundle matches the runtime schema.
func TestForkIndexesSQLNamesEveryIndexWithDirectDDL(t *testing.T) {
	got := forkIndexesSQL()
	for _, name := range []string{
		"idx_issues_closed_at",
		"idx_issues_status_closed_at",
	} {
		if !strings.Contains(got, "ALTER TABLE issues ADD INDEX "+name+" (") {
			t.Errorf("forkIndexesSQL() does not add %s on issues:\n%s", name, got)
		}
	}
	upper := strings.ToUpper(got)
	for _, banned := range []string{"PREPARE", "EXECUTE"} {
		if strings.Contains(upper, banned) {
			t.Errorf("forkIndexesSQL() uses %s:\n%s", banned, got)
		}
	}
	if n := strings.Count(got, ";\n"); n != len(forkIndexes) {
		t.Errorf("forkIndexesSQL() has %d statement terminators, want %d:\n%s", n, len(forkIndexes), got)
	}
	if !strings.HasSuffix(AllMigrationsSQL(), got) {
		t.Errorf("AllMigrationsSQL() does not end with forkIndexesSQL()")
	}
}

// expectForkIndexesPresent mocks needsForkIndexes / ensureForkIndexes on a
// database that already carries every fork index: one SHOW INDEX per table
// and no DDL.
func expectForkIndexesPresent(mock sqlmock.Sqlmock) {
	names := make(map[string][]string)
	var tables []string
	for _, idx := range forkIndexes {
		if _, ok := names[idx.Table]; !ok {
			tables = append(tables, idx.Table)
		}
		names[idx.Table] = append(names[idx.Table], idx.Name)
	}
	for _, table := range tables {
		rows := sqlmock.NewRows([]string{"Table", "Non_unique", "Key_name", "Seq_in_index", "Column_name"}).
			AddRow(table, 0, "PRIMARY", 1, "id")
		for _, name := range names[table] {
			rows.AddRow(table, 1, name, 1, "closed_at")
		}
		mock.ExpectQuery(regexp.QuoteMeta("SHOW INDEX FROM `" + table + "`")).WillReturnRows(rows)
	}
}
