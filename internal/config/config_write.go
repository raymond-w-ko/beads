package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/spf13/viper"

	"github.com/steveyegge/beads/internal/lockfile"
)

// ErrSkipRewrite, returned by a RewriteConfigFile edit func, leaves the file
// untouched; RewriteConfigFile then returns nil.
var ErrSkipRewrite = errors.New("config: skip rewrite")

// errConfigLockReentry is returned, when configLockReentryCheck is on, by a
// config reader or RewriteConfigFile called while a RewriteConfigFile edit
// callback runs. In production such a call would block forever: the edit
// holds the file's exclusive lock, and a second open of the same file
// conflicts with it even in one process.
var errConfigLockReentry = errors.New("config: config file lock taken inside a locked edit")

// configLockReentryCheck turns on the re-entry check (tests only: the check
// counts edits in progress in the whole process, so concurrent unrelated
// writers would trip it).
var configLockReentryCheck bool

// lockedEdits counts RewriteConfigFile edit callbacks in progress.
var lockedEdits atomic.Int32

func checkConfigLockReentry() error {
	if configLockReentryCheck && lockedEdits.Load() > 0 {
		return errConfigLockReentry
	}
	return nil
}

// rewriteError tags an I/O failure of RewriteConfigFile with its phase, so
// callers can keep their historic "failed to read/write config.yaml" wording.
type rewriteError struct {
	op  string // "read" or "write"
	err error
}

func (e *rewriteError) Error() string { return e.op + ": " + e.err.Error() }
func (e *rewriteError) Unwrap() error { return e.err }

// labelRewriteErr turns a RewriteConfigFile I/O error into the caller's
// historic message ("failed to <op> <what>: <cause>"); edit errors pass through.
func labelRewriteErr(err error, what string) error {
	var rw *rewriteError
	if errors.As(err, &rw) {
		return fmt.Errorf("failed to %s %s: %w", rw.op, what, rw.err)
	}
	return err
}

// RewriteConfigFileCause strips RewriteConfigFile's phase tag and returns the
// underlying I/O error (edit errors are returned unchanged), with the phase.
func RewriteConfigFileCause(err error) (op string, cause error) {
	var rw *rewriteError
	if errors.As(err, &rw) {
		return rw.op, rw.err
	}
	return "", err
}

// configIOHook, when set by a test, runs at named stages of a config write or
// read: "created" (a writer has made an absent file, before any lock),
// "truncated" (the writer has truncated the file), "prefix" (the writer has
// written the first half of the new bytes), "reader-opened" (a reader has made
// its open attempt, before any lock) and "reader-locked" (a reader holds its
// shared lock and has not read yet).
var configIOHook func(stage, path string)

// SetIOHookForTesting installs hook as the config I/O stage hook (see
// configIOHook) and returns a func that removes it. Tests only: the hook is
// process-wide.
func SetIOHookForTesting(hook func(stage, path string)) (restore func()) {
	configIOHook = hook
	return func() { configIOHook = nil }
}

func configIOStage(stage, path string) {
	if configIOHook != nil {
		configIOHook(stage, path)
	}
}

// lockConfigFile takes the shared or exclusive lock on f. A platform with no
// file locks (js/wasm, or a filesystem that reports the call unsupported)
// gets no lock; any other failure is returned.
func lockConfigFile(f *os.File, exclusive bool) error {
	var err error
	if exclusive {
		err = lockfile.FileLockExclusive(f)
	} else {
		err = lockfile.FileLockShared(f)
	}
	if errors.Is(err, errors.ErrUnsupported) {
		return nil
	}
	return err
}

