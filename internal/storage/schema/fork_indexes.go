package schema

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"

	"github.com/steveyegge/beads/internal/storage/dberrors"
)

// Fork-only secondary indexes on issues, added by an idempotent ensure step
// rather than a numbered migration.
//
// Why: everturn's bridge runs queries against the shared bd database that
// scan every closed issue, 0.3-0.8s each and several per bridge pass: a
// closed-bead history page ordered or ranged on issues.closed_at. These
// indexes turn those scans into index reads.
//
// Why not a numbered migration file: a pending migration is one whose version
// is above MAX(version) in schema_migrations (pendingVersions), and shipped
// files are frozen with content hashes (scripts/check-migration-hygiene.sh).
// A fork 0068 would collide with upstream's next 0068 (duplicate version, and
// hash skew on every upgraded clone); a high number such as 0900 would hide
// every later upstream migration behind the cursor. So the indexes live here,
// run inside the migration pass (migrateUpAfterReconcile), and are emitted in
// the fresh-schema CLI bundle (AllMigrationsSQL) so the two agree.
//
// A deliberate choice: the step runs under the migration lock
// (MigrateUpWithLock) but NOT behind the remote-migrate gate, because it adds
// no pending version. That is safe only because it is additive (it creates
// indexes and never drops or alters anything) and idempotent (every index is
// probed by name first, and a racing opener's duplicate counts as success).
// Keep it that way.
//
// The everturn audit lookups filter JSON_EXTRACT(metadata, '$.everturn.action'),
// often with created_at. They read through a STORED generated column,
// everturn_action, and an ordinary index on (everturn_action, created_at),
// not a functional index: Dolt 2.2.0, the fleet's version, rejects any
// functional index on a table with an ON UPDATE column (issues.updated_at) and
// one that mixes an expression with a column. Queries must name the column
// (WHERE everturn_action = ?); Dolt uses the index only then, not for the raw
// JSON expression. The column is on issues AND wisps, so the two tables keep
// column parity (TestSchemaParityIssuesVsWisps); only issues is indexed.
// Columns are added before indexes, since an index needs its column.
//
// everturn's mail reads filter four more keys the same way (everturn_key,
// everturn_relay_source, everturn_bead, everturn_corrects: everturnKeyColumns),
// on issues and on wisps, so those four are indexed on both tables, each as
// (column, created_at). An index on a table that does not exist yet (wisps
// before the ignored series makes it) is skipped as its column is, and does
// not count as missing; the open after wisps is made adds both.
//
// STORED, not VIRTUAL: on Dolt 2.2.0 a VIRTUAL column breaks the issue upsert
// (issueops INSERT ... ON DUPLICATE KEY UPDATE col = VALUES(col)) for an
// existing row, VALUES() shifting by one column ("'task' is not a valid value
// for 'int'"). Adding a STORED column rewrites every row once, under the
// migration lock. LEFT(..., 255): without it, a write whose action renders
// longer than the VARCHAR(255) (a long string, or an object) is rejected; with
// it the stored value is truncated to 255 characters and the write succeeds.

type forkColumn struct{ Table, Name, DDL string } // DDL: the whole ALTER TABLE ADD COLUMN statement

// everturnKeyColumns are the generated columns everturn reads through an
// index, by column name and the metadata path it holds. The names and paths
// are everturn's reader contract: do not rename. everturn_action serves the
// audit lookups; the other four serve the mail reads, which filter on a key
// inside metadata and would otherwise read every message row (bd-iqmdg).
var everturnKeyColumns = []struct{ Name, Path string }{
	{Name: "everturn_action", Path: "$.everturn.action"},
	{Name: "everturn_key", Path: "$.everturn.key"},
	{Name: "everturn_relay_source", Path: "$.everturn.relay.source"},
	{Name: "everturn_bead", Path: "$.everturn.bead"},
	{Name: "everturn_corrects", Path: "$.everturn.corrects"},
}

// everturnKeyColumnDef is the column definition of an everturnKeyColumns
// entry: STORED and cut to 255 characters (see above). A key that is not a
// string (an array, an object) stores its JSON text.
func everturnKeyColumnDef(name, path string) string {
	return name + " VARCHAR(255) GENERATED ALWAYS AS (LEFT(JSON_UNQUOTE(JSON_EXTRACT(metadata, '" + path + "')), 255)) STORED"
}

