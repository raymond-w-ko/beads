package main

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/lockfile"
)

// waitsForConfigLock proves that read takes path's shared config lock. It
// holds the exclusive lock, as a bd rewriting config.yaml does, and starts
// read while the file still holds its initial contents. Hook events are matched to path by
// file identity, not by string, since readers may canonicalize the path (a
// symlinked TMPDIR). Once the reader has opened the file ("reader-opened"),
// it must not get its lock ("reader-locked") or return while the exclusive
// lock is held. On Linux the kernel must also show the reader blocked: a
// waiting ("->") FLOCK READ entry for the file in /proc/locks, so a reader
// that skips the lock call, or fakes it, fails however long it stalls. Only
// then, still under the exclusive lock, is the file rewritten to after in
// place, and the lock released; the reader must then get its lock and report
// want, what it reports for after. A reader that read the file before its
// lock call saw only the initial contents and so fails too. Without /proc/locks the
// kernel check is skipped (logged) and a 100 ms hook window stands in.
func waitsForConfigLock(t *testing.T, path, after string, read func() string, want string) {
	t.Helper()
	isFixture := sameFileMatcher(t, path)
	opened := make(chan struct{})
	locked := make(chan struct{})
	var openedOnce, lockedOnce sync.Once
	restore := config.SetIOHookForTesting(func(stage, p string) {
		if !isFixture(p) {
			return
		}
		switch stage {
		case "reader-opened":
			openedOnce.Do(func() { close(opened) })
		case "reader-locked":
			lockedOnce.Do(func() { close(locked) })
		}
	})
	t.Cleanup(restore)

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := lockfile.FileLockExclusive(f); err != nil {
		t.Fatalf("lock %s: %v", path, err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_ = lockfile.FileUnlock(f)
		}
	}
	defer release()

	got := make(chan string, 1)
	go func() { got <- read() }()

	select {
	case <-opened:
	case v := <-got:
		t.Fatalf("reader returned %q without opening %s through the config reader", v, path)
	case <-time.After(5 * time.Second):
		t.Fatalf("reader never opened %s through the config reader", path)
	}

	// While the exclusive lock is held, the reader must neither lock nor
	// return; with /proc/locks, wait until the kernel shows it blocked.
	blocked := kernelBlockedReadCheck(t, f)
	deadline := time.After(5 * time.Second)
	window := time.After(100 * time.Millisecond)
	for waiting := true; waiting; {
		select {
		case <-locked:
			t.Fatal("reader got its shared lock while another bd held config.yaml's exclusive lock")
		case v := <-got:
			t.Fatalf("reader returned %q while another bd held config.yaml's exclusive lock", v)
		case <-deadline:
			t.Fatalf("no blocked FLOCK READ entry for %s appeared in /proc/locks within 5s: the reader is not waiting on the file's shared lock", path)
		case <-window:
			if blocked == nil {
				waiting = false // no /proc/locks: the hook window is the check
			}
		case <-time.After(5 * time.Millisecond):
			if blocked != nil && blocked() {
				waiting = false
			}
		}
	}

	// The reader is waiting on the lock: only now write the final bytes, as
	// the holder of the exclusive lock, then release.
	if err := f.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte(after), 0); err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		t.Fatal("reader never got its shared lock after the release")
	}
	select {
	case v := <-got:
		if v != want {
			t.Fatalf("reader after the release = %q, want %q (the whole new file)", v, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reader still running 5s after the lock was released")
	}
}

// sameFileMatcher returns a func reporting whether a hook's path names the
// same file as path: os.SameFile on both, else their EvalSymlinks forms.
func sameFileMatcher(t *testing.T, path string) func(p string) bool {
	t.Helper()
	want, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	wantReal, _ := filepath.EvalSymlinks(path)
	return func(p string) bool {
		if p == path {
			return true
		}
		if fi, err := os.Stat(p); err == nil {
			return os.SameFile(fi, want)
		}
		real, err := filepath.EvalSymlinks(p)
		return err == nil && wantReal != "" && real == wantReal
	}
}

// kernelBlockedReadCheck returns a func reporting whether /proc/locks shows a
// waiting ("->") FLOCK READ request on the file f is open on, which the test
// holds exclusively. The file's key ("major:minor:inode", as this kernel
// prints it) is taken from the test's own held FLOCK WRITE entry, matched by
// inode, so the device's print form never needs guessing. It returns nil,
// with a log line, where /proc/locks does not exist.
func kernelBlockedReadCheck(t *testing.T, f *os.File) func() bool {
	t.Helper()
	if _, err := os.Stat("/proc/locks"); err != nil {
		t.Logf("kernel lock check skipped: /proc/locks unavailable (%v); the hook window alone stands in", err)
		return nil
	}
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	ino, ok := inodeOf(fi)
	if !ok {
		t.Logf("kernel lock check skipped: no inode for %s", f.Name())
		return nil
	}
	inoSuffix := ":" + strconv.FormatUint(ino, 10)
	pid := strconv.Itoa(os.Getpid())
	key := ""
	for _, e := range readProcLocks(t) {
		if !e.blocked && e.kind == "FLOCK" && e.mode == "WRITE" && e.pid == pid && strings.HasSuffix(e.key, inoSuffix) {
			key = e.key
			break
		}
	}
	if key == "" {
		t.Fatalf("/proc/locks shows no FLOCK WRITE by this process on inode %d, though the test holds it", ino)
	}
	return func() bool {
		for _, e := range readProcLocks(t) {
			if e.blocked && e.kind == "FLOCK" && e.mode == "READ" && e.key == key {
				return true
			}
		}
		return false
	}
}

type procLock struct {
	blocked              bool
	kind, mode, pid, key string
}

// readProcLocks parses /proc/locks lines such as
// "1: FLOCK  ADVISORY  WRITE 1234 08:01:5678 0 EOF" and the waiters below
// them, "1: -> FLOCK  ADVISORY  READ  1234 08:01:5678 0 EOF".
func readProcLocks(t *testing.T) []procLock {
	t.Helper()
	data, err := os.ReadFile("/proc/locks")
	if err != nil {
		t.Fatalf("read /proc/locks: %v", err)
	}
	var out []procLock
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		fields = fields[1:] // the "N:" ordinal
		e := procLock{}
		if fields[0] == "->" {
			e.blocked = true
			fields = fields[1:]
		}
		if len(fields) < 5 {
			continue
		}
		e.kind, e.mode, e.pid, e.key = fields[0], fields[2], fields[3], fields[4]
		out = append(out, e)
	}
	return out
}

