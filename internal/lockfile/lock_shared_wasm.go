//go:build js && wasm

package lockfile

import "os"

// FlockSharedNonBlock is a no-op in WASM (single-process environment).
func FlockSharedNonBlock(f *os.File) error {
	return nil
}

// FlockExclusiveNonBlock is a no-op in WASM (single-process environment).
func FlockExclusiveNonBlock(f *os.File) error {
	return nil
}

// FileLockShared is a no-op in WASM (single-process environment, no locks).
func FileLockShared(f *os.File) error {
	return nil
}

// FileLockExclusive is a no-op in WASM (single-process environment, no locks).
func FileLockExclusive(f *os.File) error {
	return nil
}

// FileUnlock is a no-op in WASM.
func FileUnlock(f *os.File) error {
	return nil
}
