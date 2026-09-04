package jmsd

import "testing"

// These vectors were generated with pycryptodome (JumpServer's own crypto
// library) using the exact AESCryptoGCM/EncryptMixin code path, then verified
// to round-trip through decryptField -- not invented by hand. See
// docs/PLAN.md §5 for how they were produced.
func TestDeriveKey(t *testing.T) {
	tests := []struct {
		name      string
		secretKey string
		wantHex   string
	}{
		{
			name:      "key at least 32 bytes is truncated",
			secretKey: "test-django-secret-key-for-cross-check-only",
			wantHex:   "746573742d646a616e676f2d7365637265742d6b65792d666f722d63726f7373",
		},
		{
			name:      "short key is PKCS7-padded to 32 bytes",
			secretKey: "short",
			wantHex:   "73686f72741b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveKey(tt.secretKey)
			if len(got) != 32 {
				t.Fatalf("deriveKey() len = %d, want 32", len(got))
			}
			gotHex := hexEncode(got)
			if gotHex != tt.wantHex {
				t.Errorf("deriveKey() = %s, want %s", gotHex, tt.wantHex)
			}
		})
	}
}

func TestDecryptField(t *testing.T) {
	secretKey := "test-django-secret-key-for-cross-check-only"
	key := deriveKey(secretKey)
	ciphertext := "wZzkWbB9I5U2biTDan+fRQ==BLRXz1DmrkQY5IFR73CVCA==bp8YxF0yrZlDyMDqWyGwOw==" +
		"KK5zf5yB9vgvJgMOtU6/dGmNgm+CynsJlHpzpQACgp9f3UKo5WEhgaxVZp4N2hS3NWMQY4mKeOgi5hTFwpXhEEToFeVhrpwZVMbf1LKgGLHr/qyDXfWaB19Igw=="
	want := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmno user@example"

	got, err := decryptField(ciphertext, key)
	if err != nil {
		t.Fatalf("decryptField() error = %v", err)
	}
	if got != want {
		t.Errorf("decryptField() = %q, want %q", got, want)
	}
}

func TestDecryptFieldRejectsTooShort(t *testing.T) {
	if _, err := decryptField("short", deriveKey("k")); err == nil {
		t.Error("decryptField() expected error for too-short value, got nil")
	}
}

func hexEncode(b []byte) string {
	const hexDigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexDigits[v>>4]
		out[i*2+1] = hexDigits[v&0x0f]
	}
	return string(out)
}
