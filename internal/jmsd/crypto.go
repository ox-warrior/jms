package jmsd

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"fmt"
)

// deriveKey reproduces JumpServer's AESCryptoGCM.process_key
// (apps/common/utils/crypto.py): keys >= 32 bytes are truncated to 32; shorter
// keys are PKCS7-padded up to 32. secretKey is Core's Django SECRET_KEY.
func deriveKey(secretKey string) []byte {
	key := []byte(secretKey)
	if len(key) >= 32 {
		return key[:32]
	}
	padLen := 32 - len(key)
	out := make([]byte, 32)
	copy(out, key)
	for i := len(key); i < 32; i++ {
		out[i] = byte(padLen)
	}
	return out
}

// decryptField reverses JumpServer's EncryptMixin/AESCryptoGCM
// (apps/common/db/fields.py, apps/common/utils/crypto.py): the stored value is
// base64(header 16B) + base64(nonce 16B) + base64(tag 16B) + base64(ciphertext),
// concatenated with no separator (72 chars of metadata, since each 16-byte
// chunk base64-encodes to 24 chars). header is used as the GCM AAD.
//
// Verified byte-for-byte against a pycryptodome-generated vector (same
// AES.MODE_GCM defaults JumpServer uses) before this went into production use.
func decryptField(value string, key []byte) (string, error) {
	if len(value) < 72 {
		return "", fmt.Errorf("decrypt field: value too short (%d bytes)", len(value))
	}
	metadata := value[:72]
	header, err := base64.StdEncoding.DecodeString(metadata[:24])
	if err != nil {
		return "", fmt.Errorf("decrypt field: decode header: %w", err)
	}
	nonce, err := base64.StdEncoding.DecodeString(metadata[24:48])
	if err != nil {
		return "", fmt.Errorf("decrypt field: decode nonce: %w", err)
	}
	tag, err := base64.StdEncoding.DecodeString(metadata[48:72])
	if err != nil {
		return "", fmt.Errorf("decrypt field: decode tag: %w", err)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(value[72:])
	if err != nil {
		return "", fmt.Errorf("decrypt field: decode ciphertext: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("decrypt field: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, len(nonce))
	if err != nil {
		return "", fmt.Errorf("decrypt field: new gcm: %w", err)
	}
	// pycryptodome's decrypt_and_verify takes ciphertext and tag separately;
	// Go's GCM.Open expects them concatenated.
	combined := append(append([]byte{}, ciphertext...), tag...)
	plain, err := gcm.Open(nil, nonce, combined, header)
	if err != nil {
		return "", fmt.Errorf("decrypt field: %w", err)
	}
	return string(plain), nil
}
