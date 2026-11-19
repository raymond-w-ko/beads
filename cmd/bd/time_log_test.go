//go:build cgo

package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage/dolt"
	"github.com/steveyegge/beads/internal/timelog"
	"github.com/steveyegge/beads/internal/types"
)

// timeLogVerbRE is the bound the operator's monitor takes verbs within; bd
// writes every verb whole regardless. A command outside it is a decision.
var timeLogVerbRE = regexp.MustCompile(`^[a-z][a-z-]{0,31}$`)

// timeLogVerbsOutsidePattern are the verbs known not to match timeLogVerbRE
// (too long); bd writes them whole and the monitor decides what to do.
var timeLogVerbsOutsidePattern = map[string]bool{
	"migrate-from-proxied-server-to-server":        true,
	"migrate-from-proxied-server-to-shared-server": true,
	"migrate-from-server-to-proxied-server":        true,
	"migrate-from-shared-server-to-proxied-server": true,
}

// TestTimeLogVerbsOfCommandTree walks the whole command tree, as main() and
// ExecuteC build it (help and completion commands included), and fails for
// any command whose bd-time verb is outside the monitor's pattern and not
// one of the listed exceptions. The longest verb must still give a line
// under 512 bytes with the largest numbers.
//
// Cobra's own hidden completion-request commands (__complete,
// __completeNoDesc) are skipped by name. Cobra adds them to the shared
// rootCmd inside the first ExecuteC of a completion request and never
// removes them, so whether they are in the tree depends on which test ran
// first (TestCompleteCommandInNoDbCommandsList runs one in-process). Their
// verb is intended as cobra names it; TestTimeLogVerbs checks a completion
// request writes "verb":"__complete".
func TestTimeLogVerbsOfCommandTree(t *testing.T) {
	rootCmd.InitDefaultHelpCmd()
	rootCmd.InitDefaultCompletionCmd()
	seen := 0
	longest := ""
	excepted := map[string]bool{}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if c == rootCmd && (sub.Name() == cobra.ShellCompRequestCmd || sub.Name() == cobra.ShellCompNoDescRequestCmd) {
				continue
			}
			seen++
			v := timeLogVerb(sub)
			if len(v) > len(longest) {
				longest = v
			}
			if timeLogVerbsOutsidePattern[v] {
				excepted[v] = true
			} else if !timeLogVerbRE.MatchString(v) {
				t.Errorf("command %q has bd-time verb %q, outside %s and not a listed exception", sub.CommandPath(), v, timeLogVerbRE)
			}
			walk(sub)
		}
	}
	walk(rootCmd)
	if seen < 50 {
		t.Fatalf("walked only %d commands; the tree is not built", seen)
	}
	for v := range timeLogVerbsOutsidePattern {
		if !excepted[v] {
			t.Errorf("listed exception %q is no longer a command; drop it from the list", v)
		}
	}
	if got := timeLogVerb(rootCmd); got != "bd" {
		t.Fatalf("root verb = %q, want bd", got)
	}
	if got := timeLogVerb(nil); got != "bd" {
		t.Fatalf("nil command verb = %q, want bd", got)
	}
	for _, exit := range []int{math.MinInt, math.MaxInt} {
		line := timelog.Format(timelog.Line{
			At: time.Date(9999, 12, 31, 23, 59, 59, 999e6, time.UTC), Verb: longest, Exit: exit,
			MS: math.MinInt64, OpenMS: math.MinInt64, HasOpen: true,
		})
		if len(line) >= timelog.MaxLineBytes {
			t.Fatalf("longest verb %q gives a %d-byte line, want < %d", longest, len(line), timelog.MaxLineBytes)
		}
	}
	t.Logf("walked %d commands; longest verb %q (%d bytes)", seen, longest, len(longest))
}

type timeLogLine struct {
	Tool   string `json:"tool"`
	At     string `json:"at"`
	Verb   string `json:"verb"`
	Exit   int    `json:"exit"`
	MS     *int64 `json:"ms"`
	OpenMS *int64 `json:"open_ms"`
}

