package sshclient

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// candidateKeyNames are the keys tried, in order, when no --identity is given.
var candidateKeyNames = []string{"id_rsa", "id_ed25519"}

// LoadIdentity resolves the SSH key this caller is identified by: an explicit
// path when given, otherwise the first usable key among candidateKeyNames.
func LoadIdentity(explicit string) (ssh.Signer, string, error) {
	if explicit != "" {
		signer, err := loadPrivateKey(explicit)
		if err != nil {
			return nil, "", fmt.Errorf("load identity %s: %w", explicit, err)
		}
		return signer, explicit, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, "", err
	}
	return loadFirstUsable(filepath.Join(home, ".ssh"))
}

// loadFirstUsable returns the first key in dir that actually loads.
//
// "Usable" is the operative word: every candidate that cannot be loaded --
// missing, encrypted, malformed -- is passed over, not only the missing ones.
// Continuing on ErrNotExist alone looked equivalent and wasn't. A
// passphrase-protected ~/.ssh/id_rsa is very common (plenty are years old and
// were created with one), and it made jms refuse to start with "encrypted
// private keys are not supported" while a perfectly good ~/.ssh/id_ed25519
// sat next to it, never tried. Worse, the error named id_rsa -- so the
// obvious next move was to go edit or delete a key that was never needed.
//
// When nothing loads, the first real failure is reported instead of the
// generic "no private key found": a key that exists but is unusable is the
// likelier problem, and its own message already says what to fix.
func loadFirstUsable(dir string) (ssh.Signer, string, error) {
	var firstErr error
	for _, name := range candidateKeyNames {
		path := filepath.Join(dir, name)
		signer, loadErr := loadPrivateKey(path)
		if loadErr == nil {
			return signer, path, nil
		}
		if errors.Is(loadErr, os.ErrNotExist) {
			continue
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("load identity %s: %w", path, loadErr)
		}
	}
	if firstErr != nil {
		return nil, "", firstErr
	}
	return nil, "", fmt.Errorf("no private key found; tried %s", candidateList())
}

// candidateList renders the candidate names for the not-found message as
// ~/.ssh/<name>: that message is advice about which file to create, and the
// tilde form is how anyone would write it.
func candidateList() string {
	paths := make([]string, 0, len(candidateKeyNames))
	for _, name := range candidateKeyNames {
		paths = append(paths, "~/.ssh/"+name)
	}
	return strings.Join(paths, " then ")
}

func loadPrivateKey(path string) (ssh.Signer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(b)
	var passphraseMissing *ssh.PassphraseMissingError
	if errors.As(err, &passphraseMissing) {
		return nil, errors.New("encrypted private keys are not supported yet")
	}
	return signer, err
}
