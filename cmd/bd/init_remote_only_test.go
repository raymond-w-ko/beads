//go:build remote_dolt_only && !cgo

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/beads"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/doltserver"
	"github.com/steveyegge/beads/internal/git"
	"github.com/steveyegge/beads/internal/localdolt"
	"github.com/steveyegge/beads/internal/utils"
	"github.com/steveyegge/beads/internal/workspacegate"
)

// A remote-only build starts no Dolt server, so a port file (bd's record of
// a server it started) must never change where init or bootstrap dials, and
// init must take no local physical Dolt gate for a server workspace.

func clearRemoteOnlyInitEnv(t *testing.T) {
	t.Helper()
	resetGateTestEnv(t)
	t.Setenv("BEADS_DOLT_SERVER_PORT", "")
	t.Setenv("BEADS_DOLT_PORT", "")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "127.0.0.1")
}

func writeRemoteOnlyPortFile(t *testing.T, dir string, port string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, doltserver.PortFileName), []byte(port), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteOnlyBootstrapServerPortIgnoresPortFiles(t *testing.T) {
	clearRemoteOnlyInitEnv(t)
	cfg := &configfile.Config{DoltMode: configfile.DoltModeServer}

	t.Run("local port file", func(t *testing.T) {
		beadsDir := filepath.Join(t.TempDir(), ".beads")
		if err := os.MkdirAll(beadsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		want := bootstrapServerPort(beadsDir, cfg, false)
		if want != configfile.DefaultDoltServerPort {
			t.Fatalf("no port file: port = %d, want %d", want, configfile.DefaultDoltServerPort)
		}
		writeRemoteOnlyPortFile(t, beadsDir, "14567")
		if got := bootstrapServerPort(beadsDir, cfg, false); got != want {
			t.Errorf("local port file moved the dial: port = %d, want %d as without it", got, want)
		}
	})

	t.Run("shared-dir port file", func(t *testing.T) {
		sharedDir := t.TempDir()
		t.Setenv("BEADS_SHARED_SERVER_DIR", sharedDir)
		beadsDir := filepath.Join(t.TempDir(), ".beads")
		if err := os.MkdirAll(beadsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		want := bootstrapServerPort(beadsDir, cfg, true)
		if want != doltserver.DefaultSharedServerPort {
			t.Fatalf("no port file: port = %d, want %d", want, doltserver.DefaultSharedServerPort)
		}
		writeRemoteOnlyPortFile(t, sharedDir, "14567")
		if got := bootstrapServerPort(beadsDir, cfg, true); got != want {
			t.Errorf("shared-dir port file moved the dial: port = %d, want %d as without it", got, want)
		}
	})
}

func TestRemoteOnlyInitPortIgnoresPortFile(t *testing.T) {
	clearRemoteOnlyInitEnv(t)

	beadsDir := filepath.Join(t.TempDir(), ".beads")
	writeRemoteOnlyPortFile(t, beadsDir, "14567")
	if port, source, shared := resolveInitPort(beadsDir); port != 0 || source != doltserver.PortSourceUnset || shared {
		t.Errorf("port file: resolveInitPort = %d/%q/%v, want 0/%q/false", port, source, shared, doltserver.PortSourceUnset)
	}

	t.Setenv("BEADS_DOLT_SHARED_SERVER", "1")
	port, source, shared := resolveInitPort(beadsDir)
	if port != doltserver.DefaultSharedServerPort || source != doltserver.PortSourceSharedServerDefault || !shared {
		t.Errorf("shared mode with port file: resolveInitPort = %d/%q/%v, want %d/%q/true",
			port, source, shared, doltserver.DefaultSharedServerPort, doltserver.PortSourceSharedServerDefault)
	}
}

func TestRemoteOnlyInitGatePlanHasNoPhysicalRoot(t *testing.T) {
	clearRemoteOnlyInitEnv(t)
	oldWait := exclusiveGateWait
	exclusiveGateWait = 0 // a busy gate fails at once instead of polling
	t.Cleanup(func() { exclusiveGateWait = oldWait })

	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	doltDir := filepath.Join(beadsDir, "dolt")

	if root := initMutationGateRoot(doltDir, false); root != "" {
		t.Errorf("server init: physical gate root = %q, want none", root)
	}
	if root := initMutationGateRoot(doltDir, true); root != doltDir {
		t.Errorf("proxied-server init: physical gate root = %q, want %q (the proxy's state lives there)", root, doltDir)
	}

	// Another holder of a local physical Dolt gate must not block init:
	// neither the planned root nor whatever the resolver's no-metadata or
	// embedded-metadata branches would name.
	for _, tc := range []struct {
		name     string
		mkDolt   bool
		metadata string
		heldRoot string
	}{
		{name: "dolt present, dolt gate held", mkDolt: true, heldRoot: "dolt"},
		{name: "dolt absent, embeddeddolt gate held", heldRoot: "embeddeddolt"},
		{name: "embedded metadata reinit, embeddeddolt gate held", heldRoot: "embeddeddolt",
			metadata: `{"backend":"dolt","database":"beads.db","dolt_mode":"embedded"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beadsDir := filepath.Join(t.TempDir(), ".beads")
			if err := os.MkdirAll(beadsDir, 0o755); err != nil {
				t.Fatal(err)
			}
			doltDir := filepath.Join(beadsDir, "dolt")
			if tc.mkDolt {
				if err := os.MkdirAll(doltDir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.metadata != "" {
				if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(tc.metadata), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			holdRemoteOnlyPhysicalGate(t, filepath.Join(beadsDir, tc.heldRoot))

			h, err := acquireInitGates(context.Background(), beadsDir, initMutationGateRoot(doltDir, false), nil)
			if err != nil {
				t.Fatalf("init gates with only the %s gate held: %v", tc.heldRoot, err)
			}
			if err := h.Release(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func holdRemoteOnlyPhysicalGate(t *testing.T, root string) {
	t.Helper()
	g, err := workspacegate.ForPhysicalRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	held, err := g.Acquire(context.Background(), workspacegate.Exclusive, workspacegate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Release() })
}

func TestRemoteOnlyBootstrapGatePlanHasNoPhysicalRoot(t *testing.T) {
	clearRemoteOnlyInitEnv(t)
	oldWait := exclusiveGateWait
	exclusiveGateWait = 0 // a busy gate fails at once instead of polling
	t.Cleanup(func() { exclusiveGateWait = oldWait })

	t.Run("local dolt gate held", func(t *testing.T) {
		beadsDir := filepath.Join(t.TempDir(), ".beads")
		doltDir := filepath.Join(beadsDir, "dolt")
		if err := os.MkdirAll(doltDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if roots := bootstrapGateExtraRoots(beadsDir); len(roots) != 0 {
			t.Errorf("bootstrap extra gate roots = %v, want none", roots)
		}
		holdRemoteOnlyPhysicalGate(t, doltDir)
		h, err := acquireExclusiveWorkspaceGates(context.Background(), beadsDir, "bd bootstrap init", bootstrapGateExtraRoots(beadsDir)...)
		if err != nil {
			t.Fatalf("bootstrap gates with only the dolt gate held: %v", err)
		}
		if err := h.Release(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("shared mode creates no shared dir", func(t *testing.T) {
		sharedDir := filepath.Join(t.TempDir(), "shared-server")
		t.Setenv("BEADS_DOLT_SHARED_SERVER", "1")
		t.Setenv("BEADS_SHARED_SERVER_DIR", sharedDir)
		beadsDir := filepath.Join(t.TempDir(), ".beads")
		doltDir := filepath.Join(beadsDir, "dolt")
		if err := os.MkdirAll(doltDir, 0o755); err != nil {
			t.Fatal(err)
		}
		holdRemoteOnlyPhysicalGate(t, doltDir)
		h, err := acquireExclusiveWorkspaceGates(context.Background(), beadsDir, "bd bootstrap init", bootstrapGateExtraRoots(beadsDir)...)
		if err != nil {
			t.Fatalf("shared-mode bootstrap gates with only the dolt gate held: %v", err)
		}
		if err := h.Release(); err != nil {
			t.Fatal(err)
		}
		if _, statErr := os.Stat(sharedDir); !os.IsNotExist(statErr) {
			t.Errorf("bootstrap gate planning created the shared dir %s (stat err %v)", sharedDir, statErr)
		}
	})
}

// The bootstrap sync action on a metadata-less workspace routes to the
// embedded clone. A remote-only build has no embedded engine and takes only
// the workspace gate, so it must refuse before it creates .beads/embeddeddolt.
func TestRemoteOnlyBootstrapSyncEmbeddedCloneCreatesNoLocalDir(t *testing.T) {
	clearRemoteOnlyInitEnv(t)
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := configfile.DefaultConfig()
	if got := resolveRemoteCloneMode(beadsDir, cfg, remoteCloneAuto); got != remoteCloneEmbedded {
		t.Fatalf("metadata-less clone route = %v, want the embedded route this test guards", got)
	}

	plan := BootstrapPlan{
		Action:     "sync",
		BeadsDir:   beadsDir,
		Database:   cfg.GetDoltDatabase(),
		SyncRemote: "file://" + filepath.Join(t.TempDir(), "no-remote"),
	}
	err := executeSyncAction(context.Background(), plan, cfg)
	if !errors.Is(err, localdolt.ErrDisabled) {
		t.Fatalf("sync action error = %v, want localdolt.ErrDisabled", err)
	}
	if !strings.Contains(err.Error(), "remote_dolt_only") {
		t.Errorf("sync action error %q does not name the remote-only build", err)
	}
	if _, statErr := os.Stat(filepath.Join(beadsDir, "embeddeddolt")); !os.IsNotExist(statErr) {
		t.Errorf("sync action created .beads/embeddeddolt in a remote-only build (stat err %v)", statErr)
	}
}

// Remote-only server init keeps creating the empty .beads/dolt marker
// because workspace discovery reads it: in a git worktree, FindBeadsDir
// takes the worktree's own .beads only when it holds a database directory,
// else it falls back to the main checkout's. This shows the fallback the
// marker prevents.
func TestRemoteOnlyInitServerMarkerKeepsWorktreeWorkspace(t *testing.T) {
	clearRemoteOnlyInitEnv(t)
	t.Setenv("BEADS_DIR", "")
	mainRepo, worktree := setupWorktree(t)
	serverMeta := []byte(`{"backend":"dolt","database":"dolt","dolt_mode":"server"}`)
	mainBeads := filepath.Join(mainRepo, ".beads")
	wtBeads := filepath.Join(worktree, ".beads")
	for _, dir := range []string{filepath.Join(mainBeads, "dolt"), wtBeads} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{mainBeads, wtBeads} {
		if err := os.WriteFile(filepath.Join(dir, "metadata.json"), serverMeta, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Chdir(worktree)
	git.ResetCaches()
	t.Cleanup(git.ResetCaches)

	if got := utils.CanonicalizePath(beads.FindBeadsDir()); got != utils.CanonicalizePath(mainBeads) {
		t.Fatalf("without the marker: FindBeadsDir() = %q, want the main checkout's %q (the fallback the marker prevents)", got, mainBeads)
	}
	if err := os.MkdirAll(filepath.Join(wtBeads, "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	git.ResetCaches()
	if got := utils.CanonicalizePath(beads.FindBeadsDir()); got != utils.CanonicalizePath(wtBeads) {
		t.Errorf("with the marker: FindBeadsDir() = %q, want the worktree's own %q", got, wtBeads)
	}
}
