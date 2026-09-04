package sshclient

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// hostKey generates a real ed25519 host key. Real keys rather than fixtures
// so knownhosts' own parsing and comparison is what these tests exercise.
func hostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("wrap key: %v", err)
	}
	return key
}

// remoteAddr is deliberately unrelated to the hostname under test: knownhosts
// matches on the hostname string alone and uses the addr only in error text,
// so resolving anything here would make these tests depend on DNS for nothing.
var remoteAddr = &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2222}

// knownHostsLines returns the file's non-empty lines, so assertions can talk
// about entries rather than byte counts.
func knownHostsLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read known_hosts: %v", err)
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// The failure this covers is the one every new laptop hit: a known_hosts with
// no entry for Koko aborted the connect with "knownhosts: key is unknown",
// after a ticket had already been minted and audited.
func TestKnownHostsLearnsUnknownHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	check, err := knownHostsCallbackAt(path)
	if err != nil {
		t.Fatalf("knownHostsCallbackAt() error = %v", err)
	}
	key := hostKey(t)

	if err := check("jumpserver.example.com:2222", remoteAddr, key); err != nil {
		t.Fatalf("first connect to an unknown host = %v, want accepted", err)
	}
	if got := knownHostsLines(t, path); len(got) != 1 {
		t.Fatalf("known_hosts has %d entries, want 1: %q", len(got), got)
	}

	// A later run must accept the same host from the file alone.
	again, err := knownHostsCallbackAt(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := again("jumpserver.example.com:2222", remoteAddr, key); err != nil {
		t.Fatalf("second run rejected the key it recorded: %v", err)
	}
}

// The security invariant behind accept-new: learning an absent key is a
// convenience, overwriting a key that changed would hand the connection
// ticket's value -- which rides in the SSH password -- to whoever answered.
func TestKnownHostsRefusesChangedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	check, err := knownHostsCallbackAt(path)
	if err != nil {
		t.Fatalf("knownHostsCallbackAt() error = %v", err)
	}
	original := hostKey(t)
	if err := check("jumpserver.example.com:2222", remoteAddr, original); err != nil {
		t.Fatalf("learning the first key: %v", err)
	}
	before := knownHostsLines(t, path)

	impostor := hostKey(t)
	if err := check("jumpserver.example.com:2222", remoteAddr, impostor); err == nil {
		t.Fatal("a changed host key was accepted, want refused")
	}
	after := knownHostsLines(t, path)
	if len(after) != len(before) || (len(after) > 0 && after[0] != before[0]) {
		t.Fatalf("known_hosts was modified on mismatch: %q -> %q", before, after)
	}
}

// Recording the bare "host:2222" instead of the normalized "[host]:2222"
// produces a file that never matches anything on the next run -- the same
// dead end as a hand-run ssh-keyscan that forgot -p.
func TestKnownHostsRecordsNonDefaultPortInBracketForm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	check, err := knownHostsCallbackAt(path)
	if err != nil {
		t.Fatalf("knownHostsCallbackAt() error = %v", err)
	}
	if err := check("jumpserver.example.com:2222", remoteAddr, hostKey(t)); err != nil {
		t.Fatalf("learn: %v", err)
	}

	lines := knownHostsLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("known_hosts has %d entries, want 1: %q", len(lines), lines)
	}
	if want := "[jumpserver.example.com]:2222 "; !strings.HasPrefix(lines[0], want) {
		t.Errorf("entry = %q, want it to start with %q", lines[0], want)
	}
}

// A machine that has never used ssh has neither the file nor ~/.ssh, and
// knownhosts.New needs the file to exist before it will load anything.
func TestKnownHostsCreatesMissingFileAndDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".ssh", "known_hosts")
	check, err := knownHostsCallbackAt(path)
	if err != nil {
		t.Fatalf("knownHostsCallbackAt() with no ~/.ssh = %v, want it created", err)
	}
	if err := check("jumpserver.example.com:2222", remoteAddr, hostKey(t)); err != nil {
		t.Fatalf("learn: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat known_hosts: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("known_hosts mode = %#o, want 0600", perm)
	}
}

// The verifier snapshots the file when it loads, so an accept that does not
// refresh it keeps reporting the host as unknown -- one duplicate line per
// connect, in a TUI that opens session after session from one process.
func TestKnownHostsAcceptsRepeatedlyWithoutDuplicating(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	check, err := knownHostsCallbackAt(path)
	if err != nil {
		t.Fatalf("knownHostsCallbackAt() error = %v", err)
	}
	key := hostKey(t)
	for i := range 3 {
		if err := check("jumpserver.example.com:2222", remoteAddr, key); err != nil {
			t.Fatalf("connect %d = %v, want accepted", i+1, err)
		}
	}
	if got := knownHostsLines(t, path); len(got) != 1 {
		t.Fatalf("known_hosts has %d entries after 3 connects, want 1: %q", len(got), got)
	}
}

// Appending to a file whose last line has no terminator would otherwise glue
// our entry onto the end of it, corrupting both records.
func TestKnownHostsAppendsAfterFileMissingTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")
	existing := "other.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatalf("seed known_hosts: %v", err)
	}

	check, err := knownHostsCallbackAt(path)
	if err != nil {
		t.Fatalf("knownHostsCallbackAt() error = %v", err)
	}
	if err := check("jumpserver.example.com:2222", remoteAddr, hostKey(t)); err != nil {
		t.Fatalf("learn: %v", err)
	}

	lines := knownHostsLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("known_hosts has %d entries, want 2: %q", len(lines), lines)
	}
	if lines[0] != existing {
		t.Errorf("pre-existing entry = %q, want it untouched as %q", lines[0], existing)
	}
}
