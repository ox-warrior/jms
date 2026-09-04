package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// envUpdatedTo is set on the environment of the process Restart execs into,
// naming the version that was just installed.
//
// It closes a loop that is otherwise unbreakable from the user's side: if a
// manifest ever names a version the published binary does not actually report
// -- a botched release, a binary upload that failed while the manifest
// succeeded -- then update, restart, and re-offer repeat forever, and jms can
// never be launched again. One variable turns that into one message.
const envUpdatedTo = "JMS_UPDATED_TO"

// ErrAlreadyUpdated means this process is the restart from an update that
// installed the very version now being offered again -- so the binary on disk
// does not report the version its manifest claims. Nothing here can fix that;
// the caller's job is to say so and stop asking.
var ErrAlreadyUpdated = errors.New("the installed binary does not report the version the manifest claims")

// ErrNotWritable means the install directory is not this user's to write --
// jms sitting in a root-owned /usr/local/bin, which is how a machine that ran
// scripts/install.sh under sudo ends up.
//
// Its own error so the caller can print the one-line curl that does work
// instead of a permission-denied. jms deliberately does not try to escalate:
// prompting for a sudo password from a tool whose whole job is holding
// production credentials is the wrong habit to teach, and a user who wants the
// update can run the installer themselves.
var ErrNotWritable = errors.New("install directory is not writable")

// maxBinarySize caps the download. The binaries are around 13MB today; the cap
// is only here so a misrouted URL cannot fill the user's disk before the digest
// gets a chance to reject it.
const maxBinarySize = 256 << 20

// Check reports the published release when it is newer than this build and the
// user has not skipped it. A nil Release with a nil error is the ordinary "you
// are up to date" answer.
//
// ctx carries the deadline. The caller is expected to keep it short: this runs
// on the launch path, and a slow CDN must cost a bounded pause, not an
// indefinite one.
func Check(ctx context.Context, baseURL, localVersion string) (*Release, error) {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	// No publish location configured anywhere: the feature is off, which is a
	// nil release and no error. Returning an error instead would put a line in
	// the debug log on every launch of a stock build, describing a failure that
	// is really just an unset option.
	if baseURL == "" {
		return nil, nil
	}
	m, err := fetchManifest(ctx, &http.Client{}, baseURL)
	if err != nil {
		return nil, err
	}
	newer, err := isNewer(m.Version, localVersion)
	if err != nil {
		return nil, err
	}
	if !newer {
		return nil, nil
	}
	// Checked after "is it newer" so it only ever fires on the exact loop it
	// exists for, and not on the ordinary case of an old JMS_UPDATED_TO lying
	// around in a long-lived shell.
	if m.Version == os.Getenv(envUpdatedTo) {
		return nil, fmt.Errorf("%w: installed %s, still reporting %s",
			ErrAlreadyUpdated, m.Version, localVersion)
	}
	// Checked after the comparison, not before: the file records one declined
	// version, so it can only be consulted meaningfully once we know which
	// version is on offer.
	if m.Version == skippedVersion() {
		return nil, nil
	}
	release, err := m.release(baseURL)
	if err != nil {
		return nil, err
	}
	release.BaseURL = baseURL
	return &release, nil
}

// exePath is the binary Apply will replace, with symlinks resolved.
//
// Resolving matters: without it, an install reached through a symlink (a
// Homebrew-style bin shim, or the ~/bin/jms -> ~/work/jms/dist/... a developer
// makes) would have the *link* replaced by a regular file, quietly detaching it
// from whatever it pointed at.
func exePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

// InstallDir is the directory holding the running binary -- the directory that
// has to be writable for an update to be possible, and the one worth naming
// when it is not. Empty if the path cannot be resolved at all, which leaves the
// caller nothing useful to say and is why it returns no error of its own.
func InstallDir() string {
	exe, err := exePath()
	if err != nil {
		return ""
	}
	return filepath.Dir(exe)
}

// Apply downloads the release, verifies its digest, and atomically replaces the
// running binary. It returns the path that was replaced, for Restart.
//
// The replacement is a rename over the live binary, which is safe on darwin and
// linux: the running process holds the old inode open and keeps executing it
// undisturbed, so nothing about the current session changes. That is also why
// this cannot take effect without a restart.
//
// The download lands in the install directory rather than the system temp dir.
// Two reasons, both fatal to the alternative: rename cannot cross filesystems,
// and creating the file there is the only honest test of whether we may write
// there at all -- a stat-and-guess would race, and on macOS would also miss
// the cases where the mode bits say yes and the OS says no.
func Apply(ctx context.Context, r *Release) (string, error) {
	exe, err := exePath()
	if err != nil {
		return "", err
	}
	return apply(ctx, r, exe)
}

