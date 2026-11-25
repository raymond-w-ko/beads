package metrics

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/lockfile"
)

func setupUserConfigHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

func userConfigPath(home string) string {
	return filepath.Join(home, ".config", "bd", "config.yaml")
}

func parseUserConfig(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var got map[string]interface{}
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal %s: %v\n%s", path, err, data)
	}
	return got
}

func metricsLeaf(t *testing.T, parsed map[string]interface{}, leaf string) interface{} {
	t.Helper()
	m, ok := parsed["metrics"].(map[string]interface{})
	if !ok {
		t.Fatalf("metrics key missing or not a map: %#v", parsed["metrics"])
	}
	v, ok := m[leaf]
	if !ok {
		t.Fatalf("metrics.%s missing: %#v", leaf, m)
	}
	return v
}

func TestEnsureUserConfigDefaults_CreatesMissingFile(t *testing.T) {
	home := setupUserConfigHome(t)
	if err := EnsureUserConfigDefaults(); err != nil {
		t.Fatalf("EnsureUserConfigDefaults: %v", err)
	}
	parsed := parseUserConfig(t, userConfigPath(home))
	if got := metricsLeaf(t, parsed, "disabled"); got != false {
		t.Errorf("metrics.disabled = %v, want false", got)
	}
	if got := metricsLeaf(t, parsed, "endpoint"); got != DefaultEndpoint {
		t.Errorf("metrics.endpoint = %v, want %q", got, DefaultEndpoint)
	}
}

func TestEnsureUserConfigDefaults_AddsMissingMetricsBlock(t *testing.T) {
	home := setupUserConfigHome(t)
	path := userConfigPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("other: value\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := EnsureUserConfigDefaults(); err != nil {
		t.Fatalf("EnsureUserConfigDefaults: %v", err)
	}
	parsed := parseUserConfig(t, path)
	if parsed["other"] != "value" {
		t.Errorf("existing key clobbered: %#v", parsed)
	}
	if got := metricsLeaf(t, parsed, "disabled"); got != false {
		t.Errorf("metrics.disabled = %v, want false", got)
	}
	if got := metricsLeaf(t, parsed, "endpoint"); got != DefaultEndpoint {
		t.Errorf("metrics.endpoint = %v, want %q", got, DefaultEndpoint)
	}
}

func TestEnsureUserConfigDefaults_FillsMissingLeafKeepsExisting(t *testing.T) {
	home := setupUserConfigHome(t)
	path := userConfigPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	existing := "metrics:\n  endpoint: https://existing.example.com\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := EnsureUserConfigDefaults(); err != nil {
		t.Fatalf("EnsureUserConfigDefaults: %v", err)
	}
	parsed := parseUserConfig(t, path)
	if got := metricsLeaf(t, parsed, "endpoint"); got != "https://existing.example.com" {
		t.Errorf("metrics.endpoint clobbered = %v", got)
	}
	if got := metricsLeaf(t, parsed, "disabled"); got != false {
		t.Errorf("metrics.disabled = %v, want false", got)
	}
}

