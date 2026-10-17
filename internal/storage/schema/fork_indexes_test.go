package schema

import (
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestForkIndexesSQLNamesEveryIndexWithDirectDDL pins the fresh-bundle side of
// the fork indexes without a Dolt binary: the issues column (STORED, its value
// cut to 255 by LEFT, see fork_indexes.go) and every index are named, the
// column comes before the index that needs it, the wisps column
// is left out (the bundle creates no wisps), every statement is direct DDL (a
// PREPARE'd ALTER silently does nothing on a pre-2.3 Dolt CLI,
// dolthub/dolt#11345), and AllMigrationsSQL ends with the statements so the
// CLI bundle matches the runtime schema.
func TestForkIndexesSQLNamesEveryIndexWithDirectDDL(t *testing.T) {
	got := forkIndexesSQL()
	const column = "ALTER TABLE issues ADD COLUMN everturn_action VARCHAR(255) GENERATED ALWAYS AS (LEFT(JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.everturn.action')), 255)) STORED;\n"
	if !strings.HasPrefix(got, column) {
		t.Errorf("forkIndexesSQL() does not start with the issues.everturn_action column:\n%s", got)
	}
	if strings.Contains(got, "wisps") {
		t.Errorf("forkIndexesSQL() touches wisps, which the fresh bundle never creates:\n%s", got)
	}
	for _, name := range []string{
		"idx_issues_closed_at",
		"idx_issues_status_closed_at",
		"idx_issues_everturn_action",
	} {
		if !strings.Contains(got, "ALTER TABLE issues ADD INDEX "+name+" (") {
			t.Errorf("forkIndexesSQL() does not add %s on issues:\n%s", name, got)
		}
	}
	if !strings.Contains(got, "idx_issues_everturn_action (everturn_action, created_at)") {
		t.Errorf("idx_issues_everturn_action is not (everturn_action, created_at):\n%s", got)
	}
	upper := strings.ToUpper(got)
	for _, banned := range []string{"PREPARE", "EXECUTE"} {
		if strings.Contains(upper, banned) {
			t.Errorf("forkIndexesSQL() uses %s:\n%s", banned, got)
		}
	}
	if n, want := strings.Count(got, ";\n"), 1+len(forkIndexes); n != want {
		t.Errorf("forkIndexesSQL() has %d statement terminators, want %d:\n%s", n, want, got)
	}
	if !strings.HasSuffix(AllMigrationsSQL(), got) {
		t.Errorf("AllMigrationsSQL() does not end with forkIndexesSQL()")
	}
}

// expectForkIndexesPresent mocks needsForkIndexes / ensureForkIndexes on a
// database that already carries every fork column and index: one SHOW COLUMNS
// per column table, then one SHOW INDEX per index table, and no DDL.
func expectForkIndexesPresent(mock sqlmock.Sqlmock) {
	columns := make(map[string][]string)
	var columnTables []string
	for _, col := range forkColumns {
		if _, ok := columns[col.Table]; !ok {
			columnTables = append(columnTables, col.Table)
		}
		columns[col.Table] = append(columns[col.Table], col.Name)
	}
	for _, table := range columnTables {
		rows := sqlmock.NewRows([]string{"Field", "Type", "Null", "Key", "Default", "Extra"}).
			AddRow("id", "varchar(255)", "NO", "PRI", nil, "")
		for _, name := range columns[table] {
			rows.AddRow(name, "varchar(255)", "YES", "", nil, "")
		}
		mock.ExpectQuery(regexp.QuoteMeta("SHOW COLUMNS FROM `" + table + "`")).WillReturnRows(rows)
	}

	indexes := make(map[string][]string)
	var indexTables []string
	for _, idx := range forkIndexes {
		if _, ok := indexes[idx.Table]; !ok {
			indexTables = append(indexTables, idx.Table)
		}
		indexes[idx.Table] = append(indexes[idx.Table], idx.Name)
	}
	for _, table := range indexTables {
		rows := sqlmock.NewRows([]string{"Table", "Non_unique", "Key_name", "Seq_in_index", "Column_name"}).
			AddRow(table, 0, "PRIMARY", 1, "id")
		for _, name := range indexes[table] {
			rows.AddRow(table, 1, name, 1, "")
		}
		mock.ExpectQuery(regexp.QuoteMeta("SHOW INDEX FROM `" + table + "`")).WillReturnRows(rows)
	}
}
