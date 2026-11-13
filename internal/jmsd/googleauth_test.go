package jmsd

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Each case is one way a token that should not clear MFA could get through:
// minted for another app, a personal account, a token from another request,
// an old one. Only the first case may pass.
func TestGoogleVerifier(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "k1", "kty": "RSA",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	defer jwks.Close()

	now := time.Unix(1_800_000_000, 0)
	v := newGoogleVerifier(GoogleConfig{ClientID: "jms-client", HostedDomain: "example.com"})
	v.certsURL, v.now = jwks.URL, func() time.Time { return now }

	sign := func(claims map[string]any) string {
		enc := func(x any) string {
			b, _ := json.Marshal(x)
			return base64.RawURLEncoding.EncodeToString(b)
		}
		signed := enc(map[string]string{"alg": "RS256", "kid": "k1"}) + "." + enc(claims)
		digest := sha256.Sum256([]byte(signed))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
	}
	valid := func() map[string]any {
		return map[string]any{
			"iss": "https://accounts.google.com", "aud": "jms-client", "exp": now.Unix() + 600,
			"nonce": "n1", "email": "alice@example.com", "email_verified": true, "hd": "example.com",
		}
	}

	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		ok     bool
	}{
		{name: "valid", mutate: func(map[string]any) {}, ok: true},
		{name: "issued to another client", mutate: func(c map[string]any) { c["aud"] = "someone-else" }},
		{name: "personal account", mutate: func(c map[string]any) { delete(c, "hd") }},
		{name: "another request's token", mutate: func(c map[string]any) { c["nonce"] = "n2" }},
		{name: "expired", mutate: func(c map[string]any) { c["exp"] = now.Unix() - 1 }},
		{name: "unverified email", mutate: func(c map[string]any) { c["email_verified"] = false }},
		{name: "not Google", mutate: func(c map[string]any) { c["iss"] = "https://evil.example" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := valid()
			tc.mutate(claims)
			email, err := v.verify(sign(claims), "n1")
			if tc.ok && (err != nil || email != "alice@example.com") {
				t.Fatalf("got %q, %v; want alice's email", email, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("accepted; want refused")
			}
		})
	}

	// A token whose signature was not made by Google's key.
	forged := sign(valid())
	forged = forged[:len(forged)-4] + "AAAA"
	if _, err := v.verify(forged, "n1"); err == nil {
		t.Fatal("accepted a token with a tampered signature")
	}
}
