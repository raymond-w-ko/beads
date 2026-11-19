package timelog

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

var fixedAt = time.Date(2026, 11, 19, 18, 49, 29, 123456789, time.UTC)

func TestTimeLogFormatExactBytes(t *testing.T) {
	cases := []struct {
		name string
		line Line
		want string
	}{
		{
			name: "with open_ms",
			line: Line{At: fixedAt, Verb: "dep-add", Exit: 0, MS: 412, OpenMS: 97, HasOpen: true},
			want: `{"tool":"bd-time","at":"2026-11-19T18:49:29.123Z","verb":"dep-add","exit":0,"ms":412,"open_ms":97}` + "\n",
		},
		{
			name: "without open_ms",
			line: Line{At: fixedAt, Verb: "version", Exit: 0, MS: 3},
			want: `{"tool":"bd-time","at":"2026-11-19T18:49:29.123Z","verb":"version","exit":0,"ms":3}` + "\n",
		},
		{
			name: "nonzero exit, open_ms zero, non-UTC time",
			line: Line{At: fixedAt.In(time.FixedZone("x", -5*3600)), Verb: "update", Exit: 14, MS: 0, OpenMS: 0, HasOpen: true},
			want: `{"tool":"bd-time","at":"2026-11-19T18:49:29.123Z","verb":"update","exit":14,"ms":0,"open_ms":0}` + "\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(Format(tc.line)); got != tc.want {
				t.Fatalf("Format:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestTimeLogFormatUnder512Bytes(t *testing.T) {
	for _, exit := range []int{math.MinInt, math.MaxInt} {
		l := Line{At: time.Date(9999, 12, 31, 23, 59, 59, 999e6, time.UTC), Verb: strings.Repeat("a", 32), Exit: exit,
			MS: math.MinInt64, OpenMS: math.MinInt64, HasOpen: true}
		b := Format(l)
		if len(b) >= MaxLineBytes {
			t.Fatalf("line is %d bytes, want < %d: %s", len(b), MaxLineBytes, b)
		}
		if bytes.Count(b, []byte("\n")) != 1 || b[len(b)-1] != '\n' {
			t.Fatalf("want exactly one trailing newline: %q", b)
		}
	}
}

func TestTimeLogFormatWritesVerbWholeAsJSON(t *testing.T) {
	for _, verb := range []string{
		"migrate-from-shared-server-to-proxied-server",
		"__complete",
		"quote\"back\\slash\x01<&>é",
	} {
		b := Format(Line{At: fixedAt, Verb: verb, MS: 1})
		var got struct {
			Verb string `json:"verb"`
		}
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("verb %q: line is not JSON: %v: %s", verb, err, b)
		}
		if got.Verb != verb {
			t.Fatalf("verb round-trip: got %q, want %q", got.Verb, verb)
		}
	}
}

func TestTimeLogWriteDropsLineOf512BytesOrMore(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "time.log")
	oldPath := path
	path = p
	t.Cleanup(func() { path = oldPath })

	// Find the longest verb whose line still fits, then one byte more.
	fits := ""
	for n := 1; ; n++ {
		v := strings.Repeat("v", n)
		if len(Format(Line{At: time.Now(), Verb: v, MS: time.Since(start).Milliseconds()})) >= MaxLineBytes-8 {
			break
		}
		fits = v
	}
	Write(strings.Repeat("v", 600), 0)
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("a line of 512 bytes or more was written (stat err %v)", err)
	}
	Write(fits, 0) // positive control: the helper writes a line that fits
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("positive control wrote nothing: %v", err)
	}
	if len(data) >= MaxLineBytes || !strings.Contains(string(data), `"verb":"`+fits+`"`) {
		t.Fatalf("positive control line (%d bytes): %s", len(data), data)
	}
}

