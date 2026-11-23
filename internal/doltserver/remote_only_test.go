//go:build remote_dolt_only && !cgo

package doltserver

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/localdolt"
)

// Remote-only builds never own, embed or start a server. These tests assert
// what the fenced resolvers return for the setups the local-Dolt tests skip
// (skipIfLocalDoltDisabled).

func clearRemoteOnlyEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"BEADS_DOLT_SERVER_MODE", "BEADS_DOLT_SHARED_SERVER", "BEADS_DOLT_SERVER_HOST",
		"BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PORT", "BEADS_DOLT_AUTO_START", "GT_ROOT",
	} {
		t.Setenv(name, "")
	}
	config.ResetForTesting()
}

func TestRemoteOnlyResolveServerModeIsAlwaysExternal(t *testing.T) {
	clearRemoteOnlyEnv(t)

	empty := t.TempDir()
	if mode := ResolveServerMode(empty); mode != ServerModeExternal {
		t.Errorf("empty dir: ResolveServerMode = %v, want external", mode)
	}
	if cfg := DefaultConfig(empty); cfg.Mode != ServerModeExternal {
		t.Errorf("empty dir: DefaultConfig.Mode = %v, want external", cfg.Mode)
	}

	for _, meta := range []*configfile.Config{
		{DoltMode: configfile.DoltModeEmbedded},
		{DoltMode: configfile.DoltModeProxiedServer},
		{DoltServerHost: "127.0.0.1"},
	} {
		dir := t.TempDir()
		if err := meta.Save(dir); err != nil {
			t.Fatal(err)
		}
		if mode := ResolveServerMode(dir); mode != ServerModeExternal {
			t.Errorf("metadata %+v: ResolveServerMode = %v, want external", *meta, mode)
		}
	}

	t.Setenv("BEADS_DOLT_SERVER_HOST", "localhost")
	if mode := ResolveServerMode(empty); mode != ServerModeExternal {
		t.Errorf("localhost env host: ResolveServerMode = %v, want external", mode)
	}
}

func TestRemoteOnlyDefaultConfigUsesServerDefaultPort(t *testing.T) {
	clearRemoteOnlyEnv(t)

	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	if cfg.Port != configfile.DefaultDoltServerPort || cfg.PortSource != PortSourceExternalHostDefault {
		t.Errorf("no source: port=%d source=%q, want %d/%q",
			cfg.Port, cfg.PortSource, configfile.DefaultDoltServerPort, PortSourceExternalHostDefault)
	}

	// A port file is a local server's record; remote-only builds ignore it.
	if err := writePortFile(dir, 14567); err != nil {
		t.Fatal(err)
	}
	if cfg := DefaultConfig(dir); cfg.Port != configfile.DefaultDoltServerPort {
		t.Errorf("port file: port=%d, want %d", cfg.Port, configfile.DefaultDoltServerPort)
	}
}

func TestRemoteOnlyAutoStartIsAlwaysDisabled(t *testing.T) {
	clearRemoteOnlyEnv(t)
	for _, env := range []string{"", "1", "true"} {
		t.Setenv("BEADS_DOLT_AUTO_START", env)
		if !IsAutoStartDisabled() {
			t.Errorf("BEADS_DOLT_AUTO_START=%q: IsAutoStartDisabled() = false, want true", env)
		}
	}
}

func TestRemoteOnlyKillStaleServersKillsNothing(t *testing.T) {
	clearRemoteOnlyEnv(t)
	dir := t.TempDir()

	got, err := killStaleServersForDir(dir, []int{111, 222},
		func(int, string) bool { return true },
		func(pid int) error {
			t.Errorf("kill(%d) called; remote-only builds never kill a server", pid)
			return nil
		},
	)
	if err != nil || len(got) != 0 {
		t.Errorf("killStaleServersForDir = %v, %v; want nothing killed, no error", got, err)
	}
}

func TestRemoteOnlyEnsureDoltInitRefuses(t *testing.T) {
	doltDir := filepath.Join(t.TempDir(), "dolt")
	err := ensureDoltInit(doltDir)
	if !errors.Is(err, localdolt.ErrDisabled) {
		t.Fatalf("ensureDoltInit error = %v, want localdolt.ErrDisabled", err)
	}
	if _, statErr := os.Stat(doltDir); !os.IsNotExist(statErr) {
		t.Errorf("ensureDoltInit created %s before refusing (stat err %v)", doltDir, statErr)
	}
}

func TestRemoteOnlyPhysicalRootsServerMetadataIsRemote(t *testing.T) {
	resetPhysicalRootEnv(t)
	beadsDir := newTestBeadsDir(t)
	writeBeadsMetadata(t, beadsDir, `{"database":"beads.db","dolt_mode":"server"}`)

	pr, err := ResolvePhysicalRoots(beadsDir)
	if err != nil {
		t.Fatal(err)
	}
	if !pr.RemoteBackend || len(pr.Roots) != 0 {
		t.Errorf("RemoteBackend=%v Roots=%v, want true and none (provenance: %s)", pr.RemoteBackend, pr.Roots, pr.Provenance)
	}
}

