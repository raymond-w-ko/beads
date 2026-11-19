//go:build cgo

package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/testutil"
)

// TestServerModeUpdateMergeMetadata is TestEmbeddedUpdateMergeMetadata
// (update_merge_metadata_embedded_test.go) on the fleet's route: `bd update`
// in server mode against a real dolt sql-server, through DoltStore. The flag
// twice applies both RFC 7396 patches in order (a null, then the new object,
// replaces a nested object whole) and keeps the siblings neither names; a
// name given twice in one object takes its last value; every refusal exits
// non-zero and leaves the row's metadata, its updated_at and the issue's
// events as they were. An integer past 2^53 at depth 2 keeps its digits beside
// a patched sibling, and a patch that changes only those digits is written,
// on a Dolt that keeps such digits (2.6 and later; the 2.2 harness image
// rounds them in the column, so there that assertion is logged and skipped).
// Its last subtest runs review bd-ldmiy's cases on the proxied route against
// the same container: respellings write nothing, a raw e9 byte is refused.
//
// Unlike TestServerModeDeferAutoWake, its model, it does not ask for
// BEADS_TEST_PROXIED_SERVER=1: it needs only the shared test Dolt container,
// which it starts on demand.
func TestServerModeUpdateMergeMetadata(t *testing.T) {
	port := sharedProxiedServerPort(t)
	// TestMain terminates the shared container only when it, or the proxied
	// suite, started it (test_dolt_server_cgo_test.go: startTestDoltServer). In
	// an embedded or proxied-local run without BEADS_TEST_PROXIED_SERVER=1 no
	// other test uses it, so this test is the one that started it and stops
	// it once its subtests are done; with TESTCONTAINERS_RYUK_DISABLED=true
	// nothing else would.
	if (os.Getenv("BEADS_TEST_EMBEDDED_DOLT") == "1" || os.Getenv("BEADS_TEST_PROXIED_LOCAL") == "1") &&
		os.Getenv("BEADS_TEST_PROXIED_SERVER") != "1" {
		t.Cleanup(testutil.TerminateDoltContainer)
	}

	bd := buildEmbeddedBD(t)
	p := newMergeMetadataServerProject(t, bd, port, "smm")
	db, err := sql.Open("mysql", fmt.Sprintf("root:@tcp(127.0.0.1:%d)/%s", port, p.database))
	if err != nil {
		t.Fatalf("open %s: %v", p.database, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var version string
	if err := db.QueryRow("SELECT dolt_version()").Scan(&version); err != nil {
		t.Fatalf("dolt_version(): %v", err)
	}
	keepsDigits := doltVersionAtLeast(t, version, 2, 6)
	t.Logf("test Dolt server: dolt_version() = %s", version)

	issue := bdCreate(t, bd, p.dir, "Merge metadata (server mode)", "--type", "task",
		"--metadata", `{"top":"seeded","jira.sprint":"s1","everturn":{"landing":{"old":1,"stale":true},"worktree":"wt/a"}}`)

	type shown struct {
		metadata  any
		updatedAt string
	}
	show := func() shown {
		t.Helper()
		var row struct {
			Metadata  json.RawMessage `json:"metadata"`
			UpdatedAt string          `json:"updated_at"`
		}
		if err := json.Unmarshal(parseShowJSON(t, bdShowJSON(t, bd, p.dir, issue.ID)), &row); err != nil {
			t.Fatalf("parse bd show: %v", err)
		}
		var metadata any
		if err := json.Unmarshal(row.Metadata, &metadata); err != nil {
			t.Fatalf("parse metadata %s: %v", row.Metadata, err)
		}
		return shown{metadata: metadata, updatedAt: row.UpdatedAt}
	}
	storedText := func() string {
		t.Helper()
		var text string
		if err := db.QueryRow("SELECT metadata FROM issues WHERE id = ?", issue.ID).Scan(&text); err != nil {
			t.Fatalf("read stored metadata: %v", err)
		}
		return text
	}
	eventCount := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM events WHERE issue_id = ?", issue.ID).Scan(&n); err != nil {
			t.Fatalf("count events: %v", err)
		}
		return n
	}
	asValue := func(text string) any {
		t.Helper()
		var value any
		if err := json.Unmarshal([]byte(text), &value); err != nil {
			t.Fatalf("parse %s: %v", text, err)
		}
		return value
	}

	t.Run("null_then_set_keeps_siblings", func(t *testing.T) {
		patchFile := filepath.Join(t.TempDir(), "landing.json")
		if err := os.WriteFile(patchFile, []byte(`{"everturn":{"landing":{"new":2}},"top":null}`), 0o600); err != nil {
			t.Fatalf("write patch file: %v", err)
		}
		bdUpdate(t, bd, p.dir, issue.ID,
			"--merge-metadata", `{"everturn":{"landing":null}}`,
			"--merge-metadata", "@"+patchFile)
		want := asValue(`{"jira.sprint":"s1","everturn":{"landing":{"new":2},"worktree":"wt/a"}}`)
		if got := show().metadata; !reflect.DeepEqual(got, want) {
			t.Fatalf("metadata after two merge patches = %v, want %v", got, want)
		}
	})

	t.Run("duplicate_name_last_wins", func(t *testing.T) {
		bdUpdate(t, bd, p.dir, issue.ID, "--merge-metadata", `{"everturn":{"worktree":"x","worktree":"wt/b"},"dup":1,"dup":null}`)
		want := asValue(`{"jira.sprint":"s1","everturn":{"landing":{"new":2},"worktree":"wt/b"}}`)
		if got := show().metadata; !reflect.DeepEqual(got, want) {
			t.Fatalf("metadata after a duplicate-name patch = %v, want %v", got, want)
		}
	})

	t.Run("refusals_write_nothing", func(t *testing.T) {
		before := show()
		eventsBefore := eventCount()
		tooDeep := `{"deep":` + strings.Repeat(`{"a":`, storage.MaxMetadataMergePatchDepth) + `1` + strings.Repeat(`}`, storage.MaxMetadataMergePatchDepth) + `}`
		for _, refusal := range []struct {
			name string
			args []string
			want string
		}{
			{"null", []string{"--merge-metadata", `null`}, "must be a JSON object, got null"},
			{"array", []string{"--merge-metadata", `[1]`}, "must be a JSON object, got an array"},
			{"string", []string{"--merge-metadata", `"x"`}, "must be a JSON object, got a string"},
			{"number", []string{"--merge-metadata", `1`}, "must be a JSON object, got a number"},
			{"invalid JSON", []string{"--merge-metadata", `{`}, "is not valid JSON"},
			{"second patch invalid", []string{"--merge-metadata", `{"must_not_persist":1}`, "--merge-metadata", `[]`}, "must be a JSON object"},
			{"bad top-level key", []string{"--merge-metadata", `{"bad key":1}`}, "invalid metadata key"},
			{"nested past the bound", []string{"--merge-metadata", `{"must_not_persist":1}`, "--merge-metadata", tooDeep}, "more than the limit of 100"},
			{"raw byte e9 as a name", []string{"--merge-metadata", `{"everturn":{"` + string([]byte{0xe9}) + `":null}}`}, storage.ErrMetadataMergePatchText.Error()},
			{"lone surrogate escape", []string{"--merge-metadata", `{"must_not_persist":"\ud800"}`}, storage.ErrMetadataMergePatchText.Error()},
			{"missing file", []string{"--merge-metadata", "@" + filepath.Join(p.dir, "absent.json")}, "failed to read metadata file"},
			{"beside --metadata", []string{"--merge-metadata", `{"a":1}`, "--metadata", `{"a":1}`}, "cannot combine --merge-metadata with --metadata"},
			{"beside --set-metadata", []string{"--merge-metadata", `{"a":1}`, "--set-metadata", "a=1"}, "cannot combine --merge-metadata with --set-metadata"},
			{"beside --unset-metadata", []string{"--merge-metadata", `{"a":1}`, "--unset-metadata", "top"}, "cannot combine --merge-metadata with --unset-metadata"},
		} {
			t.Run(strings.ReplaceAll(refusal.name, " ", "_"), func(t *testing.T) {
				out := bdUpdateFail(t, bd, p.dir, append([]string{issue.ID}, refusal.args...)...)
				if !strings.Contains(out, refusal.want) {
					t.Errorf("refusal output = %q, want it to contain %q", out, refusal.want)
				}
				after := show()
				if !reflect.DeepEqual(after.metadata, before.metadata) {
					t.Errorf("metadata after refused %s = %v, want unchanged %v", refusal.name, after.metadata, before.metadata)
				}
				if after.updatedAt != before.updatedAt {
					t.Errorf("updated_at after refused %s = %s, want unchanged %s", refusal.name, after.updatedAt, before.updatedAt)
				}
				if n := eventCount(); n != eventsBefore {
					t.Errorf("events after refused %s = %d, want unchanged %d", refusal.name, n, eventsBefore)
				}
			})
		}
	})

	t.Run("int64_digits", func(t *testing.T) {
		bdUpdate(t, bd, p.dir, issue.ID, "--merge-metadata", `{"everturn":{"id":9007199254740993}}`)
		bdUpdate(t, bd, p.dir, issue.ID, "--merge-metadata", `{"everturn":{"landing":{"newer":3}}}`)
		afterSibling := storedText()
		bdUpdate(t, bd, p.dir, issue.ID, "--merge-metadata", `{"everturn":{"id":9007199254740992}}`)
		bdUpdate(t, bd, p.dir, issue.ID, "--merge-metadata", `{"everturn":{"id":9007199254740993}}`)
		afterEdit := storedText()
		if !strings.Contains(strings.ReplaceAll(afterSibling, " ", ""), `"landing":{"new":2,"newer":3}`) {
			t.Errorf("stored %s, want everturn.landing merged to {\"new\":2,\"newer\":3}", afterSibling)
		}
		if !keepsDigits {
			t.Logf("Dolt %s rounds integers past 2^53 in a JSON column: digit assertions skipped (stored %s, then %s)", version, afterSibling, afterEdit)
			return
		}
		if !strings.Contains(afterSibling, "9007199254740993") {
			t.Errorf("Dolt %s: stored %s, want everturn.id's digits 9007199254740993 kept beside the patched sibling", version, afterSibling)
		}
		if !strings.Contains(afterEdit, "9007199254740993") {
			t.Errorf("Dolt %s: stored %s after patching everturn.id from ...992 to ...993, want the edit written", version, afterEdit)
		}
		if !t.Failed() {
			t.Logf("Dolt %s: bd update kept 9007199254740993 beside the patched sibling (%s) and wrote the ...992 to ...993 edit (%s)", version, afterSibling, afterEdit)
		}
	})

	// Review bd-ldmiy's CLI cases on the proxied route (bd init
	// --proxied-server against the same container: bd update goes through the
	// unit of work behind a dbproxy). {"n":1.0} over {"n":1}, {"k":1e3} over
	// {"k":1000} and --set-metadata jira.sprint=0.10000000000000001 over 0.1
	// write nothing: no row version, updated_at or event (findings 2 and 3). A
	// patch file naming the raw byte e9 exits non-zero with the fixed
	// message, the U+FFFD member it would have removed kept (finding 1).
	t.Run("proxied_respellings_and_e9", func(t *testing.T) {
		database := uniqueProxiedDatabase()
		pp := bdProxiedInit(t, bd, "pmm",
			"--database", database,
			"--proxied-server-external-host", "127.0.0.1",
			"--proxied-server-external-port", strconv.Itoa(port))
		pdb, err := sql.Open("mysql", fmt.Sprintf("root:@tcp(127.0.0.1:%d)/%s", port, database))
		if err != nil {
			t.Fatalf("open %s: %v", database, err)
		}
		t.Cleanup(func() { _ = pdb.Close() })
		pissue := bdCreate(t, bd, pp.dir, "Merge metadata (proxied)", "--type", "task",
			"--metadata", `{"n":1,"k":1000,"jira.sprint":0.1,"everturn":{"`+"\uFFFD"+`":"keep","other":"keep"}}`)
		type rowState struct {
			metadata, lock, updated string
			events                  int
		}
		read := func() rowState {
			t.Helper()
			var s rowState
			if err := pdb.QueryRow("SELECT metadata, HEX(row_lock), CAST(updated_at AS CHAR), (SELECT COUNT(*) FROM events WHERE issue_id = ?) FROM issues WHERE id = ?", pissue.ID, pissue.ID).
				Scan(&s.metadata, &s.lock, &s.updated, &s.events); err != nil {
				t.Fatalf("read %s: %v", pissue.ID, err)
			}
			return s
		}
		for _, noop := range [][]string{
			{"--merge-metadata", `{"n":1.0}`},
			{"--merge-metadata", `{"k":1e3}`},
			{"--set-metadata", "jira.sprint=0.10000000000000001"},
		} {
			before := read()
			bdUpdate(t, bd, pp.dir, append([]string{pissue.ID}, noop...)...)
			if after := read(); after != before {
				t.Errorf("proxied bd update %s: row %+v -> %+v, want nothing written", strings.Join(noop, " "), before, after)
			}
		}
		malformed := filepath.Join(t.TempDir(), "latin1.json")
		if err := os.WriteFile(malformed, []byte(`{"everturn":{"`+string([]byte{0xe9})+`":null}}`), 0o600); err != nil {
			t.Fatalf("write patch file: %v", err)
		}
		before := read()
		out, code := bdUpdateFailCode(t, bd, pp.dir, pissue.ID, "--merge-metadata", "@"+malformed)
		after := read()
		if code == 0 || !strings.Contains(out, storage.ErrMetadataMergePatchText.Error()) {
			t.Errorf("proxied bd update with byte e9 in a name: exit %d, output %q; want non-zero and %q", code, out, storage.ErrMetadataMergePatchText)
		}
		if after != before || !strings.Contains(after.metadata, "\uFFFD") {
			t.Errorf("proxied bd update with byte e9 in a name: row %+v -> %+v, want it unchanged with the U+FFFD member kept", before, after)
		}
	})
}