func TestTimeLogAppendAppendsAndCreates0600(t *testing.T) {
	p := filepath.Join(t.TempDir(), "time.log")
	first := Format(Line{At: fixedAt, Verb: "list", MS: 1})
	second := Format(Line{At: fixedAt, Verb: "show", Exit: 1, MS: 2, OpenMS: 1, HasOpen: true})
	if err := Append(p, first); err != nil {
		t.Fatalf("first Append: %v", err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if mode := fi.Mode().Perm(); mode != 0o600 {
			t.Fatalf("new file mode = %o, want 600", mode)
		}
	}
	if err := Append(p, second); err != nil {
		t.Fatalf("second Append: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := string(first) + string(second); string(got) != want {
		t.Fatalf("file:\n got %q\nwant %q", got, want)
	}
}

func TestTimeLogWriteUsesMarks(t *testing.T) {
	p := filepath.Join(t.TempDir(), "time.log")
	oldPath := path
	path = p
	t.Cleanup(func() {
		path = oldPath
		opened.Store(false)
		openedAt.Store(0)
	})

	Write("version", 0)
	MarkOpen()
	time.Sleep(5 * time.Millisecond)
	MarkOpen() // only the first counts
	Write("dep-add", 3)

	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %q", data)
	}
	if strings.Contains(lines[0], "open_ms") || !strings.Contains(lines[0], `"verb":"version","exit":0,"ms":`) {
		t.Fatalf("line before MarkOpen: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"verb":"dep-add","exit":3,"ms":`) || !strings.Contains(lines[1], `,"open_ms":`) {
		t.Fatalf("line after MarkOpen: %s", lines[1])
	}
	if want := time.Duration(openedAt.Load()).Milliseconds(); !strings.HasSuffix(lines[1], `,"open_ms":`+strconv.FormatInt(want, 10)+`}`) {
		t.Fatalf("open_ms is not the first mark (%dms): %s", want, lines[1])
	}
}

// helperEnv marks the re-executed test binary as the timelog helper process.
const helperEnv = "BD_TIMELOG_TEST_HELPER"

// TestTimeLogHelperProcess is the child of runHelper: a process whose
// BD_TIME_LOG is whatever the parent started it with. It calls Write once and
// exits 0 without printing anything of its own.
func TestTimeLogHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		t.Skip("helper process only")
	}
	Write("helper", 7)
	os.Exit(0)
}

// runHelper runs the helper process in dir with env set (a value of "\x00"
// unsets the key) and returns its combined output and wall time.
func runHelper(t *testing.T, dir string, env map[string]string) (string, time.Duration) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestTimeLogHelperProcess$", "-test.count=1")
	cmd.Dir = dir
	base := []string{}
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if strings.EqualFold(key, EnvVar) {
			continue
		}
		if _, ok := env[key]; ok {
			continue
		}
		base = append(base, kv)
	}
	base = append(base, helperEnv+"=1")
	for k, v := range env {
		if v != "\x00" {
			base = append(base, k+"="+v)
		}
	}
	cmd.Env = base
	began := time.Now()
	out, err := cmd.CombinedOutput()
	took := time.Since(began)
	if err != nil {
		t.Fatalf("helper process: %v\n%s", err, out)
	}
	return string(out), took
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func TestTimeLogUnsetAndEmptyTouchNoFile(t *testing.T) {
	for name, val := range map[string]string{"unset": "\x00", "empty": ""} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			out, _ := runHelper(t, dir, map[string]string{EnvVar: val})
			if out != "" {
				t.Fatalf("helper printed %q", out)
			}
			if names := dirEntries(t, dir); len(names) != 0 {
				t.Fatalf("helper created %v in its working directory", names)
			}
		})
	}
	// Positive control: the same helper with the variable set writes a line,
	// so the empty directories above prove the variable, not a dead helper.
	dir := t.TempDir()
	p := filepath.Join(dir, "time.log")
	if out, _ := runHelper(t, dir, map[string]string{EnvVar: p}); out != "" {
		t.Fatalf("helper printed %q", out)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("positive control wrote no file: %v", err)
	}
	if !strings.HasPrefix(string(data), `{"tool":"bd-time","at":"`) || !strings.Contains(string(data), `"verb":"helper","exit":7,"ms":`) {
		t.Fatalf("positive control line: %q", data)
	}
}

func TestTimeLogUnwritableTargetsReturnAtOnceSilently(t *testing.T) {
	dir := t.TempDir()
	targets := map[string]string{
		"missing parent": filepath.Join(dir, "no", "such", "dir", "time.log"),
		"directory":      dir,
	}
	if runtime.GOOS != "windows" {
		ro := filepath.Join(dir, "ro")
		if err := os.Mkdir(ro, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
		if os.Geteuid() != 0 {
			targets["read-only directory"] = filepath.Join(ro, "time.log")
		}
		fifo := filepath.Join(dir, "fifo")
		if out, err := exec.Command("mkfifo", fifo).CombinedOutput(); err != nil {
			t.Fatalf("mkfifo: %v\n%s", err, out)
		}
		targets["fifo with no reader"] = fifo
	}
	for name, p := range targets {
		t.Run(name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() { done <- Append(p, Format(Line{At: fixedAt, Verb: "x"})) }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatalf("Append(%s) succeeded, want an error", p)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("Append(%s) blocked", p)
			}
			out, took := runHelper(t, t.TempDir(), map[string]string{EnvVar: p})
			if out != "" {
				t.Fatalf("Write printed %q", out)
			}
			if took > 10*time.Second {
				t.Fatalf("helper took %v", took)
			}
		})
	}
}

