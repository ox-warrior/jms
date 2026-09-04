package k8sclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// remoteKubeconfig is exactly what koko v4.10.16's init-kubectl.sh produces
// via `kubectl config set-*` (see conn_k8s_proxy.go's KUBECTL_TOKEN/
// KUBECTL_CLUSTER/KUBECTL_NAMESPACE env vars): server points at Koko's own
// loopback proxy, and the token is the session's bearer token.
const remoteKubeconfig = `apiVersion: v1
clusters:
- cluster:
    server: https://127.0.0.1:5002
  name: kubernetes
contexts:
- context:
    cluster: kubernetes
    namespace: prod
    user: JumpServer-user
  name: kubernetes
current-context: kubernetes
kind: Config
preferences: {}
users:
- name: JumpServer-user
  user:
    token: session-token-abc123
`

func TestRewriteServerReplacesClusterAddress(t *testing.T) {
	out, err := rewriteServer([]byte(remoteKubeconfig), "https://k8s-proxy.internal.example.com", "cluster-a", false)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if strings.Contains(text, "127.0.0.1:5002") {
		t.Errorf("rewritten config still references Koko's internal address:\n%s", text)
	}
	if !strings.Contains(text, "https://k8s-proxy.internal.example.com") {
		t.Errorf("rewritten config missing the proxy address:\n%s", text)
	}
}

func TestRewriteServerPreservesToken(t *testing.T) {
	out, err := rewriteServer([]byte(remoteKubeconfig), "https://proxy.example.com", "cluster-a", false)
	if err != nil {
		t.Fatal(err)
	}
	// The bearer token is exactly what :5002 checks against its token map --
	// it must survive untouched, only the server address changes.
	if !strings.Contains(string(out), "session-token-abc123") {
		t.Errorf("rewritten config lost the bearer token:\n%s", out)
	}
}

func TestRewriteServerPreservesNamespace(t *testing.T) {
	out, err := rewriteServer([]byte(remoteKubeconfig), "https://proxy.example.com", "cluster-a", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "namespace: prod") {
		t.Errorf("rewritten config lost the namespace:\n%s", out)
	}
}

// Koko names the cluster and context "kubernetes" for every asset alike, so
// the rewrite renames both: k9s shows the name in its header, and keys its
// per-context state on it, so leaving it would have every asset share one
// state file.
func TestRewriteServerRenamesClusterAndContextAfterTheAsset(t *testing.T) {
	out, err := rewriteServer([]byte(remoteKubeconfig), "https://proxy.example.com", "prod-eks-v2", false)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, want := range []string{
		"name: prod-eks-v2",
		"cluster: prod-eks-v2",
		"current-context: prod-eks-v2",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("rewritten config missing %q:\n%s", want, text)
		}
	}
	// The user entry keeps its own name -- only the cluster and context are
	// renamed, and the context must still point at a user that exists.
	if !strings.Contains(text, "name: JumpServer-user") {
		t.Errorf("rewritten config lost the user entry:\n%s", text)
	}
	if strings.Contains(text, "kubernetes") {
		t.Errorf("rewritten config still refers to koko's generic name:\n%s", text)
	}
}

// The name becomes a path segment in k9s's own state directory, so anything
// that would create a level or an awkward filename has to go.
func TestSanitizeName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"prod-eks-v2", "prod-eks-v2"},
		{"prod/eks cluster", "prod-eks-cluster"},
		{"../../etc/passwd", "etc-passwd"},
		{"生产集群", "jumpserver"},
		{"", "jumpserver"},
		{"...", "jumpserver"},
	} {
		if got := sanitizeName(tc.in); got != tc.want {
			t.Errorf("sanitizeName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The doc's own reasoning: Koko's TLS material describes ITS certificate,
// not the office-network proxy's, so it must never survive the rewrite --
// regardless of insecureSkipVerify, which governs only what's set for the
// NEW proxy address.
func withOriginalTLSSettings() string {
	return strings.Replace(remoteKubeconfig,
		"    server: https://127.0.0.1:5002\n",
		"    server: https://127.0.0.1:5002\n    insecure-skip-tls-verify: true\n    certificate-authority-data: bm90LWEtcmVhbC1jYQ==\n",
		1)
}

func TestRewriteServerDropsOriginalCAData(t *testing.T) {
	for _, skipVerify := range []bool{false, true} {
		out, err := rewriteServer([]byte(withOriginalTLSSettings()), "https://proxy.example.com", "cluster-a", skipVerify)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), "certificate-authority") {
			t.Errorf("insecureSkipVerify=%v: rewritten config kept Koko's own CA data:\n%s", skipVerify, out)
		}
	}
}

func TestRewriteServerDropsSkipVerifyWhenNotRequested(t *testing.T) {
	out, err := rewriteServer([]byte(withOriginalTLSSettings()), "https://proxy.example.com", "cluster-a", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "insecure-skip-tls-verify") {
		t.Errorf("rewritten config kept Koko's own insecure-skip-tls-verify setting:\n%s", out)
	}
}

// The concrete scenario this exists for: Koko's :5002 presents a self-signed
// certificate, so pointing straight at it (rather than a proper front door)
// needs --k8s-insecure-skip-verify set for the NEW proxy address too.
func TestRewriteServerSetsSkipVerifyWhenRequested(t *testing.T) {
	out, err := rewriteServer([]byte(remoteKubeconfig), "https://k8s-proxy.example.com:5002", "cluster-a", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "insecure-skip-tls-verify: true") {
		t.Errorf("rewritten config missing insecure-skip-tls-verify: true:\n%s", out)
	}
}

func TestRewriteServerRejectsNoClusters(t *testing.T) {
	_, err := rewriteServer([]byte("apiVersion: v1\nkind: Config\n"), "https://proxy.example.com", "cluster-a", false)
	if err == nil {
		t.Error("expected an error for a kubeconfig with no clusters")
	}
}

func TestRewriteServerRejectsGarbage(t *testing.T) {
	_, err := rewriteServer([]byte("{{{ not yaml"), "https://proxy.example.com", "cluster-a", false)
	if err == nil {
		t.Error("expected an error for unparsable input")
	}
}

func TestWriteTempIsPrivateAndCleansUp(t *testing.T) {
	path, cleanup, err := WriteTemp([]byte(remoteKubeconfig), "https://proxy.example.com", "cluster-a", false)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("kubeconfig file mode = %o, want 600", perm)
	}
	// Never ~/.kube/config: that would clobber the user's own config.
	if filepath.Base(filepath.Dir(path)) == ".kube" {
		t.Errorf("kubeconfig was written under a .kube directory: %s", path)
	}

	dir := filepath.Dir(path)
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("cleanup did not remove %s", dir)
	}
}

func TestWriteTempPropagatesRewriteError(t *testing.T) {
	if _, _, err := WriteTemp([]byte("not a kubeconfig"), "https://proxy.example.com", "cluster-a", false); err == nil {
		t.Error("expected an error for an unparsable kubeconfig")
	}
}
