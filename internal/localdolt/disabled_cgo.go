//go:build remote_dolt_only && cgo

package localdolt

// A cgo build links the embedded Dolt engine back in, which a remote-only
// build must not have. Fail the build instead of shipping it.
const disabled = remote_dolt_only_requires_CGO_ENABLED_0
