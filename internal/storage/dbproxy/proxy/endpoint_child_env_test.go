package proxy

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/timelog"
)

// TestProxyChildCommandDropsTimeLog: the detached db-proxy-child is no call an
// operator made, so the command that starts it carries this process's
// environment without BD_TIME_LOG (bd-2r2cuv P2-1) and nothing else changed.
func TestProxyChildCommandDropsTimeLog(t *testing.T) {
	t.Setenv(timelog.EnvVar, "/tmp/bd-time-parent.log")
	t.Setenv("BD_PROXY_CHILD_ENV_PROBE", "kept")

	cmd := proxyChildCommand("/usr/bin/bd-self", []string{"db-proxy-child", "--port", "1"})
	if cmd.Env == nil {
		t.Fatal("proxy child Env is nil: it would inherit BD_TIME_LOG")
	}
	probe := false
	for _, kv := range cmd.Env {
		key, _, _ := strings.Cut(kv, "=")
		if key == timelog.EnvVar {
			t.Fatalf("proxy child env kept %q", kv)
		}
		if kv == "BD_PROXY_CHILD_ENV_PROBE=kept" {
			probe = true
		}
	}
	if !probe {
		t.Fatalf("proxy child env lost another variable: %q", cmd.Env)
	}
	if want := timelog.EnvWithout(os.Environ()); !slices.Equal(cmd.Env, want) {
		t.Fatalf("proxy child env = %q, want the parent's without BD_TIME_LOG, %q", cmd.Env, want)
	}
	if cmd.Stdin != nil || cmd.SysProcAttr == nil {
		t.Fatalf("proxy child: Stdin %v, SysProcAttr %v; want nil stdin, detached", cmd.Stdin, cmd.SysProcAttr)
	}
	if got := strings.Join(cmd.Args, " "); got != "/usr/bin/bd-self db-proxy-child --port 1" {
		t.Fatalf("proxy child args = %q", got)
	}
}