// forkColumns is every everturn key column on issues, then on wisps, one
// whole ALTER each: one ALTER adding five columns measured slower than five
// ALTERs. wisps is clone-local (dolt_ignore'd) and missing on a fresh database
// until the ignored series creates it; the fresh CLI bundle creates no wisps,
// so forkIndexesSQL leaves the wisps columns out.
var forkColumns = func() []forkColumn {
	var cols []forkColumn
	for _, table := range []string{"issues", "wisps"} {
		for _, c := range everturnKeyColumns {
			cols = append(cols, forkColumn{
				Table: table,
				Name:  c.Name,
				DDL:   "ALTER TABLE " + table + " ADD COLUMN " + everturnKeyColumnDef(c.Name, c.Path),
			})
		}
	}
	return cols
}()

type forkIndex struct{ Table, Name, DDL string } // DDL: the whole ALTER TABLE statement

// forkIndexes lists the issues indexes, then the wisps ones, then the
// wisp_dependencies one. Every everturn key column but everturn_action is
// indexed on both tables as (column, created_at); everturn_action is indexed
// on issues only.
var forkIndexes = func() []forkIndex {
	idx := []forkIndex{
		{
			Table: "issues",
			Name:  "idx_issues_closed_at",
			DDL:   "ALTER TABLE issues ADD INDEX idx_issues_closed_at (closed_at)",
		},
		{
			// Serves a status equality plus a closed_at range, e.g. the paged
			// status = 'closed' AND closed_at > ? ORDER BY status DESC, closed_at DESC
			// LIMIT n, as a reverse index read with no sort. Name status in the
			// ORDER BY: ordered by closed_at alone, Dolt 2.2.0 sorts the range (TopN)
			// while 2.4.2 does not. It does not remove the sort from an unbounded
			// status = 'closed' ORDER BY closed_at DESC LIMIT n: Dolt still reads
			// every closed row and sorts it (TopN), with or without this index.
			Table: "issues",
			Name:  "idx_issues_status_closed_at",
			DDL:   "ALTER TABLE issues ADD INDEX idx_issues_status_closed_at (status, closed_at)",
		},
	}
	for _, table := range []string{"issues", "wisps"} {
		for _, c := range everturnKeyColumns {
			if table == "wisps" && c.Name == "everturn_action" {
				continue
			}
			name := "idx_" + table + "_" + c.Name
			idx = append(idx, forkIndex{
				Table: table,
				Name:  name,
				DDL:   "ALTER TABLE " + table + " ADD INDEX " + name + " (" + c.Name + ", created_at)",
			})
		}
	}
	// The external branch of the dependents count and of the dependents rows
	// (depends_on_external = ? with no type) seeks this index; without it that
	// branch reads all of wisp_dependencies on every bd show --json (23,000 to
	// 24,000 rows on the fleet's databases, bd-1gxdn). Migration 0021 creates
	// wisp_dependencies already split, with no index led by depends_on_external
	// (its issue and wisp targets are led by their foreign keys' indexes); only
	// the upgrade paths 0047 and ignored/0003 add this one, and both skip a
	// table that already has the split columns. Name and definition are theirs,
	// so a database that came through them already has it and the step sends no
	// DDL there; the step goes by name, so an index of that name with another
	// column list is left as it is. wisp_dependencies is dolt_ignored like wisps:
	// each clone adds its own, and nothing is staged or committed for it. It has
	// no fork column, so the probe reads only its SHOW INDEX.
	idx = append(idx, forkIndex{
		Table: "wisp_dependencies",
		Name:  "idx_wisp_dep_external_target",
		DDL:   "ALTER TABLE wisp_dependencies ADD INDEX idx_wisp_dep_external_target (depends_on_external)",
	})
	return idx
}()

// needsForkIndexes reports whether any listed column or index is missing. A
// column or index on a table that does not exist yet does not count, and a
// table found absent gets no further probe. It runs on every open
// (migrationWorkNeeded, alreadyConverged), so it reads SHOWs, two per table
// (one, SHOW INDEX, for a table with no fork column: wisp_dependencies): an
// INFORMATION_SCHEMA query costs ~20ms on a server hosting many databases, a
// SHOW under 1ms.
//
// A table the step commits (forkCommittedTables: issues) is read twice, as
// SHOW CREATE TABLE of the working table and of HEAD, and work is needed when
// either lacks a fork column or index (bd-7bqv6, review bd-313ml finding 1).
// Read in the working table alone, an upgrade cancelled after its last ALTER
// and before its commit looked complete, so no open ever ran the pass that
// commits it: HEAD kept the old schema and issues stayed dirty. A HEAD that
// lacks the fork objects of a working table that holds them is that lost
// commit (or a step under a user's uncommitted rows, which the pass finishes
// and leaves to the user's commit); the pass classifies the table
// (classifyForkDirtyTables) and commits only the step's own DDL. SHOW INDEX
// has no AS OF form, so the HEAD read is SHOW CREATE TABLE, which carries the
// columns and the indexes in one statement; the working read is the same
// statement in place of SHOW COLUMNS and SHOW INDEX, so the open sends as many
// statements as before. A table that is not at HEAD (a database whose first
// pass never committed) counts by its working table alone. A dolt_ignored
// table (wisps) is never at HEAD: SHOW COLUMNS and SHOW INDEX, as before.
//
// Names are compared without regard to case, as the server compares them
// (review bd-8b72l finding 3): an IDX_WISP_DEP_EXTERNAL_TARGET counts as the
// step's index, where an exact look-up sent every writable open down the
// locked path to retry a duplicate ALTER.
func needsForkIndexes(ctx context.Context, db DBConn) (bool, error) {
	need, err := forkIndexesNeed(ctx, db)
	return need != forkNeedNone, err
}

