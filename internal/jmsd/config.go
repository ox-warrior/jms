package jmsd

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// DefaultConfigPath is used when no --config flag or JMSD_CONFIG env var is
// set.
const DefaultConfigPath = "/etc/jmsd/config.yaml"

// JumpServerBaseline is the JumpServer release JMSD was verified against.
//
// This is jmsd's OWN declaration and is deliberately a separate constant from
// the client's (internal/version.JumpServerBaseline), not a shared one. A
// single shared constant could never disagree with itself, so there would be
// nothing to report; two independent declarations can, and jmsd hands its copy
// to the client over /v1/whoami for exactly that comparison.
//
// Keep it in step with the version cited throughout this package's comments
// (apps/perms/... line numbers, koko's init-kubectl.sh behaviour) -- those
// citations are only meaningful against a known release.
const JumpServerBaseline = "v4.10.16"

// DefaultMFAIntervalDays is how often a user must re-verify MFA when
// mfa_interval_days is left unset.
const DefaultMFAIntervalDays = 10

// Config holds jmsd's own runtime settings, loaded from a YAML file rather
// than argv so its secrets never show up in `ps`. The file should be owned by
// whichever user runs jmsd and mode 0600 -- it holds the Access Key, database
// DSN, and Core's SECRET_KEY in plaintext. SECRET_KEY in particular is
// JumpServer's master key for every EncryptField-protected column in the
// database, not just the ones jmsd reads -- keep the DB account read-only and
// this file locked down (see docs/PLAN.md §7.2).
type Config struct {
	// Listen is the local address jmsd's HTTP API binds to, e.g. ":8391".
	Listen string `yaml:"listen"`
	// CoreHost is the JumpServer Core base URL, e.g. "https://jumpserver.example.com".
	CoreHost string `yaml:"core_host"`
	// AccessKeyID/AccessKeySecret belong to jmsd's own JumpServer account
	// (see docs/PLAN.md §7.1), scoped down via jmsd-admin setup-role to the
	// minimum permission set, not the default system_component role.
	AccessKeyID     string `yaml:"access_key_id"`
	AccessKeySecret string `yaml:"access_key_secret"`
	// CoreDBDSN points at Core's own PostgreSQL database, which jmsd reads
	// (never writes) to verify a caller's SSH signature and TOTP code -- see
	// docs/PLAN.md §7.2. Format:
	// "postgres://user:pass@host:5432/jumpserver?sslmode=disable".
	CoreDBDSN string `yaml:"core_db_dsn"`
	// InventoryDBDSN points at the MySQL holding cloud-sync's
	// cloud_instance_info table, which jmsd reads (never writes) to answer
	// "describe". Optional: left empty, describe reports that it is not
	// configured and everything else works as before.
	//
	// Must carry parseTime=true&loc=UTC -- the DATETIME columns hold UTC with
	// no zone of their own, and without loc every timestamp shown would be
	// off by the server's offset. Format:
	// "user:pass@tcp(host:3306)/cloud_meta?parseTime=true&loc=UTC".
	InventoryDBDSN string `yaml:"inventory_db_dsn"`
	// SecretKey is Core's Django SECRET_KEY, copied verbatim from Core's own
	// config.txt. It decrypts the EncryptField columns jmsd reads.
	SecretKey string `yaml:"secret_key"`
	// MFAIntervalDays is how often a user must re-verify MFA. Defaults to
	// DefaultMFAIntervalDays when zero.
	MFAIntervalDays int `yaml:"mfa_interval_days"`
	// K8s is the office network's front door to Koko's :5002 k8s proxy.
	// Every k8s connection requires it because k9s always runs locally.
	K8s K8sProxyConfig `yaml:"k8s"`
	// LogLevel is "info" (the default) or "debug". info logs errors, writes
	// and a handful of significant events; debug adds a line per database
	// query and per Core API call, with timings -- several per request, which
	// is why it is not the default.
	LogLevel string `yaml:"log_level"`
	// Redis, when addr is set, moves jmsd's auth state out of process memory
	// so a restart no longer resets every user's MFA clock. Left unset, jmsd
	// keeps the in-memory store and its restart-loses-everything behaviour.
	Redis RedisConfig `yaml:"redis"`
}

type K8sProxyConfig struct {
	ProxyURL           string `yaml:"proxy_url"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
}

// RedisConfig points jmsd at a Redis holding its auth state. There is
// deliberately no password or TLS field: this is meant for a loopback-bound,
// unauthenticated local Redis.
//
// That choice has a security consequence worth naming. The values stored are
// not secrets -- a username, a unix timestamp, and short-lived random nonces
// -- so nothing leaks by reading them. Writing is the exposure: anything that
// can reach this Redis can create a jmsd:mfa:<username> key and thereby skip
// that user's MFA gate entirely. Bind Redis to 127.0.0.1.
type RedisConfig struct {
	// Addr is host:port, e.g. "127.0.0.1:6379". Empty means "use the
	// in-memory store", which is the default.
	Addr string `yaml:"addr"`
	// DB is the numbered Redis database, 0 unless the same server is shared
	// with something else.
	DB int `yaml:"db"`
}

// DebugEnabled reports whether jmsd should log every database query and Core
// API call with its timing. A method rather than the comparison written out at
// each site, because cmd/jmsd and jmsd.New both ask.
func (c Config) DebugEnabled() bool { return c.LogLevel == "debug" }

// LoadConfig reads and validates Config from a YAML file at path. An empty
// path falls back to DefaultConfigPath.
func LoadConfig(path string) (Config, error) {
	if path == "" {
		path = DefaultConfigPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}

	for _, required := range []struct{ name, value string }{
		{"core_host", cfg.CoreHost},
		{"access_key_id", cfg.AccessKeyID},
		{"access_key_secret", cfg.AccessKeySecret},
		{"core_db_dsn", cfg.CoreDBDSN},
		{"secret_key", cfg.SecretKey},
	} {
		if required.value == "" {
			return cfg, fmt.Errorf("%s: %s is required", path, required.name)
		}
	}

	if cfg.Listen == "" {
		cfg.Listen = ":8391"
	}
	if cfg.MFAIntervalDays == 0 {
		cfg.MFAIntervalDays = DefaultMFAIntervalDays
	}
	return cfg, nil
}
