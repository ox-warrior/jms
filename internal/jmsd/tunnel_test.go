package jmsd

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

// Clients pin the tunnel's host key, so it must survive a restart. A key that
// regenerated on every start would make every client refuse to connect --
// correctly, and the obvious fix is the insecure one (InsecureIgnoreHostKey).
func TestTunnelHostKeyIsStableAcrossLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tunnel_host_key")

	first, err := loadOrCreateTunnelHostKey(path)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat host key: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("host key mode = %o, want 0600", mode)
	}

	second, err := loadOrCreateTunnelHostKey(path)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if a, b := ssh.MarshalAuthorizedKey(first.PublicKey()), ssh.MarshalAuthorizedKey(second.PublicKey()); string(a) != string(b) {
		t.Errorf("host key changed across restarts:\n%s\n%s", a, b)
	}
}

// Turning the tunnel on without an allow list must fail the start. The tunnel
// is the one jmsd feature Core records nothing about, so "enabled with no
// rules" reading as "every destination" would be the quiet way to hand raw
// network reachability to anyone who can authenticate.
func TestLoadConfigRefusesUnboundedTunnel(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "host_key")
	if _, err := loadOrCreateTunnelHostKey(keyPath); err != nil {
		t.Fatalf("host key: %v", err)
	}

	base := func(tunnel string) string {
		path := filepath.Join(dir, "cfg.yaml")
		body := `core_host: "https://core.example.com"
access_key_id: "id"
access_key_secret: "secret"
core_db_dsn: "postgres://u:p@h:5432/db?sslmode=disable"
secret_key: "0123456789012345678901234567890123456789012345678901234567890123"
` + tunnel
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		return path
	}

	for _, tc := range []struct {
		name    string
		tunnel  string
		wantErr bool
	}{
		{
			name:   "disabled needs nothing",
			tunnel: "tunnel:\n  enabled: false\n",
		},
		{
			name: "enabled with no allow list",
			tunnel: `tunnel:
  enabled: true
  host_key: "` + keyPath + `"
`,
			wantErr: true,
		},
		{
			name: "enabled with no host key",
			tunnel: `tunnel:
  enabled: true
  allow:
    - {cidr: "10.20.0.0/16", ports: [443]}
`,
			wantErr: true,
		},
		{
			name: "enabled with a typo'd cidr",
			tunnel: `tunnel:
  enabled: true
  host_key: "` + keyPath + `"
  allow:
    - {cidr: "10.20.0.0/33", ports: [443]}
`,
			wantErr: true,
		},
		{
			name: "valid",
			tunnel: `tunnel:
  enabled: true
  host_key: "` + keyPath + `"
  allow:
    - {cidr: "10.20.0.0/16", ports: [443]}
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadConfig(base(tc.tunnel))
			if (err != nil) != tc.wantErr {
				t.Fatalf("LoadConfig error = %v, want error = %t", err, tc.wantErr)
			}
			if err != nil || !cfg.Tunnel.Enabled {
				return
			}
			// The two defaults are what a minimal-but-valid config relies on.
			if cfg.Tunnel.Listen != DefaultTunnelListen {
				t.Errorf("Listen = %q, want %q", cfg.Tunnel.Listen, DefaultTunnelListen)
			}
			if cfg.Tunnel.MaxChannels != DefaultTunnelMaxChannels {
				t.Errorf("MaxChannels = %d, want %d", cfg.Tunnel.MaxChannels, DefaultTunnelMaxChannels)
			}
		})
	}
}
