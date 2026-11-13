package sshclient

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

// LoadIdentity resolves the SSH key this caller is identified by: an explicit
// path when given (a service key, see jmsd's service_keys), otherwise jms's
// own device key, which a Google sign-in binds to its owner.
func LoadIdentity(explicit string) (ssh.Signer, string, error) {
	if explicit != "" {
		signer, err := loadPrivateKey(explicit)
		if err != nil {
			return nil, "", fmt.Errorf("load identity %s: %w", explicit, err)
		}
		return signer, explicit, nil
	}
	return deviceKey()
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

// deviceKey loads jms's own key, creating it on first use. Nobody registers
// it anywhere: a Google sign-in binds it to its owner on jmsd, and that
// binding is the only thing that makes it worth anything. One per machine,
// so losing a laptop costs that laptop's binding and nothing else.
func deviceKey() (ssh.Signer, string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, "", err
	}
	path := filepath.Join(dir, "jms", "device_key")
	signer, err := loadPrivateKey(path)
	if !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return nil, "", fmt.Errorf("load device key %s: %w", path, err)
		}
		return signer, path, nil
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, "jms device key")
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, "", err
	}
	// O_EXCL: two jms processes starting together must not each write a key
	// and leave one of them signing with a key that is no longer on disk.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return deviceKey()
	}
	if err != nil {
		return nil, "", err
	}
	if err := pem.Encode(f, block); err != nil {
		f.Close()
		os.Remove(path)
		return nil, "", err
	}
	if err := f.Close(); err != nil {
		return nil, "", err
	}
	signer, err = ssh.NewSignerFromKey(priv)
	return signer, path, err
}
