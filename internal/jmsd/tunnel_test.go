package jmsd

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// The advertised address is what clients dial. Deriving it wrong is not a
// visible configuration error -- it is every user's tunnel failing to connect
// to an address that was never listening, while jmsd itself looks healthy.
func TestDeriveTunnelAddr(t *testing.T) {
	for _, tc := range []struct{ name, requestHost, listen, want string }{
		// The common deployment: one host, both planes on it, control plane
		// reached with a port in the Host header (which net/http includes).
		{"host with port", "jmsd.example.com:8391", ":8392", "jmsd.example.com:8392"},
		{"host without port", "jmsd.example.com", ":8392", "jmsd.example.com:8392"},
		// A bind address is not something a client can dial, so only the port
		// may come from it -- never the host.
		{"listen bound to an interface", "jmsd.example.com:8391", "10.0.0.1:8392", "jmsd.example.com:8392"},
		{"ip request host", "10.0.0.9:8391", ":8392", "10.0.0.9:8392"},
		{"ipv6 listen", "jmsd.example.com", "[::]:8392", "jmsd.example.com:8392"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveTunnelAddr(tc.requestHost, tc.listen); got != tc.want {
				t.Fatalf("deriveTunnelAddr(%q, %q) = %q, want %q", tc.requestHost, tc.listen, got, tc.want)
			}
		})
	}
}

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

// authTunnelKey is the tunnel's entire authentication step, so its decision
// table is the thing to nail down: which callers it admits, and that a lapsed
// MFA interval -- the case a legitimate client should never hit, because jms
// clears MFA on the control plane first -- is refused here rather than
// answered with anything that says why.
//
// It needs no Core and no database: the userDirectory's loader and the state
// store are both interfaces with in-memory test doubles.
func newTunnelAuthTestServer(t *testing.T, rows []sshKeyRow, mfaIntervalDays int) *Server {
	t.Helper()
	return &Server{
		cfg:   Config{MFAIntervalDays: mfaIntervalDays},
		users: newUserDirectory(func() ([]sshKeyRow, error) { return rows, nil }, keyIndexTTL),
		state: newMemStateStore(),
	}
}

func mustPublicKey(t *testing.T, authorized string) ssh.PublicKey {
	t.Helper()
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorized))
	if err != nil {
		t.Fatalf("parse test key: %v", err)
	}
	return key
}

// connMeta is the minimal ssh.ConnMetadata authTunnelKey reads, which is only
// RemoteAddr for the log line.
type connMeta struct{ remote string }

func (c connMeta) User() string          { return "jms" }
func (c connMeta) SessionID() []byte     { return nil }
func (c connMeta) ClientVersion() []byte { return nil }
func (c connMeta) ServerVersion() []byte { return nil }
func (c connMeta) RemoteAddr() net.Addr  { return &net.TCPAddr{IP: net.ParseIP(c.remote)} }
func (c connMeta) LocalAddr() net.Addr   { return nil }

func TestAuthTunnelKey(t *testing.T) {
	rows := []sshKeyRow{{userID: "u1", username: "alice", publicKey: testKeyA}}

	t.Run("mfa never recorded is refused", func(t *testing.T) {
		srv := newTunnelAuthTestServer(t, rows, 10)
		if _, err := srv.authTunnelKey(connMeta{"10.0.0.5"}, mustPublicKey(t, testKeyA)); err == nil {
			t.Fatal("a caller who has never verified MFA was admitted")
		}
	})

	t.Run("fresh mfa is admitted with the caller's identity", func(t *testing.T) {
		srv := newTunnelAuthTestServer(t, rows, 10)
		if err := srv.state.RecordMFA("alice", time.Now()); err != nil {
			t.Fatalf("record mfa: %v", err)
		}
		perms, err := srv.authTunnelKey(connMeta{"10.0.0.5"}, mustPublicKey(t, testKeyA))
		if err != nil {
			t.Fatalf("authTunnelKey: %v", err)
		}
		// Everything downstream -- the audit lines, the permitted-asset
		// lookup, the revocation recheck -- reads its identity from these
		// extensions, so a wrong or missing one is an unattributed stream.
		if got := perms.Extensions["username"]; got != "alice" {
			t.Errorf("username = %q, want alice", got)
		}
		if got := perms.Extensions["user-id"]; got != "u1" {
			t.Errorf("user-id = %q, want u1", got)
		}
		if got := perms.Extensions["fingerprint"]; got != testKeyAFP {
			t.Errorf("fingerprint = %q, want %q", got, testKeyAFP)
		}
	})

	t.Run("stale mfa is refused", func(t *testing.T) {
		srv := newTunnelAuthTestServer(t, rows, 10)
		if err := srv.state.RecordMFA("alice", time.Now().Add(-11*24*time.Hour)); err != nil {
			t.Fatalf("record mfa: %v", err)
		}
		if _, err := srv.authTunnelKey(connMeta{"10.0.0.5"}, mustPublicKey(t, testKeyA)); err == nil {
			t.Fatal("a caller whose MFA interval lapsed was admitted")
		}
	})

	t.Run("unregistered key is refused", func(t *testing.T) {
		srv := newTunnelAuthTestServer(t, rows, 10)
		if err := srv.state.RecordMFA("bob", time.Now()); err != nil {
			t.Fatalf("record mfa: %v", err)
		}
		// MFA recorded for a DIFFERENT user must not help this key through:
		// identity comes from the key, never from anything the caller asserts.
		if _, err := srv.authTunnelKey(connMeta{"10.0.0.5"}, mustPublicKey(t, testKeyB)); err == nil {
			t.Fatal("a key nobody registered was admitted")
		}
	})
}

// ssh.Unmarshal refuses a payload with bytes left over after the last
// declared field, so tunnelChannelData has to spell out the entire wire
// layout -- including the two origin fields nothing reads. Dropping them
// "to tidy up" fails every channel open, which is every stream the tunnel
// will ever carry, and the client only sees "destination not permitted".
func TestTunnelChannelDataParsesWhatTheClientSends(t *testing.T) {
	// Exactly what x/crypto/ssh's Client.Dial marshals for a direct-tcpip
	// channel open (tcpip.go's channelOpenDirectMsg, RFC 4254 §7.2).
	payload := ssh.Marshal(struct {
		RAddr string
		RPort uint32
		LAddr string
		LPort uint32
	}{"db.internal", 5432, "10.0.0.5", 51234})

	var got tunnelChannelData
	if err := ssh.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal a real direct-tcpip payload: %v "+
			"(tunnelChannelData must declare the whole RFC 4254 §7.2 layout)", err)
	}
	if got.DestAddr != "db.internal" || got.DestPort != 5432 {
		t.Errorf("parsed %s:%d, want db.internal:5432", got.DestAddr, got.DestPort)
	}
	// The origin fields are deliberately not read: doing so would make a
	// dropped field fail to COMPILE here, hiding the diagnostic that
	// matters -- without them the payload has bytes left over and every
	// channel open is rejected at runtime with "bad direct-tcpip payload".
	// The Unmarshal above succeeding is the whole assertion.
}