// newMergeMetadataServerProject is newServerModeProject
// (serve_server_mode_integration_test.go) on a port the caller already holds,
// without its BEADS_TEST_PROXIED_SERVER gate: a `bd init --server` workspace
// pointed at the shared test Dolt container.
func newMergeMetadataServerProject(t *testing.T, bd string, port int, prefix string) serverModeProject {
	t.Helper()
	dir := t.TempDir()
	initGitRepoAt(t, dir)
	database := uniqueProxiedDatabase()
	env := bdEnv(dir)
	cmd := exec.Command(bd, "init", "--quiet", "--server",
		"--server-host", "127.0.0.1",
		"--server-port", strconv.Itoa(port),
		"--database", database,
		"--prefix", prefix,
		"--non-interactive", "--skip-agents", "--skip-hooks")
	cmd.Dir = dir
	cmd.Env = env
	stdout, stderr, err := runCommandBuffers(t, cmd)
	if err != nil {
		t.Fatalf("bd init --server failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	return serverModeProject{dir: dir, beadsDir: filepath.Join(dir, ".beads"), database: database, env: env}
}

// doltVersionAtLeast reports whether a dolt_version() string is major.minor
// or later.
func doltVersionAtLeast(t *testing.T, version string, major, minor int) bool {
	t.Helper()
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		t.Fatalf("dolt_version() = %q, want major.minor", version)
	}
	gotMajor, err1 := strconv.Atoi(parts[0])
	gotMinor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		t.Fatalf("dolt_version() = %q, want numeric major.minor", version)
	}
	return gotMajor > major || (gotMajor == major && gotMinor >= minor)
}
