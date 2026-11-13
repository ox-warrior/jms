package connectflow

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/leiyupeng/jumpserver-cli/internal/jmsdclient"
)

// selfPath returns a real, non-directory file path for k8sclient.Locate to
// resolve -- the running test binary itself, so this needs no dependency on
// k9s actually being installed on the machine running the test.
func selfPath(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	return path
}

// fakeJmsd stands in for jmsd's read endpoints, real enough that
// jmsdclient.Client's actual HTTP path runs against it. Each handler is
// nil-safe: a nil entry means "this endpoint must not be called", which is
// what the ordering test below relies on.
type fakeJmsd struct {
	assetDetail func(w http.ResponseWriter, r *http.Request)
	k8sConfig   func(w http.ResponseWriter, r *http.Request)
	connections func(w http.ResponseWriter, r *http.Request)
	calls       []string
}

func newFakeJmsd(t *testing.T) (*fakeJmsd, *jmsdclient.Client) {
	t.Helper()
	f := &fakeJmsd{}
	mux := http.NewServeMux()
	record := func(path string, h func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.calls = append(f.calls, path)
			if h == nil {
				t.Errorf("%s was called but no handler was armed for it", path)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/v1/assets/detail", record("/v1/assets/detail", func(w http.ResponseWriter, r *http.Request) { f.assetDetail(w, r) }))
	mux.HandleFunc("/v1/k8s-config", record("/v1/k8s-config", func(w http.ResponseWriter, r *http.Request) { f.k8sConfig(w, r) }))
	mux.HandleFunc("/v1/connections", record("/v1/connections", func(w http.ResponseWriter, r *http.Request) { f.connections(w, r) }))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("build test signer: %v", err)
	}
	return f, jmsdclient.New(server.URL, signer)
}

func jsonOK(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}
}

func TestFetchDetailRejectsEmptyProtocolsOrAccounts(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"no protocols", `{"accounts":[{"alias":"a","name":"n","username":"u"}],"protocols":[]}`, "no compatible protocol for db-1"},
		{"no accounts", `{"accounts":[],"protocols":[{"name":"ssh","port":22}]}`, "no permitted account for db-1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, jc := newFakeJmsd(t)
			f.assetDetail = jsonOK(c.body)
			_, err := FetchDetail(&Gate{JC: jc, Ctx: context.Background()}, "asset-1", "db-1")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("FetchDetail() error = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

func TestFetchDetailSucceeds(t *testing.T) {
	f, jc := newFakeJmsd(t)
	f.assetDetail = jsonOK(`{"accounts":[{"alias":"a","name":"root","username":"root"}],"protocols":[{"name":"ssh","port":22}]}`)
	detail, err := FetchDetail(&Gate{JC: jc, Ctx: context.Background()}, "asset-1", "db-1")
	if err != nil {
		t.Fatalf("FetchDetail() error = %v, want nil", err)
	}
	if len(detail.Accounts) != 1 || detail.Accounts[0].Username != "root" {
		t.Errorf("FetchDetail() accounts = %v, want one account named root", detail.Accounts)
	}
}

func TestFetchDetailWrapsTheUnderlyingError(t *testing.T) {
	f, jc := newFakeJmsd(t)
	f.assetDetail = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"core unreachable"}`))
	}
	_, err := FetchDetail(&Gate{JC: jc, Ctx: context.Background()}, "asset-1", "db-1")
	if err == nil || !strings.HasPrefix(err.Error(), "load asset detail failed: ") || !strings.Contains(err.Error(), "core unreachable") {
		t.Errorf("FetchDetail() error = %v, want it prefixed \"load asset detail failed: \" and to contain the jmsd message", err)
	}
}

func TestMintTicketWrapsTheUnderlyingError(t *testing.T) {
	f, jc := newFakeJmsd(t)
	f.connections = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"core rejected the request"}`))
	}
	_, err := MintTicket(&Gate{JC: jc, Ctx: context.Background()}, "asset-1", "db-1", "root", "ssh", "")
	if err == nil || !strings.HasPrefix(err.Error(), "mint connection ticket failed: ") {
		t.Errorf("MintTicket() error = %v, want it prefixed \"mint connection ticket failed: \"", err)
	}
}

func TestMintTicketSucceeds(t *testing.T) {
	f, jc := newFakeJmsd(t)
	f.connections = jsonOK(`{"id":"ticket-1","value":"secret","expire_time":300,"account_name":"root","protocol":"ssh"}`)
	ticket, err := MintTicket(&Gate{JC: jc, Ctx: context.Background()}, "asset-1", "db-1", "root", "ssh", "")
	if err != nil {
		t.Fatalf("MintTicket() error = %v, want nil", err)
	}
	if ticket.ID != "ticket-1" {
		t.Errorf("MintTicket() ticket.ID = %q, want %q", ticket.ID, "ticket-1")
	}
}

// PrepareK8s's central invariant, called out in its own doc comment: k9s is
// located BEFORE any network call, so a laptop without it installed never
// burns a real ticket or leaves a session in Core's audit trail. This is
// exactly the ordering docs/PLAN.md requires a unit test for.
func TestPrepareK8sLocatesK9sBeforeAnyNetworkCall(t *testing.T) {
	f, jc := newFakeJmsd(t)
	// Deliberately not armed: a call here fails the test via fakeJmsd's own
	// "no handler was armed" check, which is the point.
	f.k8sConfig = nil

	_, err := PrepareK8s(&Gate{JC: jc, Ctx: context.Background()}, "/definitely/not/a/real/k9s/binary")
	if err == nil {
		t.Fatal("PrepareK8s() error = nil, want an error -- the k9s path does not exist")
	}
	if len(f.calls) != 0 {
		t.Errorf("jmsd was called (%v) before k9s was even located", f.calls)
	}
}

func TestPrepareK8sRejectsAnEmptyProxyURL(t *testing.T) {
	// A resolvable "k9s" -- os.Args[0] always exists and is executable enough
	// to satisfy k8sclient.Locate's os.Stat check, without depending on k9s
	// actually being installed on the machine running this test.
	self := selfPath(t)

	f, jc := newFakeJmsd(t)
	f.k8sConfig = jsonOK(`{"proxy_url":"","insecure_skip_verify":false}`)
	_, err := PrepareK8s(&Gate{JC: jc, Ctx: context.Background()}, self)
	if err == nil || !strings.Contains(err.Error(), "no k8s.proxy_url configured") {
		t.Errorf("PrepareK8s() error = %v, want the missing-proxy-url message", err)
	}
}

func TestPrepareK8sSucceeds(t *testing.T) {
	self := selfPath(t)

	f, jc := newFakeJmsd(t)
	f.k8sConfig = jsonOK(`{"proxy_url":"https://127.0.0.1:5002","insecure_skip_verify":true}`)
	prep, err := PrepareK8s(&Gate{JC: jc, Ctx: context.Background()}, self)
	if err != nil {
		t.Fatalf("PrepareK8s() error = %v, want nil", err)
	}
	if prep.K9sPath != self {
		t.Errorf("PrepareK8s() K9sPath = %q, want %q", prep.K9sPath, self)
	}
	if prep.Config.ProxyURL != "https://127.0.0.1:5002" {
		t.Errorf("PrepareK8s() Config.ProxyURL = %q, want the configured value", prep.Config.ProxyURL)
	}
}
