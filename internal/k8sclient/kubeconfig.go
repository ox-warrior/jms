// Package k8sclient implements the client side of local K9s mode
// (docs/K9S_LOCAL_PROXY_PLAN.md): holding a Koko k8s session open in the
// background, extracting the kubeconfig it writes, and running a local k9s
// against Koko's own :5002 reverse proxy instead of Koko's built-in kubectl
// shell.
package k8sclient

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// rewriteServer replaces every cluster's server address with proxyURL -- the
// office network's protected front door to Koko's :5002, since the original
// "https://127.0.0.1:5002" Koko wrote (koko v4.10.16 utils/init-kubectl.sh +
// pkg/srvconn/conn_k8s_proxy.go) is only reachable from inside Koko's own
// network (docs/K9S_LOCAL_PROXY_PLAN.md §6/§8). It always drops Koko's own
// certificate-authority-data/certificate-authority: that names Koko's
// internal TLS material, which says nothing about the office-network
// proxy's certificate. What replaces insecure-skip-tls-verify depends on
// insecureSkipVerify: true sets it (needed for :5002's own self-signed
// certificate, --k8s-insecure-skip-verify / config.K8sUserConfig), false
// drops it too, so client-go falls back to verifying proxyURL against the
// system trust store -- the safe default once a real front door is in
// place.
//
// Parsed generically (map[string]any) rather than into a typed struct: a
// kubeconfig's clusters/contexts/users entries nest their payload one level
// down under a "cluster"/"context"/"user" key (`clusters[].cluster.server`,
// not a flat `clusters[].server`), and going generic here means every field
// jms doesn't know about round-trips untouched instead of needing its own
// struct tag.
func rewriteServer(raw []byte, proxyURL, name string, insecureSkipVerify bool) ([]byte, error) {
	var cfg map[string]any
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		// Wrapping the parser's message is safe here even though raw carries
		// Koko's bearer token, and the error travels as far as the log file
		// (RunK9s's failure line, then connectResource's "session error").
		// Checked against yaml.v3 rather than assumed: its messages are line
		// numbers and type names, and the one form that does quote the input
		// -- "cannot unmarshal !!str `abcdefg...`" -- truncates at seven
		// characters. The line number is worth far more than that, since the
		// way this fails in practice is shell noise mixed into the payload.
		return nil, fmt.Errorf("parse remote kubeconfig: %w", err)
	}
	clusters, ok := cfg["clusters"].([]any)
	if !ok || len(clusters) == 0 {
		return nil, errors.New("remote kubeconfig has no clusters")
	}
	for _, entry := range clusters {
		named, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		cluster, ok := named["cluster"].(map[string]any)
		if !ok {
			return nil, errors.New("remote kubeconfig cluster entry missing its \"cluster\" map")
		}
		cluster["server"] = proxyURL
		delete(cluster, "certificate-authority-data")
		delete(cluster, "certificate-authority")
		if insecureSkipVerify {
			cluster["insecure-skip-tls-verify"] = true
		} else {
			delete(cluster, "insecure-skip-tls-verify")
		}
		named["name"] = name
	}
	renameContexts(cfg, name)
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("re-encode kubeconfig: %w", err)
	}
	return out, nil
}

// renameContexts points every context at the renamed cluster and gives it the
// same name, then makes it current.
//
// Koko names both the cluster and the context "kubernetes" for every asset --
// hardcoded in init-kubectl.sh's `kubectl config set-cluster kubernetes` /
// `set-context kubernetes` (koko v4.10.16), with nothing in it derived from
// which cluster this actually is. That is worth overwriting for two reasons:
// k9s's header then names the asset instead of every cluster looking alike,
// and k9s keys its per-context state (active view, active namespace) on the
// context name, storing it under clusters/<cluster>/<context>/ -- so left as
// "kubernetes" every asset shares one state file and they overwrite each
// other. The name doubles as a path segment there, which is what sanitizeName
// is for.
func renameContexts(cfg map[string]any, name string) {
	contexts, ok := cfg["contexts"].([]any)
	if !ok {
		return
	}
	for _, entry := range contexts {
		named, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		named["name"] = name
		if inner, ok := named["context"].(map[string]any); ok {
			inner["cluster"] = name
		}
	}
	if len(contexts) > 0 {
		cfg["current-context"] = name
	}
}

// sanitizeName keeps an asset name usable both as a kubeconfig entry name and
// as the directory segment k9s derives from it. Anything that would create a
// path level or an awkward filename becomes a dash; an asset whose name is
// entirely such characters falls back to a fixed name rather than an empty
// one.
func sanitizeName(assetName string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, assetName)
	cleaned = strings.Trim(cleaned, "-.")
	if cleaned == "" {
		return "jumpserver"
	}
	return cleaned
}

// WriteTemp rewrites the remote kubeconfig's cluster address to proxyURL,
// renames its cluster and context after assetName (see renameContexts), and
// writes it to a fresh, private temp directory. The returned cleanup func
// removes it; callers must invoke it on every exit path
// (docs/K9S_LOCAL_PROXY_PLAN.md §8: never left behind, mode 0600, its own
// directory rather than ~/.kube/config).
func WriteTemp(raw []byte, proxyURL, assetName string, insecureSkipVerify bool) (path string, cleanup func(), err error) {
	rewritten, err := rewriteServer(raw, proxyURL, sanitizeName(assetName), insecureSkipVerify)
	if err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp("", "jms-k8s-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temp dir: %w", err)
	}
	path = filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(path, rewritten, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("write kubeconfig: %w", err)
	}
	return path, func() { _ = os.RemoveAll(dir) }, nil
}
