//go:build !remote_dolt_only

package localdolt

const disabled = false

// Server connection defaults, used when env, config, and credentials are unset.
const (
	DefaultServerHost     = "127.0.0.1"
	DefaultServerUser     = "root"
	DefaultServerPassword = ""
)
