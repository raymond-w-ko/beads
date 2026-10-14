//go:build !cgo

package main

// federation_nocgo.go compiles the federation parent as a stub with no
// subcommands; the subcommands and their registry rows are the cgo build's.
func init() {
	for _, path := range []string{
		"federation sync", "federation status", "federation add-peer",
		"federation remove-peer", "federation list-peers",
	} {
		cgoOnlyCommandPaths[path] = true
	}
}
