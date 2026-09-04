package jmsd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The three POST bodies embed signedFields rather than repeating fp/ts/nonce/
// sig. encoding/json promotes the exported fields of an embedded unexported
// struct type, so the wire format is unchanged -- but that is exactly the kind
// of assumption that would silently 400 every write if it were wrong.
func TestSignedPostBodiesDecodeFlatJSON(t *testing.T) {
	t.Parallel()
	body := `{"fp":"SHA256:abc","ts":"1700000000","nonce":"n1","sig":"c2ln",
	          "code":"123456","asset":"a1","account":"root","protocol":"ssh","starred":true}`

	for _, tc := range []struct {
		name string
		req  signedBody
	}{
		{"mfa", &mfaRequest{}},
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
	body := `{"fp":"f","ts":"1","nonce":"n","sig":"s","code":"123456","asset":"a1","account":"root","protocol":"ssh","starred":true}`
	newRequest := func() *http.Request {
		return httptest.NewRequest(http.MethodPost, "/v1/x", strings.NewReader(body))
	}

	var mfa mfaRequest
	if decodeSignedPost(httptest.NewRecorder(), newRequest(), &mfa); mfa.Code != "123456" {
		t.Errorf("mfaRequest.Code = %q", mfa.Code)
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
			if decodeSignedPost(w, r, &mfaRequest{}) {
				t.Fatal("expected the request to be refused")
			}
			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tc.wantStatus)
			}
		})
	}
}