// apply is Apply with the target named, which is the only way to exercise it:
// Apply's target is os.Executable(), and under `go test` that is the test
// binary itself.
func apply(ctx context.Context, r *Release, exe string) (string, error) {
	// The leading dot is deliberate. If cleanup is ever missed -- a kill -9
	// mid-download -- what is left behind is a hidden file, not something that
	// looks like a second jms sitting on the user's PATH.
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".jms-update-*")
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return "", fmt.Errorf("%w: %s", ErrNotWritable, filepath.Dir(exe))
		}
		return "", err
	}
	tmpName := tmp.Name()
	// Removed on every failure path. After a successful rename the name is
	// gone, so the deferred Remove fails harmlessly and its error is ignored
	// for that reason rather than out of carelessness.
	committed := false
	defer func() {
		tmp.Close()
		if !committed {
			os.Remove(tmpName)
		}
	}()

	if err := download(ctx, r, tmp); err != nil {
		return "", err
	}
	// Synced before the rename: the rename is atomic with respect to other
	// processes, but not with respect to a power loss, and the failure it would
	// leave behind is a truncated jms -- the tool the user needs in order to
	// fix anything.
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("flush download: %w", err)
	}
	// CreateTemp makes the file 0600; it has to be executable to be worth
	// renaming into place. 0755 matches what scripts/install.sh leaves.
	if err := tmp.Chmod(0o755); err != nil {
		return "", fmt.Errorf("mark downloaded binary executable: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close download: %w", err)
	}
	if err := os.Rename(tmpName, exe); err != nil {
		return "", fmt.Errorf("replace %s: %w", exe, err)
	}
	committed = true
	return exe, nil
}

// download streams r.URL into dst, refusing anything whose digest does not
// match the manifest.
//
// The hash is computed as the bytes stream past rather than by re-reading the
// finished file: re-reading would verify whatever is on disk at that later
// moment, which is not quite the same claim.
func download(ctx context.Context, r *Release, dst io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", r.URL, resp.Status)
	}

	digest := sha256.New()
	written, err := io.Copy(io.MultiWriter(dst, digest), io.LimitReader(resp.Body, maxBinarySize))
	if err != nil {
		return fmt.Errorf("download %s: %w", r.URL, err)
	}
	if written == maxBinarySize {
		return fmt.Errorf("download %s: refused at the %d byte cap", r.URL, int64(maxBinarySize))
	}
	if got := hex.EncodeToString(digest.Sum(nil)); got != r.SHA256 {
		// Named in full on purpose: the two cases behind this are a corrupted
		// transfer and a manifest that no longer describes what the CDN is
		// serving, and telling them apart starts with the digest that actually
		// arrived.
		return fmt.Errorf("download %s: sha256 %s, manifest says %s", r.URL, got, r.SHA256)
	}
	return nil
}

// Restart replaces this process with the binary at path, keeping the arguments
// and environment it was launched with.
//
// exec rather than spawn-and-exit: the user typed one command and is waiting at
// one terminal, and a child process would leave the shell's job control and the
// TUI's grip on the tty split between two processes. It only returns on
// failure -- on success there is no "after" in this process.
//
// argv[0] stays as the user typed it rather than becoming the resolved path, so
// anything downstream that reports the command line reports what was run.
//
// version goes into the child's environment as envUpdatedTo, so the new process
// can recognise the one case where it is about to offer the update it just
// installed.
func Restart(path, version string) error {
	return syscall.Exec(path, os.Args, withUpdatedTo(os.Environ(), version))
}

// withUpdatedTo returns env with envUpdatedTo set to version.
//
// Any existing entry is dropped rather than shadowed by appending: a second
// update in one chain would otherwise leave two, and which of a duplicate pair
// os.Getenv resolves to is not something to build a loop-breaker on.
func withUpdatedTo(env []string, version string) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, envUpdatedTo+"=") {
			out = append(out, entry)
		}
	}
	return append(out, envUpdatedTo+"="+version)
}
