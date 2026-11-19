package main

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/timelog"
)

// timeLogCommand is the command root's PersistentPreRunE is running for. The
// exits inside a command (CheckReadonly) have no ExecuteC result to name
// their verb from, so they read it here.
var timeLogCommand *cobra.Command

// timeLogVerb names cmd for the bd-time line: its path below the root by
// cobra's canonical names (never an alias), joined by "-". The root itself,
// and no command at all, is "bd", whatever BD_NAME renames the root to.
func timeLogVerb(cmd *cobra.Command) string {
	var parts []string
	for c := cmd; c != nil && c.HasParent(); c = c.Parent() {
		parts = append(parts, c.Name())
	}
	if len(parts) == 0 {
		return "bd"
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, "-")
}

// The line's open_ms comes from timelog.MarkOpen, called at the return of the
// command's own store open only: the root pre-run's two opens (main.go) and
// ensureStoreActiveWithContext's late open (direct_mode.go), for a command
// the pre-run left without a store (prime, for one). No other open marks it:
// init creating a database, bootstrap, doctor's own open, config drift and
// apply, migrations, a routed or preview second store, the read-only reopen,
// proxied init and prime. The first mark of a process stands.

// writeTimeLog appends the process's bd-time line when BD_TIME_LOG named a
// file at start. Call it only at bd's own exits, after the hook wait and the
// metrics flush, with the code bd is about to exit with. It is silent and
// never panics.
func writeTimeLog(cmd *cobra.Command, exit int) {
	defer func() { _ = recover() }()
	if timelog.Path() == "" {
		return
	}
	timelog.Write(timeLogVerb(cmd), exit)
}
