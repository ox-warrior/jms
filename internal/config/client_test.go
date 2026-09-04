package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadClientConfigFileNotExists(t *testing.T) {
	cfg, err := LoadClientConfig(filepath.Join(t.TempDir(), "nonexistent.yaml"))
	if err != nil {
		t.Fatalf("LoadClientConfig: %v", err)
	}
	if cfg.LogLevel != "" || cfg.LogFile != "" {
		t.Errorf("expected zero config, got log_level=%q log_file=%q", cfg.LogLevel, cfg.LogFile)
	}
	if cfg.Server != "" || cfg.JmsdAddr != "" {
		t.Errorf("expected zero config, got server=%q jmsd_addr=%q", cfg.Server, cfg.JmsdAddr)
	}
}

// Both addresses are read from the config file so that a deployment can point
// jms at its own JumpServer and jmsd once, rather than every user passing two
// flags on every run.
func TestLoadClientConfigParsesAddresses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "server: jumpserver.example.com\njmsd_addr: https://jmsd.example.com\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadClientConfig(path)
	if err != nil {
		t.Fatalf("LoadClientConfig: %v", err)
	}
	if cfg.Server != "jumpserver.example.com" {
		t.Errorf("Server = %q, want jumpserver.example.com", cfg.Server)
	}
	if cfg.JmsdAddr != "https://jmsd.example.com" {
		t.Errorf("JmsdAddr = %q, want https://jmsd.example.com", cfg.JmsdAddr)
	}
}

func TestLoadClientConfigParsesFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("log_level: debug\nlog_file: /tmp/jms.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadClientConfig(path)
	if err != nil {
		t.Fatalf("LoadClientConfig: %v", err)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
	if cfg.LogFile != "/tmp/jms.log" {
		t.Errorf("LogFile = %q, want /tmp/jms.log", cfg.LogFile)
	}
}

func TestLoadClientConfigPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("log_level: info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadClientConfig(path)
	if err != nil {
		t.Fatalf("LoadClientConfig: %v", err)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", cfg.LogLevel)
	}
	if cfg.LogFile != "" {
		t.Errorf("LogFile = %q, want empty", cfg.LogFile)
	}
}

func TestDefaultClientConfigPath(t *testing.T) {
	path, err := DefaultClientConfigPath()
	if err != nil {
		t.Skipf("no user config dir: %v", err)
	}
	if filepath.Base(path) != "config.yaml" || filepath.Base(filepath.Dir(path)) != "jms" {
		t.Errorf("DefaultClientConfigPath = %q, want .../jms/config.yaml", path)
	}
}
