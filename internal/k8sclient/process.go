package k8sclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Locate resolves the k9s binary to run: an explicit --k9s-path if given,
// otherwise whatever "k9s" resolves to on PATH.
//
// Everything knowable about the binary is checked HERE, because this runs
// before any ticket is minted (connectflow.PrepareK8s) and Run does not run
// until after -- so anything this lets through fails on the far side of a real
// Core audit entry and a burnt ticket. The executable bit is the case that
// actually happens: a k9s pulled out of a release tarball by hand arrives
// without it, and --k9s-path pointed at that file used to pass this check and
// then fail at exec.
func Locate(path string) (string, error) {
	if path != "" {
		info, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("k9s path %q: %w", path, err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("k9s path %q is a directory, not an executable", path)
		}
		if info.Mode().Perm()&0o111 == 0 {
			return "", fmt.Errorf("k9s path %q is not executable (chmod +x it)", path)
		}
		return path, nil
	}
	resolved, err := exec.LookPath("k9s")
	if err != nil {
		return "", errors.New("k9s not found on PATH -- install it or pass --k9s-path")
	}
	return resolved, nil
}

// killGrace is how long k9s gets to wind itself down after ctx is cancelled
// before it is killed outright. Cancellation here means the Koko session died
// under it (connectflow.RunK9s), so the run is over either way -- this only
// decides whether k9s gets to put the terminal back and write its own state
// out first.
const killGrace = 3 * time.Second

// Run starts k9s against kubeconfigPath, taking over the current terminal
// (stdio wired straight through, matching sshclient.RunInteractiveShell's
// treatment of the remote shell) until the user quits it or ctx is
// cancelled.
//
// Cancellation asks with SIGTERM rather than exec.CommandContext's default
// SIGKILL: k9s owns the terminal at that point (alternate screen, raw mode,
// mouse reporting) and persists its per-context view state on the way out --
// state this client goes out of its way to keep per-asset, by renaming the
// kubeconfig's context (see kubeconfig.go's renameContexts). SIGKILL denies it
// both. WaitDelay keeps the guarantee that the process does go away: it is
// killed for real if it has not exited within killGrace.
func Run(ctx context.Context, k9sPath, kubeconfigPath string) error {
	cmd := exec.CommandContext(ctx, k9sPath, "--kubeconfig", kubeconfigPath)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = killGrace
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
