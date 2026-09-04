package sshclient

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// writeKey generates a real ed25519 private key and writes it to dir/name in
// OpenSSH format, encrypted with a passphrase when one is given. Real keys
// rather than fixtures so ssh.ParsePrivateKey's own encrypted-vs-plaintext
// detection is what the tests exercise.
func writeKey(t *testing.T, dir, name, passphrase string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	var block *pem.Block
	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(priv, "")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	}
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}

// The regression this function exists for: a passphrase-protected id_rsa used
// to abort the search, so a usable id_ed25519 sitting right next to it was
// never tried and jms refused to start.
func TestLoadFirstUsableSkipsEncryptedKeyForUsableOne(t *testing.T) {
	dir := t.TempDir()
	writeKey(t, dir, "id_rsa", "correct horse")
	writeKey(t, dir, "id_ed25519", "")

	signer, path, err := loadFirstUsable(dir)
	if err != nil {
		t.Fatalf("loadFirstUsable() error = %v, want the plaintext id_ed25519", err)
	}
	if signer == nil {
		t.Fatal("loadFirstUsable() returned a nil signer with no error")
	}
	if want := filepath.Join(dir, "id_ed25519"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

// A malformed key must be passed over for the same reason an encrypted one is.
func TestLoadFirstUsableSkipsMalformedKeyForUsableOne(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "id_rsa"), []byte("not a key"), 0o600); err != nil {
		t.Fatalf("write junk: %v", err)
	}
	writeKey(t, dir, "id_ed25519", "")

	_, path, err := loadFirstUsable(dir)
	if err != nil {
		t.Fatalf("loadFirstUsable() error = %v", err)
	}
	if want := filepath.Join(dir, "id_ed25519"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

// Order still matters: id_rsa wins when both are usable.
func TestLoadFirstUsablePrefersIDRSAWhenBothLoad(t *testing.T) {
	dir := t.TempDir()
	writeKey(t, dir, "id_rsa", "")
	writeKey(t, dir, "id_ed25519", "")

	_, path, err := loadFirstUsable(dir)
	if err != nil {
		t.Fatalf("loadFirstUsable() error = %v", err)
	}
	if want := filepath.Join(dir, "id_rsa"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

// Nothing usable: the report must name the key that exists but can't be used,
// not the generic "no private key found" -- that file is what needs fixing.
func TestLoadFirstUsableReportsTheUnusableKey(t *testing.T) {
	dir := t.TempDir()
	writeKey(t, dir, "id_rsa", "correct horse")

	_, _, err := loadFirstUsable(dir)
	if err == nil {
		t.Fatal("loadFirstUsable() expected an error when the only key is encrypted")
	}
	if !strings.Contains(err.Error(), "id_rsa") {
		t.Errorf("error = %q, want it to name id_rsa", err)
	}
	if !strings.Contains(err.Error(), "encrypted") {
		t.Errorf("error = %q, want it to say the key is encrypted", err)
	}
}

// An empty directory is the genuinely-nothing-there case, and gets the message
// that lists what was looked for.
func TestLoadFirstUsableEmptyDir(t *testing.T) {
	_, _, err := loadFirstUsable(t.TempDir())
	if err == nil {
		t.Fatal("loadFirstUsable() expected an error for an empty dir")
	}
	for _, want := range []string{"no private key found", "~/.ssh/id_rsa", "~/.ssh/id_ed25519"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}
