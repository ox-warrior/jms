package sshclient

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/leiyupeng/jumpserver-cli/internal/jmslog"
)

// KnownHostsCallback builds the host key verifier every Koko dial runs
// through. The policy is OpenSSH's StrictHostKeyChecking=accept-new: a host
// with no entry at all gets recorded and let through, a host whose recorded
// key no longer matches is refused.
//
// The asymmetry is the entire design. Refusing the unknown host was correct
// in the abstract and useless in practice -- it meant every new laptop hit
// "knownhosts: key is unknown" on its first connect, and the way out was to
// tell people to run ssh-keyscan, which trusts whatever answers the port with
// nobody looking at the fingerprint. That is the same leap of faith recorded
// here, minus the step where a confused user does it by hand.
//
// What must not be softened is the mismatch case. The connection ticket's
// value travels as the SSH password (see TokenGatewayOptions), so whoever
// terminates this handshake receives a live, Core-issued credential. A
// changed key is the one signal that something other than Koko is on the
// other end, and it stays fatal.
func KnownHostsCallback() (ssh.HostKeyCallback, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return knownHostsCallbackAt(filepath.Join(home, ".ssh", "known_hosts"))
}

// knownHostsCallbackAt is KnownHostsCallback with the path injected, so the
// tests can drive a temp file instead of the caller's real ~/.ssh.
func knownHostsCallbackAt(path string) (ssh.HostKeyCallback, error) {
	learner := &hostKeyLearner{path: path}
	if err := learner.reload(); err != nil {
		return nil, err
	}
	return learner.check, nil
}

// hostKeyLearner owns the known_hosts file and the verifier built from it.
//
// The pair has to be kept in step: knownhosts.New snapshots the file when it
// loads, so a verifier built before an append keeps reporting the host as
// unknown for the rest of the process. In a long-running TUI that opens
// session after session that is one appended duplicate line per connect, so
// every accept reloads rather than patching an in-memory set.
type hostKeyLearner struct {
	path string

	// mu covers both verify and the file underneath it. Held across the whole
	// check, not just the swap: two concurrent first-time dials would
	// otherwise both see "unknown" and both append.
	mu     sync.Mutex
	verify ssh.HostKeyCallback
}

func (l *hostKeyLearner) reload() error {
	if err := ensureKnownHostsFile(l.path); err != nil {
		return err
	}
	verify, err := knownhosts.New(l.path)
	if err != nil {
		return fmt.Errorf("load known_hosts %s: %w", l.path, err)
	}
	l.verify = verify
	return nil
}

func (l *hostKeyLearner) check(hostname string, remote net.Addr, key ssh.PublicKey) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	err := l.verify(hostname, remote, key)
	if err == nil {
		return nil
	}
	// A KeyError with a non-empty Want is "we have a key on file for this
	// host and it is not the one you just presented" -- the MITM signal.
	// Anything that is not a KeyError at all (malformed line, unreadable
	// file) is likewise not ours to paper over.
	var keyErr *knownhosts.KeyError
	if !errors.As(err, &keyErr) || len(keyErr.Want) > 0 {
		return err
	}

	if err := appendKnownHost(l.path, hostname, key); err != nil {
		return fmt.Errorf("record host key for %s: %w", hostname, err)
	}
	// Fingerprint of a public key, safe to log, and the only forensic trace
	// that this machine ever took a new key on trust.
	jmslog.Printf("known_hosts: learned %s %s %s",
		knownhosts.Normalize(hostname), key.Type(), ssh.FingerprintSHA256(key))
	return l.reload()
}

// ensureKnownHostsFile makes knownhosts.New's precondition true: it needs the
// file to exist, and on a laptop that has never used ssh neither the file nor
// ~/.ssh is there. Permissions match what ssh-keygen would have created.
func ensureKnownHostsFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return file.Close()
}

func appendKnownHost(path, hostname string, key ssh.PublicKey) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()

	var line strings.Builder
	// An existing file whose last line has no terminator would otherwise get
	// our entry glued onto the end of it, corrupting both.
	if needsNewline, err := lacksTrailingNewline(path); err != nil {
		return err
	} else if needsNewline {
		line.WriteString("\n")
	}
	// Line normalizes the address, which is what puts a non-default port into
	// the [host]:port form known_hosts lookups actually match. Writing the
	// bare "host:2222" here would produce a file that never matches anything
	// -- the exact shape of a hand-run "ssh-keyscan" that forgot -p.
	line.WriteString(knownhosts.Line([]string{hostname}, key))
	line.WriteString("\n")

	_, err = file.WriteString(line.String())
	return err
}

func lacksTrailingNewline(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if info.Size() == 0 {
		return false, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()

	last := make([]byte, 1)
	if _, err := file.ReadAt(last, info.Size()-1); err != nil {
		return false, err
	}
	return last[0] != '\n', nil
}
