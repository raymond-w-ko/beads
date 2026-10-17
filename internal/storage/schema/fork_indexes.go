package schema

import (
	"context"
	"database/sql"
	"fmt"
	"log"
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
// STORED, not VIRTUAL: on Dolt 2.2.0 a VIRTUAL column breaks the issue upsert
// (issueops INSERT ... ON DUPLICATE KEY UPDATE col = VALUES(col)) for an
// existing row, VALUES() shifting by one column ("'task' is not a valid value
// for 'int'"). Adding a STORED column rewrites every row once, under the
// migration lock. LEFT(..., 255): without it, a write whose action renders
// longer than the VARCHAR(255) (a long string, or an object) is rejected; with
// it the stored value is truncated to 255 characters and the write succeeds.

type forkColumn struct{ Table, Name, DDL string } // DDL: the whole ALTER TABLE ADD COLUMN statement

const everturnActionColumnDef = "everturn_action VARCHAR(255) GENERATED ALWAYS AS (LEFT(JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.everturn.action')), 255)) STORED"

var forkColumns = []forkColumn{
	{
		Table: "issues",
		Name:  "everturn_action",
		DDL:   "ALTER TABLE issues ADD COLUMN " + everturnActionColumnDef,
	},
	{
		// wisps is clone-local (dolt_ignore'd) and missing on a fresh database
		// until the ignored series creates it; the fresh CLI bundle creates no
		// wisps, so forkIndexesSQL leaves this one out.
		Table: "wisps",
		Name:  "everturn_action",
		DDL:   "ALTER TABLE wisps ADD COLUMN " + everturnActionColumnDef,
	},
}

type forkIndex struct{ Table, Name, DDL string } // DDL: the whole ALTER TABLE statement

var forkIndexes = []forkIndex{
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
	{
		Table: "issues",
		Name:  "idx_issues_everturn_action",
		DDL:   "ALTER TABLE issues ADD INDEX idx_issues_everturn_action (everturn_action, created_at)",
	},
}

// needsForkIndexes reports whether any listed column or index is missing. A
// column on a table that does not exist yet does not count. It runs on every
// open (migrationWorkNeeded, alreadyConverged), so it reads one SHOW COLUMNS
// and one SHOW INDEX per table: an INFORMATION_SCHEMA query costs ~20ms on a
// server hosting many databases, a SHOW under 1ms.
func needsForkIndexes(ctx context.Context, db DBConn) (bool, error) {
	columns := make(map[string]map[string]bool)
	for _, col := range forkColumns {
		names, ok := columns[col.Table]
		if !ok {
			var err error
			if names, err = forkTableColumnNames(ctx, db, col.Table); err != nil {
				return false, err
			}
			columns[col.Table] = names
		}
		if names != nil && !names[col.Name] {
			return true, nil
		}
	}
	present := make(map[string]map[string]bool)
	for _, idx := range forkIndexes {
		names, ok := present[idx.Table]
		if !ok {
			var err error
			if names, err = forkTableIndexNames(ctx, db, idx.Table); err != nil {
				return false, err
			}
			present[idx.Table] = names
		}
		if !names[idx.Name] {
			return true, nil
		}
	}
	return false, nil
}

// forkTableColumnNames returns the column names on table, read with one
// SHOW COLUMNS, or nil when the table does not exist.
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
			names[string(cells[0])] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing columns on %s: %w", table, err)
	}
	return names, nil
}

// forkTableIndexNames returns the index names on table, read with one
// SHOW INDEX. A missing table has no indexes, as schemaIndexExists reports.
func forkTableIndexNames(ctx context.Context, db DBConn, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, "SHOW INDEX FROM `"+table+"`")
	if err != nil {
		if dberrors.IsTableNotExist(err) {
			return map[string]bool{}, nil
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
		names[string(cells[keyCol])] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing indexes on %s: %w", table, err)
	}
	return names, nil
}

// ensureForkIndexes adds every missing column, then every missing index. It
// returns changed=true when it ran any DDL. A table in dirtyBefore is skipped
// without error (the next open retries) so the changed-signature guard cannot
// trip, and a column on a table that does not exist yet is skipped too.
//
// dirtyBefore is the committable map (committableDirtyTables), so the skip is
// for committable tables only: a dolt_ignored table such as wisps is changed
// even when dirty. That is deliberate. wisps is clone-local, never staged or
// committed, so the changed-signature guard and the staging commit the skip
// protects never see it; and a wisps that stays dirty indefinitely would keep
// needsForkIndexes true, putting every open on the locked migration path.
func ensureForkIndexes(ctx context.Context, db DBConn, dirtyBefore map[string]dirtyTableState) (changed bool, err error) {
	skipped := make(map[string]bool)
	// skipDirty reports a committable table with uncommitted changes; a
	// dolt_ignored table is never in dirtyBefore (see above).
	skipDirty := func(table string) bool {
		if _, dirty := dirtyBefore[table]; !dirty {
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
		if skipDirty(col.Table) {
			continue
		}
		names, ok := columns[col.Table]
		if !ok {
			if names, err = forkTableColumnNames(ctx, db, col.Table); err != nil {
				return changed, err
			}
			columns[col.Table] = names
		}
		if names == nil || names[col.Name] {
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
		if skipDirty(idx.Table) {
			continue
		}
		names, ok := present[idx.Table]
		if !ok {
			if names, err = forkTableIndexNames(ctx, db, idx.Table); err != nil {
				return changed, err
			}
			present[idx.Table] = names
		}
		if names[idx.Name] {
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

// forkIndexesSQL returns the DDL statements, each ending in ";\n", for the
// fresh bundle: the issues columns, then the indexes. The bundle creates no
// wisps, so the wisps column is left to the runtime step.
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
		b.WriteString(idx.DDL)
		b.WriteString(";\n")
	}
	return b.String()
}
