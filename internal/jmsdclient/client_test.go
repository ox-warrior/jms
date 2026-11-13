package jmsdclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
)

// The on-behalf-of name must travel with the request and be inside the
// signature, in the exact form jmsd's authorize rebuilds; without it, the
// signed payload must be what it always was, so older jmsd keep verifying.
func TestOnBehalfOfIsSent(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	var got, payload string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		got = q.Get("on_behalf_of")
		payload = strings.Join([]string{q.Get("fp"), q.Get("ts"), q.Get("nonce")}, "|")
		if got != "" {
			payload += "|" + jmsd.OnBehalfPrefix + got
		}
		raw, _ := base64.StdEncoding.DecodeString(q.Get("sig"))
		var sig ssh.Signature
		if ssh.Unmarshal(raw, &sig) != nil || signer.PublicKey().Verify([]byte(payload), &sig) != nil {
			http.Error(w, `{"error":"bad signature"}`, http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"username":"sre-triage-agent"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, signer)
	if _, err := c.Whoami(context.Background()); err != nil || got != "" {
		t.Fatalf("without OnBehalfOf: err=%v, on_behalf_of=%q; want a plain, verifying request", err, got)
	}
	c.OnBehalfOf = "bob@example.com"
	if _, err := c.Whoami(context.Background()); err != nil || got != "bob@example.com" {
		t.Fatalf("with OnBehalfOf: err=%v, on_behalf_of=%q; want it sent and signed", err, got)
	}
}
