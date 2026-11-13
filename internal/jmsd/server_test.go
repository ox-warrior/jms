package jmsd

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// The three POST bodies embed signedFields rather than repeating fp/ts/nonce/
// sig. encoding/json promotes the exported fields of an embedded unexported
// struct type, so the wire format is unchanged -- but that is exactly the kind
// of assumption that would silently 400 every write if it were wrong.
func TestSignedPostBodiesDecodeFlatJSON(t *testing.T) {
	t.Parallel()
	body := `{"fp":"SHA256:abc","ts":"1700000000","nonce":"n1","sig":"c2ln",
	          "asset":"a1","account":"root","protocol":"ssh","starred":true}`

	for _, tc := range []struct {
		name string
		req  signedBody
	}{
		{"google", &googleSignInRequest{}},
		{"favorite", &favoriteRequest{}},
		{"connection", &connectionRequest{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/v1/x", strings.NewReader(body))
			if !decodeSignedPost(w, r, tc.req) {
				t.Fatalf("decodeSignedPost rejected a complete body: %d %s", w.Code, w.Body)
			}
			got := tc.req.signature()
			want := signedFields{Fingerprint: "SHA256:abc", Ts: "1700000000", Nonce: "n1", Sig: "c2ln"}
			if got != want {
				t.Fatalf("signature() = %+v, want %+v", got, want)
			}
		})
	}
}

func TestSignedPostBodiesKeepTheirOwnFields(t *testing.T) {
	t.Parallel()
	body := `{"fp":"f","ts":"1","nonce":"n","sig":"s","asset":"a1","account":"root","protocol":"ssh","starred":true}`
	newRequest := func() *http.Request {
		return httptest.NewRequest(http.MethodPost, "/v1/x", strings.NewReader(body))
	}

	var fav favoriteRequest
	decodeSignedPost(httptest.NewRecorder(), newRequest(), &fav)
	if fav.Asset != "a1" || !fav.Starred {
		t.Errorf("favoriteRequest = %+v", fav)
	}
	var conn connectionRequest
	decodeSignedPost(httptest.NewRecorder(), newRequest(), &conn)
	if conn.Asset != "a1" || conn.Account != "root" || conn.Protocol != "ssh" {
		t.Errorf("connectionRequest = %+v", conn)
	}
}