// forkNeed is what needsForkIndexes found missing.
type forkNeed int

const (
	forkNeedNone forkNeed = iota
	// forkNeedCloneLocal: only objects on dolt_ignored, clone-local tables
	// (wisps, wisp_dependencies) are missing. They are never staged or
	// committed, so adding them needs none of the whole pass
	// (ensureCloneLocalForkObjects, bd-30lnk), when this database ignores
	// them: migrationWork asks dolt_ignore before taking that step (review
	// bd-8fwl1 finding 2).
	forkNeedCloneLocal
	// forkNeedCommitted: an object on a table the pass commits (issues) is
	// missing from its working table or from HEAD: the whole pass.
	forkNeedCommitted
)

// forkIndexesNeed is needsForkIndexes's probe, telling a missing object on a
// committed table from one on a clone-local table only. The committed tables
// are probed first, so the answer costs no statement more than the yes/no
// question did: the first missing clone-local object ends the probe as before.
func forkIndexesNeed(ctx context.Context, db DBConn) (forkNeed, error) {
	need, _, err := forkIndexesNeedTables(ctx, db, false)
	return need, err
}

// forkIndexesNeedTables is forkIndexesNeed that, with all set, goes on past
// the first clone-local table lacking an object and also returns every
// clone-local table that lacks one, in forkTables order: migrationWork asks
// whether each is ignored in this database. It reads more than forkIndexesNeed
// only once an object is missing (the rest of the clone-local tables' SHOWs);
// on a database with every object present both send the same statements.
func forkIndexesNeedTables(ctx context.Context, db DBConn, all bool) (forkNeed, []string, error) {
	tables := forkTables()
	for _, table := range tables {
		if !forkCommittedTables[table] {
			continue
		}
		work, err := forkCreateTableNames(ctx, db, table, "")
		if err != nil {
			return forkNeedNone, nil, err
		}
		if work == nil {
			continue // the table does not exist
		}
		if work.lacksForkObject(table) {
			return forkNeedCommitted, nil, nil
		}
		head, err := forkCreateTableNames(ctx, db, table, " AS OF 'HEAD'")
		if err != nil {
			return forkNeedNone, nil, err
		}
		if head != nil && head.lacksForkObject(table) {
			return forkNeedCommitted, nil, nil
		}
	}
	var lacking []string
	for _, table := range tables {
		if forkCommittedTables[table] {
			continue
		}
		lacks, err := cloneLocalTableLacksForkObject(ctx, db, table)
		if err != nil {
			return forkNeedNone, nil, err
		}
		if !lacks {
			continue
		}
		if !all {
			return forkNeedCloneLocal, nil, nil
		}
		lacking = append(lacking, table)
	}
	if len(lacking) > 0 {
		return forkNeedCloneLocal, lacking, nil
	}
	return forkNeedNone, nil, nil
}

// cloneLocalTableLacksForkObject probes one table that is not at HEAD: its
// SHOW COLUMNS (when it has a fork column), then its SHOW INDEX; a missing
// column ends the probe. A table that does not exist lacks nothing.
func cloneLocalTableLacksForkObject(ctx context.Context, db DBConn, table string) (bool, error) {
	if forkTableHasColumns(table) {
		columns, err := forkTableColumnNames(ctx, db, table)
		if err != nil {
			return false, err
		}
		if columns == nil {
			return false, nil // the table does not exist
		}
		for _, col := range forkColumns {
			if col.Table == table && !columns[forkName(col.Name)] {
				return true, nil
			}
		}
	}
	if !forkTableHasIndexes(table) {
		return false, nil
	}
	indexes, err := forkTableIndexNames(ctx, db, table)
	if err != nil {
		return false, err
	}
	if indexes == nil {
		return false, nil // the table does not exist
	}
	for _, idx := range forkIndexes {
		if idx.Table == table && !indexes[forkName(idx.Name)] {
			return true, nil
		}
	}
	return false, nil
}