// A port file is bookkeeping for a server bd started; a remote-only build
// starts none, so the file must not move the dial target. In shared mode on
// a loopback host the target stays the shared default whether or not the
// shared dir holds a dolt-server.port.
func TestRemoteOnlySharedLoopbackPortIgnoresPortFile(t *testing.T) {
	clearRemoteOnlyEnv(t)
	sharedDir := t.TempDir()
	t.Setenv("BEADS_DOLT_SHARED_SERVER", "1")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "127.0.0.1")
	t.Setenv("BEADS_SHARED_SERVER_DIR", sharedDir)

	beadsDir := newTestBeadsDir(t)
	writeBeadsMetadata(t, beadsDir, `{"database":"beads.db","dolt_mode":"server"}`)

	before := DefaultConfig(beadsDir)
	if before.Port != DefaultSharedServerPort || before.PortSource != PortSourceSharedServerDefault {
		t.Errorf("no port file: port=%d source=%q, want %d/%q",
			before.Port, before.PortSource, DefaultSharedServerPort, PortSourceSharedServerDefault)
	}

	if err := writePortFile(sharedDir, 14567); err != nil {
		t.Fatal(err)
	}
	after := DefaultConfig(beadsDir)
	if after.Port != before.Port || after.PortSource != before.PortSource {
		t.Errorf("shared port file moved the dial: port=%d source=%q, want %d/%q as without it",
			after.Port, after.PortSource, before.Port, before.PortSource)
	}
}

func assertRemoteNoRoots(t *testing.T, pr PhysicalRoots) {
	t.Helper()
	if !pr.RemoteBackend || len(pr.Roots) != 0 {
		t.Errorf("RemoteBackend=%v Roots=%v, want true and none (provenance: %s)", pr.RemoteBackend, pr.Roots, pr.Provenance)
	}
	if pr.Mode != "server" {
		t.Errorf("Mode = %q, want %q (provenance: %s)", pr.Mode, "server", pr.Provenance)
	}
}

// The CLI picks server mode from env with no metadata.json (it substitutes
// the default config), so gate planning must not fall back to the local
// directory discovery and gate .beads/dolt.
func TestRemoteOnlyPhysicalRootsEnvServerModeWithoutMetadataIsRemote(t *testing.T) {
	resetPhysicalRootEnv(t)
	t.Setenv("BEADS_DOLT_SERVER_MODE", "1")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "127.0.0.1")
	beadsDir := newTestBeadsDir(t)
	if err := os.MkdirAll(filepath.Join(beadsDir, "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}

	pr, err := ResolvePhysicalRoots(beadsDir)
	if err != nil {
		t.Fatal(err)
	}
	assertRemoteNoRoots(t, pr)
}

func TestRemoteOnlyPhysicalRootsMetadataServerLoopbackIsRemote(t *testing.T) {
	resetPhysicalRootEnv(t)
	beadsDir := newTestBeadsDir(t)
	if err := os.MkdirAll(filepath.Join(beadsDir, "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeBeadsMetadata(t, beadsDir, `{"database":"beads.db","dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":43567}`)

	pr, err := ResolvePhysicalRoots(beadsDir)
	if err != nil {
		t.Fatal(err)
	}
	assertRemoteNoRoots(t, pr)
}

// With no metadata.json and no server env/config, a remote-only build still
// opens no embedded or local-server data, so the on-disk discovery must not
// name a root, whatever directories exist.
func TestRemoteOnlyPhysicalRootsNoMetadataIsRemote(t *testing.T) {
	resetPhysicalRootEnv(t)
	for _, tc := range []struct {
		name string
		dirs []string
	}{
		{name: "nothing on disk"},
		{name: "dolt present", dirs: []string{"dolt"}},
		{name: "embeddeddolt present", dirs: []string{"embeddeddolt"}},
		{name: "both present", dirs: []string{"dolt", "embeddeddolt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beadsDir := newTestBeadsDir(t)
			for _, d := range tc.dirs {
				if err := os.MkdirAll(filepath.Join(beadsDir, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			pr, err := ResolvePhysicalRoots(beadsDir)
			if err != nil {
				t.Fatal(err)
			}
			if !pr.RemoteBackend || len(pr.Roots) != 0 {
				t.Errorf("RemoteBackend=%v Roots=%v, want true and none (provenance: %s)", pr.RemoteBackend, pr.Roots, pr.Provenance)
			}
			if !strings.Contains(pr.Provenance, "remote-only") {
				t.Errorf("provenance %q does not name the remote-only build", pr.Provenance)
			}
		})
	}
}
