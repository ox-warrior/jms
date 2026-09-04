package clipboard

import (
	"errors"
	"os/exec"
	"runtime"
	"testing"
)

// swapWriter replaces the real clipboard writer for one test. Without this
// every run of "go test" would silently overwrite the clipboard of whoever
// ran it.
func swapWriter(t *testing.T, fn func(string) error) {
	t.Helper()
	original := write
	write = fn
	t.Cleanup(func() { write = original })
}

func TestCopyPassesTheTextThroughUnchanged(t *testing.T) {
	var got string
	swapWriter(t, func(text string) error {
		got = text
		return nil
	})

	// An asset name is what this carries, and names can hold characters a
	// shell would treat specially -- nothing here may quote or escape them.
	const want = `prod-web-api "01" $HOME`
	if err := Copy(want); err != nil {
		t.Fatalf("Copy() error = %v, want nil", err)
	}
	if got != want {
		t.Errorf("Copy() delivered %q, want %q", got, want)
	}
}

// A failed copy has to reach the caller: reporting "copied" when nothing was
// copied sends someone off to paste a name that is not there.
func TestCopySurfacesTheWriterError(t *testing.T) {
	boom := errors.New("pbcopy: no such file")
	swapWriter(t, func(string) error { return boom })

	if err := Copy("prod-web-api"); !errors.Is(err, boom) {
		t.Errorf("Copy() error = %v, want it to wrap %v", err, boom)
	}
}

// The shipped writer is pbcopy, so on the one platform this supports the
// binary it shells out to has to actually be there.
func TestPbcopyExistsOnTheSupportedPlatform(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("not macOS (%s); Copy returns ErrUnsupported here", runtime.GOOS)
	}
	if _, err := exec.LookPath("pbcopy"); err != nil {
		t.Errorf("pbcopy not on PATH: %v -- the macOS clipboard path cannot work", err)
	}
}