// forkName is the key of a column or index name in the step's name sets: the
// server compares those names without regard to case, so the step does too.
func forkName(name string) string { return strings.ToLower(name) }

// forkCommittedTables are the fork tables the migration pass stages and
// commits. wisps is dolt_ignored (clone-local): never staged, never at HEAD.
var forkCommittedTables = map[string]bool{"issues": true}

// forkTables is every table with a fork column or index, in forkColumns then
// forkIndexes order (issues, wisps, then wisp_dependencies).
func forkTables() []string {
	var tables []string
	seen := make(map[string]bool)
	add := func(table string) {
		if !seen[table] {
			seen[table] = true
			tables = append(tables, table)
		}
	}
	for _, col := range forkColumns {
		add(col.Table)
	}
	for _, idx := range forkIndexes {
		add(idx.Table)
	}
	return tables
}

func forkTableHasColumns(table string) bool {
	for _, col := range forkColumns {
		if col.Table == table {
			return true
		}
	}
	return false
}

func forkTableHasIndexes(table string) bool {
	for _, idx := range forkIndexes {
		if idx.Table == table {
			return true
		}
	}
	return false
}

// forkTableNames are the column and index names of one CREATE TABLE.
type forkTableNames struct{ columns, indexes map[string]bool }

func (n *forkTableNames) lacksForkObject(table string) bool {
	for _, col := range forkColumns {
		if col.Table == table && !n.columns[forkName(col.Name)] {
			return true
		}
	}
	for _, idx := range forkIndexes {
		if idx.Table == table && !n.indexes[forkName(idx.Name)] {
			return true
		}
	}
	return false
}

var createTableIndexRE = regexp.MustCompile("^(?:(?:UNIQUE|FULLTEXT|SPATIAL|VECTOR) )?KEY `([^`]+)` ")

// forkCreateTableNames reads the column and index names of table with one
// SHOW CREATE TABLE (asOf "" for the working table, " AS OF 'HEAD'" for HEAD),
// or nil when the table does not exist there.
func forkCreateTableNames(ctx context.Context, db DBConn, table, asOf string) (*forkTableNames, error) {
	ddl, err := showCreateTable(ctx, db, "SHOW CREATE TABLE `"+table+"`"+asOf)
	if err != nil {
		if dberrors.IsTableNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the definition of %s%s: %w", table, asOf, err)
	}
	names := &forkTableNames{columns: make(map[string]bool), indexes: make(map[string]bool)}
	for line := range createTableLines(ddl) {
		if m := forkColumnNameRE.FindStringSubmatch(line); m != nil {
			names.columns[forkName(m[1])] = true
		} else if m := createTableIndexRE.FindStringSubmatch(line); m != nil {
			names.indexes[forkName(m[1])] = true
		}
	}
	return names, nil
}

// forkTableColumnNames returns the column names on table (forkName keys),
// read with one SHOW COLUMNS, or nil when the table does not exist.
func forkTableColumnNames(ctx context.Context, db DBConn, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, "SHOW COLUMNS FROM `"+table+"`")
	if err != nil {
		if dberrors.IsTableNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing columns on %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("listing columns on %s: %w", table, err)
	}
	// SHOW COLUMNS returns Field, Type, Null, Key, Default, Extra (and possibly
	// more on some servers); scan every column into RawBytes and read the first
	// ("Field"), which is the column name.
	cells := make([]sql.RawBytes, len(cols))
	dest := make([]any, len(cols))
	for i := range cells {
		dest[i] = &cells[i]
	}
	names := make(map[string]bool)
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("listing columns on %s: %w", table, err)
		}
		if len(cells) > 0 {
			names[forkName(string(cells[0]))] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing columns on %s: %w", table, err)
	}
	return names, nil
}

// forkTableIndexNames returns the index names on table (forkName keys), read
// with one SHOW INDEX, or nil when the table does not exist.
func forkTableIndexNames(ctx context.Context, db DBConn, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, "SHOW INDEX FROM `"+table+"`")
	if err != nil {
		if dberrors.IsTableNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing indexes on %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("listing indexes on %s: %w", table, err)
	}
	// SHOW INDEX returns Table, Non_unique, Key_name, ... (the tail varies by
	// server); scan every column into RawBytes and read Key_name by name.
	keyCol := -1
	for i, col := range cols {
		if strings.EqualFold(col, "Key_name") {
			keyCol = i
		}
	}
	if keyCol < 0 {
		return nil, fmt.Errorf("listing indexes on %s: no Key_name column in %v", table, cols)
	}
	cells := make([]sql.RawBytes, len(cols))
	dest := make([]any, len(cols))
	for i := range cells {
		dest[i] = &cells[i]
	}
	names := make(map[string]bool)
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("listing indexes on %s: %w", table, err)
		}
		names[forkName(string(cells[keyCol]))] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing indexes on %s: %w", table, err)
	}
	return names, nil
}

