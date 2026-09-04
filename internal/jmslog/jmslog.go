// Package jmslog is jms's log file.
//
// A TUI has nowhere to print diagnostics: tview owns the terminal, so a
// log.Printf would paint over the interface, and anything written before or
// after is erased by the alternate-screen switch. The result until now was a
// client with no diagnostics at all -- the one-line status bar hard-truncates
// at terminal width and the next update overwrites it, so a failure was
// visible for a moment and then gone for good.
//
// This writes to a file instead, so "it hung" and "it failed and I couldn't
// read the message" both leave something behind to look at afterwards.
//
// Every function is safe to call before Open and after Close: the whole
// package no-ops when there is no file, so callers never guard their logging.
package jmslog

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// maxSize caps the log so an always-on file cannot fill a laptop's disk. On
// reaching it the file is truncated rather than rotated: this is a debugging
// aid for the session in front of you, not an audit trail -- Core's own
// terminal_session table is the record of who connected to what.
const maxSize = 4 << 20 // 4 MiB

var (
	mu  sync.Mutex
	out *os.File
	// written tracks out's size so the maxSize cap holds during a run and not
	// only at the moment the file is opened. A jms left running for days --
	// which is the normal way to use it -- otherwise grew past the cap
	// indefinitely, since nothing looked again until the next launch.
	written int64
	verbose bool
)

// DefaultPath is where the log goes when --log-file says nothing. It sits
// beside the asset cache in the user's cache directory, which is the same
// place assetcache picks for the same reason -- os.UserCacheDir is the
// per-platform location for regenerable files, and nothing here is worth
// preserving across a cache wipe.
func DefaultPath() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate cache dir: %w", err)
	}
	return filepath.Join(dir, "jms", "jms.log"), nil
}

// Open starts logging to path, creating its directory if needed. An empty
// path means DefaultPath. debug turns on Debugf.
//
// It returns the path actually in use so the caller can tell the user where
// to look. A returned error means logging is off, which is never fatal --
// refusing to start jms because a log file is unwritable would trade a
// diagnostic for the thing being diagnosed.
func Open(path string, debug bool) (string, error) {
	mu.Lock()
	defer mu.Unlock()

	if path == "" {
		var err error
		if path, err = DefaultPath(); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return path, fmt.Errorf("create log dir: %w", err)
	}
	// Truncating on size rather than on every run: the complaint this exists
	// to answer ("it hung ten minutes ago") needs the previous run to still
	// be there.
	flag, size := os.O_CREATE|os.O_WRONLY|os.O_APPEND, int64(0)
	if info, err := os.Stat(path); err == nil {
		if size = info.Size(); size > maxSize {
			flag, size = os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0
		}
	}
	// 0600: the log names assets, accounts and usernames. No secrets go in it
	// (see the package rule in internal/k8sclient), but it is nobody else's
	// business either.
	file, err := os.OpenFile(path, flag, 0o600)
	if err != nil {
		return path, fmt.Errorf("open log %s: %w", path, err)
	}
	if out != nil {
		_ = out.Close()
	}
	out, written, verbose = file, size, debug
	return path, nil
}

func Close() error {
	mu.Lock()
	defer mu.Unlock()
	if out == nil {
		return nil
	}
	err := out.Close()
	out = nil
	return err
}

// Printf writes one line. Always on once Open has succeeded.
func Printf(format string, args ...any) {
	write("", format, args...)
}

// Debugf writes one line only under --debug. Use it for anything that would
// be one entry per keystroke or per row -- Printf is for the handful of
// events that explain a session.
func Debugf(format string, args ...any) {
	mu.Lock()
	on := verbose
	mu.Unlock()
	if on {
		write("debug: ", format, args...)
	}
}

func write(prefix, format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	if out == nil {
		return
	}
	// Start over rather than rotate, for the same reason Open does: this is a
	// debugging aid for the session in front of you, and the record of who
	// connected to what is Core's terminal_session table. No Seek is needed --
	// the file is O_APPEND, so the next write lands at the new end either way.
	if written > maxSize {
		if err := out.Truncate(0); err == nil {
			written = 0
		}
	}
	n, _ := fmt.Fprintf(out, "%s %s%s\n", time.Now().Format("15:04:05.000"), prefix, fmt.Sprintf(format, args...))
	written += int64(n)
}

// Phase times one step of a multi-step operation and is the answer to "which
// part was slow?". The connect flow is three HTTP calls, an SSH handshake and
// a 30-second banner wait, all of which look identical from the outside --
// one frozen "Connecting to X…".
//
// Call the returned function with the step's error, or nil:
//
//	done := jmslog.Phase("asset detail %s", asset.ID)
//	detail, err := jc.AssetDetail(ctx, asset.ID)
//	done(err)
func Phase(format string, args ...any) func(error) {
	name := fmt.Sprintf(format, args...)
	Debugf("%s: start", name)
	started := time.Now()
	return func(err error) {
		elapsed := time.Since(started).Round(time.Millisecond)
		if err != nil {
			Printf("%s: FAILED after %s: %v", name, elapsed, err)
			return
		}
		// A step that worked is only interesting when you are asking "which
		// part was slow", which is what --debug is for. Without it the log
		// stays a list of things that went wrong.
		Debugf("%s: ok in %s", name, elapsed)
	}
}
