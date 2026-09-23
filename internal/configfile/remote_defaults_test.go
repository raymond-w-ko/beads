//go:build remote_dolt_only && !cgo

package configfile

import (
	"path/filepath"
	"testing"
)

// Remote-only builds default to the tailnet server when env, config, and
// credentials are all unset.
func TestRemoteOnlyServerDefaults(t *testing.T) {
	for _, name := range []string{
		"BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_USER", "BEADS_DOLT_PASSWORD",
		"BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PORT",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("BEADS_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "missing"))

	c := &Config{}
	if got := c.GetDoltServerHost(); got != "dolt-db" {
		t.Errorf("GetDoltServerHost() = %q, want %q", got, "dolt-db")
	}
	if got := c.GetDoltServerUser(); got != "beads" {
		t.Errorf("GetDoltServerUser() = %q, want %q", got, "beads")
	}
	if got := c.GetDoltServerPassword(); got != "beads" {
		t.Errorf("GetDoltServerPassword() = %q, want %q", got, "beads")
	}

	t.Setenv("BEADS_DOLT_PASSWORD", "from-env")
	if got := c.GetDoltServerPassword(); got != "from-env" {
		t.Errorf("GetDoltServerPassword() with env = %q, want %q", got, "from-env")
	}
}