// ensureForkIndexes adds every missing column, then every missing index. It
// returns changed=true when it ran any DDL. A table in dirtyBefore is skipped
// without error (the next open retries) so the changed-signature guard cannot
// trip, unless it is in resume: a table whose uncommitted changes are this
// step's own interrupted DDL plus a user's rows (classifyForkDirtyTables), on
// which the step finishes its columns and indexes and leaves the table
// uncommitted. A column or index on a table that does not exist yet is
// skipped too.
//
// dirtyBefore is the committable map (committableDirtyTables), so the skip is
// for committable tables only: a dolt_ignored table such as wisps is changed
// even when dirty. That is deliberate. wisps is clone-local, never staged or
// committed, so the changed-signature guard and the staging commit the skip
// protects never see it; and a wisps that stays dirty indefinitely would keep
// needsForkIndexes true, putting every open on the locked migration path.
func ensureForkIndexes(ctx context.Context, db DBConn, dirtyBefore map[string]dirtyTableState, resume map[string]bool) (changed bool, err error) {
	return ensureForkObjects(ctx, db, dirtyBefore, resume, func(string) bool { return true })
}

// ensureCloneLocalForkObjects adds the missing fork columns and indexes of the
// dolt_ignored, clone-local tables (wisps, wisp_dependencies) and of no other
// table. It is the whole of the migration work of an open whose only need is
// there (forkNeedCloneLocal, bd-30lnk, review bd-8b72l findings 1 and 2): those
// tables are never staged or committed, so the open takes none of the pass's
// steps for committed data. It does not unstage a user's staged tables, it
// does not refuse a dirty dependencies, it does not re-key dependency ids, and
// it runs no DOLT_ADD and no commit, so HEAD does not move. No table it
// touches is in a dirty-table map (they are not committable), so it runs with
// none.
func ensureCloneLocalForkObjects(ctx context.Context, db DBConn) (bool, error) {
	return ensureForkObjects(ctx, db, nil, nil, func(table string) bool { return !forkCommittedTables[table] })
}

// ensureForkObjects is ensureForkIndexes on the tables include accepts.
func ensureForkObjects(ctx context.Context, db DBConn, dirtyBefore map[string]dirtyTableState, resume map[string]bool, include func(table string) bool) (changed bool, err error) {
	skipped := make(map[string]bool)
	// skipDirty reports a committable table with uncommitted changes that are
	// not the step's own; a dolt_ignored table is never in dirtyBefore (see
	// above).
	skipDirty := func(table string) bool {
		if _, dirty := dirtyBefore[table]; !dirty || resume[table] {
			return false
		}
		if !skipped[table] {
			log.Printf("schema migration skipping fork indexes on %s: table has uncommitted changes; the next open retries", table)
			skipped[table] = true
		}
		return true
	}

	columns := make(map[string]map[string]bool)
	for _, col := range forkColumns {
		if !include(col.Table) || skipDirty(col.Table) {
			continue
		}
		names, ok := columns[col.Table]
		if !ok {
			if names, err = forkTableColumnNames(ctx, db, col.Table); err != nil {
				return changed, err
			}
			columns[col.Table] = names
		}
		if names == nil || names[forkName(col.Name)] {
			continue
		}
		if _, execErr := db.ExecContext(ctx, col.DDL); execErr != nil {
			// Same race as the indexes below: re-probe, do not match wording.
			exists, err := schemaColumnExists(ctx, db, col.Table, col.Name)
			if err != nil {
				return changed, err
			}
			if !exists {
				return changed, fmt.Errorf("adding %s.%s: %w", col.Table, col.Name, execErr)
			}
			continue
		}
		changed = true
	}

	present := make(map[string]map[string]bool)
	for _, idx := range forkIndexes {
		if !include(idx.Table) || skipDirty(idx.Table) {
			continue
		}
		if names, probed := columns[idx.Table]; probed && names == nil {
			continue // the table does not exist
		}
		names, ok := present[idx.Table]
		if !ok {
			if names, err = forkTableIndexNames(ctx, db, idx.Table); err != nil {
				return changed, err
			}
			present[idx.Table] = names
		}
		if names == nil || names[forkName(idx.Name)] {
			continue
		}
		if _, execErr := db.ExecContext(ctx, idx.DDL); execErr != nil {
			// A racing opener may have added it between the probe and the DDL:
			// re-probe rather than match the engine's duplicate-key wording.
			exists, err := schemaIndexExists(ctx, db, idx.Table, idx.Name)
			if err != nil {
				return changed, err
			}
			if !exists {
				return changed, fmt.Errorf("creating %s on %s: %w", idx.Name, idx.Table, execErr)
			}
			continue
		}
		changed = true
	}
	return changed, nil
}