func TestTimeLogFullPipeCostsNoWait(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no FIFOs on Windows")
	}
	fifo := filepath.Join(t.TempDir(), "fifo")
	if out, err := exec.Command("mkfifo", fifo).CombinedOutput(); err != nil {
		t.Fatalf("mkfifo: %v\n%s", err, out)
	}
	// A reader that never reads: open for read without blocking, then fill
	// the pipe through a second writer.
	r, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	line := Format(Line{At: fixedAt, Verb: "fill"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := Append(fifo, line); err != nil {
			break // pipe full
		}
		if time.Now().After(deadline) {
			t.Fatal("pipe never filled")
		}
	}
	done := make(chan error, 1)
	go func() { done <- Append(fifo, line) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Append to a full pipe succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Append to a full pipe blocked")
	}
}

func TestTimeLogEnvWithoutDropsOnlyTheKey(t *testing.T) {
	env := []string{
		"HOME=/home/u",
		"BD_TIME_LOG=/tmp/bd-time.log",
		"BD_TIME_LOGX=/kept",
		"bd_time_log=/tmp/lower.log",
		"=C:=C:\\",
		"NOEQUALS",
		"PATH=/usr/bin",
		"BD_TIME_LOG=",
	}
	got := EnvWithout(env)
	want := []string{"HOME=/home/u", "BD_TIME_LOGX=/kept"}
	if runtime.GOOS != "windows" {
		// Exact on Unix, as os.Getenv reads it: another case is another
		// variable, which the child keeps.
		want = append(want, "bd_time_log=/tmp/lower.log")
	}
	want = append(want, "=C:=C:\\", "NOEQUALS", "PATH=/usr/bin")
	if !slices.Equal(got, want) {
		t.Fatalf("EnvWithout(%q)\n got %q\nwant %q", env, got, want)
	}

	plain := []string{"HOME=/home/u", "PATH=/usr/bin"}
	same := EnvWithout(plain)
	if !slices.Equal(same, plain) {
		t.Fatalf("EnvWithout(%q) = %q, want it unchanged", plain, same)
	}
	same[0] = "HOME=/changed"
	if plain[0] != "HOME=/home/u" {
		t.Fatalf("EnvWithout returned its input, not a copy")
	}
}

func TestTimeLogSymlinkLastComponentRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no O_NOFOLLOW on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.log")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "dangling.log")
	missing := filepath.Join(dir, "would-be-created")
	if err := os.Symlink(missing, dangling); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{"to a file": link, "dangling": dangling} {
		t.Run(name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() { done <- Append(p, Format(Line{At: fixedAt, Verb: "x"})) }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatalf("Append(%s) through a symlink succeeded", p)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("Append(%s) blocked", p)
			}
			if out, _ := runHelper(t, t.TempDir(), map[string]string{EnvVar: p}); out != "" {
				t.Fatalf("Write printed %q", out)
			}
		})
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "kept\n" {
		t.Fatalf("symlink target = %q, %v; want it untouched", data, err)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatalf("dangling symlink's target was created: %v", err)
	}
}

func TestTimeLogRelativePathTouchesNoFile(t *testing.T) {
	for name, val := range map[string]string{
		"bare name":  "time.log",
		"dot slash":  "./time.log",
		"subdir":     filepath.Join("sub", "time.log"),
		"parent dir": filepath.Join("..", "time.log"),
	} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "wd")
			if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
				t.Fatal(err)
			}
			if out, _ := runHelper(t, dir, map[string]string{EnvVar: val}); out != "" {
				t.Fatalf("helper printed %q", out)
			}
			for _, d := range []string{parent, dir, filepath.Join(dir, "sub")} {
				for _, n := range dirEntries(t, d) {
					if n != "wd" && n != "sub" {
						t.Fatalf("relative BD_TIME_LOG=%q created %s in %s", val, n, d)
					}
				}
			}
		})
	}
}
