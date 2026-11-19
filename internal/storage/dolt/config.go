package dolt

import (
	"context"
	"database/sql"
	"fmt"
	"log"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// SetConfig sets a configuration value
func (s *DoltStore) SetConfig(ctx context.Context, key, value string) error {
	if err := s.withRetryTxNoJournal(ctx, func(tx *sql.Tx) error {
		if err := issueops.SetConfigInTx(ctx, tx, key, value); err != nil {
			return err
		}
		// Sync normalized tables when config keys change
		_, err := issueops.SyncConfigTables(ctx, tx, key, value)
		return err
	}); err != nil {
		return err
	}

	// Invalidate caches for keys that affect cached data
	s.invalidateConfigCaches(key)

	return nil
}

// invalidateConfigCaches drops the store-level caches derived from a config
// key. Every path that writes config — store-level SetConfig and
// doltTransaction.SetConfig alike — must call this, or a long-lived process
// (server/daemon) keeps serving the pre-write set from GetCustomTypes /
// GetCustomStatuses / GetInfraTypes until restart. Invalidating for a
// transaction that later rolls back is harmless: the lazy reload just
// re-reads the committed state.
func (s *DoltStore) invalidateConfigCaches(key string) {
	s.cacheMu.Lock()
	switch key {
	case "status.custom":
		s.customStatusCached = false
		s.customStatusCache = nil
		s.customStatusDetailedCache = nil
	case "types.custom":
		s.customTypeCached = false
		s.customTypeCache = nil
	case "types.infra":
		s.infraTypeCached = false
		s.infraTypeCache = nil
	}
	s.cacheMu.Unlock()
}

// GetConfig retrieves a configuration value
func (s *DoltStore) GetConfig(ctx context.Context, key string) (string, error) {
	var value string
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		value, err = issueops.GetConfigInTx(ctx, tx, key)
		return err
	})
	return value, err
}

// GetAllConfig retrieves all configuration values. On a session with
// autocommit on it is one SELECT, which reads one snapshot on its own, so it
// runs without a transaction: the START TRANSACTION and ROLLBACK around it
// were two round trips on every bd list and bd ready (the routing read) and
// returned the same rows.
//
// With autocommit off (a server run with --no-auto-commit) a bare SELECT
// joins the implicit transaction an earlier bare statement on that pooled
// session left open and can return rows older than the latest commit (review
// bd-w6ppm, finding 2). The SELECT carries the session's @@autocommit, also
// for an empty table (allConfigOneStatementSQL's table-less branch), so such
// a session reads again with HEAD's three statements, START TRANSACTION, the
// SELECT and ROLLBACK: its START TRANSACTION ends the stale snapshot and
// reads the latest commit, and its ROLLBACK leaves no transaction open.
//
// The whole call runs on one connection pinned from the pool (review
// bd-ee2ci, finding 2): the bare SELECT opens an implicit transaction on a
// session with autocommit off, and only a transaction on that same session
// ends it. Read on the pool, the fallback could run on another connection
// and leave the first one's snapshot to its next reader. With autocommit on
// the pin sends nothing, and the call is the one SELECT.
func (s *DoltStore) GetAllConfig(ctx context.Context) (map[string]string, error) {
	if s.closed.Load() {
		return nil, ErrStoreClosed
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result map[string]string
	err := s.withRetry(ctx, func() error {
		conn, err := s.db.Conn(ctx)
		if err != nil {
			return fmt.Errorf("get all config: %w", err)
		}
		defer func() { _ = conn.Close() }()
		rows, fresh, err := getAllConfigOneStatement(ctx, conn)
		if err != nil {
			return err
		}
		if fresh {
			result = rows
			return nil
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin read tx: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		result, err = issueops.GetAllConfigInTx(ctx, tx)
		return err
	})
	return result, err
}

// allConfigOneStatementSQL is GetAllConfig's bare read: every config row,
// each with the session's autocommit, and one table-less row (NULL key, a
// value the table's NOT NULL primary key never holds) that carries the
// session's autocommit when the table is empty (review bd-ee2ci, finding 3:
// without it an empty table gave no answer and took the three-statement
// read on every call).
const allConfigOneStatementSQL = "SELECT @@autocommit, `key`, value FROM config UNION ALL SELECT @@autocommit, NULL, NULL"

// getAllConfigOneStatement reads the config table in one bare SELECT on
// conn. fresh reports that every row came from a session with autocommit
// on, so the rows are the latest commit's; false (autocommit off, or no row
// at all) means the caller must read again in a transaction on conn.
func getAllConfigOneStatement(ctx context.Context, conn *sql.Conn) (result map[string]string, fresh bool, err error) {
	rows, err := conn.QueryContext(ctx, allConfigOneStatementSQL)
	if err != nil {
		return nil, false, fmt.Errorf("get all config: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result = make(map[string]string)
	answered, autocommitOn := false, true
	for rows.Next() {
		var autocommit int64
		var k, v sql.NullString
		if err := rows.Scan(&autocommit, &k, &v); err != nil {
			return nil, false, fmt.Errorf("get all config: scan: %w", err)
		}
		answered = true
		autocommitOn = autocommitOn && autocommit != 0
		if !k.Valid {
			continue // the table-less row: autocommit only
		}
		if !v.Valid {
			// config.value is NOT NULL; HEAD's scan into a string fails here.
			return nil, false, fmt.Errorf("get all config: scan: NULL value for %q", k.String)
		}
		result[k.String] = v.String
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return result, answered && autocommitOn, nil
}

// DeleteConfig removes a configuration value
func (s *DoltStore) DeleteConfig(ctx context.Context, key string) error {
	return s.withRetryTxNoJournal(ctx, func(tx *sql.Tx) error {
		return issueops.DeleteConfigInTx(ctx, tx, key)
	})
}

// SetMetadata sets a metadata value
func (s *DoltStore) SetMetadata(ctx context.Context, key, value string) error {
	return s.withRetryTxNoJournal(ctx, func(tx *sql.Tx) error {
		return issueops.SetMetadataInTx(ctx, tx, key, value)
	})
}

// GetMetadata retrieves a metadata value
func (s *DoltStore) GetMetadata(ctx context.Context, key string) (string, error) {
	var value string
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		value, err = issueops.GetMetadataInTx(ctx, tx, key)
		return err
	})
	return value, err
}

// SetLocalMetadata sets a value in the dolt-ignored local_metadata table.
// Used for clone-local state that should not generate merge conflicts.
func (s *DoltStore) SetLocalMetadata(ctx context.Context, key, value string) error {
	return s.withRetryTxNoJournal(ctx, func(tx *sql.Tx) error {
		return issueops.SetLocalMetadataInTx(ctx, tx, key, value)
	})
}

// GetLocalMetadata retrieves a value from the dolt-ignored local_metadata table.
// Returns ("", nil) if the key does not exist.
func (s *DoltStore) GetLocalMetadata(ctx context.Context, key string) (string, error) {
	var value string
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		value, err = issueops.GetLocalMetadataInTx(ctx, tx, key)
		return err
	})
	return value, err
}

func (s *DoltStore) loadCustomConfigCache(ctx context.Context) {
	s.cacheMu.Lock()
	if s.customStatusCached && s.customTypeCached {
		s.cacheMu.Unlock()
		return
	}
	s.cacheMu.Unlock()

	var statuses []types.CustomStatus
	var customTypes []string
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var resolveErr error
		statuses, customTypes, resolveErr = issueops.ResolveCustomConfigInTx(ctx, tx)
		return resolveErr
	})
	if err != nil {
		log.Printf("warning: failed to resolve custom config: %v", err)
		if yamlStatuses := config.GetCustomStatusesFromYAML(); len(yamlStatuses) > 0 {
			statuses = issueops.ParseStatusFallback(yamlStatuses)
		}
		if yamlTypes := config.GetCustomTypesFromYAML(); len(yamlTypes) > 0 {
			customTypes = yamlTypes
		}
	}

	s.cacheMu.Lock()
	if !s.customStatusCached {
		s.customStatusDetailedCache = statuses
		s.customStatusCache = types.CustomStatusNames(statuses)
		s.customStatusCached = true
	}
	if !s.customTypeCached {
		s.customTypeCache = customTypes
		s.customTypeCached = true
	}
	s.cacheMu.Unlock()
}

