package jmsd

import (
	"os"
	"path/filepath"
	"testing"
)

const minimalConfig = `core_host: "https://jumpserver.example.com"
access_key_id: "ak"
access_key_secret: "sk"
core_db_dsn: "postgres://user:pass@host:5432/jumpserver?sslmode=disable"
google: {client_id: "c", hosted_domain: "example.com"}
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Absent, config-jmsd.yaml's k8s section must not turn into a required
// field: local k9s mode is optional, existing deployments shouldn't need to
// add anything to keep working.
func TestLoadConfigWithoutK8sSection(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.K8s.ProxyURL != "" || cfg.K8s.InsecureSkipVerify {
		t.Errorf("K8s = %+v, want zero value", cfg.K8s)
	}
}

func TestLoadConfigReadsK8sSection(t *testing.T) {
	body := minimalConfig + "k8s:\n  proxy_url: https://k8s-proxy.example.com:5002\n  insecure_skip_verify: true\n"
	cfg, err := LoadConfig(writeConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.K8s.ProxyURL != "https://k8s-proxy.example.com:5002" || !cfg.K8s.InsecureSkipVerify {
		t.Errorf("K8s = %+v, want the configured proxy", cfg.K8s)
	}
}

// An empty Redis.Addr is what New reads as "keep the in-memory store", so a
// config with no redis section must leave it empty rather than fail -- every
// deployment predating this feature has no such section.
func TestLoadConfigWithoutRedisSection(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Redis.Addr != "" || cfg.Redis.DB != 0 {
		t.Errorf("Redis = %+v, want zero value", cfg.Redis)
	}
}

func TestLoadConfigReadsRedisSection(t *testing.T) {
	body := minimalConfig + "redis:\n  addr: 127.0.0.1:6379\n  db: 3\n"
	cfg, err := LoadConfig(writeConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Redis.Addr != "127.0.0.1:6379" || cfg.Redis.DB != 3 {
		t.Errorf("Redis = %+v, want the configured server", cfg.Redis)
	}
}

// MFAIntervalDays feeds both the freshness comparison and the Redis key's
// TTL, so its default landing at 10 rather than 0 is load-bearing: a zero
// interval would expire every MFA key instantly and re-prompt on every
// request.
func TestLoadConfigDefaultsMFAInterval(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MFAIntervalDays != DefaultMFAIntervalDays {
		t.Errorf("MFAIntervalDays = %d, want %d", cfg.MFAIntervalDays, DefaultMFAIntervalDays)
	}
}
