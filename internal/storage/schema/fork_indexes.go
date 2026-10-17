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
// Only issues is indexed, and no column is added, so issues/wisps column
// parity is untouched. A functional index for the everturn metadata action
// lookup is deliberately absent: Dolt 2.2.0 rejects any functional index on a
// table with an ON UPDATE column (issues.updated_at), and it also rejects one
// that mixes an expression with a column.

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
}

// needsForkIndexes reports whether any listed index is missing. It runs on
// every open (migrationWorkNeeded, alreadyConverged), so it reads one
// SHOW INDEX per table: an INFORMATION_SCHEMA.STATISTICS query costs ~20ms on
// a server hosting many databases, SHOW INDEX under 1ms.
func needsForkIndexes(ctx context.Context, db DBConn) (bool, error) {
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

// ensureForkIndexes adds every missing index. It returns changed=true when it
// ran any DDL. When table "issues" is in dirtyBefore it skips without error
// (the next open retries) so the changed-signature guard cannot trip.
func ensureForkIndexes(ctx context.Context, db DBConn, dirtyBefore map[string]dirtyTableState) (changed bool, err error) {
	present := make(map[string]map[string]bool)
	skipped := make(map[string]bool)
	for _, idx := range forkIndexes {
		if _, dirty := dirtyBefore[idx.Table]; dirty {
			if !skipped[idx.Table] {
				log.Printf("schema migration skipping fork indexes on %s: table has uncommitted changes; the next open retries", idx.Table)
				skipped[idx.Table] = true
			}
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

// forkIndexesSQL returns the DDL statements, each ending in ";\n", for the fresh bundle.
func forkIndexesSQL() string {
	var b strings.Builder
	for _, idx := range forkIndexes {
		b.WriteString(idx.DDL)
		b.WriteString(";\n")
	}
	return b.String()
}