// inodeOf returns fi's inode number where the platform's stat has one (Ino).
func inodeOf(fi os.FileInfo) (uint64, bool) {
	v := reflect.ValueOf(fi.Sys())
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return 0, false
	}
	ino := v.FieldByName("Ino")
	if !ino.IsValid() || !ino.CanUint() {
		return 0, false
	}
	return ino.Uint(), true
}

// TestValidateSyncConfig_WaitsForConfigLock: validateSyncConfig reads
// config.yaml under its shared config lock, so while another bd holds the
// exclusive lock to rewrite the file it waits, then validates the whole new
// file.
func TestValidateSyncConfig_WaitsForConfigLock(t *testing.T) {
	t.Setenv("BEADS_DIR", "")
	repo := t.TempDir()
	beadsDir := filepath.Join(repo, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(beadsDir, "config.yaml")
	if err := os.WriteFile(path, []byte("prefix: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	after := "prefix: test\nfederation:\n  sovereignty: \"invalid-value\"\n"
	waitsForConfigLock(t, path, after, func() string {
		if strings.Contains(strings.Join(validateSyncConfig(repo), "\n"), "federation.sovereignty") {
			return "sovereignty flagged"
		}
		return "sovereignty not flagged"
	}, "sovereignty flagged")
}

// TestInitConfigYaml_CreatesWholeUnderConfigLock: bd init's createConfigYaml
// creates config.yaml whole: at the config writer's "created" stage the file
// already holds the whole template, so no reader ever sees it empty or
// partial. An existing config.yaml is left as it is.
func TestInitConfigYaml_CreatesWholeUnderConfigLock(t *testing.T) {
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(beadsDir, "config.yaml")
	want := string(renderInitConfigYAML("", false))

	var seen []string
	var mu sync.Mutex
	restore := config.SetIOHookForTesting(func(stage, p string) {
		if stage != "created" || filepath.Base(p) != "config.yaml" {
			return
		}
		data, err := os.ReadFile(path)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			seen = append(seen, "error: "+err.Error())
			return
		}
		seen = append(seen, string(data))
	})
	t.Cleanup(restore)

	if err := createConfigYaml(beadsDir, false, ""); err != nil {
		t.Fatalf("createConfigYaml: %v", err)
	}
	restore()
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("config writer \"created\" stage fired %d times, want once: config.yaml was not created through the config writer", len(seen))
	}
	if hardLinksWork(t, beadsDir) {
		if seen[0] != want {
			t.Fatalf("config.yaml just after its creation is %d bytes, want the whole %d-byte template", len(seen[0]), len(want))
		}
	} else {
		t.Log("whole-create check skipped: no hard links here, so the documented in-place create fallback runs")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != want {
		t.Fatalf("config.yaml after createConfigYaml is %d bytes (%v), want the %d-byte template", len(data), err, len(want))
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows' os.Stat reports 0666 for any writable file.
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("config.yaml mode %v; want 0600", fi.Mode())
	}

	const custom = "issue-prefix: mine\n"
	if err := os.WriteFile(path, []byte(custom), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := createConfigYaml(beadsDir, true, "other"); err != nil {
		t.Fatalf("createConfigYaml on an existing file: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != custom {
		t.Fatalf("createConfigYaml rewrote an existing config.yaml to %q", data)
	}
}

// hardLinksWork reports whether dir's filesystem makes hard links (the config
// writer's whole create needs them; without, it creates in place).
func hardLinksWork(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, ".hardlink-probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(probe)
	if err := os.Link(probe, probe+".link"); err != nil {
		return false
	}
	_ = os.Remove(probe + ".link")
	return true
}
