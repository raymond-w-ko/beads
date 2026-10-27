//go:build cgo

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/dolt"
	"github.com/steveyegge/beads/internal/testutil"
)

// setupStrictExportStore opens a classic server-mode store in a fresh tmpDir
// (cwd moved there, BEADS_DIR at its .beads) and seeds one durable issue and
// one wisps-table row, each with a label, a dependency and a comment.
func setupStrictExportStore(t *testing.T) (*dolt.DoltStore, string) {
	t.Helper()
	if testDoltServerPort == 0 {
		t.Skip("Dolt test server not available")
	}
	if testutil.DoltContainerCrashed() {
		t.Skipf("Dolt test server crashed: %v", testutil.DoltContainerCrashError())
	}

	ensureTestMode(t)
	saveAndRestoreGlobals(t)

	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(tmpDir)
	t.Setenv("BEADS_DIR", beadsDir)

	testDBPath := filepath.Join(beadsDir, "dolt")
	writeTestMetadata(t, testDBPath, uniqueTestDBName(t))
	s := newTestStore(t, testDBPath)
	store = s
	storeMutex.Lock()
	storeActive = true
	storeMutex.Unlock()
	t.Cleanup(func() {
		store = nil
		storeMutex.Lock()
		storeActive = false
		storeMutex.Unlock()
	})

	ctx := context.Background()
	rootCtx = ctx
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type, created_by) VALUES (?, ?, '', '', '', '', 'open', 1, 'task', ?)`,
			[]any{"strict-1", "Durable issue", "carol"}},
		{`INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type, created_by) VALUES (?, ?, '', '', '', '', 'open', 1, 'task', ?)`,
			[]any{"strict-2", "Durable issue by alice", "alice"}},
		{`INSERT INTO labels (issue_id, label) VALUES (?, ?)`, []any{"strict-1", "durable-label"}},
		{`INSERT INTO dependencies (id, issue_id, depends_on_external, type, created_at, created_by) VALUES (UUID(), ?, ?, 'related', NOW(), 'test')`,
			[]any{"strict-1", "external:other:cap"}},
		{`INSERT INTO comments (id, issue_id, author, text, created_at) VALUES (UUID(), ?, 'test', ?, NOW())`,
			[]any{"strict-1", "durable comment"}},
		{`INSERT INTO wisps (id, title) VALUES (?, ?)`, []any{"strict-wisp-1", "Wisp row"}},
		{`INSERT INTO wisp_labels (issue_id, label) VALUES (?, ?)`, []any{"strict-wisp-1", "wisp-label"}},
		{`INSERT INTO wisp_dependencies (id, issue_id, depends_on_issue_id, type, created_at, created_by) VALUES (UUID(), ?, ?, 'related', NOW(), 'test')`,
			[]any{"strict-wisp-1", "strict-1"}},
		{`INSERT INTO wisp_comments (id, issue_id, author, text, created_at) VALUES (UUID(), ?, 'test', ?, NOW())`,
			[]any{"strict-wisp-1", "wisp comment"}},
	} {
		if _, err := s.DB().ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed %q: %v", q.sql, err)
		}
	}

	exportOutput = ""
	exportAll = true
	exportIncludeInfra = false
	exportScrub = false
	exportIncludeMemories = false
	exportNoMemories = false
	exportExcludeOwners = nil
	exportStrict = false
	return s, beadsDir
}

// runExportCaptured runs runExport with stdout and stderr captured.
func runExportCaptured(t *testing.T) (stdout, stderr string, err error) {
	t.Helper()
	oldStdout, oldStderr := os.Stdout, os.Stderr
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	os.Stdout, os.Stderr = outW, errW
	outCh := make(chan string)
	errCh := make(chan string)
	go func() { b, _ := io.ReadAll(outR); outCh <- string(b) }()
	go func() { b, _ := io.ReadAll(errR); errCh <- string(b) }()

	err = runExport(nil, nil)

	os.Stdout, os.Stderr = oldStdout, oldStderr
	outW.Close()
	errW.Close()
	return <-outCh, <-errCh, err
}

// exportedRecords parses JSONL issue lines keyed by id.
func exportedRecords(t *testing.T, out string) map[string]map[string]any {
	t.Helper()
	recs := map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("parse line %q: %v", line, err)
		}
		if rec["_type"] == "issue" {
			recs[rec["id"].(string)] = rec
		}
	}
	return recs
}

// failingLabelsStore fails the export's bulk label load. The search hydrates
// labels from the same tables with the same statement, so no DDL breaks the
// bulk load alone; this models a read that fails between the two (the classic
// loaders each run their own read transaction). Unwrap keeps the partitioner
// reachable for WispPlaneIDs.
type failingLabelsStore struct {
	storage.DoltStorage
}

func (f failingLabelsStore) GetLabelsForIssues(context.Context, []string) (map[string][]string, error) {
	return nil, errors.New("injected labels read failure")
}

func (f failingLabelsStore) Unwrap() storage.DoltStorage { return f.DoltStorage }

func TestExportStrictFailedRelationRead(t *testing.T) {
	tests := []struct {
		name     string
		ddl      string // empty: wrap the store in failingLabelsStore
		relation string // named in the strict error
		id       string // row whose relation reads empty without --strict
		field    string
	}{
		{"labels", "", "load labels", "strict-1", "labels"},
		{"issue dependencies", "RENAME TABLE dependencies TO dependencies_broken", "load dependency records", "strict-1", "dependencies"},
		{"issue comments", "RENAME TABLE comments TO comments_broken", "load comments", "strict-1", "comments"},
		// wisp_dependencies is an optional wisp table: a missing one reads as
		// empty, so break a column the loader selects instead.
		{"wisp dependencies", "ALTER TABLE wisp_dependencies RENAME COLUMN type TO type_broken", "load dependency records", "strict-wisp-1", "dependencies"},
		{"wisp comments", "RENAME TABLE wisp_comments TO wisp_comments_broken", "load comments", "strict-wisp-1", "comments"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := setupStrictExportStore(t)
			if tt.ddl == "" {
				store = failingLabelsStore{s}
			} else if _, err := s.DB().ExecContext(context.Background(), tt.ddl); err != nil {
				t.Fatalf("%s: %v", tt.ddl, err)
			}

			exportStrict = true
			stdout, stderr, err := runExportCaptured(t)
			if err == nil {
				t.Fatalf("strict export with %s succeeded; stdout:\n%s", tt.ddl, stdout)
			}
			if !strings.Contains(stderr, tt.relation) {
				t.Errorf("strict error does not name %q; stderr:\n%s", tt.relation, stderr)
			}
			if stdout != "" {
				t.Errorf("strict export printed lines before failing:\n%s", stdout)
			}

			// Without --strict the same export succeeds with the relation empty.
			exportStrict = false
			stdout, stderr, err = runExportCaptured(t)
			if err != nil {
				t.Fatalf("non-strict export: %v; stderr:\n%s", err, stderr)
			}
			recs := exportedRecords(t, stdout)
			for _, id := range []string{"strict-1", "strict-2", "strict-wisp-1"} {
				if recs[id] == nil {
					t.Errorf("non-strict export lacks %s; stdout:\n%s", id, stdout)
				}
			}
			if rec := recs[tt.id]; rec != nil {
				if v, ok := rec[tt.field]; ok {
					t.Errorf("non-strict export of %s has %s = %v, want it empty", tt.id, tt.field, v)
				}
			}
		})
	}
}

// dropWispPlane drops the wisp tables, first removing other tables' foreign
// keys into wisps (wisp_child_counters), which block the drop.
func dropWispPlane(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	drop := []string{"wisp_labels", "wisp_comments", "wisp_dependencies", "wisps"}
	rows, err := db.QueryContext(ctx, `SELECT TABLE_NAME, CONSTRAINT_NAME FROM information_schema.REFERENTIAL_CONSTRAINTS
		WHERE CONSTRAINT_SCHEMA = DATABASE() AND REFERENCED_TABLE_NAME = 'wisps'`)
	if err != nil {
		t.Fatalf("list foreign keys into wisps: %v", err)
	}
	var fks [][2]string
	for rows.Next() {
		var table, name string
		if err := rows.Scan(&table, &name); err != nil {
			t.Fatal(err)
		}
		fks = append(fks, [2]string{table, name})
	}
	_ = rows.Close()
	for _, fk := range fks {
		if slices.Contains(drop, fk[0]) {
			continue
		}
		if _, err := db.ExecContext(ctx, "ALTER TABLE "+fk[0]+" DROP FOREIGN KEY "+fk[1]); err != nil {
			t.Fatalf("drop foreign key %s.%s: %v", fk[0], fk[1], err)
		}
	}
	for _, table := range drop {
		if _, err := db.ExecContext(ctx, "DROP TABLE "+table); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
	}
}

func TestExportStrictMissingWispsTableIsEmpty(t *testing.T) {
	s, _ := setupStrictExportStore(t)
	dropWispPlane(t, s.DB())

	exportStrict = true
	stdout, stderr, err := runExportCaptured(t)
	if err != nil {
		t.Fatalf("strict export without wisp tables: %v; stderr:\n%s", err, stderr)
	}
	recs := exportedRecords(t, stdout)
	if len(recs) != 2 || recs["strict-1"] == nil || recs["strict-2"] == nil {
		t.Fatalf("want the two durable issues, got:\n%s", stdout)
	}
	if labels, _ := recs["strict-1"]["labels"].([]any); len(labels) != 1 {
		t.Errorf("strict-1 labels = %v, want [durable-label]", recs["strict-1"]["labels"])
	}
}

func TestExportStrictOwnerExclusionRefused(t *testing.T) {
	loadYaml := func(t *testing.T, beadsDir, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("BEADS_TEST_IGNORE_REPO_CONFIG", "1")
		config.ResetForTesting()
		t.Cleanup(config.ResetForTesting)
		if err := config.Initialize(); err != nil {
			t.Fatalf("config.Initialize: %v", err)
		}
	}

	tests := []struct {
		name  string
		setup func(t *testing.T, s *dolt.DoltStore, beadsDir string)
		want  string
	}{
		{"exclude_owners in config.yaml", func(t *testing.T, _ *dolt.DoltStore, beadsDir string) {
			loadYaml(t, beadsDir, "export:\n  exclude_owners: carol\n")
		}, "export.exclude_owners is set in config.yaml"},
		{"exclude_owner in config.yaml", func(t *testing.T, _ *dolt.DoltStore, beadsDir string) {
			loadYaml(t, beadsDir, "export:\n  exclude_owner: carol\n")
		}, "export.exclude_owner is set in config.yaml"},
		{"exclude_owners in database", func(t *testing.T, s *dolt.DoltStore, _ string) {
			if err := s.SetConfig(context.Background(), "export.exclude_owners", "carol"); err != nil {
				t.Fatal(err)
			}
		}, "export.exclude_owners is set in the database"},
		{"exclude_owner in database", func(t *testing.T, s *dolt.DoltStore, _ string) {
			if err := s.SetConfig(context.Background(), "export.exclude_owner", "carol"); err != nil {
				t.Fatal(err)
			}
		}, "export.exclude_owner is set in the database"},
		{"exclude-owner flag", func(t *testing.T, _ *dolt.DoltStore, _ string) {
			exportExcludeOwners = []string{"carol"}
		}, "--exclude-owner is set"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, beadsDir := setupStrictExportStore(t)
			tt.setup(t, s, beadsDir)

			exportStrict = true
			outFile := filepath.Join(t.TempDir(), "strict.jsonl")
			exportOutput = outFile
			stdout, stderr, err := runExportCaptured(t)
			if err == nil {
				t.Fatalf("strict export with an owner exclusion succeeded")
			}
			if !strings.Contains(stderr, tt.want) {
				t.Errorf("stderr does not name %q:\n%s", tt.want, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout not empty:\n%s", stdout)
			}
			if _, statErr := os.Stat(outFile); !os.IsNotExist(statErr) {
				t.Errorf("output file created despite the refusal (stat err %v)", statErr)
			}

			// Without --strict the exclusion still filters, as today.
			exportStrict = false
			exportOutput = ""
			stdout, stderr, err = runExportCaptured(t)
			if err != nil {
				t.Fatalf("non-strict export: %v; stderr:\n%s", err, stderr)
			}
			recs := exportedRecords(t, stdout)
			if recs["strict-1"] != nil || recs["strict-2"] == nil {
				t.Errorf("non-strict export did not exclude carol's issue only:\n%s", stdout)
			}
		})
	}
}

func TestExportStrictFailedConfigRead(t *testing.T) {
	s, _ := setupStrictExportStore(t)
	if _, err := s.DB().ExecContext(context.Background(), "RENAME TABLE config TO config_broken"); err != nil {
		t.Fatalf("rename config: %v", err)
	}

	exportStrict = true
	stdout, stderr, err := runExportCaptured(t)
	if err == nil {
		t.Fatalf("strict export with an unreadable config table succeeded; stdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, "read export.exclude_owners from database") {
		t.Errorf("strict error does not name the config read; stderr:\n%s", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout not empty:\n%s", stdout)
	}
}

// TestExportStrictProxied runs the strict policy through the proxied-server
// entry point (uowExportSource), as a real bd process against the shared
// proxied server. Its wisp-plane readers tolerate a missing table, which must
// mean an absent wisps plane, never a wisp_comments read that failed while
// wisps exist (review bd-r1vsh).
func TestExportStrictProxied(t *testing.T) {
	requireSharedProxiedServer(t)
	bd := buildEmbeddedBD(t)

	// seed creates a durable issue and an ephemeral wisp with a comment, and
	// checks a healthy strict export carries the comment.
	seed := func(t *testing.T, prefix string) (proxiedProject, string, string) {
		t.Helper()
		p := newSharedProxiedProject(t, bd, prefix)
		durable := bdProxiedCreateSilent(t, bd, p.dir, "Durable issue")
		wisp := bdProxiedCreateSilent(t, bd, p.dir, "Snapshot wisp", "--ephemeral", "--wisp-type", "heartbeat")
		if out, err := bdProxiedRun(t, bd, p.dir, "comment", wisp, "must survive export"); err != nil {
			t.Fatalf("seed comment: %v\n%s", err, out)
		}
		out, stderr, err := bdProxiedRunBuffers(t, bd, p.dir, "export", "--all", "--strict")
		if err != nil {
			t.Fatalf("healthy strict export: %v\n%s", err, stderr)
		}
		recs := exportLines(t, out)
		if exportIssueRecordByID(recs, durable) == nil {
			t.Fatalf("healthy strict export lacks %s:\n%s", durable, out)
		}
		if rec := exportIssueRecordByID(recs, wisp); rec == nil || len(exportCommentTexts(rec["comments"])) != 1 {
			t.Fatalf("healthy strict export lacks the wisp's comment:\n%s", out)
		}
		return p, durable, wisp
	}

	t.Run("unreadable wisp comments", func(t *testing.T) {
		p, _, wisp := seed(t, "spc")
		if _, err := openProxiedDB(t, p).Exec("RENAME TABLE wisp_comments TO wisp_comments_broken"); err != nil {
			t.Fatal(err)
		}

		out, stderr, err := bdProxiedRunBuffers(t, bd, p.dir, "export", "--all", "--strict")
		if err == nil {
			t.Fatalf("strict export succeeded despite unreadable wisp_comments:\nstdout:\n%s\nstderr:\n%s", out, stderr)
		}
		if !strings.Contains(stderr, "load wisp comments") {
			t.Errorf("strict error does not name the wisp comments read:\n%s", stderr)
		}
		if out != "" {
			t.Errorf("strict export printed lines before failing:\n%s", out)
		}

		// Without --strict the wisp is exported without its comments, as today.
		out, stderr, err = bdProxiedRunBuffers(t, bd, p.dir, "export", "--all")
		if err != nil {
			t.Fatalf("non-strict export: %v\n%s", err, stderr)
		}
		rec := exportIssueRecordByID(exportLines(t, out), wisp)
		if rec == nil || len(exportCommentTexts(rec["comments"])) != 0 {
			t.Errorf("non-strict export: want the wisp without comments, got:\n%s", out)
		}
	})

	t.Run("missing wisps plane", func(t *testing.T) {
		p, durable, _ := seed(t, "spm")
		dropWispPlane(t, openProxiedDB(t, p))

		out, stderr, err := bdProxiedRunBuffers(t, bd, p.dir, "export", "--all", "--strict")
		if err != nil {
			t.Fatalf("strict export without the wisps plane: %v\n%s", err, stderr)
		}
		recs := exportLines(t, out)
		if exportIssueRecordByID(recs, durable) == nil {
			t.Errorf("strict export without the wisps plane lacks %s:\n%s", durable, out)
		}
	})

	t.Run("owner exclusion refused", func(t *testing.T) {
		p, _, _ := seed(t, "spo")
		for _, tc := range []struct {
			name  string
			setup func(t *testing.T)
			args  []string
			want  string
		}{
			{"flag", func(*testing.T) {}, []string{"--exclude-owner", "carol"}, "--exclude-owner is set"},
			{"database", func(t *testing.T) {
				if _, err := openProxiedDB(t, p).Exec("REPLACE INTO config (`key`, value) VALUES ('export.exclude_owner', 'carol')"); err != nil {
					t.Fatal(err)
				}
			}, nil, "export.exclude_owner is set in the database"},
			{"config.yaml", func(t *testing.T) {
				if out, err := bdProxiedRun(t, bd, p.dir, "config", "set", "export.exclude_owners", "carol"); err != nil {
					t.Fatalf("config set: %v\n%s", err, out)
				}
			}, nil, "export.exclude_owners is set in config.yaml"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				tc.setup(t)
				args := append([]string{"export", "--all", "--strict"}, tc.args...)
				out, stderr, err := bdProxiedRunBuffers(t, bd, p.dir, args...)
				if err == nil {
					t.Fatalf("strict export with an owner exclusion succeeded:\n%s", out)
				}
				if !strings.Contains(stderr, tc.want) {
					t.Errorf("stderr does not name %q:\n%s", tc.want, stderr)
				}
				if out != "" {
					t.Errorf("stdout not empty:\n%s", out)
				}
			})
		}
	})
}