// forkDirtyKind is what classifyForkDirtyTables makes of a committable fork
// table with uncommitted changes.
type forkDirtyKind int

const (
	// forkDirtyUser: the changes are not only the step's own (a user's rows
	// with no fork DDL, or any other schema change): skip the table.
	forkDirtyUser forkDirtyKind = iota
	// forkDirtyOwn: the only changes are fork columns and indexes added since
	// HEAD, the DDL of a step that was interrupted before its commit: finish,
	// stage and commit the table as an uninterrupted pass does.
	forkDirtyOwn
	// forkDirtyOwnAndRows: the step's interrupted DDL plus a user's
	// uncommitted rows: finish the columns and indexes so reads work, but
	// neither stage nor commit the table; the user's commit takes both.
	forkDirtyOwnAndRows
)

// forkDirtyTable is the classification of one dirty fork table. compare are
// its HEAD columns outside forkColumns, the columns a user's row change shows
// in; forkRowsSignature reads them. complete: the working table already holds
// every fork column and index with the step's definition, so the step has
// nothing to add there (the commit was lost, or a user's rows sit on a
// finished step).
type forkDirtyTable struct {
	kind     forkDirtyKind
	compare  []string
	complete bool
}

var forkColumnNameRE = regexp.MustCompile("^`([A-Za-z_][A-Za-z0-9_]*)` ")

// classifyForkDirtyTables tells, for each fork table in dirtyBefore, the step's
// own interrupted DDL from a user's change (bd-gcmmv, review bd-suuqd). Before
// this, an open killed between two of the step's ALTERs left issues dirty, so
// every later open skipped it as a user's table and reported success with
// columns missing for good.
//
// The test compares the table at HEAD with the working table. Its definition
// (SHOW CREATE TABLE ... AS OF 'HEAD' against SHOW CREATE TABLE, line by
// line) must differ only by added lines, each a fork column or fork index of
// that table with the step's own definition (forkOwnDefinitions: the
// generated expression with its JSON path, STORED and the type of a column;
// the column list of an index); any removed or other line, or a fork-named
// object defined otherwise, is a user's schema change (review bd-313ml
// finding 2: names alone let a user's everturn_key column or
// idx_issues_everturn_key (title) index be committed as the step's). Then its
// rows: a row the user changed differs from HEAD in a column outside the fork
// columns, compared byte for byte (forkRowsDifferPredicate), and an added
// STORED generated column marks the others modified with those columns
// equal, so one dolt_diff read with LIMIT 1 tells the two kinds apart.
//
// Cost: it runs only on a pass that already has work (migrationWorkNeeded)
// and only for a fork table in dolt_status, so an open with nothing to do
// sends nothing more. Then two SHOW CREATE TABLE; the dolt_diff read only when
// the definition differs by fork lines alone. That read can walk every row of
// the table (an added column rewrites them all), which happens only on the
// pass that resumes an interrupted step.
func classifyForkDirtyTables(ctx context.Context, db DBConn, dirtyBefore map[string]dirtyTableState) (map[string]forkDirtyTable, error) {
	tables := make(map[string]bool)
	for _, col := range forkColumns {
		tables[col.Table] = true
	}
	for _, idx := range forkIndexes {
		tables[idx.Table] = true
	}
	out := make(map[string]forkDirtyTable)
	for table := range tables {
		if _, dirty := dirtyBefore[table]; !dirty {
			continue
		}
		st, err := classifyForkDirtyTable(ctx, db, table)
		if err != nil {
			return nil, fmt.Errorf("classifying uncommitted changes on %s: %w", table, err)
		}
		out[table] = st
	}
	return out, nil
}

