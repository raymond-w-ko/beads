package main

import (
	"testing"

	"github.com/steveyegge/beads/internal/localdolt"
)

// skipIfLocalDoltDisabled skips a test that needs local Dolt: a `bd init`
// without --server, or a migration onto a proxied local root. A remote-only
// build fences both, and with no BEADS_* env its server defaults point at the
// fleet Dolt server, so an init there would write to it instead of failing.
func skipIfLocalDoltDisabled(t *testing.T) {
	t.Helper()
	if localdolt.Disabled() {
		t.Skip("local Dolt is disabled in remote-only builds")
	}
}
