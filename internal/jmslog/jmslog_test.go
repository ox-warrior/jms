package jmslog

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openTemp points the package at a fresh file for one test. The package holds
// its state in globals, so these tests cannot run in parallel with each other.
func openTemp(t *testing.T, debug bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sub", "jms.log")
	if _, err := Open(path, debug); err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = Close() })
	return path
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	return string(data)
}

// Nothing may panic or block before Open -- packages log from init-time and
// library paths that run whether or not the caller wanted a log, so a guard
// at every call site would be the alternative.
func TestWritingBeforeOpenIsSilent(t *testing.T) {
	_ = Close()
	Printf("dropped %d", 1)
	Debugf("dropped too")
	Phase("nothing")(errors.New("boom"))
}

func TestOpenCreatesTheDirectory(t *testing.T) {
	path := openTemp(t, false)
	Printf("hello %s", "world")
	if got := read(t, path); !strings.Contains(got, "hello world") {
		t.Errorf("log = %q, want it to contain the message", got)
	}
}

// Debugf must cost nothing unless --debug was passed, or the always-on log
// fills with one line per keystroke.
func TestDebugfOnlyWritesWhenEnabled(t *testing.T) {
	path := openTemp(t, false)
	Debugf("quiet")
	Printf("loud")
	got := read(t, path)
	if strings.Contains(got, "quiet") {
		t.Error("Debugf wrote a line without --debug")
	}
	if !strings.Contains(got, "loud") {
		t.Error("Printf did not write")
	}
}

func TestDebugfWritesWhenEnabled(t *testing.T) {
	path := openTemp(t, true)
	Debugf("verbose detail")
	if got := read(t, path); !strings.Contains(got, "verbose detail") {
		t.Errorf("log = %q, want the debug line", got)
	}
}

// A failed phase must always be recorded, with its name and its cause. A
// successful one must not be: the connect flow is half a dozen phases, and
// logging each success turned a log of problems into a transcript nobody
// reads. Timings are what --debug is for.
func TestPhaseOnlyRecordsFailuresByDefault(t *testing.T) {
	path := openTemp(t, false)
	Phase("koko ssh")(nil)
	Phase("mint ticket")(errors.New("connection refused"))
	got := read(t, path)
	if strings.Contains(got, "koko ssh") {
		t.Errorf("log = %q, want the successful phase left out without --debug", got)
	}
	if !strings.Contains(got, "mint ticket: FAILED after") || !strings.Contains(got, "connection refused") {
		t.Errorf("log = %q, want a failure line carrying the error", got)
	}
}

func TestPhaseRecordsTimingsUnderDebug(t *testing.T) {
	path := openTemp(t, true)
	Phase("koko kubectl session")(nil)
	if got := read(t, path); !strings.Contains(got, "koko kubectl session: ok in") {
		t.Errorf("log = %q, want the timing line under --debug", got)
	}
}

// A relaunch must not erase the previous run: "it hung ten minutes ago" is
// the complaint this log exists to answer.
func TestReopenAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jms.log")
	if _, err := Open(path, false); err != nil {
		t.Fatal(err)
	}
	Printf("first run")
	if err := Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, false); err != nil {
		t.Fatal(err)
	}
	defer Close()
	Printf("second run")

	got := read(t, path)
	if !strings.Contains(got, "first run") || !strings.Contains(got, "second run") {
		t.Errorf("log = %q, want both runs", got)
	}
}

// The flip side of appending: an always-on log must not grow without bound.
func TestOversizedLogIsTruncated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jms.log")
	if err := os.WriteFile(path, make([]byte, maxSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, false); err != nil {
		t.Fatal(err)
	}
	defer Close()
	Printf("fresh start")

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 1024 {
		t.Errorf("log is %d bytes, want it truncated back to just the new line", info.Size())
	}
}

// Logging must never be the reason jms won't start, so an unopenable path is
// an error the caller can report and ignore -- not a panic, and not a state
// where later writes blow up.
func TestOpenFailureLeavesLoggingSafelyOff(t *testing.T) {
	_ = Close()
	dir := t.TempDir()
	// A file where the log's parent directory needs to be.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(blocker, "jms.log"), false); err == nil {
		t.Fatal("Open accepted a path whose parent is a regular file")
	}
	Printf("must not panic")
	Phase("must not panic either")(nil)
}

func TestDefaultPathIsUnderTheCacheDir(t *testing.T) {
	path, err := DefaultPath()
	if err != nil {
		t.Skipf("no user cache dir on this machine: %v", err)
	}
	if filepath.Base(path) != "jms.log" || filepath.Base(filepath.Dir(path)) != "jms" {
		t.Errorf("DefaultPath = %q, want .../jms/jms.log", path)
	}
}

// The cap has to hold DURING a run, not only at the moment the file is opened.
// jms is normally left running for days; checking once at Open let the log
// grow past maxSize indefinitely, since nothing looked again until relaunch.
func TestLogIsTruncatedWhileRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jms.log")
	if _, err := Open(path, false); err != nil {
		t.Fatal(err)
	}
	defer Close()

	line := strings.Repeat("x", 4096)
	for written := 0; written <= maxSize; written += len(line) {
		Printf("%s", line)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > maxSize {
		t.Errorf("log grew to %d bytes without ever reopening, want it capped at %d", info.Size(), maxSize)
	}
	if info.Size() == 0 {
		t.Error("log is empty; truncating must keep writing, not stop")
	}
}
