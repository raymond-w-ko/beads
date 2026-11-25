//go:build windows

package lockfile

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// FlockSharedNonBlock acquires a shared non-blocking lock on the file.
// Multiple processes can hold shared locks concurrently.
// Returns ErrLockBusy if an exclusive lock is already held.
func FlockSharedNonBlock(f *os.File) error {
	// Shared + fail immediately (no LOCKFILE_EXCLUSIVE_LOCK)
	const flags = windows.LOCKFILE_FAIL_IMMEDIATELY

	ol := &windows.Overlapped{}
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		flags,
		0,
		0xFFFFFFFF,
		0xFFFFFFFF,
		ol,
	)

	if err == windows.ERROR_LOCK_VIOLATION || err == syscall.EWOULDBLOCK {
		return ErrLockBusy
	}
	return err
}

// FlockExclusiveNonBlock acquires an exclusive non-blocking lock on the file.
// Returns ErrLockBusy if any lock (shared or exclusive) is already held.
func FlockExclusiveNonBlock(f *os.File) error {
	const flags = windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY

	ol := &windows.Overlapped{}
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		flags,
		0,
		0xFFFFFFFF,
		0xFFFFFFFF,
		ol,
	)

	if err == windows.ERROR_LOCK_VIOLATION || err == syscall.EWOULDBLOCK {
		return ErrLockBusy
	}
	return err
}

// FileLockOffset is the offset of the single byte FileLockShared and
// FileLockExclusive lock on Windows. LockFileEx locks are mandatory: a
// locked range refuses other handles' reads and writes. Locking one byte far
// beyond any real content (as SQLite locks its lock-byte page) serializes the
// lockers while every plain reader or writer of the content, an editor for
// one, is never blocked. Windows permits locking a range past the end of
// the file.
const FileLockOffset = 1 << 62

// fileLockOverlapped addresses the byte at FileLockOffset.
func fileLockOverlapped() *windows.Overlapped {
	return &windows.Overlapped{
		Offset:     uint32(FileLockOffset & 0xFFFFFFFF),
		OffsetHigh: uint32(FileLockOffset >> 32),
	}
}

// FileLockShared takes a shared lock on the file f is open on, waiting while
// another handle holds FileLockExclusive. The lock belongs to the file, so
// every name of the file shares it, and it needs no write access to f or its
// directory. On Windows it locks the one byte at FileLockOffset, so the
// content stays readable and writable by handles that take no lock. Two
// handles conflict even in one process, so a holder must not take a second
// lock on the same file; release with FileUnlock, or by closing f.
func FileLockShared(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), 0, 0, 1, 0, fileLockOverlapped())
}

// FileLockExclusive is FileLockShared's exclusive form: it waits until no
// other handle holds either lock on the file.
func FileLockExclusive(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, fileLockOverlapped())
}

// FileUnlock releases FileLockShared or FileLockExclusive.
func FileUnlock(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, fileLockOverlapped())
}
