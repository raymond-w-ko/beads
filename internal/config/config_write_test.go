package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"

	"github.com/steveyegge/beads/internal/lockfile"
)

const (
	rewriteHelperEnv  = "BD_TEST_REWRITE_HELPER_HOME"
	rewriteHelperName = "BD_TEST_REWRITE_HELPER_WORKER"
	rewriteKeysPerRun = 4
)

func setRewriteTestHome(t *testing.T, home string) string {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return filepath.Join(home, ".config", "bd", "config.yaml")
}

func rewriteWorkerKey(worker string, i int) string {
	return fmt.Sprintf("rewrite.%s_k%d", worker, i)
}

func setRewriteWorkerKeys(worker string) error {
	for i := 0; i < rewriteKeysPerRun; i++ {
		if err := SetUserYamlConfig(rewriteWorkerKey(worker, i), "v-"+worker); err != nil {
			return err
		}
	}
	return nil
}

// TestRewriteConfigFileHelperProcess is the child body of
// TestRewriteConfigFile_ConcurrentSettersLoseNoUpdate; it does nothing unless
// re-executed by that test.
func TestRewriteConfigFileHelperProcess(t *testing.T) {
	home := os.Getenv(rewriteHelperEnv)
	if home == "" {
		t.Skip("helper process only")
	}
	setRewriteTestHome(t, home)
	if err := setRewriteWorkerKeys(os.Getenv(rewriteHelperName)); err != nil {
		t.Fatalf("child SetUserYamlConfig: %v", err)
	}
}

func rewriteLeaves(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var parsed map[string]interface{}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("config.yaml does not parse after concurrent writes: %v\n%s", err, data)
	}
	m, _ := parsed["rewrite"].(map[string]interface{})
	return m
}

func TestRewriteConfigFile_ConcurrentSettersLoseNoUpdate(t *testing.T) {
	home := t.TempDir()
	path := setRewriteTestHome(t, home)

	const goroutines = 16
	const children = 4

	var workers []string
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, goroutines+children)

	for c := 0; c < children; c++ {
		name := fmt.Sprintf("p%02d", c)
		workers = append(workers, name)
		cmd := exec.Command(os.Args[0], "-test.run=^TestRewriteConfigFileHelperProcess$", "-test.count=1")
		cmd.Env = append(os.Environ(), rewriteHelperEnv+"="+home, rewriteHelperName+"="+name)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if out, err := cmd.CombinedOutput(); err != nil {
				errs <- fmt.Errorf("child %s: %v\n%s", name, err, out)
			}
		}()
	}
	for g := 0; g < goroutines; g++ {
		name := fmt.Sprintf("g%02d", g)
		workers = append(workers, name)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := setRewriteWorkerKeys(name); err != nil {
				errs <- fmt.Errorf("goroutine %s: %w", name, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	leaves := rewriteLeaves(t, path)
	var missing []string
	for _, w := range workers {
		for i := 0; i < rewriteKeysPerRun; i++ {
			key := strings.TrimPrefix(rewriteWorkerKey(w, i), "rewrite.")
			if leaves[key] != "v-"+w {
				missing = append(missing, key)
			}
		}
	}
	if len(missing) > 0 {
		t.Fatalf("lost %d of %d updates: %v", len(missing), len(workers)*rewriteKeysPerRun, missing)
	}
}

func TestRewriteConfigFile_SymlinkStaysSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	home := t.TempDir()
	path := setRewriteTestHome(t, home)

	dotfiles := filepath.Join(t.TempDir(), "dotfiles")
	if err := os.MkdirAll(dotfiles, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dotfiles, "bd-config.yaml")
	if err := os.WriteFile(target, []byte("existing: kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	if err := SetUserYamlConfig("rewrite.via_link", "yes"); err != nil {
		t.Fatalf("SetUserYamlConfig: %v", err)
	}

	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("config.yaml mode = %v after write, want a symlink", fi.Mode())
	}
	if got, _ := os.Readlink(path); got != target {
		t.Fatalf("symlink now points at %q, want %q", got, target)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]interface{}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("symlink target does not parse: %v\n%s", err, data)
	}
	if leaves := rewriteLeaves(t, target); parsed["existing"] != "kept" || leaves["via_link"] != "yes" {
		t.Fatalf("symlink target lacks old or new key:\n%s", data)
	}
	for _, p := range []string{path + ".lock", target + ".lock"} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("a sibling lock file %s exists (%v): the lock is on the file itself", p, err)
		}
	}
}

func TestRewriteConfigFile_ModeAndNoTempLeft(t *testing.T) {
	home := t.TempDir()
	path := setRewriteTestHome(t, home)

	if err := SetUserYamlConfig("rewrite.first", "1"); err != nil {
		t.Fatalf("SetUserYamlConfig (create): %v", err)
	}
	if err := SetUserYamlConfig("rewrite.second", "2"); err != nil {
		t.Fatalf("SetUserYamlConfig (update): %v", err)
	}
	if err := UnsetUserYamlConfig("rewrite.first"); err != nil {
		t.Fatalf("UnsetUserYamlConfig: %v", err)
	}

	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Fatalf("config.yaml mode = %o, want 600", got)
		}
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"config.yaml"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("config dir holds %v, want exactly %v (no temp or lock file left)", names, want)
	}
}

