// Package localdolt reports whether this build may use Dolt on the local
// machine: the embedded engine, or a `dolt` binary run as a subprocess
// (managed sql-server, CLI push/pull routing, fsck, gc, clone, remotes).
//
// Builds tagged remote_dolt_only talk to an external dolt sql-server over
// SQL and nothing else. Every local-dolt entry point checks Disabled or
// Check and refuses with ErrDisabled instead of looking for a binary.
package localdolt

import (
	"errors"
	"fmt"
)

// ErrDisabled is returned when a remote-only build reaches a code path that
// needs local Dolt.
var ErrDisabled = errors.New("local Dolt is disabled in this remote-only build " +
	"(remote_dolt_only); point bd at an external dolt sql-server " +
	"(dolt.mode: server, dolt.host: <server host>)")

// Disabled reports whether this is a remote-only build.
func Disabled() bool { return disabled }

// Check returns ErrDisabled, prefixed with op, in a remote-only build, and
// nil otherwise.
func Check(op string) error {
	if !disabled {
		return nil
	}
	return fmt.Errorf("%s: %w", op, ErrDisabled)
}
