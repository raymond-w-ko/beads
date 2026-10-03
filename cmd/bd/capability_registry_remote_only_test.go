//go:build remote_dolt_only && !cgo

package main

import "testing"

// Remote-only builds own no Dolt server, so the backup family refuses on every
// proxied topology, managed-local included.
func TestRemoteOnlyBackupRefusedOnEveryTopology(t *testing.T) {
	topologies := []ProxyTopology{
		ProxyTopologyManagedLocal, ProxyTopologyExternalTCP, ProxyTopologyExternalUnix, ProxyTopologyUnknown,
	}
	for _, path := range []string{"backup", "backup init", "backup sync", "backup remove", "backup status", "backup restore"} {
		row, ok := LookupCapabilityRow(path, "")
		if !ok {
			t.Fatalf("no capability row for %q", path)
		}
		for _, topology := range topologies {
			if got := row.ruleFor(topology).Outcome; got == ProxyOutcomeHonored {
				t.Errorf("%q on %s: outcome %q, want a refusal", path, topology, got)
			}
		}
	}
}