func TestRewriteConfigFile_SkipLeavesFileUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := RewriteConfigFile(path, 0o600, func([]byte, bool) ([]byte, error) {
		return nil, ErrSkipRewrite
	}); err != nil {
		t.Fatalf("RewriteConfigFile skip: %v", err)
	}
	refused := errors.New("refused")
	if err := RewriteConfigFile(path, 0o600, func([]byte, bool) ([]byte, error) {
		return nil, refused
	}); !errors.Is(err, refused) {
		t.Fatalf("RewriteConfigFile edit error = %v, want %v", err, refused)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("an absent file's rewrite that writes nothing left %v", entries)
	}
}

// holdConfigLock takes the exclusive config write lock of path (a lock on the
// file itself), as a writer in another bd would, and returns its release.
func holdConfigLock(t *testing.T, path string) func() {
	t.Helper()
	lf, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockfile.FileLockExclusive(lf); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			_ = lockfile.FileUnlock(lf)
			_ = lf.Close()
		})
	}
	t.Cleanup(release)
	return release
}

// TestAddRepo_ConcurrentAddsBothSurvive: two AddRepo calls queued behind a
// held lock each add their repo to what the other wrote, so both survive.
func TestAddRepo_ConcurrentAddsBothSurvive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("issue-prefix: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	release := holdConfigLock(t, path)

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, repo := range []string{"/repo/one", "/repo/two"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- AddRepo(path, repo)
		}()
	}
	time.Sleep(200 * time.Millisecond) // both reach the lock
	release()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("AddRepo: %v", err)
		}
	}

	repos, err := GetReposFromYAML(path)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(repos.Additional, ",")
	if len(repos.Additional) != 2 || !strings.Contains(got, "/repo/one") || !strings.Contains(got, "/repo/two") {
		t.Fatalf("repos.additional = %v, want both /repo/one and /repo/two", repos.Additional)
	}
}

// TestRewriteConfigFile_RelativeSymlinkThroughDirLink: config.yaml ->
// hop/../picked.yaml with hop -> dotfiles/sub names dotfiles/picked.yaml (the
// kernel follows hop before ".."). The set must change the file a read through
// config.yaml sees.
func TestRewriteConfigFile_RelativeSymlinkThroughDirLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	path := setRewriteTestHome(t, t.TempDir())
	dir := filepath.Dir(path)
	if err := os.MkdirAll(filepath.Join(dir, "dotfiles", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("dotfiles", "sub"), filepath.Join(dir, "hop")); err != nil {
		t.Fatal(err)
	}
	picked := filepath.Join(dir, "dotfiles", "picked.yaml")
	if err := os.WriteFile(picked, []byte("existing: kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hop/../picked.yaml", path); err != nil {
		t.Fatal(err)
	}

	if err := SetUserYamlConfig("rewrite.through_hop", "yes"); err != nil {
		t.Fatalf("SetUserYamlConfig: %v", err)
	}
	if got, ok := readYamlValueAtPath(path, "rewrite.through_hop"); !ok || got != "yes" {
		t.Fatalf("read through config.yaml: rewrite.through_hop = %q (found %v), want yes", got, ok)
	}
	if got, _ := readYamlValueAtPath(path, "existing"); got != "kept" {
		t.Fatalf("read through config.yaml lost existing key: %q", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, "picked.yaml")); !os.IsNotExist(err) {
		t.Fatalf("a lexically resolved picked.yaml beside config.yaml exists: %v", err)
	}

	// A dangling link with the same shape creates the file it names.
	fresh := filepath.Join(dir, "dotfiles", "fresh.yaml")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hop/../fresh.yaml", path); err != nil {
		t.Fatal(err)
	}
	if err := SetUserYamlConfig("rewrite.dangling", "made"); err != nil {
		t.Fatalf("SetUserYamlConfig (dangling): %v", err)
	}
	if got, _ := readYamlValueAtPath(fresh, "rewrite.dangling"); got != "made" {
		t.Fatalf("dangling link: %s holds rewrite.dangling = %q, want made", fresh, got)
	}
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("dangling link: config.yaml is no longer a symlink (%v)", err)
	}
}

