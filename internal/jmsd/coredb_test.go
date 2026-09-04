package jmsd

import "testing"

// Two real ed25519 public keys with known SHA256 fingerprints (generated
// with ssh-keygen, not hand-crafted), so the fingerprint math itself is
// exercised rather than just the string plumbing.
const (
	testKeyA          = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPGP5ZvdiWddIaQ3KlS0em39NFHk70jtiU0c3S08sXEd"
	testKeyAFP        = "SHA256:T3wzOO7/LV2PXaP/QNlpwfbrsmgwx3EKveQpPMnftLc"
	testKeyB          = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIE1Fwd436ofnSyDypwEOCn0mMGWeFd2C8nP1DxoP4jxU"
	testKeyBFP        = "SHA256:dzmjyPv93IdvMtZHW3Z9bOjw1SGjWd7eJwCGeqvjN6c"
	testKeyBogus      = "not-a-valid-key at all"
	testFingerprintFP = "SHA256:doesNotMatchAnything=="
)

// matchFingerprint resolves a fingerprint to the single user who registered
// that key, over a set of rows, by building a throwaway index over them.
//
// It lives here because the request path does not use it: that goes through
// userDirectory's cached sshKeyIndex (keycache.go). Keeping the tests below
// pointed at the same buildSSHKeyIndex/lookupUserID pair means there is still
// exactly one implementation of the rules they cover -- refusing a fingerprint
// two distinct users registered, and not double-counting one user who
// registered the same key twice.
func matchFingerprint(rows []sshKeyRow, fingerprint string) (userID string, err error) {
	return buildSSHKeyIndex(rows).lookupUserID(fingerprint)
}

func TestMatchFingerprintSingleMatch(t *testing.T) {
	rows := []sshKeyRow{
		{userID: "u1", username: "alice", publicKey: testKeyA},
		{userID: "u2", username: "bob", publicKey: testKeyB},
	}
	got, err := matchFingerprint(rows, testKeyAFP)
	if err != nil {
		t.Fatalf("matchFingerprint() error = %v", err)
	}
	if got != "u1" {
		t.Errorf("matchFingerprint() = %q, want u1", got)
	}
}

func TestMatchFingerprintNoMatch(t *testing.T) {
	rows := []sshKeyRow{{userID: "u1", username: "alice", publicKey: testKeyA}}
	if _, err := matchFingerprint(rows, testFingerprintFP); err == nil {
		t.Fatal("matchFingerprint() expected error for unregistered fingerprint, got nil")
	}
}

// A user who registered the same key twice (two rows, one user) must resolve
// cleanly -- it is not the ambiguous case.
func TestMatchFingerprintSameUserTwice(t *testing.T) {
	rows := []sshKeyRow{
		{userID: "u1", username: "alice", publicKey: testKeyA},
		{userID: "u1", username: "alice", publicKey: testKeyA},
	}
	got, err := matchFingerprint(rows, testKeyAFP)
	if err != nil {
		t.Fatalf("matchFingerprint() error = %v", err)
	}
	if got != "u1" {
		t.Errorf("matchFingerprint() = %q, want u1", got)
	}
}

// The security-critical case: two DISTINCT users registered the same key.
// Must refuse rather than silently pick one -- see matchFingerprint's doc
// comment for why.
func TestMatchFingerprintAmbiguousAcrossUsersIsRejected(t *testing.T) {
	rows := []sshKeyRow{
		{userID: "u1", username: "alice", publicKey: testKeyA},
		{userID: "u2", username: "eve", publicKey: testKeyA},
	}
	_, err := matchFingerprint(rows, testKeyAFP)
	if err == nil {
		t.Fatal("matchFingerprint() must reject a fingerprint shared by two distinct users, got nil error")
	}
}

func TestMatchFingerprintSkipsUnparseableKeys(t *testing.T) {
	rows := []sshKeyRow{
		{userID: "u1", username: "alice", publicKey: testKeyBogus},
		{userID: "u2", username: "bob", publicKey: testKeyB},
	}
	got, err := matchFingerprint(rows, testKeyBFP)
	if err != nil {
		t.Fatalf("matchFingerprint() error = %v", err)
	}
	if got != "u2" {
		t.Errorf("matchFingerprint() = %q, want u2 (bogus row must be skipped, not fatal)", got)
	}
}

func TestMatchFingerprintEmptyInput(t *testing.T) {
	if _, err := matchFingerprint(nil, testKeyAFP); err == nil {
		t.Fatal("matchFingerprint(nil, ...) expected error, got nil")
	}
}

// The record has to carry EVERY key the matched user registered, not just the
// one whose fingerprint matched -- verifySignedRequest accepts a signature
// from any of them -- and none of anybody else's.
func TestCollectUserGathersAllOfOneUsersKeys(t *testing.T) {
	rows := []sshKeyRow{
		{userID: "u1", username: "alice", otpSecretKey: "SECRETA", publicKey: testKeyA},
		{userID: "u2", username: "bob", otpSecretKey: "SECRETB", publicKey: testKeyB},
		{userID: "u1", username: "alice", otpSecretKey: "SECRETA", publicKey: testKeyB},
	}
	got := collectUser(rows, "u1")
	if got.ID != "u1" || got.Username != "alice" {
		t.Errorf("identity = %s/%s, want u1/alice", got.ID, got.Username)
	}
	if got.OTPSecretKey != "SECRETA" {
		t.Errorf("OTPSecretKey = %q, want SECRETA (bob's must not leak in)", got.OTPSecretKey)
	}
	if len(got.PublicKeys) != 2 {
		t.Fatalf("PublicKeys = %v, want both of alice's", got.PublicKeys)
	}
}

// A user with no MFA device enrolled leaves otp_secret_key empty, which
// handleMFA reports as "no MFA device enrolled" rather than accepting any code.
func TestCollectUserWithoutOTPSecret(t *testing.T) {
	rows := []sshKeyRow{{userID: "u1", username: "alice", publicKey: testKeyA}}
	if got := collectUser(rows, "u1"); got.OTPSecretKey != "" {
		t.Errorf("OTPSecretKey = %q, want empty", got.OTPSecretKey)
	}
}