func classifyForkDirtyTable(ctx context.Context, db DBConn, table string) (forkDirtyTable, error) {
	user := forkDirtyTable{kind: forkDirtyUser}
	headDDL, err := showCreateTable(ctx, db, "SHOW CREATE TABLE `"+table+"` AS OF 'HEAD'")
	if err != nil {
		if dberrors.IsTableNotExist(err) {
			return user, nil // new since HEAD: not the step's
		}
		return user, err
	}
	workDDL, err := showCreateTable(ctx, db, "SHOW CREATE TABLE `"+table+"`")
	if err != nil {
		return user, err
	}
	head, work := createTableLines(headDDL), createTableLines(workDDL)
	own := forkOwnDefinitions(table)
	added := 0
	for line := range head {
		if !work[line] {
			return user, nil // a removed or changed line: a user's schema change
		}
	}
	for line := range work {
		if head[line] {
			continue
		}
		name, def, forkNamed := forkObjectOfLine(own, line)
		if !forkNamed {
			return user, nil // a user's column, index or other change
		}
		if normalizeDefinition(line) != def {
			// Named like one of the step's objects but defined otherwise: a
			// user's DDL (review bd-313ml finding 2), never committed as the
			// step's.
			log.Printf("schema migration skipping fork indexes on %s: %s has a foreign definition, not the fork step's: %s", table, name, line)
			return user, nil
		}
		added++
	}
	if added == 0 {
		return user, nil // the same definition: a user's rows
	}
	complete := true
	for prefix, d := range own {
		found := false
		for line := range work {
			if strings.HasPrefix(line, prefix) && normalizeDefinition(line) == d.def {
				found = true
				break
			}
		}
		if !found {
			complete = false
			break
		}
	}

	// Every HEAD column but the step's own generated ones: a fork-named
	// column at HEAD with another definition holds a user's values.
	var compare []string
	for line := range head {
		if m := forkColumnNameRE.FindStringSubmatch(line); m != nil {
			if d, isFork := own["`"+m[1]+"` "]; !isFork || normalizeDefinition(line) != d.def {
				compare = append(compare, m[1])
			}
		}
	}
	sort.Strings(compare)
	//nolint:gosec // table is a fork table name and columns match forkColumnNameRE.
	q := "SELECT 1 FROM dolt_diff('HEAD', 'WORKING', " + sqlStringLiteral(table) + ") WHERE " + forkRowsDifferPredicate(compare) + " LIMIT 1"
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return user, err
	}
	userRows := rows.Next()
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return user, err
	}
	if err := rows.Close(); err != nil {
		return user, err
	}
	if userRows {
		return forkDirtyTable{kind: forkDirtyOwnAndRows, compare: compare, complete: complete}, nil
	}
	return forkDirtyTable{kind: forkDirtyOwn, compare: compare, complete: complete}, nil
}

// forkRowsDifferPredicate matches a dolt_diff row the user changed: added,
// removed, or modified in a column outside the fork columns.
//
// Byte-exact (review bd-313ml finding 3): each pair is compared as
// CAST(... AS BINARY) <=> CAST(... AS BINARY), not as the bare columns. A
// bare <=> compares in the column's collation, so with a case-insensitive
// title a case-only edit read as unchanged, was taken into the schema
// commit, and was invisible to the changed-signature guard. A BINARY cast
// carries no collation and compares the bytes: of a TEXT or VARCHAR value, its
// stored characters (no case folding, accent folding or trailing-space
// padding); of a JSON value, its serialized text, which is the same for the
// unchanged document on both sides; of a number or a date, its decimal or
// date text. A cast of NULL is NULL and <=> stays null-safe: NULL equals only
// NULL, and NULL against ” differs.
func forkRowsDifferPredicate(compare []string) string {
	eq := make([]string, 0, len(compare))
	for _, c := range compare {
		eq = append(eq, "CAST(`from_"+c+"` AS BINARY) <=> CAST(`to_"+c+"` AS BINARY)")
	}
	if len(eq) == 0 {
		return "diff_type <> 'modified'"
	}
	return "diff_type <> 'modified' OR NOT (" + strings.Join(eq, " AND ") + ")"
}

// forkRowsSignature is the changed-signature guard's reading of a
// forkDirtyOwnAndRows table: the user's changed rows (forkRowsDifferPredicate)
// over the columns outside the fork columns. The step's DDL does not change
// it, since the fork columns are left out and the rows an added column marks
// modified are filtered out; a change of a user's row, or of any other row
// outside the fork columns, does.
func forkRowsSignature(ctx context.Context, db DBConn, table string, compare []string) (string, error) {
	cols := []string{"diff_type"}
	for _, c := range compare {
		cols = append(cols, "`from_"+c+"`", "`to_"+c+"`")
	}
	//nolint:gosec // table is a fork table name and columns match forkColumnNameRE.
	q := "SELECT " + strings.Join(cols, ", ") + " FROM dolt_diff('HEAD', 'WORKING', " + sqlStringLiteral(table) + ") WHERE " + forkRowsDifferPredicate(compare)
	return diffQuerySignature(ctx, db, q)
}

