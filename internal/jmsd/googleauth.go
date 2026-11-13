package jmsd

// Google sign-in is how a person gets in: jms runs Google's OAuth flow in the
// user's browser and hands jmsd the resulting ID token along with jms's own
// key; jmsd checks the token here and binds the key to the JumpServer user
// with that email (handleGoogleSignIn).
//
// Verified with the standard library rather than an OIDC package: it is one
// algorithm (RS256), one issuer and a handful of claims, and every check is
// listed in verify where it can be read in one place.

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

const googleCertsURL = "https://www.googleapis.com/oauth2/v3/certs"

// googleKeysMaxAge is how long a fetched key set is trusted before the next
// verify refetches it. Google rotates its signing keys every few weeks and
// publishes new ones well before using them, so an hour is generous.
const googleKeysMaxAge = time.Hour

// googleKeysMinRefetch bounds refetching on an unknown kid. Without it, a
// stream of tokens with made-up kids would turn into a stream of requests to
// Google from jmsd.
const googleKeysMinRefetch = time.Minute

type googleVerifier struct {
	clientID     string
	hostedDomain string
	certsURL     string
	client       *http.Client
	now          func() time.Time

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
	// attempted is the last fetch, successful or not; see key.
	attempted time.Time
}

func newGoogleVerifier(cfg GoogleConfig) *googleVerifier {
	return &googleVerifier{
		clientID:     cfg.ClientID,
		hostedDomain: cfg.HostedDomain,
		certsURL:     googleCertsURL,
		client:       &http.Client{Timeout: 10 * time.Second},
		now:          time.Now,
	}
}

// googleClaims is the part of a Google ID token jmsd looks at.
type googleClaims struct {
	Issuer        string `json:"iss"`
	Audience      string `json:"aud"`
	Expiry        int64  `json:"exp"`
	Nonce         string `json:"nonce"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	HostedDomain  string `json:"hd"`
}

// verify checks raw is a Google ID token issued to this client, for a verified
// account in the company's Workspace, carrying nonce, and not expired. It
// returns the account's email.
//
// nonce is what makes the token single-use: jms puts the nonce of the signed
// request into the Google flow, and jmsd's replay check already refuses a
// nonce it has seen -- so a token captured in transit cannot be submitted a
// second time, and cannot be attached to someone else's request.
func (v *googleVerifier) verify(raw, nonce string) (string, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", errors.New("malformed id_token")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeJWTPart(parts[0], &header); err != nil {
		return "", fmt.Errorf("id_token header: %w", err)
	}
	// Pinned, not read from the token: accepting whatever alg the token names
	// is the classic way a JWT check gets talked out of verifying anything.
	if header.Alg != "RS256" {
		return "", fmt.Errorf("id_token alg %q, want RS256", header.Alg)
	}
	key, err := v.key(header.Kid)
	if err != nil {
		return "", err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", errors.New("id_token signature encoding")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return "", errors.New("id_token signature does not verify")
	}

	var c googleClaims
	if err := decodeJWTPart(parts[1], &c); err != nil {
		return "", fmt.Errorf("id_token claims: %w", err)
	}
	switch {
	case c.Issuer != "https://accounts.google.com" && c.Issuer != "accounts.google.com":
		return "", fmt.Errorf("id_token issuer %q is not Google", c.Issuer)
	case c.Audience != v.clientID:
		return "", errors.New("id_token was issued to a different OAuth client")
	case v.now().Unix() >= c.Expiry:
		return "", errors.New("id_token has expired")
	case c.Nonce == "" || c.Nonce != nonce:
		return "", errors.New("id_token nonce does not match this request")
	case !c.EmailVerified:
		return "", errors.New("Google account email is not verified")
	// hd, not the email suffix: hd is set only for Workspace-managed
	// accounts, while a personal Google account can be registered under any
	// address, a company one included.
	case !strings.EqualFold(c.HostedDomain, v.hostedDomain):
		return "", fmt.Errorf("%s is not a %s account", c.Email, v.hostedDomain)
	}
	return c.Email, nil
}

// key returns the signing key named kid, refetching Google's key set when it
// is stale or does not have that kid yet.
func (v *googleVerifier) key(kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if k, ok := v.keys[kid]; ok && v.now().Sub(v.fetched) < googleKeysMaxAge {
		return k, nil
	}
	// At most one fetch per googleKeysMinRefetch, failed ones included: the
	// lock is held across it, and during a Google outage every sign-in would
	// otherwise queue behind a fresh 10s attempt. A failure keeps serving the
	// old set -- Google rotates keys over days, not minutes.
	if v.keys == nil || v.now().Sub(v.attempted) >= googleKeysMinRefetch {
		v.attempted = v.now()
		keys, err := v.fetchKeys()
		switch {
		case err == nil:
			v.keys, v.fetched = keys, v.now()
		case v.keys == nil:
			return nil, fmt.Errorf("fetch Google signing keys: %w", err)
		}
	}
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("id_token signed with unknown key %q", kid)
}

func (v *googleVerifier) fetchKeys() (map[string]*rsa.PublicKey, error) {
	resp, err := v.client.Get(v.certsURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", v.certsURL, resp.Status)
	}
	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return nil, err
	}
	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, errN := base64.RawURLEncoding.DecodeString(k.N)
		e, errE := base64.RawURLEncoding.DecodeString(k.E)
		if errN != nil || errE != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(keys) == 0 {
		return nil, errors.New("no usable RSA keys in the response")
	}
	return keys, nil
}

func decodeJWTPart(part string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
