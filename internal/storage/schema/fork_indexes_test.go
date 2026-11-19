package schema

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestForkIndexesSQLNamesEveryIndexWithDirectDDL pins the fresh-bundle side of
// the fork indexes without a Dolt binary: the issues columns (STORED, each
// value cut to 255 by LEFT, see fork_indexes.go) and every issues index are
// named, the columns come before the indexes that need them, the wisps
// columns and indexes are left out (the bundle creates no wisps), every
// statement is direct DDL (a
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
		"idx_issues_everturn_key",
		"idx_issues_everturn_relay_source",
		"idx_issues_everturn_bead",
		"idx_issues_everturn_corrects",
	} {
		if !strings.Contains(got, "ALTER TABLE issues ADD INDEX "+name+" (") {
			t.Errorf("forkIndexesSQL() does not add %s on issues:\n%s", name, got)
		}
	}
	lastColumn := strings.LastIndex(got, "ADD COLUMN")
	for _, c := range []struct{ name, path string }{
		{"everturn_action", "$.everturn.action"},
		{"everturn_key", "$.everturn.key"},
		{"everturn_relay_source", "$.everturn.relay.source"},
		{"everturn_bead", "$.everturn.bead"},
		{"everturn_corrects", "$.everturn.corrects"},
	} {
		column := "ALTER TABLE issues ADD COLUMN " + c.name + " VARCHAR(255) GENERATED ALWAYS AS (LEFT(JSON_UNQUOTE(JSON_EXTRACT(metadata, '" + c.path + "')), 255)) STORED;\n"
		if !strings.Contains(got, column) {
			t.Errorf("forkIndexesSQL() lacks the issues.%s column %q:\n%s", c.name, column, got)
		}
		index := "idx_issues_" + c.name + " (" + c.name + ", created_at)"
		if at := strings.Index(got, index); at < 0 {
			t.Errorf("idx_issues_%s is not (%s, created_at):\n%s", c.name, c.name, got)
		} else if at < lastColumn {
			t.Errorf("idx_issues_%s comes before a column it may need:\n%s", c.name, got)
		}
	}
	upper := strings.ToUpper(got)
	for _, banned := range []string{"PREPARE", "EXECUTE"} {
		if strings.Contains(upper, banned) {
			t.Errorf("forkIndexesSQL() uses %s:\n%s", banned, got)
		}
	}
	if n, want := strings.Count(got, ";\n"), 5+7; n != want { // five issues columns, seven issues indexes
		t.Errorf("forkIndexesSQL() has %d statement terminators, want %d:\n%s", n, want, got)
	}
	if !strings.HasSuffix(AllMigrationsSQL(), got) {
		t.Errorf("AllMigrationsSQL() does not end with forkIndexesSQL()")
	}
}

// expectForkIndexesPresent mocks needsForkIndexes on a database that already
// carries every fork column and index, committed: for issues (a table the
// step commits) one SHOW CREATE TABLE of the working table and one of HEAD,
// then for wisps (dolt_ignored) one SHOW COLUMNS and one SHOW INDEX, then for
// wisp_dependencies (dolt_ignored, no fork column) one SHOW INDEX.
func expectForkIndexesPresent(mock sqlmock.Sqlmock) {
	for _, table := range forkTables() {
		if forkCommittedTables[table] {
			expectForkCreateTable(mock, table, "", nil)
			expectForkCreateTable(mock, table, " AS OF 'HEAD'", nil)
			continue
		}
		if forkTableHasColumns(table) {
			expectForkColumnsPresent(mock, table)
		}
		expectForkIndexNamesPresent(mock, table)
	}
}

// expectForkStepPresent mocks ensureForkIndexes, the step itself, on the same
// database: one SHOW COLUMNS per column table (issues, wisps), then one SHOW
// INDEX per index table (issues, wisps, wisp_dependencies), and no DDL.
func expectForkStepPresent(mock sqlmock.Sqlmock) {
	for _, table := range forkTables() {
		if forkTableHasColumns(table) {
			expectForkColumnsPresent(mock, table)
		}
	}
	for _, table := range forkTables() {
		expectForkIndexNamesPresent(mock, table)
	}
}

func expectForkColumnsPresent(mock sqlmock.Sqlmock, table string) {
	rows := sqlmock.NewRows([]string{"Field", "Type", "Null", "Key", "Default", "Extra"}).
		AddRow("id", "varchar(255)", "NO", "PRI", nil, "")
	for _, col := range forkColumns {
		if col.Table == table {
			rows.AddRow(col.Name, "varchar(255)", "YES", "", nil, "")
		}
	}
	mock.ExpectQuery(regexp.QuoteMeta("SHOW COLUMNS FROM `" + table + "`")).WillReturnRows(rows)
}

func expectForkIndexNamesPresent(mock sqlmock.Sqlmock, table string) {
	rows := sqlmock.NewRows([]string{"Table", "Non_unique", "Key_name", "Seq_in_index", "Column_name"}).
		AddRow(table, 0, "PRIMARY", 1, "id")
	for _, idx := range forkIndexes {
		if idx.Table == table {
			rows.AddRow(table, 1, idx.Name, 1, "")
		}
	}
	mock.ExpectQuery(regexp.QuoteMeta("SHOW INDEX FROM `" + table + "`")).WillReturnRows(rows)
}