// FORK: config.yaml rewrites are serialized under a lock on the config file
// itself, and bd's readers of it wait for them. Two bd processes that share a
// HOME used to tear ~/.config/bd/config.yaml: each read-modify-write ran
// unlocked, so a second writer could read the file while the first had it
// truncated and write its short result over the first one's tail, leaving a
// file no later bd can parse.
//
// RewriteConfigFile opens path read-write (creating it with perm if absent),
// takes an exclusive lock on that descriptor (lockfile.FileLockExclusive:
// flock on unix, one byte far past the content on Windows), reads the file
// through it, calls edit with its bytes, and truncates and writes edit's
// result through the same descriptor. The lock belongs to the file's inode,
// so every name of the file, symlink or hard link, shares it, and it needs no
// write access to the directory. bd's readers (readConfigShared,
// withConfigReadLock) open the file read-only and hold a shared lock on it
// for their read, so they never see a half-written file, and no concurrent
// update is lost.
//
// The write goes through path itself, so the file keeps its inode, mode, ACL,
// owner, hard links and symlinks, and a file the user cannot write (0444) is
// refused as before. A new file gets perm.
//
// If the file is absent, edit is first called without the lock with exists
// false; when it would write nothing (ErrSkipRewrite or an error), nothing is
// created. Otherwise its result is created whole (createConfigWhole: a temp
// file hard-linked into place), so no reader or writer ever sees the new file
// empty. If another writer created the file first, edit is called again under
// the lock on what that writer wrote, so edit must be a pure function of its
// arguments. Where no whole create is possible (no hard links, a dangling
// symlink), the file is created in place and locked as before; exists is then
// false while it is still empty under the lock. A pre-existing empty file
// takes the existing-file path. There is no unlocked fallback: a failed lock
// fails the write.
//
// Limits: a crash during the write can leave a partial file, as the unlocked
// in-place write could. On the in-place create fallback, another writer can
// see the new file empty before its creator holds the lock, and then edits it
// as an existing empty file. Programs other than bd (an editor) take no lock.
//
// Lock, read and write failures come back tagged with their phase (see
// RewriteConfigFileCause). edit must not call RewriteConfigFile or a config
// reader on the same file: the lock is not reentrant, a second open of the
// file conflicts with it even in this process, and the call would block.
func RewriteConfigFile(path string, perm os.FileMode, edit func(old []byte, exists bool) ([]byte, error)) error {
	if err := checkConfigLockReentry(); err != nil {
		return err
	}
	created := false
	f, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // caller-resolved config path, not user input
	if errors.Is(err, fs.ErrNotExist) {
		// Absent file: learn whether this edit writes at all before creating it.
		out, editErr := edit(nil, false)
		if editErr != nil {
			if errors.Is(editErr, ErrSkipRewrite) {
				return nil
			}
			return editErr
		}
		switch made, createErr := createConfigWhole(path, out, perm); {
		case createErr != nil:
			return &rewriteError{op: "write", err: createErr}
		case made:
			configIOStage("created", path)
			return nil
		}
		// Another writer created the file first (or a dangling symlink names
		// it): the locked edit below runs again on what is there.
		f, err = os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // caller-resolved config path
		if errors.Is(err, fs.ErrNotExist) {
			// No whole create here (hard links unsupported, or path is a
			// dangling symlink): create in place as before. Limit: until this
			// writer holds the lock, another can see the new file empty.
			created = true
			f, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE, perm) //nolint:gosec // caller-resolved config path
			if err == nil {
				configIOStage("created", path)
			}
		}
	}
	if err != nil {
		return &rewriteError{op: openFailurePhase(path, err), err: err}
	}
	defer func() { _ = f.Close() }() // closing also releases the lock

	if err := lockConfigFile(f, true); err != nil {
		return &rewriteError{op: "write", err: fmt.Errorf("lock %s: %w", path, err)}
	}

	old, err := io.ReadAll(f)
	if err != nil {
		return &rewriteError{op: "read", err: err}
	}
	exists := !created || len(old) > 0
	if !exists {
		old = nil
	}

	lockedEdits.Add(1)
	out, err := edit(old, exists)
	lockedEdits.Add(-1)
	if errors.Is(err, ErrSkipRewrite) {
		return nil
	}
	if err != nil {
		return err
	}

	if err := writeConfigLocked(f, path, out); err != nil {
		return &rewriteError{op: "write", err: err}
	}
	_ = lockfile.FileUnlock(f)
	if err := f.Close(); err != nil {
		return &rewriteError{op: "write", err: err}
	}
	return nil
}

// configNoHardLinks makes createConfigWhole report hard links unavailable, so
// tests can exercise the in-place create fallback.
var configNoHardLinks bool

