//go:build remote_dolt_only && !cgo

package localdolt

const disabled = true

// Server connection defaults, used when env, config, and credentials are unset.
// They match the default build's: an unconfigured binary names no shared server.
const (
	DefaultServerHost     = "127.0.0.1"
	DefaultServerUser     = "root"
	DefaultServerPassword = ""
)