// TestRewriteConfigFile_ReadOnlyFileRefused: a 0444 config.yaml in a writable
// directory is refused, as the in-place write always refused it.
func TestRewriteConfigFile_ReadOnlyFileRefused(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
	path := setRewriteTestHome(t, t.TempDir())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("existing: kept\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	err := SetUserYamlConfig("rewrite.forced", "yes")
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("SetUserYamlConfig on a 0444 file = %v, want permission denied", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "existing: kept\n" {
		t.Fatalf("0444 file changed:\n%s", data)
	}
}

// TestRewriteConfigFile_ACLSurvives: a named-user ACL on config.yaml survives
// a set, because the file is rewritten in place.
func TestRewriteConfigFile_ACLSurvives(t *testing.T) {
	setfacl, err1 := exec.LookPath("setfacl")
	getfacl, err2 := exec.LookPath("getfacl")
	if err1 != nil || err2 != nil {
		t.Skip("setfacl/getfacl not installed")
	}
	path := setRewriteTestHome(t, t.TempDir())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("existing: kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(setfacl, "-m", "u:65534:r--", path).CombinedOutput(); err != nil {
		t.Skipf("filesystem refuses ACLs: %v %s", err, out)
	}
	if err := SetUserYamlConfig("rewrite.acl", "kept"); err != nil {
		t.Fatalf("SetUserYamlConfig: %v", err)
	}
	out, err := exec.Command(getfacl, "-n", "-p", path).CombinedOutput()
	if err != nil {
		t.Fatalf("getfacl: %v %s", err, out)
	}
	if !strings.Contains(string(out), "user:65534:r--") {
		t.Fatalf("named-user ACL lost by the set:\n%s", out)
	}
}

// TestUnsetUserYamlConfig_EmptyHomeCreatesNothing: unsetting on an empty HOME
// is a successful no-op that makes no directory and no file.
func TestUnsetUserYamlConfig_EmptyHomeCreatesNothing(t *testing.T) {
	home := t.TempDir()
	setRewriteTestHome(t, home)
	if err := UnsetUserYamlConfig("rewrite.absent"); err != nil {
		t.Fatalf("UnsetUserYamlConfig on an empty HOME: %v", err)
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Fatalf("UnsetUserYamlConfig created %v in an empty HOME", entries)
	}
}

// TestRewriteConfigFile_ReaderNeverSeesPartialWrite pauses a writer between
// its truncate and its write and starts each config reader meanwhile. A reader
// must wait for the writer and see the whole new file, never the empty one.
func TestRewriteConfigFile_ReaderNeverSeesPartialWrite(t *testing.T) {
	readers := map[string]func(path string) (string, error){
		"readUserGlobalYamlValue": func(string) (string, error) {
			v, _ := readUserGlobalYamlValue("rewrite.old")
			return v, nil
		},
		"GetReposFromYAML": func(path string) (string, error) {
			repos, err := GetReposFromYAML(path)
			if err != nil {
				return "", err
			}
			return repos.Primary, nil
		},
		"Initialize": func(string) (string, error) {
			if err := Initialize(); err != nil {
				return "", err
			}
			return GetString("rewrite.old"), nil
		},
		"WorkspaceYamlValueStrict": func(path string) (string, error) {
			v, _, err := WorkspaceYamlValueStrict(filepath.Dir(path), "rewrite.old")
			return v, err
		},
		"GetStringFromDir": func(path string) (string, error) {
			return GetStringFromDir(filepath.Dir(path), "rewrite.old"), nil
		},
	}
	want := map[string]string{
		"readUserGlobalYamlValue":  "value",
		"GetReposFromYAML":         ".",
		"Initialize":               "value",
		"WorkspaceYamlValueStrict": "value",
		"GetStringFromDir":         "value",
	}

	for name, read := range readers {
		t.Run(name, func(t *testing.T) {
			path := setRewriteTestHome(t, t.TempDir())
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			seed := "rewrite:\n    old: value\nrepos:\n    primary: .\n"
			if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
				t.Fatal(err)
			}

			truncated := make(chan struct{})
			resume := make(chan struct{})
			var once sync.Once
			configIOHook = func(stage, _ string) {
				if stage == "truncated" {
					once.Do(func() {
						close(truncated)
						<-resume
					})
				}
			}
			t.Cleanup(func() { configIOHook = nil })

			writeErr := make(chan error, 1)
			go func() { writeErr <- SetUserYamlConfig("rewrite.new", "added") }()
			select {
			case <-truncated:
			case <-time.After(5 * time.Second):
				t.Fatal("the writer never truncated config.yaml in place")
			}

			type result struct {
				v   string
				err error
			}
			got := make(chan result, 1)
			go func() {
				v, err := read(path)
				got <- result{v, err}
			}()
			early := false
			select {
			case <-got:
				early = true // read while the file was truncated
			case <-time.After(200 * time.Millisecond):
			}
			close(resume)
			if err := <-writeErr; err != nil {
				t.Fatalf("SetUserYamlConfig: %v", err)
			}
			if early {
				t.Fatalf("%s returned while the writer had the file truncated", name)
			}
			r := <-got
			if r.err != nil || r.v != want[name] {
				t.Fatalf("%s = %q, %v; want %q (the whole file)", name, r.v, r.err, want[name])
			}
		})
	}
}

// TestRewriteConfigFile_FirstCreateReaderNeverSeesPrefix: on the in-place
// create fallback (no hard links), a reader that found no config.yaml, and a
// reader that starts while the first writer has written only a prefix of the
// new file, must each see the file absent, empty or whole, never the prefix.
func TestRewriteConfigFile_FirstCreateReaderNeverSeesPrefix(t *testing.T) {
	configNoHardLinks = true
	t.Cleanup(func() { configNoHardLinks = false })
	path := filepath.Join(t.TempDir(), "config.yaml")
	const endpoint = "https://example.invalid/collect"
	body := "metrics:\n  endpoint: " + endpoint + "\n"

	probed := make(chan struct{})    // the first reader made its open attempt
	letProbe := make(chan struct{})  // ... and may go on
	prefixed := make(chan struct{})  // the writer wrote half the file
	letWriter := make(chan struct{}) // ... and may finish
	var probeOnce, prefixOnce sync.Once
	configIOHook = func(stage, _ string) {
		switch stage {
		case "reader-opened":
			probeOnce.Do(func() {
				close(probed)
				<-letProbe
			})
		case "prefix":
			prefixOnce.Do(func() {
				close(prefixed)
				<-letWriter
			})
		}
	}
	t.Cleanup(func() { configIOHook = nil })

	type result struct {
		v     string
		found bool
	}
	first := make(chan result, 1)
	go func() {
		v, ok := readYamlValueAtPath(path, "metrics.endpoint")
		first <- result{v, ok}
	}()
	<-probed

	writeErr := make(chan error, 1)
	go func() {
		writeErr <- RewriteConfigFile(path, 0o600, func([]byte, bool) ([]byte, error) {
			return []byte(body), nil
		})
	}()
	select {
	case <-prefixed:
	case <-time.After(5 * time.Second):
		t.Fatal("the writer never wrote a prefix of the new file")
	}

	close(letProbe) // the first reader resumes while only the prefix is on disk
	r := <-first
	if r.found && r.v != endpoint {
		t.Fatalf("first-create reader returned metrics.endpoint=%q, a prefix of %q", r.v, endpoint)
	}

	second := make(chan result, 1)
	go func() {
		v, ok := readYamlValueAtPath(path, "metrics.endpoint")
		second <- result{v, ok}
	}()
	select {
	case r := <-second:
		t.Fatalf("a reader returned (%q, found %v) while the first writer had written only a prefix", r.v, r.found)
	case <-time.After(200 * time.Millisecond):
	}
	close(letWriter)
	if err := <-writeErr; err != nil {
		t.Fatalf("RewriteConfigFile: %v", err)
	}
	if r := <-second; !r.found || r.v != endpoint {
		t.Fatalf("reader after the first write = %q (found %v), want the whole %q", r.v, r.found, endpoint)
	}
}

// TestRewriteConfigFile_FirstCreateIsWhole: a new config.yaml appears whole.
// While its creator is paused just after the create, the file on disk is
// already the full new content, and a reader returns it without waiting; a
// reader that found no file before the create sees it absent or whole. No
// temp file is left.
func TestRewriteConfigFile_FirstCreateIsWhole(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	const endpoint = "https://example.invalid/collect"
	body := "metrics:\n  endpoint: " + endpoint + "\n"

	probed := make(chan struct{})
	letProbe := make(chan struct{})
	created := make(chan struct{})
	letWriter := make(chan struct{})
	var probeOnce, createOnce sync.Once
	configIOHook = func(stage, _ string) {
		switch stage {
		case "reader-opened":
			probeOnce.Do(func() {
				close(probed)
				<-letProbe
			})
		case "created":
			createOnce.Do(func() {
				close(created)
				<-letWriter
			})
		}
	}
	t.Cleanup(func() { configIOHook = nil })

	type result struct {
		v     string
		found bool
	}
	first := make(chan result, 1)
	go func() {
		v, ok := readYamlValueAtPath(path, "metrics.endpoint")
		first <- result{v, ok}
	}()
	<-probed

	writeErr := make(chan error, 1)
	go func() {
		writeErr <- RewriteConfigFile(path, 0o600, func([]byte, bool) ([]byte, error) {
			return []byte(body), nil
		})
	}()
	select {
	case <-created:
	case <-time.After(5 * time.Second):
		t.Fatal("the writer never created the file")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != body {
		t.Fatalf("just-created config.yaml = %q (%v), want the whole %q", data, err, body)
	}
	close(letProbe)
	if r := <-first; r.found && r.v != endpoint {
		t.Fatalf("first-create reader returned metrics.endpoint=%q, want absent or %q", r.v, endpoint)
	}
	second := make(chan result, 1)
	go func() {
		v, ok := readYamlValueAtPath(path, "metrics.endpoint")
		second <- result{v, ok}
	}()
	select {
	case r := <-second:
		if !r.found || r.v != endpoint {
			t.Fatalf("reader after the create = %q (found %v), want the whole %q", r.v, r.found, endpoint)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a reader waited on a file created whole")
	}
	close(letWriter)
	if err := <-writeErr; err != nil {
		t.Fatalf("RewriteConfigFile: %v", err)
	}
	if fi, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Fatalf("created config.yaml: %v, mode %v; want 0600", err, fi.Mode())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("config dir holds %v, want only config.yaml (no temp file)", entries)
	}
}

// TestRewriteConfigFile_HardLinkedNamesShareTheLock: two hard-linked names of
// one config.yaml. While the first writer is paused in its locked edit, a set
// through the other name must wait, and both keys must survive.
func TestRewriteConfigFile_HardLinkedNamesShareTheLock(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "config.yaml")
	second := filepath.Join(dir, "linked.yaml")
	if err := os.WriteFile(first, []byte("existing: kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, second); err != nil {
		t.Skipf("hard links unsupported here: %v", err)
	}

	inEdit := make(chan struct{})
	resume := make(chan struct{})
	firstErr := make(chan error, 1)
	go func() {
		firstErr <- RewriteConfigFile(first, 0o600, func(old []byte, _ bool) ([]byte, error) {
			close(inEdit)
			<-resume
			out, err := updateYamlKey(string(old), "rewrite.first", "one")
			return []byte(out), err
		})
	}()
	select {
	case <-inEdit:
	case <-time.After(5 * time.Second):
		t.Fatal("the first writer never reached its locked edit")
	}

	secondErr := make(chan error, 1)
	go func() { secondErr <- setYamlConfigAtPath(second, "rewrite.second", "two") }()
	select {
	case err := <-secondErr:
		close(resume)
		<-firstErr
		t.Fatalf("a set through the hard-linked name returned (%v) while the first writer held the lock", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(resume)
	if err := <-firstErr; err != nil {
		t.Fatalf("first writer: %v", err)
	}
	if err := <-secondErr; err != nil {
		t.Fatalf("second writer: %v", err)
	}

	for _, name := range []string{first, second} {
		leaves := rewriteLeaves(t, name)
		if leaves["first"] != "one" || leaves["second"] != "two" {
			data, _ := os.ReadFile(name)
			t.Fatalf("%s lost an update, want rewrite.first and rewrite.second:\n%s", filepath.Base(name), data)
		}
	}
	a, err1 := os.Stat(first)
	b, err2 := os.Stat(second)
	if err1 != nil || err2 != nil || !os.SameFile(a, b) {
		t.Fatalf("the two names no longer share one file (%v, %v)", err1, err2)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("config dir holds %d entries, want the two names only", len(entries))
	}
}

// TestAddRepo_ViperReloadWaitsForWriter: AddRepo reloads Viper after its own
// write. If another bd's writer has the file truncated at that moment, the
// reload must wait for it, not load the empty file and drop settings.
func TestAddRepo_ViperReloadWaitsForWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("retained: keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	saved := v
	t.Cleanup(func() { v = saved })
	v = viper.New()
	v.SetConfigType("yaml")
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		t.Fatal(err)
	}

	truncated := make(chan struct{})
	resume := make(chan struct{})
	otherErr := make(chan error, 1)
	var reloadOnce, truncOnce sync.Once
	var armed atomic.Bool // set once AddRepo's own write is done
	configIOHook = func(stage, _ string) {
		switch stage {
		case "reader-opened":
			// AddRepo's write is done and its reload begins: another bd's
			// writer truncates the file now and stalls.
			reloadOnce.Do(func() {
				armed.Store(true)
				go func() { otherErr <- setYamlConfigAtPath(path, "other.key", "x") }()
				select {
				case <-truncated:
				case <-time.After(5 * time.Second):
				}
				// Let the writer finish only once the reload had its chance
				// to read the truncated file.
				go func() {
					time.Sleep(200 * time.Millisecond)
					close(resume)
				}()
			})
		case "truncated":
			if !armed.Load() {
				return // AddRepo's own write
			}
			truncOnce.Do(func() {
				close(truncated)
				<-resume
			})
		}
	}
	t.Cleanup(func() { configIOHook = nil })

	if err := AddRepo(path, "/repo/one"); err != nil {
		t.Fatalf("AddRepo: %v", err)
	}
	if err := <-otherErr; err != nil {
		t.Fatalf("other writer: %v", err)
	}
	if got := v.GetString("retained"); got != "keep" {
		t.Fatalf("after AddRepo's Viper reload GetString(retained) = %q, want keep (the reload read a truncated file)", got)
	}
}

// TestRewriteConfigFile_DeadlockInLockedEditCaught: an edit callback that
// reads the file or rewrites it again would block forever on the lock it
// holds. With the re-entry check on, both are caught as errors, and every
// production edit path passes the check (its callback reads no file).
func TestRewriteConfigFile_DeadlockInLockedEditCaught(t *testing.T) {
	configLockReentryCheck = true
	t.Cleanup(func() { configLockReentryCheck = false })

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("existing: kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	bad := map[string]func([]byte, bool) ([]byte, error){
		"reader in edit": func(old []byte, _ bool) ([]byte, error) {
			_, err := readConfigShared(path)
			return old, err
		},
		"rewrite in edit": func(old []byte, _ bool) ([]byte, error) {
			err := RewriteConfigFile(path, 0o600, func(b []byte, _ bool) ([]byte, error) { return b, nil })
			return old, err
		},
	}
	for name, edit := range bad {
		done := make(chan error, 1)
		go func() { done <- RewriteConfigFile(path, 0o600, edit) }()
		select {
		case err := <-done:
			if !errors.Is(err, errConfigLockReentry) {
				t.Fatalf("%s: RewriteConfigFile = %v, want errConfigLockReentry", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: deadlocked on the file's own lock", name)
		}
	}

	home := t.TempDir()
	setRewriteTestHome(t, home)
	saved := v
	t.Cleanup(func() { v = saved })
	v = viper.New()
	v.SetConfigType("yaml")
	repos := &ReposConfig{Primary: ".", Additional: []string{"/repo/a"}}
	// In order: RemoveRepo removes what AddRepo added after SetReposInYAML.
	paths := []struct {
		name string
		run  func() error
	}{
		{"SetUserYamlConfig", func() error { return SetUserYamlConfig("rewrite.k", "v") }},
		{"UnsetUserYamlConfig", func() error { return UnsetUserYamlConfig("rewrite.k") }},
		{"setYamlConfigAtPath", func() error { return setYamlConfigAtPath(path, "rewrite.k", "v") }},
		{"SaveConfigValue", func() error { return SaveConfigValue("routing.mode", "auto", dir) }},
		{"SetReposInYAML", func() error { return SetReposInYAML(path, repos) }},
		{"AddRepo", func() error { return AddRepo(path, "/repo/b") }},
		{"RemoveRepo", func() error { return RemoveRepo(path, "/repo/b") }},
	}
	for _, p := range paths {
		if err := p.run(); err != nil {
			t.Fatalf("%s under the re-entry check: %v", p.name, err)
		}
	}
}

// retargetFixture is a BEADS_DIR whose config.yaml is a symlink to orig.yaml
// ("retained: original"), beside other.yaml ("retained: prefix-finished").
func retargetFixture(t *testing.T) (path, other string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	setRewriteTestHome(t, t.TempDir())
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_DIR", beadsDir)
	orig := filepath.Join(beadsDir, "orig.yaml")
	other = filepath.Join(beadsDir, "other.yaml")
	path = filepath.Join(beadsDir, "config.yaml")
	if err := os.WriteFile(orig, []byte("retained: original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("retained: prefix-finished\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("orig.yaml", path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ResetForTesting)
	return path, other
}

// retargetWhileLocked arms the hook so that, once a reader of path holds its
// shared lock and before it reads, another writer holds other's exclusive lock
// with only a prefix written ("retained: pref") and path is atomically
// retargeted to other. It returns a func that finishes that write and
// releases it, and a channel closed when the retarget happened.
func retargetWhileLocked(t *testing.T, path, other string) (finish func(), fired <-chan struct{}) {
	t.Helper()
	done := make(chan struct{})
	var once sync.Once
	var of *os.File
	configIOHook = func(stage, p string) {
		if stage != "reader-locked" || p != path {
			return
		}
		once.Do(func() {
			f, err := os.OpenFile(other, os.O_RDWR, 0)
			if err != nil {
				t.Error(err)
				return
			}
			if err := lockfile.FileLockExclusive(f); err != nil {
				t.Error(err)
			}
			_ = f.Truncate(0)
			_, _ = f.WriteAt([]byte("retained: pref"), 0)
			of = f
			tmpLink := path + ".retarget"
			if err := os.Symlink("other.yaml", tmpLink); err != nil {
				t.Error(err)
			}
			if err := os.Rename(tmpLink, path); err != nil {
				t.Error(err)
			}
			close(done)
		})
	}
	var finishOnce sync.Once
	finish = func() {
		finishOnce.Do(func() {
			configIOHook = nil
			if of != nil {
				_, _ = of.WriteAt([]byte("retained: prefix-finished\n"), 0)
				_ = lockfile.FileUnlock(of)
				_ = of.Close()
			}
		})
	}
	t.Cleanup(finish)
	return finish, done
}

// runWithin runs fn and fails the test if it does not return within d.
func runWithin(t *testing.T, d time.Duration, what string, fn func() error) error {
	t.Helper()
	errc := make(chan error, 1)
	go func() { errc <- fn() }()
	select {
	case err := <-errc:
		return err
	case <-time.After(d):
		t.Fatalf("%s did not return within %v (it waited on the retargeted file's lock)", what, d)
		return nil
	}
}

// TestInitialize_RetargetedSymlinkReadsLockedFile: a symlink retargeted after
// Initialize's reader took its shared lock must not make Viper read another
// file: Initialize loads the bytes of the file it locked.
func TestInitialize_RetargetedSymlinkReadsLockedFile(t *testing.T) {
	path, other := retargetFixture(t)
	finish, fired := retargetWhileLocked(t, path, other)
	err := runWithin(t, 5*time.Second, "Initialize", Initialize)
	finish()
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	select {
	case <-fired:
	default:
		t.Fatalf("no reader of %s reached its locked stage", path)
	}
	if got := GetString("retained"); got != "original" {
		t.Fatalf("Initialize loaded retained=%q, want \"original\" (the locked file)", got)
	}
}

// TestSetReposInYAML_RetargetedSymlinkReloadReadsLockedFile: the same for the
// Viper reload after a repos write.
func TestSetReposInYAML_RetargetedSymlinkReloadReadsLockedFile(t *testing.T) {
	path, other := retargetFixture(t)
	if err := Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if got := v.ConfigFileUsed(); got != path {
		t.Fatalf("ConfigFileUsed = %q, want %q", got, path)
	}
	finish, fired := retargetWhileLocked(t, path, other)
	err := runWithin(t, 5*time.Second, "SetReposInYAML", func() error {
		return SetReposInYAML(path, &ReposConfig{Primary: "."})
	})
	finish()
	if err != nil {
		t.Fatalf("SetReposInYAML: %v", err)
	}
	select {
	case <-fired:
	default:
		t.Fatal("the reload never reached its locked stage")
	}
	if got := GetString("retained"); got != "original" {
		t.Fatalf("repos reload loaded retained=%q, want \"original\" (the locked file)", got)
	}
	if got := v.ConfigFileUsed(); got != path {
		t.Fatalf("ConfigFileUsed after the reload = %q, want %q", got, path)
	}
}

// danglingBootstrapBody is what a first metrics bootstrap writes to an absent
// user config (the body internal/metrics writes).
const danglingBootstrapBody = "metrics:\n  disabled: false\n  endpoint: https://example.invalid/collect\n"

// bootstrapLikeEdit is the metrics bootstrap's edit: the whole body for an
// absent file, else the missing keys set as SetUserYamlConfig would (the
// existing-file formatting), else nothing.
func bootstrapLikeEdit(old []byte, exists bool) ([]byte, error) {
	if !exists {
		return []byte(danglingBootstrapBody), nil
	}
	var root map[string]interface{}
	if err := yaml.Unmarshal(old, &root); err != nil {
		return nil, err
	}
	metrics, _ := root["metrics"].(map[string]interface{})
	content, changed := string(old), false
	for _, kv := range [][2]string{{"disabled", "false"}, {"endpoint", "https://example.invalid/collect"}} {
		if _, ok := metrics[kv[0]]; ok {
			continue
		}
		var err error
		if content, err = UpdateYamlConfigContent(content, "metrics."+kv[0], kv[1]); err != nil {
			return nil, err
		}
		changed = true
	}
	if !changed {
		return nil, ErrSkipRewrite
	}
	return []byte(content), nil
}

// requireHardLinks skips unless dir's filesystem supports hard links.
func requireHardLinks(t *testing.T, dir string) {
	t.Helper()
	probe := filepath.Join(dir, ".probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(probe)
	if err := os.Link(probe, probe+".link"); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	_ = os.Remove(probe + ".link")
}

// TestRewriteConfigFile_DanglingSymlinkBootstrapRace: config.yaml is a
// symlink to an absent dotfile. The first bootstrap is stopped right after it
// creates the file, before any lock; a second bootstrap runs to the end. The
// referent must have been created whole, so the final bytes equal one serial
// bootstrap's, and config.yaml stays a symlink.
func TestRewriteConfigFile_DanglingSymlinkBootstrapRace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	setup := func(t *testing.T) (dir, path, referent string) {
		dir = t.TempDir()
		requireHardLinks(t, dir)
		path = filepath.Join(dir, "config.yaml")
		referent = filepath.Join(dir, "dotfile.yaml")
		if err := os.Symlink("dotfile.yaml", path); err != nil {
			t.Fatal(err)
		}
		return dir, path, referent
	}

	_, serialPath, _ := setup(t)
	if err := RewriteConfigFile(serialPath, 0o600, bootstrapLikeEdit); err != nil {
		t.Fatalf("serial bootstrap: %v", err)
	}
	if err := RewriteConfigFile(serialPath, 0o600, bootstrapLikeEdit); err != nil {
		t.Fatalf("serial second bootstrap: %v", err)
	}
	want, err := os.ReadFile(serialPath)
	if err != nil {
		t.Fatal(err)
	}

	dir, path, referent := setup(t)
	created := make(chan struct{})
	resume := make(chan struct{})
	var once sync.Once
	configIOHook = func(stage, p string) {
		if stage == "created" && p == path {
			once.Do(func() {
				close(created)
				<-resume
			})
		}
	}
	t.Cleanup(func() { configIOHook = nil })

	first := make(chan error, 1)
	go func() { first <- RewriteConfigFile(path, 0o600, bootstrapLikeEdit) }()
	select {
	case <-created:
	case <-time.After(5 * time.Second):
		t.Fatal("the first bootstrap never created the file")
	}
	second := make(chan error, 1)
	go func() { second <- RewriteConfigFile(path, 0o600, bootstrapLikeEdit) }()
	select {
	case err := <-second:
		if err != nil {
			close(resume)
			t.Fatalf("second bootstrap: %v", err)
		}
	case <-time.After(5 * time.Second):
		close(resume)
		t.Fatal("the second bootstrap waited on the paused first one")
	}
	close(resume)
	if err := <-first; err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("raced bootstrap through a dangling symlink wrote %q, want the serial bootstrap's %q", got, want)
	}
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("config.yaml is no longer a symlink (%v)", err)
	}
	if fi, err := os.Stat(referent); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("referent dotfile.yaml: %v, want a 0600 file", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("config dir holds %v, want config.yaml and dotfile.yaml only (no temp file)", entries)
	}
}

// TestRewriteConfigFile_DanglingSymlinkChain: config.yaml -> a, a -> sub/b,
// sub -> a directory elsewhere, b absent. A set creates the file the chain
// names (in sub's real directory), a read through config.yaml sees it, and
// both links stay links.
func TestRewriteConfigFile_DanglingSymlinkChain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	path := setRewriteTestHome(t, t.TempDir())
	dir := filepath.Dir(path)
	realSub := filepath.Join(t.TempDir(), "real-sub")
	for _, d := range []string{dir, realSub} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(realSub, filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("sub", "b"), filepath.Join(dir, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a", path); err != nil {
		t.Fatal(err)
	}

	if err := SetUserYamlConfig("rewrite.chain", "made"); err != nil {
		t.Fatalf("SetUserYamlConfig through a dangling chain: %v", err)
	}
	if got, ok := readYamlValueAtPath(path, "rewrite.chain"); !ok || got != "made" {
		t.Fatalf("read through config.yaml: rewrite.chain = %q (found %v), want made", got, ok)
	}
	if got, _ := readYamlValueAtPath(filepath.Join(realSub, "b"), "rewrite.chain"); got != "made" {
		t.Fatalf("the chain's referent %s holds rewrite.chain = %q, want made", filepath.Join(realSub, "b"), got)
	}
	for _, link := range []string{path, filepath.Join(dir, "a")} {
		if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s is no longer a symlink (%v)", filepath.Base(link), err)
		}
	}
	if entries, _ := os.ReadDir(realSub); len(entries) != 1 {
		t.Fatalf("%s holds %v, want only b (no temp file)", realSub, entries)
	}

	// A dangling link whose referent's directory is absent fails as before.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("missing-dir", "c.yaml"), path); err != nil {
		t.Fatal(err)
	}
	if err := SetUserYamlConfig("rewrite.chain", "x"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("SetUserYamlConfig through a link into an absent directory = %v, want not-exist", err)
	}
}

// TestRewriteConfigFile_LongNameDanglingBootstrapRace: config.yaml links to an
// absent file whose basename is 254 bytes, valid where NAME_MAX is 255. The
// temp name must not grow with the target's name: otherwise its create fails,
// the in-place fallback publishes an empty file, and a concurrent bootstrap
// writes the existing-file formatting. The first bootstrap-like edit is paused
// at "created" and a second runs to the end; the final bytes equal a serial
// bootstrap's and no temp file is left.
func TestRewriteConfigFile_LongNameDanglingBootstrapRace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	long := strings.Repeat("d", 249) + ".yaml" // 254 bytes
	setup := func(t *testing.T) (dir, path, referent string) {
		dir = t.TempDir()
		requireHardLinks(t, dir)
		probe := filepath.Join(dir, strings.Repeat("n", 255))
		if err := os.WriteFile(probe, nil, 0o600); err != nil {
			t.Skipf("NAME_MAX below 255 here: %v", err)
		}
		if err := os.Remove(probe); err != nil {
			t.Fatal(err)
		}
		path = filepath.Join(dir, "config.yaml")
		referent = filepath.Join(dir, long)
		if err := os.Symlink(long, path); err != nil {
			t.Fatal(err)
		}
		return dir, path, referent
	}

	_, serialPath, _ := setup(t)
	for i := 0; i < 2; i++ {
		if err := RewriteConfigFile(serialPath, 0o600, bootstrapLikeEdit); err != nil {
			t.Fatalf("serial bootstrap %d: %v", i, err)
		}
	}
	want, err := os.ReadFile(serialPath)
	if err != nil {
		t.Fatal(err)
	}

	dir, path, referent := setup(t)
	created := make(chan struct{})
	resume := make(chan struct{})
	var once sync.Once
	configIOHook = func(stage, p string) {
		if stage == "created" && p == path {
			once.Do(func() {
				close(created)
				<-resume
			})
		}
	}
	t.Cleanup(func() { configIOHook = nil })

	first := make(chan error, 1)
	go func() { first <- RewriteConfigFile(path, 0o600, bootstrapLikeEdit) }()
	select {
	case <-created:
	case <-time.After(5 * time.Second):
		t.Fatal("the first bootstrap never created the file")
	}
	second := make(chan error, 1)
	go func() { second <- RewriteConfigFile(path, 0o600, bootstrapLikeEdit) }()
	select {
	case err := <-second:
		if err != nil {
			close(resume)
			t.Fatalf("second bootstrap: %v", err)
		}
	case <-time.After(5 * time.Second):
		close(resume)
		t.Fatal("the second bootstrap waited on the paused first one")
	}
	close(resume)
	if err := <-first; err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}

	got, err := os.ReadFile(referent)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("raced bootstrap to a 254-byte referent wrote %q, want the serial bootstrap's %q", got, want)
	}
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("config.yaml is no longer a symlink (%v)", err)
	}
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		if e.Name() != "config.yaml" && e.Name() != long {
			names = append(names, e.Name())
		}
	}
	if len(names) != 0 {
		t.Fatalf("config dir holds extra entries %v, want config.yaml and the referent only (no temp file)", names)
	}
}