func TestDecodeSignedPostRejections(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, method, body string
		wantStatus         int
	}{
		{"GET", http.MethodGet, `{}`, http.StatusMethodNotAllowed},
		{"garbage", http.MethodPost, `not json`, http.StatusBadRequest},
		{"missing sig", http.MethodPost, `{"fp":"f","ts":"1","nonce":"n"}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(tc.method, "/v1/x", strings.NewReader(tc.body))
			if decodeSignedPost(w, r, &googleSignInRequest{}) {
				t.Fatal("expected the request to be refused")
			}
			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tc.wantStatus)
			}
		})
	}
}

// A service key from the config is its user with no sign-in -- the AI agent
// cannot open a browser. Its owner must still be active in JumpServer.
func TestServiceKeyActsAsItsUser(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	service, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	users := []userRecord{{ID: "u1", Username: "sre-triage-agent"}}
	srv := &Server{
		cfg: Config{ServiceKeys: []ServiceKey{{
			User: "sre-triage-agent", PublicKey: string(ssh.MarshalAuthorizedKey(service.PublicKey())),
		}}},
		users: newUserDirectory(func() ([]userRecord, error) { return users, nil }, 0),
		state: newMemStateStore(),
	}
	fp := ssh.FingerprintSHA256(service.PublicKey())
	if u, err := srv.identify(fp); err != nil || u.Username != "sre-triage-agent" {
		t.Fatalf("identify(service key) = %v, %v; want sre-triage-agent", u, err)
	}
	users = nil // disabled in JumpServer
	if _, err := srv.identify(fp); err == nil {
		t.Fatal("service key still works after its user was disabled")
	}
}

// jms's own key: unknown until a Google sign-in binds it, usable after, and dead as soon as its owner is disabled in JumpServer --
// without waiting for the binding to expire.
func TestBoundKeyFollowsItsOwner(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	device, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	fp := ssh.FingerprintSHA256(device.PublicKey())
	rows := []userRecord{{ID: "u1", Username: "alice", Email: "alice@example.com"}}
	srv := &Server{
		cfg:   Config{MFAIntervalDays: 10},
		users: newUserDirectory(func() ([]userRecord, error) { return rows, nil }, 0),
		state: newMemStateStore(),
	}
	n := 0
	authorize := func() (*userRecord, *httptest.ResponseRecorder) {
		n++
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		nonce := fmt.Sprint("n", n)
		sig, err := device.Sign(rand.Reader, []byte(fp+"|"+ts+"|"+nonce))
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		user := srv.authorize(w, httptest.NewRequest(http.MethodGet, "/v1/whoami", nil), signedFields{
			Fingerprint: fp, Ts: ts, Nonce: nonce, Sig: base64.StdEncoding.EncodeToString(ssh.Marshal(sig)),
		})
		return user, w
	}

	if _, w := authorize(); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "mfa_required") {
		t.Fatalf("unbound key: got %d %s, want 401 mfa_required so jms starts the sign-in", w.Code, w.Body)
	}

	// What handleGoogleSignIn leaves behind.
	pub := string(ssh.MarshalAuthorizedKey(device.PublicKey()))
	if err := srv.state.BindKey(fp, keyBinding{UserID: "u1", PublicKey: pub}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if user, w := authorize(); user == nil || user.Username != "alice" {
		t.Fatalf("bound key: got %d %s, want alice", w.Code, w.Body)
	}

	rows = nil // alice disabled in JumpServer
	if user, _ := authorize(); user != nil {
		t.Fatal("bound key still works after its owner was disabled")
	}
}

// on_behalf_of is how the sre-triage gateway, running jms for many people
// under one service key, says who asked. It must be signed (or anyone could
// relabel a captured request) and only a service key may use it (or a person
// could put someone else's name in the audit).
func TestOnBehalfOf(t *testing.T) {
	newKey := func() ssh.Signer {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		s, err := ssh.NewSignerFromKey(priv)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	service, device := newKey(), newKey()
	srv := &Server{
		cfg: Config{ServiceKeys: []ServiceKey{{
			User: "sre-triage-agent", PublicKey: string(ssh.MarshalAuthorizedKey(service.PublicKey())),
		}}},
		users: newUserDirectory(func() ([]userRecord, error) {
			return []userRecord{{ID: "u1", Username: "sre-triage-agent"}, {ID: "u2", Username: "alice"}}, nil
		}, 0),
		state: newMemStateStore(),
	}
	devFP := ssh.FingerprintSHA256(device.PublicKey())
	if err := srv.state.BindKey(devFP, keyBinding{UserID: "u2",
		PublicKey: string(ssh.MarshalAuthorizedKey(device.PublicKey()))}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	n := 0
	call := func(key ssh.Signer, obo string, signObo bool) (*userRecord, int) {
		n++
		fp := ssh.FingerprintSHA256(key.PublicKey())
		ts, nonce := strconv.FormatInt(time.Now().Unix(), 10), fmt.Sprint("n", n)
		payload := fp + "|" + ts + "|" + nonce
		if signObo {
			payload += "|" + OnBehalfPrefix + obo
		}
		sig, err := key.Sign(rand.Reader, []byte(payload))
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		user := srv.authorize(w, httptest.NewRequest(http.MethodGet, "/v1/whoami", nil), signedFields{
			Fingerprint: fp, Ts: ts, Nonce: nonce, Sig: base64.StdEncoding.EncodeToString(ssh.Marshal(sig)),
			OnBehalfOf: obo,
		})
		return user, w.Code
	}

	if u, _ := call(service, "bob@example.com", true); u == nil || u.OnBehalfOf != "bob@example.com" || u.Username != "sre-triage-agent" {
		t.Fatalf("signed on_behalf_of from a service key: got %+v, want sre-triage-agent acting for bob", u)
	}
	if u, code := call(service, "bob@example.com", false); u != nil || code != http.StatusUnauthorized {
		t.Fatalf("unsigned on_behalf_of: got %+v / %d, want refused with 401", u, code)
	}
	if u, code := call(device, "bob@example.com", true); u != nil || code != http.StatusForbidden {
		t.Fatalf("on_behalf_of from a person's key: got %+v / %d, want refused with 403", u, code)
	}
	if u, _ := call(device, "", false); u == nil || u.Username != "alice" || u.OnBehalfOf != "" {
		t.Fatalf("plain request from a person's key: got %+v, want alice", u)
	}
}

// A service key with no user of its own is the gateway's: it can only ever act
// as the person it names, with that person's permissions. Each refusal here is
// a way to become someone else.
func TestServiceKeyActsAsPerson(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		cfg: Config{ServiceKeys: []ServiceKey{{
			PublicKey: string(ssh.MarshalAuthorizedKey(key.PublicKey())),
		}}},
		users: newUserDirectory(func() ([]userRecord, error) {
			return []userRecord{{ID: "u2", Username: "alice", Email: "alice@example.com"}}, nil
		}, 0),
		state: newMemStateStore(),
	}
	n := 0
	call := func(obo, from string) (*userRecord, int) {
		n++
		fp := ssh.FingerprintSHA256(key.PublicKey())
		ts, nonce := strconv.FormatInt(time.Now().Unix(), 10), fmt.Sprint("p", n)
		payload := fp + "|" + ts + "|" + nonce
		if obo != "" {
			payload += "|" + OnBehalfPrefix + obo
		}
		sig, err := key.Sign(rand.Reader, []byte(payload))
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodGet, "/v1/whoami", nil)
		r.RemoteAddr = from
		w := httptest.NewRecorder()
		user := srv.authorize(w, r, signedFields{Fingerprint: fp, Ts: ts, Nonce: nonce,
			Sig: base64.StdEncoding.EncodeToString(ssh.Marshal(sig)), OnBehalfOf: obo})
		return user, w.Code
	}

	u, _ := call("alice@example.com", "10.1.2.3:5000")
	if u == nil || u.ID != "u2" || u.Username != "alice" || !u.Service || u.OnBehalfOf != "alice@example.com" {
		t.Fatalf("acting for alice: got %+v, want alice's own record marked as via a service key", u)
	}
	if u, code := call("", "10.1.2.3:5000"); u != nil || code != http.StatusForbidden {
		t.Fatalf("no on_behalf_of: got %+v / %d, want refused -- the key is nobody by itself", u, code)
	}
	if u, code := call("mallory@example.com", "10.1.2.3:5000"); u != nil || code != http.StatusForbidden {
		t.Fatalf("unknown person: got %+v / %d, want 403", u, code)
	}
}

// The detail cache is for programs only, per user, and forgets after detailTTL.
func TestDetailCache(t *testing.T) {
	now := time.Unix(1700000000, 0)
	c := detailCache{now: func() time.Time { return now }}
	alice := &userRecord{ID: "u2", Service: true}
	want := AssetDetail{Accounts: []AccountOption{{Alias: "root"}}}

	c.put(alice, "a1", want)
	if got, ok := c.get(alice, "a1"); !ok || len(got.Accounts) != 1 {
		t.Fatalf("same user and asset: got %+v / %v, want a hit", got, ok)
	}
	if _, ok := c.get(&userRecord{ID: "u3", Service: true}, "a1"); ok {
		t.Fatal("another user got alice's accounts")
	}
	person := &userRecord{ID: "u2"}
	c.put(person, "a2", want)
	if _, ok := c.get(person, "a1"); ok {
		t.Fatal("a person's own key must always ask Core")
	}
	if _, ok := c.get(alice, "a2"); ok {
		t.Fatal("a person's lookup was cached")
	}
	now = now.Add(detailTTL)
	if _, ok := c.get(alice, "a1"); ok {
		t.Fatal("entry outlived detailTTL")
	}
}
