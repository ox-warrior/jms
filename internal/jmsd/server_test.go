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
