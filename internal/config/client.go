package config

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// DefaultClientConfigPath is where jms looks for its config file when
// --config is not passed. It sits beside the log and asset cache in the
// user's config directory, which is the standard location for user-specific
// application settings.
func DefaultClientConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "jms", "config.yaml"), nil
}

// ClientConfig holds jms's runtime settings, loaded from a YAML file so
// that log level and other preferences persist across sessions without
// requiring command-line flags every time.
type ClientConfig struct {
	// Server is the JumpServer host to reach Koko's SSH port on. Empty means
	// the compiled-in default.
	//
	// This exists because the built-in default can only ever be right for one
	// deployment, and a wrong one fails in a way that reads like a network
	// outage rather than a misconfiguration ("dial tcp <some IP>: i/o
	// timeout"). Setting it once in the config file beats every user having to
	// remember --server on every invocation.
	Server string `yaml:"server"`
	// JmsdAddr is the base URL of the jmsd control-plane service. Empty means
	// the compiled-in default, which is a localhost address only useful when
	// running jmsd on the same machine for development.
	JmsdAddr string `yaml:"jmsd_addr"`
	// LogLevel controls jms's verbosity. Valid values are "info" (default)
	// and "debug". At info level, only errors and significant events (login,
	// session open/close) are logged. At debug level, every request and
	// phase timing is logged.
	LogLevel string `yaml:"log_level"`
	// UpdateURL is the base URL scripts/release.sh publishes to, which jms
	// checks at launch for a newer build. Empty means the compiled-in default
	// (selfupdate.DefaultBaseURL). Set it to point a laptop at a scratch bucket
	// while rehearsing a release, so the rehearsal does not offer itself to
	// everybody else.
	UpdateURL string `yaml:"update_url"`
	// DisableUpdateCheck turns the launch-time update offer off entirely.
	//
	// Spelled as the negative so that the zero value -- a config file that
	// never mentions it, or no config file at all -- leaves the check on. A
	// positive "update_check: true" would read the same in YAML and mean the
	// opposite on every machine that had not been told about it.
	DisableUpdateCheck bool `yaml:"disable_update_check"`
	// LogFile is the path to jms's log file. Empty means the default
	// location (~/Library/Caches/jms/jms.log on macOS, ~/.cache/jms/jms.log
	// on Linux). Use "-" or "stdout" to write to stdout (not recommended
	// since jms is a TUI and stdout is used for rendering).
	LogFile string `yaml:"log_file"`
}

// LoadClientConfig reads ClientConfig from a YAML file at path. If the file
// does not exist, returns a zero-value ClientConfig (all defaults). Other
// errors (parse failure, permission denied) are returned as-is.
func LoadClientConfig(path string) (ClientConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ClientConfig{}, nil
		}
		return ClientConfig{}, err
	}
	var cfg ClientConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return ClientConfig{}, err
	}
	return cfg, nil
}