// createConfigWhole makes the absent file path holding data, so that no
// reader or writer ever sees it empty or partial: it writes data to a temp file
// in path's directory and hard-links that to path, which fails if path exists.
// made is true when path now holds data. made is false, with no error, when
// path appeared first (or is a dangling symlink, which a link never follows),
// and when hard links or the temp file are unavailable here (a filesystem
// without hard links, js/wasm, a temp name the directory refuses): the caller
// then falls back to its open, whose errors are the historic ones.
func createConfigWhole(path string, data []byte, perm os.FileMode) (made bool, err error) {
	if configNoHardLinks {
		return false, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".~"+filepath.Base(path)+".")
	if err != nil {
		return false, nil
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if perm != 0o600 { // CreateTemp makes 0600
		if err := tmp.Chmod(perm); err != nil {
			_ = tmp.Close()
			return false, err
		}
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Link(tmpName, path); err != nil {
		// EEXIST: another writer won, or a dangling symlink; anything else:
		// no hard links here. Both fall back to the caller's open.
		return false, nil
	}
	return true, nil
}

// openFailurePhase names the phase a failed read-write open reports: "read"
// for a file that exists but cannot even be read (as the old read-first code
// reported it), else "write" (a 0444 file, an absent or read-only directory).
func openFailurePhase(path string, err error) string {
	if !errors.Is(err, fs.ErrPermission) {
		return "write"
	}
	rf, rerr := os.Open(path) //nolint:gosec // caller-resolved config path
	if rerr != nil {
		return "read"
	}
	_ = rf.Close()
	return "write"
}

// writeConfigLocked replaces f's content with data in place, as os.WriteFile
// does (truncate, then write), with test stages between the steps.
func writeConfigLocked(f *os.File, path string, data []byte) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	configIOStage("truncated", path)
	if configIOHook != nil {
		half := len(data) / 2
		if _, err := f.WriteAt(data[:half], 0); err != nil {
			return err
		}
		configIOStage("prefix", path)
		_, err := f.WriteAt(data[half:], int64(half))
		return err
	}
	_, err := f.WriteAt(data, 0)
	return err
}

// withConfigReadLock opens path read-only, takes a shared lock on it and runs
// read while holding it, so read never sees a RewriteConfigFile half done. The
// open's error (an absent file is fs.ErrNotExist) is returned without calling
// read: an absent config is absent, never read later without a lock. read
// gets the locked descriptor; a reader that reopens the path by name (Viper)
// still reads under the lock, which belongs to the file. No file or
// directory is ever created to read, and the lock is released on return.
func withConfigReadLock(path string, read func(f *os.File) error) error {
	if err := checkConfigLockReentry(); err != nil {
		return err
	}
	f, err := os.Open(path) //nolint:gosec // caller-resolved config path
	configIOStage("reader-opened", path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := lockConfigFile(f, false); err != nil {
		return fmt.Errorf("lock %s: %w", path, err)
	}
	defer func() { _ = lockfile.FileUnlock(f) }()
	configIOStage("reader-locked", path)
	return read(f)
}

// loadViperConfigLocked loads path into vp from the bytes read through the
// file's locked descriptor, never by name: a symlink retargeted after the lock
// cannot make Viper read another file. merge selects MergeConfig (as
// MergeInConfig) over ReadConfig (as ReadInConfig). vp's config file name is
// the caller's, as before, and its config type is yaml (Initialize sets it),
// so the parse, its errors and the state left on an error are ReadInConfig's
// and MergeInConfig's.
func loadViperConfigLocked(vp *viper.Viper, path string, merge bool) error {
	data, err := readConfigShared(path)
	if err != nil {
		return err
	}
	if merge {
		return vp.MergeConfig(bytes.NewReader(data))
	}
	return vp.ReadConfig(bytes.NewReader(data))
}

// readConfigShared reads path's bytes under its shared config lock. An absent
// file returns an fs.ErrNotExist error; an empty file returns no bytes, which
// every caller reads as an absent config.
func readConfigShared(path string) ([]byte, error) {
	var data []byte
	err := withConfigReadLock(path, func(f *os.File) error {
		var err error
		data, err = io.ReadAll(f)
		return err
	})
	return data, err
}

// ReadConfigFileShared reads a config.yaml's bytes under its shared config
// lock, as bd's own readers do: it waits for a RewriteConfigFile in progress
// and never returns a half-written file. An absent file returns an
// fs.ErrNotExist error. It must not be called from a RewriteConfigFile edit.
func ReadConfigFileShared(path string) ([]byte, error) {
	return readConfigShared(path)
}
