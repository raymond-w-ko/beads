//go:build cgo

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
)

// TestEmbeddedUpdateMergeMetadata pins `bd update --merge-metadata`: the flag
// twice applies both RFC 7396 patches in order (a null, then the new object,
// replaces a nested object whole), siblings neither patch names survive, a
// name given twice in one object takes its last value, and every refusal exits
// non-zero having written nothing (metadata and updated_at unchanged).
func TestEmbeddedUpdateMergeMetadata(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "tm")
	issue := bdCreate(t, bd, dir, "Merge metadata", "--type", "task",
		"--metadata", `{"top":"seeded","jira.sprint":"s1","everturn":{"landing":{"old":1,"stale":true},"worktree":"wt/a"}}`)

	storedRow := func() (any, string) {
		t.Helper()
		var shown struct {
			Metadata  json.RawMessage `json:"metadata"`
			UpdatedAt string          `json:"updated_at"`
		}
		if err := json.Unmarshal(parseShowJSON(t, bdShowJSON(t, bd, dir, issue.ID)), &shown); err != nil {
			t.Fatalf("parse bd show: %v", err)
		}
		var metadata any
		if err := json.Unmarshal(shown.Metadata, &metadata); err != nil {
			t.Fatalf("parse metadata %s: %v", shown.Metadata, err)
		}
		return metadata, shown.UpdatedAt
	}
	storedMetadata := func() any {
		t.Helper()
		metadata, _ := storedRow()
		return metadata
	}
	asValue := func(text string) any {
		t.Helper()
		var value any
		if err := json.Unmarshal([]byte(text), &value); err != nil {
			t.Fatalf("parse %s: %v", text, err)
		}
		return value
	}

	patchFile := filepath.Join(t.TempDir(), "landing.json")
	if err := os.WriteFile(patchFile, []byte(`{"everturn":{"landing":{"new":2}},"top":null}`), 0o600); err != nil {
		t.Fatalf("write patch file: %v", err)
	}
	bdUpdate(t, bd, dir, issue.ID,
		"--merge-metadata", `{"everturn":{"landing":null}}`,
		"--merge-metadata", "@"+patchFile)
	if got, want := storedMetadata(), asValue(`{"jira.sprint":"s1","everturn":{"landing":{"new":2},"worktree":"wt/a"}}`); !reflect.DeepEqual(got, want) {
		t.Fatalf("metadata after two merge patches = %v, want %v", got, want)
	}

	// A name twice in one object takes its last value (as on the server route,
	// TestServerModeUpdateMergeMetadata).
	bdUpdate(t, bd, dir, issue.ID, "--merge-metadata", `{"everturn":{"worktree":"x","worktree":"wt/b"},"dup":1,"dup":null}`)
	want := asValue(`{"jira.sprint":"s1","everturn":{"landing":{"new":2},"worktree":"wt/b"}}`)
	if got := storedMetadata(); !reflect.DeepEqual(got, want) {
		t.Fatalf("metadata after a duplicate-name patch = %v, want %v", got, want)
	}
	_, updatedBefore := storedRow()
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
		{"missing file", []string{"--merge-metadata", "@" + filepath.Join(dir, "absent.json")}, "failed to read metadata file"},
		{"beside --metadata", []string{"--merge-metadata", `{"a":1}`, "--metadata", `{"a":1}`}, "cannot combine --merge-metadata with --metadata"},
		{"beside --set-metadata", []string{"--merge-metadata", `{"a":1}`, "--set-metadata", "a=1"}, "cannot combine --merge-metadata with --set-metadata"},
		{"beside --unset-metadata", []string{"--merge-metadata", `{"a":1}`, "--unset-metadata", "top"}, "cannot combine --merge-metadata with --unset-metadata"},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			out := bdUpdateFail(t, bd, dir, append([]string{issue.ID}, refusal.args...)...)
			if !strings.Contains(out, refusal.want) {
				t.Errorf("refusal output = %q, want it to contain %q", out, refusal.want)
			}
			got, updatedAt := storedRow()
			if !reflect.DeepEqual(got, want) {
				t.Errorf("metadata after refused %s = %v, want unchanged %v", refusal.name, got, want)
			}
			if updatedAt != updatedBefore {
				t.Errorf("updated_at after refused %s = %s, want unchanged %s", refusal.name, updatedAt, updatedBefore)
			}
		})
	}
}