func (s *DoltStore) GetCustomStatuses(ctx context.Context) ([]string, error) {
	s.loadCustomConfigCache(ctx)
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	return s.customStatusCache, nil
}

func (s *DoltStore) GetCustomStatusesDetailed(ctx context.Context) ([]types.CustomStatus, error) {
	s.loadCustomConfigCache(ctx)
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	return s.customStatusDetailedCache, nil
}

// GetCustomTypes returns custom issue type values from config.
// If the database doesn't have custom types configured, falls back to config.yaml.
// Returns an empty slice if no custom types are configured.
// Results are cached per DoltStore lifetime and invalidated when SetConfig
// updates the "types.custom" key.
func (s *DoltStore) GetCustomTypes(ctx context.Context) ([]string, error) {
	s.loadCustomConfigCache(ctx)
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	return s.customTypeCache, nil
}

// GetInfraTypes returns infrastructure type names from config.
// Infrastructure types are routed to the wisps table to keep the versioned
// issues table clean. Defaults to ["agent", "role", "message"] if
// no custom configuration exists.
// Falls back: DB config "types.infra" → config.yaml types.infra → defaults.
// Results are cached per DoltStore lifetime and invalidated when SetConfig
// updates the "types.infra" key.
func (s *DoltStore) GetInfraTypes(ctx context.Context) map[string]bool {
	s.cacheMu.Lock()
	if s.infraTypeCached {
		result := s.infraTypeCache
		s.cacheMu.Unlock()
		return result
	}
	s.cacheMu.Unlock()

	var result map[string]bool
	if err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		result = issueops.ResolveInfraTypesInTx(ctx, tx)
		return nil
	}); err != nil || result == nil {
		// DB unavailable — fall back to YAML then defaults.
		var typeList []string
		if yamlTypes := config.GetInfraTypesFromYAML(); len(yamlTypes) > 0 {
			typeList = yamlTypes
		} else {
			typeList = domain.DefaultInfraTypes()
		}
		result = make(map[string]bool, len(typeList))
		for _, t := range typeList {
			result[t] = true
		}
	}

	s.cacheMu.Lock()
	s.infraTypeCache = result
	s.infraTypeCached = true
	s.cacheMu.Unlock()

	return result
}
