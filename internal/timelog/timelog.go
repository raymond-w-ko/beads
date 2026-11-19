// Package timelog writes bd's per-process timing line, the "bd-time" line,
// to the file BD_TIME_LOG names. An operator's load monitor reads the file to
// time each bd call by verb without adding a statement to the server.
//
// The package imports only the standard library so that it initialises
// early: its start mark is a package-level time.Now(), set before main()
// runs. The path is read once, at that same initialisation, from the
// environment the process started with; nothing bd loads later (a project's
// .beads/.env among it) can change where the line goes. Only an absolute
// path names the file; a relative value is the same as unset. The open
// refuses a symlink as the path's last component where the platform has
// O_NOFOLLOW (not on Windows). Every directory of the path is the caller's:
// a symlinked directory inside it is followed.
//
// Writing is best effort: one write of one line on a descriptor opened for
// append, create and (where the platform has it) without blocking, so a FIFO
// with no reader or a full pipe costs no wait. Every failure, a panic
// included, is silent.
//
// Two limits are stated, not handled. The append is one synchronous open,
// write and close: O_NONBLOCK covers a FIFO and a pipe only, so a filesystem
// that stalls holds the exit as it holds any of bd's own file reads. With no
// read and no lock, a write cut short (a file-size limit, a full disk) leaves
// a fragment with no newline, and the next line lands on the same physical
// line, so a reader loses that record with it.
//
// "open_ms" is the time to the return of the command's own store open: the
// root pre-run's, or ensureStoreActive's for a command that opens late. No
// other open marks it (init creating a database, bootstrap, doctor's own
// open, config drift and apply, migrations, a routed or preview second
// store, the read-only reopen, proxied init and prime), and the first mark
// stands.
package timelog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// EnvVar names the file the line is appended to. Unset or empty: no line.
const EnvVar = "BD_TIME_LOG"

// Tool is the line's "tool" value.
const Tool = "bd-time"

// MaxLineBytes is the bound every written line stays under, so one write of
// it is atomic on a pipe (PIPE_BUF is at least 512 on POSIX). Write drops a
// line of MaxLineBytes or more rather than cut it.
const MaxLineBytes = 512

var (
	// start is the process's start mark.
	start = time.Now()

	// path is BD_TIME_LOG as the process started with it, if absolute.
	path = absOnly(os.Getenv(EnvVar))

	opened   atomic.Bool
	openedAt atomic.Int64 // nanoseconds after start; valid once opened is set
)

// absOnly returns p if it is an absolute path, else "": a relative value is
// the same as unset, since it would resolve against the working directory,
// which can be a checkout someone else prepared.
func absOnly(p string) string {
	if !filepath.IsAbs(p) {
		return ""
	}
	return p
}

// Start returns the process's start mark.
func Start() time.Time { return start }

// Path returns the file the line goes to, as read at initialisation; empty
// means no line.
func Path() string { return path }

// MarkOpen records the return of the process's store open. Only the first
// call counts; call it only for an open that succeeded.
func MarkOpen() {
	d := time.Since(start)
	if opened.CompareAndSwap(false, true) {
		openedAt.Store(int64(d))
	}
}

// EnvWithout returns a copy of env, a list of "key=value" entries as
// os.Environ gives, without the BD_TIME_LOG entries, keeping every other
// entry and their order. A bd process that bd starts detached for itself is
// no call an operator made, so its environment goes through this and it
// writes no line. The key matches as the reading side (os.Getenv) does:
// exactly on Unix, and in any letter case on Windows, where names fold.
func EnvWithout(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if key, _, ok := strings.Cut(kv, "="); ok && isEnvVar(key) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// isEnvVar reports whether key names BD_TIME_LOG on this platform.
func isEnvVar(key string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(key, EnvVar)
	}
	return key == EnvVar
}

// Line is one bd-time record.
type Line struct {
	At      time.Time
	Verb    string
	Exit    int
	MS      int64
	OpenMS  int64
	HasOpen bool // false: no "open_ms" key
}

// wireLine is the line's JSON shape; encoding/json keeps the field order.
type wireLine struct {
	Tool   string `json:"tool"`
	At     string `json:"at"`
	Verb   string `json:"verb"`
	Exit   int    `json:"exit"`
	MS     int64  `json:"ms"`
	OpenMS *int64 `json:"open_ms,omitempty"`
}

// Format renders l as one JSON line, newline included, keys in the order
// tool, at, verb, exit, ms, open_ms. The verb is written whole.
func Format(l Line) []byte {
	w := wireLine{
		Tool: Tool,
		At:   l.At.UTC().Format("2006-01-02T15:04:05.000Z"),
		Verb: l.Verb,
		Exit: l.Exit,
		MS:   l.MS,
	}
	if l.HasOpen {
		openMS := l.OpenMS
		w.OpenMS = &openMS
	}
	b, err := json.Marshal(w)
	if err != nil {
		return nil
	}
	return append(b, '\n')
}

// Append writes b to the file at p with one write, on a descriptor opened
// write-only for append and create with mode 0600, without blocking and,
// where the platform has O_NOFOLLOW, refusing a symlink as the last
// component. It neither reads, locks, syncs nor rotates the file.
func Append(p string, b []byte) error {
	fd, err := syscall.Open(p, syscall.O_WRONLY|syscall.O_APPEND|syscall.O_CREAT|syscall.O_NONBLOCK|syscall.O_CLOEXEC|oNoFollow, 0o600)
	if err != nil {
		return err
	}
	_, err = syscall.Write(fd, b)
	if cerr := syscall.Close(fd); err == nil {
		err = cerr
	}
	return err
}

// Write appends the process's line for verb and exit to the BD_TIME_LOG
// file, if one was named. It is silent: a failure or a panic inside it
// leaves no output and changes nothing about the exit.
func Write(verb string, exit int) {
	defer func() { _ = recover() }()
	if path == "" {
		return
	}
	now := time.Now()
	l := Line{
		At:   now,
		Verb: verb,
		Exit: exit,
		MS:   now.Sub(start).Milliseconds(),
	}
	if opened.Load() {
		l.HasOpen = true
		l.OpenMS = time.Duration(openedAt.Load()).Milliseconds()
	}
	b := Format(l)
	if len(b) == 0 || len(b) >= MaxLineBytes {
		return
	}
	_ = Append(path, b)
}