// expectForkCreateTable mocks SHOW CREATE TABLE of table (asOf "" or
// " AS OF 'HEAD'") printing every fork column and index of it but those in
// omit, in Dolt's form.
func expectForkCreateTable(mock sqlmock.Sqlmock, table, asOf string, omit map[string]bool) {
	lines := []string{"  `id` varchar(255) NOT NULL", "  `metadata` json", "  `created_at` datetime"}
	for _, col := range forkColumns {
		if col.Table == table && !omit[col.Name] {
			lines = append(lines, "  `"+col.Name+"` varchar(255) GENERATED ALWAYS AS (LEFT(json_unquote(json_extract(`metadata`, '$.x')), 255)) STORED")
		}
	}
	lines = append(lines, "  PRIMARY KEY (`id`)")
	for _, idx := range forkIndexes {
		if idx.Table == table && !omit[idx.Name] {
			lines = append(lines, "  KEY `"+idx.Name+"` (`created_at`)")
		}
	}
	ddl := "CREATE TABLE `" + table + "` (\n" + strings.Join(lines, ",\n") + "\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin"
	mock.ExpectQuery(regexp.QuoteMeta("SHOW CREATE TABLE `" + table + "`" + asOf)).
		WillReturnRows(sqlmock.NewRows([]string{"Table", "Create Table"}).AddRow(table, ddl))
}

// TestForkIndexesSkipAbsentWisps: a wisps table that does not exist yet (a
// fresh database before the ignored series makes it) needs no column or index:
// needsForkIndexes says false without a SHOW INDEX FROM wisps, and
// ensureForkIndexes runs no DDL and no error. A wisps whose column probe
// answers and whose index probe finds it gone counts as absent too. The
// wisp_dependencies index is probed after wisps either way (present here).
func TestForkIndexesSkipAbsentWisps(t *testing.T) {
	notFound := errors.New("Error 1146 (HY000): table not found: wisps")
	expectIssuesProbed := func(mock sqlmock.Sqlmock, probe string) {
		if probe == "needsForkIndexes" {
			expectForkCreateTable(mock, "issues", "", nil)
			expectForkCreateTable(mock, "issues", " AS OF 'HEAD'", nil)
			return
		}
		expectForkColumnsPresent(mock, "issues")
	}

	for _, probe := range []string{"needsForkIndexes", "ensureForkIndexes"} {
		t.Run(probe+"/no wisps", func(t *testing.T) {
			db, mock := newMockDB(t)
			expectIssuesProbed(mock, probe)
			mock.ExpectQuery(regexp.QuoteMeta("SHOW COLUMNS FROM `wisps`")).WillReturnError(notFound)
			var got bool
			var err error
			if probe == "needsForkIndexes" {
				expectForkIndexNamesPresent(mock, "wisp_dependencies")
				got, err = needsForkIndexes(context.Background(), db)
			} else {
				expectForkIndexNamesPresent(mock, "issues")
				expectForkIndexNamesPresent(mock, "wisp_dependencies")
				got, err = ensureForkIndexes(context.Background(), db, nil, nil)
			}
			if got || err != nil {
				t.Errorf("%s with no wisps = %v, %v; want false, nil", probe, got, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet SQL expectations: %v", err)
			}
		})
	}

	t.Run("needsForkIndexes/wisps gone before its index probe", func(t *testing.T) {
		db, mock := newMockDB(t)
		expectIssuesProbed(mock, "needsForkIndexes")
		expectForkColumnsPresent(mock, "wisps")
		mock.ExpectQuery(regexp.QuoteMeta("SHOW INDEX FROM `wisps`")).WillReturnError(notFound)
		expectForkIndexNamesPresent(mock, "wisp_dependencies")
		if got, err := needsForkIndexes(context.Background(), db); got || err != nil {
			t.Errorf("needsForkIndexes with wisps gone = %v, %v; want false, nil", got, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet SQL expectations: %v", err)
		}
	})
}

// TestForkIndexesSkipAbsentWispDependencies: a wisp_dependencies that does not
// exist (the ignored series has not made it) is no probe failure and gets no
// DDL: needsForkIndexes says false, and ensureForkIndexes runs no ALTER and
// returns no error. wisp_dependencies has no fork column, so neither function
// sends SHOW COLUMNS FROM it: its one probe is SHOW INDEX.
func TestForkIndexesSkipAbsentWispDependencies(t *testing.T) {
	notFound := errors.New("Error 1146 (HY000): table not found: wisp_dependencies")
	t.Run("needsForkIndexes", func(t *testing.T) {
		db, mock := newMockDB(t)
		expectForkCreateTable(mock, "issues", "", nil)
		expectForkCreateTable(mock, "issues", " AS OF 'HEAD'", nil)
		expectForkColumnsPresent(mock, "wisps")
		expectForkIndexNamesPresent(mock, "wisps")
		mock.ExpectQuery(regexp.QuoteMeta("SHOW INDEX FROM `wisp_dependencies`")).WillReturnError(notFound)
		if got, err := needsForkIndexes(context.Background(), db); got || err != nil {
			t.Errorf("needsForkIndexes with no wisp_dependencies = %v, %v; want false, nil", got, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet SQL expectations: %v", err)
		}
	})
	t.Run("ensureForkIndexes", func(t *testing.T) {
		db, mock := newMockDB(t)
		expectForkColumnsPresent(mock, "issues")
		expectForkColumnsPresent(mock, "wisps")
		expectForkIndexNamesPresent(mock, "issues")
		expectForkIndexNamesPresent(mock, "wisps")
		mock.ExpectQuery(regexp.QuoteMeta("SHOW INDEX FROM `wisp_dependencies`")).WillReturnError(notFound)
		if got, err := ensureForkIndexes(context.Background(), db, nil, nil); got || err != nil {
			t.Errorf("ensureForkIndexes with no wisp_dependencies = %v, %v; want false, nil", got, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet SQL expectations: %v", err)
		}
	})
}