func showCreateTable(ctx context.Context, db DBConn, query string) (string, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	var name, ddl string
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("%s: no row", query)
	}
	if err := rows.Scan(&name, &ddl); err != nil {
		return "", err
	}
	return ddl, rows.Err()
}

// createTableLines is the set of a CREATE TABLE statement's lines, trimmed
// and without a trailing comma, so an added line does not change its
// neighbour's.
func createTableLines(ddl string) map[string]bool {
	lines := make(map[string]bool)
	for _, line := range strings.Split(ddl, "\n") {
		line = strings.TrimSuffix(strings.TrimSpace(line), ",")
		if line != "" {
			lines[line] = true
		}
	}
	return lines
}

// forkOwnDefinition is one of the step's columns or indexes: its name and its
// normalized definition line (normalizeDefinition), as SHOW CREATE TABLE
// prints it.
type forkOwnDefinition struct{ name, def string }

// forkOwnDefinitions are the step's own definitions on table, keyed by the
// line prefix SHOW CREATE TABLE prints for the object ("`name` " for a column,
// "KEY `name` " for an index), derived from
// the DDL it runs (forkColumns, forkIndexes), so they cannot drift from it:
// the column definition after "ADD COLUMN ", and "KEY " plus the index name
// and column list after "ADD INDEX ", each normalized (normalizeDefinition).
func forkOwnDefinitions(table string) map[string]forkOwnDefinition {
	own := make(map[string]forkOwnDefinition)
	for _, col := range forkColumns {
		if col.Table != table {
			continue
		}
		_, def, _ := strings.Cut(col.DDL, " ADD COLUMN ")
		own["`"+col.Name+"` "] = forkOwnDefinition{name: col.Name, def: normalizeDefinition(def)}
	}
	for _, idx := range forkIndexes {
		if idx.Table != table {
			continue
		}
		_, def, _ := strings.Cut(idx.DDL, " ADD INDEX ")
		own["KEY `"+idx.Name+"` "] = forkOwnDefinition{name: idx.Name, def: normalizeDefinition("KEY " + def)}
	}
	return own
}

// forkObjectOfLine reports whether a CREATE TABLE line names one of the
// step's objects (a column or an index line, any index kind), and that
// object's name and own normalized definition.
func forkObjectOfLine(own map[string]forkOwnDefinition, line string) (name, def string, ok bool) {
	var key string
	if m := forkColumnNameRE.FindStringSubmatch(line); m != nil {
		key = "`" + m[1] + "` "
	} else if m := createTableIndexRE.FindStringSubmatch(line); m != nil {
		key = "KEY `" + m[1] + "` "
	} else {
		return "", "", false
	}
	d, ok := own[key]
	return d.name, d.def, ok
}

// normalizeDefinition reduces a column or index definition to a form that
// is the same for the step's DDL and for what SHOW CREATE TABLE prints of it
// on Dolt 2.2.0 and 2.6.2: identifier backticks dropped, letters lowercased
// and whitespace runs made one space, or none next to "(", ")" and ",", all
// outside single-quoted strings, which are kept byte for byte (a JSON path is
// compared exactly). A UNIQUE KEY keeps its UNIQUE, so it differs from KEY.
func normalizeDefinition(s string) string {
	var b strings.Builder
	inQuote, space := false, false
	last := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote {
			b.WriteByte(c)
			switch {
			case c == '\\' && i+1 < len(s):
				i++
				b.WriteByte(s[i])
			case c == '\'' && i+1 < len(s) && s[i+1] == '\'':
				i++
				b.WriteByte(s[i])
			case c == '\'':
				inQuote = false
				last = c
			}
			continue
		}
		switch c {
		case '`':
			continue
		case ' ', '\t', '\n', '\r':
			space = true
			continue
		}
		if space && last != 0 && !strings.ContainsRune("(),", rune(last)) && !strings.ContainsRune("(),", rune(c)) {
			b.WriteByte(' ')
		}
		space = false
		if c == '\'' {
			inQuote = true
		} else if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
		last = c
	}
	return b.String()
}

// forkIndexesSQL returns the DDL statements, each ending in ";\n", for the
// fresh bundle: the issues columns, then the issues indexes. The wisps and
// wisp_dependencies columns and indexes are left to the runtime step, which
// adds them at the first writable open: those tables are clone-local.
func forkIndexesSQL() string {
	var b strings.Builder
	for _, col := range forkColumns {
		if col.Table != "issues" {
			continue
		}
		b.WriteString(col.DDL)
		b.WriteString(";\n")
	}
	for _, idx := range forkIndexes {
		if idx.Table != "issues" {
			continue
		}
		b.WriteString(idx.DDL)
		b.WriteString(";\n")
	}
	return b.String()
}