func TestEnsureUserConfigDefaults_BothLeavesPresent_NoWrite(t *testing.T) {
	home := setupUserConfigHome(t)
	path := userConfigPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	original := []byte("metrics:\n  disabled: true\n  endpoint: https://kept.example.com\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	preStat, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if err := EnsureUserConfigDefaults(); err != nil {
		t.Fatalf("EnsureUserConfigDefaults: %v", err)
	}
	postStat, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !postStat.ModTime().Equal(preStat.ModTime()) {
		t.Errorf("file was rewritten; mtime changed from %v to %v", preStat.ModTime(), postStat.ModTime())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(original) {
		t.Errorf("file content changed.\nwant: %q\ngot:  %q", original, got)
	}
}

func TestEnsureUserConfigDefaults_CommentedBlock_SkippedSilently(t *testing.T) {
	home := setupUserConfigHome(t)
	path := userConfigPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	original := []byte("# metrics:\n#   disabled: true\nother: value\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := EnsureUserConfigDefaults(); err != nil {
		t.Fatalf("EnsureUserConfigDefaults: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(original) {
		t.Errorf("commented-out metrics block triggered a write.\nwant: %q\ngot:  %q", original, got)
	}
}

func TestEnsureUserConfigDefaults_MalformedYaml_FailsLoudly(t *testing.T) {
	home := setupUserConfigHome(t)
	path := userConfigPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("metrics:\n  disabled: [unclosed\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := EnsureUserConfigDefaults()
	if err == nil {
		t.Fatal("expected an error for malformed yaml, got nil")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error should mention parse failure, got: %v", err)
	}
}

func TestEnsureUserConfigDefaults_ConfigDirIsFile_FailsLoudly(t *testing.T) {
	home := setupUserConfigHome(t)
	blocker := filepath.Join(home, ".config")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}

	err := EnsureUserConfigDefaults()
	if err == nil {
		t.Fatal("expected an error when ~/.config is a file, got nil")
	}
}

func TestEnsureUserConfigDefaults_UnsafeRootsFailBeforeMutation(t *testing.T) {
	sentinel := t.TempDir()
	t.Chdir(sentinel)
	t.Setenv("HOME", "~")
	t.Setenv("USERPROFILE", "~")
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
	t.Setenv("XDG_CONFIG_HOME", "relative-xdg")
	t.Setenv("APPDATA", "relative-appdata")

	err := EnsureUserConfigDefaults()
	if err == nil {
		t.Fatal("EnsureUserConfigDefaults returned nil error for unsafe user roots")
	}
	if !strings.Contains(err.Error(), "resolve user config.yaml") ||
		!strings.Contains(err.Error(), "not an absolute native path") {
		t.Fatalf("EnsureUserConfigDefaults error = %v, want native path resolution context", err)
	}

	for _, relativeRoot := range []string{"~", "relative-xdg", "relative-appdata"} {
		if _, statErr := os.Stat(filepath.Join(sentinel, relativeRoot)); !os.IsNotExist(statErr) {
			t.Errorf("unsafe relative root %q was materialized: %v", relativeRoot, statErr)
		}
	}
}

// TestEnsureUserConfigDefaults_ConcurrentBootstrap races several first-start
// bootstraps against the first-run notice's write on an absent user config, as
// parallel bd processes sharing a HOME do. Each write is whole and serialized,
// so the file parses and keeps every key.
func TestEnsureUserConfigDefaults_ConcurrentBootstrap(t *testing.T) {
	for round := 0; round < 20; round++ {
		home := setupUserConfigHome(t)
		path := userConfigPath(home)

		const ensurers = 6
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make(chan error, ensurers+1)
		for i := 0; i < ensurers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if err := EnsureUserConfigDefaults(); err != nil {
					errs <- err
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := config.SetUserYamlConfig("metrics.notice_shown", "true"); err != nil {
				errs <- err
			}
		}()
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("round %d: %v", round, err)
		}

		parsed := parseUserConfig(t, path)
		if got := metricsLeaf(t, parsed, "disabled"); got != false {
			t.Fatalf("round %d: metrics.disabled = %v, want false", round, got)
		}
		if got := metricsLeaf(t, parsed, "endpoint"); got != DefaultEndpoint {
			t.Fatalf("round %d: metrics.endpoint = %v, want %s", round, got, DefaultEndpoint)
		}
		if got := metricsLeaf(t, parsed, "notice_shown"); got != true {
			t.Fatalf("round %d: metrics.notice_shown = %v, want true", round, got)
		}
	}
}

// TestEnsureUserConfigDefaults_KeepsValuesWrittenWhileWaiting: start-up finds
// metrics.disabled missing while another bd holds the config lock; that bd
// writes metrics.disabled: true and a custom endpoint, then releases. Start-up
// must decide from what it reads under the lock and keep both.
func TestEnsureUserConfigDefaults_KeepsValuesWrittenWhileWaiting(t *testing.T) {
	home := setupUserConfigHome(t)
	path := userConfigPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("metrics:\n  endpoint: "+DefaultEndpoint+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Another bd's writer holds the exclusive lock on the config file itself.
	lf, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	if err := lockfile.FileLockExclusive(lf); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- EnsureUserConfigDefaults() }()
	time.Sleep(200 * time.Millisecond) // start-up waits on the lock for its read
	select {
	case err := <-done:
		t.Fatalf("EnsureUserConfigDefaults returned (%v) while another writer held the config lock", err)
	default:
	}

	const custom = "https://custom.example/collect"
	if err := os.WriteFile(path, []byte("metrics:\n  disabled: true\n  endpoint: "+custom+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := lockfile.FileUnlock(lf); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("EnsureUserConfigDefaults: %v", err)
	}

	parsed := parseUserConfig(t, path)
	if got := metricsLeaf(t, parsed, "disabled"); got != true {
		t.Errorf("metrics.disabled = %v, want true (written while start-up waited)", got)
	}
	if got := metricsLeaf(t, parsed, "endpoint"); got != custom {
		t.Errorf("metrics.endpoint = %v, want %s", got, custom)
	}
}

// TestEnsureUserConfigDefaults_ReadOnlyCompleteConfig: a user config that
// needs nothing is only read, under the shared lock, so a 0555 directory (no
// lock or temp file can be made there) or a 0444 file returns nil and changes
// nothing. A commented-out metrics block is the same.
func TestEnsureUserConfigDefaults_ReadOnlyCompleteConfig(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
	cases := []struct {
		name     string
		body     string
		fileMode os.FileMode
		dirMode  os.FileMode
	}{
		{"complete file in 0555 dir", "metrics:\n  disabled: true\n  endpoint: https://custom.example/collect\n", 0o600, 0o555},
		{"commented metrics block in 0555 dir", "# metrics:\n#   disabled: false\nother: value\n", 0o600, 0o555},
		{"complete 0444 file", "metrics:\n  disabled: false\n  endpoint: " + DefaultEndpoint + "\n", 0o444, 0o755},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := setupUserConfigHome(t)
			path := userConfigPath(home)
			dir := filepath.Dir(path)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.body), tc.fileMode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tc.fileMode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, tc.dirMode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

			if err := EnsureUserConfigDefaults(); err != nil {
				t.Fatalf("EnsureUserConfigDefaults on a read-only config that needs nothing = %v, want nil", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "config.yaml" {
				var names []string
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Fatalf("config dir holds %v, want only config.yaml", names)
			}
			if got, _ := os.ReadFile(path); string(got) != tc.body {
				t.Fatalf("config.yaml changed:\nwant %q\ngot  %q", tc.body, got)
			}
		})
	}
}

// TestEnsureUserConfigDefaults_FirstCreateBootstrapRace: two bootstraps from
// an absent user config, the first stopped right after it creates the file and
// before any lock. The second must find the file whole, and the final bytes
// must equal one serial bootstrap's exactly.
func TestEnsureUserConfigDefaults_FirstCreateBootstrapRace(t *testing.T) {
	serialHome := setupUserConfigHome(t)
	if err := EnsureUserConfigDefaults(); err != nil {
		t.Fatalf("serial EnsureUserConfigDefaults: %v", err)
	}
	want, err := os.ReadFile(userConfigPath(serialHome))
	if err != nil {
		t.Fatal(err)
	}

	home := setupUserConfigHome(t)
	path := userConfigPath(home)
	created := make(chan struct{})
	resume := make(chan struct{})
	var once sync.Once
	restore := config.SetIOHookForTesting(func(stage, p string) {
		if stage == "created" && p == path {
			once.Do(func() {
				close(created)
				<-resume
			})
		}
	})
	defer restore()

	first := make(chan error, 1)
	go func() { first <- EnsureUserConfigDefaults() }()
	select {
	case <-created:
	case <-time.After(5 * time.Second):
		t.Fatal("the first bootstrap never created the file")
	}

	second := make(chan error, 1)
	go func() { second <- EnsureUserConfigDefaults() }()
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("second EnsureUserConfigDefaults: %v", err)
		}
	case <-time.After(5 * time.Second):
		close(resume)
		t.Fatal("the second bootstrap waited on the paused first one")
	}
	close(resume)
	if err := <-first; err != nil {
		t.Fatalf("first EnsureUserConfigDefaults: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("raced bootstrap wrote %q, want the serial bootstrap's %q", got, want)
	}
}
