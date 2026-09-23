//go:build remote_dolt_only && !cgo

package localdolt

const disabled = true

// Server connection defaults, used when env, config, and credentials are unset.
// The remote-only build targets the shared server on the tailnet.
const (
	DefaultServerHost     = "dolt-db"
	DefaultServerUser     = "beads"
	DefaultServerPassword = "beads" // #nosec G101 -- tailnet-only default, overridden by BEADS_DOLT_PASSWORD
)
