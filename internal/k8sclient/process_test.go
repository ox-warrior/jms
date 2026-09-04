package k8sclient

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLocateExplicitPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "my-k9s")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Locate(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Errorf("got %q, want %q", got, path)
	}
}

func TestLocateExplicitPathMissing(t *testing.T) {
	if _, err := Locate(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Error("expected an error for a nonexistent --k9s-path")
	}
}

func TestLocateExplicitPathIsDirectory(t *testing.T) {
	if _, err := Locate(t.TempDir()); err == nil {
		t.Error("expected an error when --k9s-path names a directory")
	}
}

func TestLocateFallsBackToPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH lookup semantics differ on windows")
	}
	dir := t.TempDir()
	name := "k9s"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	got, err := Locate("")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(dir, name) {
		t.Errorf("got %q, want a k9s under %q", got, dir)
	}
}

func TestLocateFailsWhenNotOnPATH(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := Locate(""); err == nil {
		t.Error("expected an error when k9s is on neither --k9s-path nor PATH")
	}
}

// A k9s unpacked from a release tarball by hand arrives without the
// executable bit. Locate has to be the one that says so: it runs before the
// ticket is minted, and Run only runs after -- so letting this through spends
// a real ticket and a Core audit entry on a file that was never going to exec.
func TestLocateExplicitPathNotExecutable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k9s")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Locate(path); err == nil {
		t.Error("expected an error when --k9s-path names a file without the executable bit")
	}
}
