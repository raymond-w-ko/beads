//go:build unix

package lockfile

import (
	"os"

	"golang.org/x/sys/unix"
)

// FlockSharedNonBlock acquires a shared non-blocking lock on the file.
// Multiple processes can hold shared locks concurrently.
// Returns ErrLockBusy if an exclusive lock is already held.
func FlockSharedNonBlock(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB)
	if err == unix.EWOULDBLOCK {
		return ErrLockBusy
	}
	return err
}

// FlockExclusiveNonBlock acquires an exclusive non-blocking lock on the file.
// Returns ErrLockBusy if any lock (shared or exclusive) is already held.
func FlockExclusiveNonBlock(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == unix.EWOULDBLOCK {
		return ErrLockBusy
	}
	return err
}

// FileLockShared takes a shared lock on the file f is open on, waiting while
// another open file description holds FileLockExclusive. It locks the file's
// inode, so every name of the file (symlinks, hard links) shares it, and it
// needs no write access to f or its directory. On unix it is flock(2), an
// advisory whole-file lock: readers and writers that take no lock are never
// blocked. Two descriptors opened separately conflict even in one process, so
// a holder must not take a second lock on the same file; release with
// FileUnlock, or by closing f.
func FileLockShared(f *os.File) error {
	return flockRetry(f, unix.LOCK_SH)
}

// FileLockExclusive is FileLockShared's exclusive form: it waits until no
// other open file description holds either lock on the file.
func FileLockExclusive(f *os.File) error {
	return flockRetry(f, unix.LOCK_EX)
}

// FileUnlock releases FileLockShared or FileLockExclusive.
func FileUnlock(f *os.File) error {
	return flockRetry(f, unix.LOCK_UN)
}

func flockRetry(f *os.File, how int) error {
	for {
		err := unix.Flock(int(f.Fd()), how)
		if err != unix.EINTR {
			return err
		}
	}
}