// TestRewriteConfigFile_FirstCreateErrorNamesConfig: an I/O error on the
// whole create's temp file is reported against config.yaml, the file the
// caller asked for, never the temp name.
func TestRewriteConfigFile_FirstCreateErrorNamesConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	configTempFileHook = func(tmp *os.File) { _ = tmp.Close() } // the write then fails
	t.Cleanup(func() { configTempFileHook = nil })

	err := RewriteConfigFile(path, 0o600, func([]byte, bool) ([]byte, error) {
		return []byte("k: v\n"), nil
	})
	if err == nil {
		t.Fatal("RewriteConfigFile with a failing temp write = nil, want an error")
	}
	msg := labelRewriteErr(err, "config.yaml").Error()
	if strings.Contains(msg, ".bd-config-") || !strings.Contains(msg, path) {
		t.Fatalf("first-create write error = %q, want it to name %s, not the temp file", msg, path)
	}
	var pe *fs.PathError
	if !errors.As(err, &pe) || pe.Path != path || pe.Op != "write" {
		t.Fatalf("first-create write error = %#v, want a *fs.PathError{Op: write, Path: %s}", err, path)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("a failed first create left %v", entries)
	}
}

// TestConfigWriters_DanglingFirstCreateErrorNamesRequestedPath: config.yaml
// is a symlink to an absent referent.yaml. An I/O error on the whole create's
// temp file must name the requested config.yaml (as os.WriteFile did), not the
// referent or the temp file, with the temp error's Op and Err.
func TestConfigWriters_DanglingFirstCreateErrorNamesRequestedPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	writers := map[string]func(path string) error{
		"RewriteConfigFile": func(path string) error {
			return RewriteConfigFile(path, 0o600, func([]byte, bool) ([]byte, error) {
				return []byte("k: v\n"), nil
			})
		},
		"ReplaceConfigFile": func(path string) error {
			return ReplaceConfigFile(path, 0o600, []byte("k: v\n"))
		},
	}
	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.Symlink("referent.yaml", path); err != nil {
				t.Fatal(err)
			}
			configTempFileHook = func(tmp *os.File) { _ = tmp.Close() } // the write then fails
			t.Cleanup(func() { configTempFileHook = nil })

			err := write(path)
			var pe *fs.PathError
			if !errors.As(err, &pe) {
				t.Fatalf("%s with a failing temp write = %v, want a *fs.PathError", name, err)
			}
			if pe.Path != path || pe.Op != "write" || !errors.Is(pe.Err, os.ErrClosed) {
				t.Fatalf("%s error = {Op: %q, Path: %q, Err: %v}, want {Op: write, Path: %s, Err: %v}", name, pe.Op, pe.Path, pe.Err, path, os.ErrClosed)
			}
			if _, err := os.Lstat(filepath.Join(dir, "referent.yaml")); !os.IsNotExist(err) {
				t.Fatalf("a failed first create left referent.yaml (%v)", err)
			}
		})
	}
}