var timeLogAtRE = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`)

// readTimeLog parses every line of the BD_TIME_LOG file at p.
func readTimeLog(t *testing.T, p string) ([]timeLogLine, []string) {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	raw := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	lines := make([]timeLogLine, 0, len(raw))
	for _, r := range raw {
		var l timeLogLine
		if err := json.Unmarshal([]byte(r), &l); err != nil {
			t.Fatalf("line %q: %v", r, err)
		}
		if l.Tool != timelog.Tool || !timeLogAtRE.MatchString(l.At) || l.MS == nil || *l.MS < 0 {
			t.Fatalf("malformed line %q", r)
		}
		lines = append(lines, l)
	}
	return lines, raw
}

// timeLogEnv is the subprocess environment: no inherited BEADS_*/BD_*
// selection, a scratch HOME, no metrics flusher, plus extra.
func timeLogEnv(t *testing.T, extra ...string) []string {
	t.Helper()
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "BEADS_") || strings.HasPrefix(kv, "BD_") {
			continue
		}
		key, _, _ := strings.Cut(kv, "=")
		if key == "HOME" || key == "XDG_CONFIG_HOME" {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"HOME="+t.TempDir(),
		"XDG_CONFIG_HOME="+t.TempDir(),
		"BEADS_TEST_MODE=1",
		"BD_DISABLE_METRICS=1",
		"BD_DISABLE_EVENT_FLUSH=1",
		"GIT_TERMINAL_PROMPT=0",
	)
	return append(env, extra...)
}

// runTimeLogBD runs bd in dir and returns its combined output and exit code.
func runTimeLogBD(t *testing.T, bd, dir string, env []string, args ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bd, args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() < 0 {
			t.Fatalf("bd %v: %v\n%s", args, err, out)
		}
		code = ee.ExitCode()
	}
	return string(out), code
}

// TestTimeLogVerbs runs the built bd for each kind of exit that names its
// verb from main()'s ExecuteC result and checks the one line each appends.
func TestTimeLogVerbs(t *testing.T) {
	bd := buildBDForInitTests(t)
	cases := []struct {
		name     string
		env      []string
		args     []string
		verb     string
		exitZero bool
	}{
		{name: "bare root", args: nil, verb: "bd", exitZero: true},
		{name: "unknown command", args: []string{"no-such-command"}, verb: "bd"},
		{name: "flag error", args: []string{"version", "--no-such-flag"}, verb: "version"},
		{name: "help flag", args: []string{"dep", "add", "--help"}, verb: "dep-add", exitZero: true},
		{name: "alias", args: []string{"done"}, verb: "close"},
		{name: "renamed root", env: []string{"BD_NAME=ops"}, args: nil, verb: "bd", exitZero: true},
		{name: "renamed root subcommand", env: []string{"BD_NAME=ops"}, args: []string{"dep", "add", "--help"}, verb: "dep-add", exitZero: true},
		{name: "version", args: []string{"version"}, verb: "version", exitZero: true},
		// A shell-completion request: cobra's hidden command, named as cobra
		// names it, outside the monitor's pattern.
		{name: "completion request", args: []string{"__complete", "li"}, verb: "__complete", exitZero: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			logPath := filepath.Join(t.TempDir(), "bd-time.log")
			env := timeLogEnv(t, append([]string{timelog.EnvVar + "=" + logPath}, tc.env...)...)
			out, code := runTimeLogBD(t, bd, dir, env, tc.args...)
			if tc.exitZero != (code == 0) {
				t.Fatalf("bd %v exited %d\n%s", tc.args, code, out)
			}
			lines, raw := readTimeLog(t, logPath)
			if len(lines) != 1 {
				t.Fatalf("want one line, got %q", raw)
			}
			l := lines[0]
			if l.Verb != tc.verb || l.Exit != code {
				t.Fatalf("line %q: want verb %q exit %d", raw[0], tc.verb, code)
			}
			if l.OpenMS != nil {
				t.Fatalf("line %q has open_ms; bd %v opened no store", raw[0], tc.args)
			}
		})
	}
}

// TestTimeLogProjectEnvFileRefused: a .beads/.env that sets BD_TIME_LOG gets
// the refusal line and never names the file, while the shell's BD_TIME_LOG
// still gets the line.
func TestTimeLogProjectEnvFileRefused(t *testing.T) {
	bd := buildBDForInitTests(t)
	repo := t.TempDir()
	beadsDir := filepath.Join(repo, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A server-mode workspace whose server is unreachable: bd selects the
	// workspace and loads its .env, then fails the open.
	if err := (&configfile.Config{
		Database:       "dolt",
		Backend:        configfile.BackendDolt,
		DoltMode:       configfile.DoltModeServer,
		DoltServerHost: "127.0.0.1",
		DoltServerPort: 1,
		DoltDatabase:   "timelog_probe",
	}).Save(beadsDir); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(repo, "planted.log")
	if err := os.WriteFile(filepath.Join(beadsDir, ".env"), []byte("BD_TIME_LOG="+planted+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const refusal = "beads: ignoring BD_TIME_LOG from .beads/.env (output file paths must come from your shell environment)"

	t.Run("unset in shell", func(t *testing.T) {
		out, code := runTimeLogBD(t, bd, repo, timeLogEnv(t), "list")
		if code == 0 {
			t.Fatalf("bd list against an unreachable server exited 0\n%s", out)
		}
		if !strings.Contains(out, refusal) {
			t.Fatalf("no refusal line in output:\n%s", out)
		}
		if _, err := os.Stat(planted); !os.IsNotExist(err) {
			t.Fatalf("the .env path was written (stat err %v)", err)
		}
	})
	t.Run("set in shell", func(t *testing.T) {
		logPath := filepath.Join(t.TempDir(), "bd-time.log")
		out, code := runTimeLogBD(t, bd, repo, timeLogEnv(t, timelog.EnvVar+"="+logPath), "list")
		if !strings.Contains(out, refusal) {
			t.Fatalf("no refusal line in output:\n%s", out)
		}
		if _, err := os.Stat(planted); !os.IsNotExist(err) {
			t.Fatalf("the .env path was written (stat err %v)", err)
		}
		lines, raw := readTimeLog(t, logPath)
		if len(lines) != 1 || lines[0].Verb != "list" || lines[0].Exit != code || code == 0 {
			t.Fatalf("shell log: %q (bd exit %d)", raw, code)
		}
		if lines[0].OpenMS != nil {
			t.Fatalf("a failed open wrote open_ms: %q", raw[0])
		}
	})
}

// TestTimeLogLiveStore runs bd against the test harness's Dolt server
// (BEADS_TEST_ENV_RUN_DOLT=1): a command that opens the store writes open_ms
// within ms, one that opens none writes no open_ms, and a write refused in
// read-only mode (CheckReadonly's exit) writes its line with exit 1.
func TestTimeLogLiveStore(t *testing.T) {
	if testDoltServerPort == 0 {
		t.Skip("Dolt test server not available, skipping")
	}
	bd := buildBDForInitTests(t)

	repo := t.TempDir()
	beadsDir := filepath.Join(repo, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	database := uniqueTestDBName(t)
	if err := (&configfile.Config{
		Database:       "dolt",
		Backend:        configfile.BackendDolt,
		DoltMode:       configfile.DoltModeServer,
		DoltServerHost: "127.0.0.1",
		DoltServerPort: testDoltServerPort,
		DoltDatabase:   database,
	}).Save(beadsDir); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := dolt.New(ctx, &dolt.Config{
		Path:            filepath.Join(beadsDir, "dolt"),
		BeadsDir:        beadsDir,
		ServerHost:      "127.0.0.1",
		ServerPort:      testDoltServerPort,
		Database:        database,
		CreateIfMissing: true,
	})
	if err != nil {
		t.Fatalf("create test store: %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
		dropTestDatabase(database, testDoltServerPort)
	})
	if err := st.SetConfig(ctx, "issue_prefix", "tl"); err != nil {
		t.Fatalf("set issue_prefix: %v", err)
	}
	now := time.Now()
	if err := st.CreateIssue(ctx, &types.Issue{
		ID: "tl-1", Title: "bd-time live proof", Status: types.StatusOpen,
		Priority: 2, IssueType: types.TypeTask, CreatedAt: now, UpdatedAt: now,
	}, "test-user"); err != nil {
		t.Fatalf("create issue: %v", err)
	}

	logPath := filepath.Join(t.TempDir(), "bd-time.log")
	env := timeLogEnv(t,
		timelog.EnvVar+"="+logPath,
		"BEADS_TEST_SERVER=1",
		"BEADS_DIR="+beadsDir,
	)
	if out, code := runTimeLogBD(t, bd, repo, env, "list", "--json"); code != 0 || !strings.Contains(out, "bd-time live proof") {
		t.Fatalf("bd list exited %d\n%s", code, out)
	}
	if out, code := runTimeLogBD(t, bd, repo, env, "version"); code != 0 {
		t.Fatalf("bd version exited %d\n%s", code, out)
	}
	out, roCode := runTimeLogBD(t, bd, repo, env, "--readonly", "create", "refused in read-only mode")
	if roCode == 0 || !strings.Contains(out, "not allowed in read-only mode") {
		t.Fatalf("read-only create exited %d\n%s", roCode, out)
	}
	// prime skips the root pre-run's open and opens its store late, through
	// ensureStoreActive (formatMemoriesForPrime): its line still has open_ms.
	if out, code := runTimeLogBD(t, bd, repo, env, "prime"); code != 0 {
		t.Fatalf("bd prime exited %d\n%s", code, out)
	}

	lines, raw := readTimeLog(t, logPath)
	for _, r := range raw {
		t.Logf("bd-time line: %s", r)
	}
	if len(lines) != 4 {
		t.Fatalf("want 4 lines, got %q", raw)
	}
	list, version, ro, prime := lines[0], lines[1], lines[2], lines[3]
	if list.Verb != "list" || list.Exit != 0 || list.OpenMS == nil || *list.OpenMS < 0 || *list.OpenMS > *list.MS {
		t.Fatalf("list line %q: want verb list, exit 0, 0 <= open_ms <= ms", raw[0])
	}
	if version.Verb != "version" || version.Exit != 0 || version.OpenMS != nil {
		t.Fatalf("version line %q: want verb version, exit 0, no open_ms", raw[1])
	}
	if ro.Verb != "create" || ro.Exit != roCode || ro.Exit != 1 {
		t.Fatalf("read-only line %q: want verb create, exit %d", raw[2], roCode)
	}
	if prime.Verb != "prime" || prime.Exit != 0 || prime.OpenMS == nil || *prime.OpenMS < 0 || *prime.OpenMS > *prime.MS {
		t.Fatalf("late-open line %q: want verb prime, exit 0, 0 <= open_ms <= ms", raw[3])
	}
}
