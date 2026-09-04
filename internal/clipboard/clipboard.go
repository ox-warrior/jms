// Package clipboard puts text on the system clipboard.
//
// macOS only, deliberately. jms also ships a linux/amd64 build
// (scripts/release.sh), but a Linux clipboard needs an X or Wayland helper
// (xclip / wl-copy) that is frequently not installed, and OSC 52 -- the one
// mechanism that would cover both -- depends on terminal support this has no
// way to detect. Rather than three paths with three different failure modes,
// this implements the one that is always right on the platform jms is actually
// used on, and says so plainly everywhere else.
//
// Only non-secret text is ever passed here. Connection tickets, kubeconfigs
// and MFA codes must not reach the clipboard: it is readable by every other
// process on the machine and survives the session that produced it.
package clipboard

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// ErrUnsupported is what Copy returns on any platform this does not implement.
// Callers are expected to surface it rather than treat a copy as done.
var ErrUnsupported = errors.New("clipboard copy is macOS-only in this build")

// write is what actually delivers the text. It is a variable so tests can
// replace it: the real one would otherwise overwrite the clipboard of whoever
// runs "go test".
var write = pbcopy

// Copy puts text on the system clipboard, replacing whatever was there.
func Copy(text string) error { return write(text) }

func pbcopy(text string) error {
	if runtime.GOOS != "darwin" {
		return ErrUnsupported
	}
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(text)
	// pbcopy prints nothing on success, so anything it did print is the
	// explanation -- worth more in a status line than "exit status 1".
	if out, err := cmd.CombinedOutput(); err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("pbcopy: %s", msg)
		}
		return fmt.Errorf("pbcopy: %w", err)
	}
	return nil
}
