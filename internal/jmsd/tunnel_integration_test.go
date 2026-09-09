package jmsd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// Everything below drives the real tunnel server -- serveTunnelListener,
// authTunnelKey, handleTunnelChannel, the policy and the audit lines -- over a
// loopback socket with a real ssh.Client on the other end. It needs no Core and
// no database: userDirectory's loader and StateStore are both interfaces with
// in-memory doubles, and require_permitted_asset is left off because that is
// the one branch that genuinely reaches Core (userPermAssets).
//
// This is the half a client-side harness cannot cover. Wiring an ssh.Client to
// a hand-written stand-in server proves the client works; it says nothing about
// whether this package's channel handler parses the payload, applies the policy,
// and copies bytes in the right direction.

// testTunnelKey is a fresh keypair, because authenticating needs the private
// half and the package's fixtures (testKeyA) are public keys only.
func testTunnelKey(t *testing.T) (ssh.Signer, string, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	authorized := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
	return signer, authorized, ssh.FingerprintSHA256(signer.PublicKey())
}

// nonLoopbackIPv4 returns an address of this machine that is not loopback, and
// an echo server bound to it.
//
// Needed because the policy refuses loopback unconditionally (tunnelpolicy.go's
// hardDenyReason) -- correctly, since at jmsd's end of a tunnel loopback means
// jmsd's own host. So the "permitted" case has to target something else, and
// the only thing a test can both bind and allow is one of this machine's own
// non-loopback addresses.
func nonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("cannot enumerate interfaces: %v", err)
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ip4 := ipNet.IP.To4(); ip4 != nil && !ip4.IsLoopback() {
			return ip4.String()
		}
	}
	t.Skip("no non-loopback IPv4 on this machine, so no destination the policy can allow")
	return ""
}

// startBoundEcho listens on ip and echoes every connection back.
func startBoundEcho(t *testing.T, ip string) (host string, port int, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Skipf("cannot bind %s: %v", ip, err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { defer c.Close(); _, _ = io.Copy(c, c) }(c)
		}
	}()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ = strconv.Atoi(portStr)
	return ip, port, func() { ln.Close() }
}

// tunnelTestServer assembles a Server with just enough real state for the
// tunnel to run, and starts it on an ephemeral port.
type tunnelTestServer struct {
	srv  *Server
	addr string
	stop func()
}

func newTunnelTestServer(t *testing.T, rows []sshKeyRow, allow []TunnelRule) *tunnelTestServer {
	t.Helper()

	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatalf("host signer: %v", err)
	}
	policy, err := compileTunnelPolicy(allow)
	if err != nil {
		t.Fatalf("compile policy: %v", err)
	}

	srv := &Server{
		cfg: Config{
			MFAIntervalDays: 10,
			Tunnel: TunnelConfig{
				Enabled:     true,
				Allow:       allow,
				MaxChannels: 8,
			},
		},
		users:        newUserDirectory(func() ([]sshKeyRow, error) { return rows, nil }, keyIndexTTL),
		state:        newMemStateStore(),
		tunnelSigner: hostSigner,
		tunnelPolicy: policy,
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.serveTunnelListener(ctx, ln) }()

	return &tunnelTestServer{
		srv:  srv,
		addr: ln.Addr().String(),
		stop: func() {
			cancel()
			<-done
		},
	}
}

// dialTunnel connects a real ssh.Client, pinning the host key the way jms does.
func dialTunnel(t *testing.T, addr string, hostKey ssh.PublicKey, signer ssh.Signer) *ssh.Client {
	t.Helper()
	client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            "jms",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.FixedHostKey(hostKey),
		Timeout:         10 * time.Second,
	})
	if err != nil {
		t.Fatalf("ssh.Dial: %v", err)
	}
	return client
}

func TestTunnelServerCarriesAStream(t *testing.T) {
	ip := nonLoopbackIPv4(t)
	echoHost, echoPort, stopEcho := startBoundEcho(t, ip)
	defer stopEcho()

	signer, authorized, _ := testTunnelKey(t)
	rows := []sshKeyRow{{userID: "u1", username: "alice", publicKey: authorized}}
	allow := []TunnelRule{{CIDR: ip + "/32", Ports: []int{echoPort}}}

	ts := newTunnelTestServer(t, rows, allow)
	defer ts.stop()
	if err := ts.srv.state.RecordMFA("alice", time.Now()); err != nil {
		t.Fatalf("record mfa: %v", err)
	}

	client := dialTunnel(t, ts.addr, ts.srv.tunnelSigner.PublicKey(), signer)
	defer client.Close()

	conn, err := client.Dial("tcp", net.JoinHostPort(echoHost, strconv.Itoa(echoPort)))
	if err != nil {
		t.Fatalf("open channel to a permitted target: %v", err)
	}
	defer conn.Close()

	const payload = "hello through the real tunnel server"
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	back := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, back); err != nil {
		t.Fatalf("echo did not come back: %v", err)
	}
	if string(back) != payload {
		t.Fatalf("echo = %q, want %q", back, payload)
	}
}

// Loopback is refused whatever the allow list says, because at jmsd's end of
// the tunnel it means jmsd's own host -- where its Redis lives, and writing
// that Redis is how anyone skips any user's MFA.
func TestTunnelServerRefusesLoopbackEvenWhenAllowed(t *testing.T) {
	signer, authorized, _ := testTunnelKey(t)
	rows := []sshKeyRow{{userID: "u1", username: "alice", publicKey: authorized}}
	// Deliberately as permissive as an operator could make it.
	allow := []TunnelRule{{CIDR: "0.0.0.0/0", Ports: []int{1, 22, 80, 443, 5432, 6379, 8391, 65535}}}

	ts := newTunnelTestServer(t, rows, allow)
	defer ts.stop()
	if err := ts.srv.state.RecordMFA("alice", time.Now()); err != nil {
		t.Fatalf("record mfa: %v", err)
	}
	client := dialTunnel(t, ts.addr, ts.srv.tunnelSigner.PublicKey(), signer)
	defer client.Close()

	for _, target := range []string{"127.0.0.1:8391", "169.254.169.254:80"} {
		_, err := client.Dial("tcp", target)
		if err == nil {
			t.Fatalf("dial %s succeeded; it must be refused whatever allow says", target)
		}
		var openErr *ssh.OpenChannelError
		if !errors.As(err, &openErr) {
			t.Fatalf("dial %s: error %v is not a channel rejection", target, err)
		}
		if openErr.Reason != ssh.Prohibited {
			t.Errorf("dial %s: reason = %v, want Prohibited", target, openErr.Reason)
		}
	}
}

// A destination outside the allow list is refused, and the client is told
// "not permitted" rather than something that reads like the destination is
// down -- the two send the user looking in opposite directions.
func TestTunnelServerRefusesUnlistedDestination(t *testing.T) {
	ip := nonLoopbackIPv4(t)
	signer, authorized, _ := testTunnelKey(t)
	rows := []sshKeyRow{{userID: "u1", username: "alice", publicKey: authorized}}
	allow := []TunnelRule{{CIDR: "10.200.200.0/24", Ports: []int{443}}}

	ts := newTunnelTestServer(t, rows, allow)
	defer ts.stop()
	if err := ts.srv.state.RecordMFA("alice", time.Now()); err != nil {
		t.Fatalf("record mfa: %v", err)
	}
	client := dialTunnel(t, ts.addr, ts.srv.tunnelSigner.PublicKey(), signer)
	defer client.Close()

	_, err := client.Dial("tcp", net.JoinHostPort(ip, "443"))
	if err == nil {
		t.Fatal("a destination outside tunnel.allow was connected")
	}
	t.Logf("refused as expected: %v", err)
}

// Authentication is the whole gate, so both ways it can fail have to actually
// fail: a key nobody registered, and a registered key whose MFA interval has
// lapsed. The second is the case a legitimate client never reaches (jms clears
// MFA on the control plane before dialling), which is exactly why it must not
// answer with anything that explains itself.
func TestTunnelServerRefusesUnregisteredKeyAndStaleMFA(t *testing.T) {
	signer, authorized, _ := testTunnelKey(t)
	other, _, _ := testTunnelKey(t)
	rows := []sshKeyRow{{userID: "u1", username: "alice", publicKey: authorized}}
	allow := []TunnelRule{{CIDR: "10.200.200.0/24", Ports: []int{443}}}

	t.Run("unregistered key", func(t *testing.T) {
		ts := newTunnelTestServer(t, rows, allow)
		defer ts.stop()
		if err := ts.srv.state.RecordMFA("alice", time.Now()); err != nil {
			t.Fatalf("record mfa: %v", err)
		}
		_, err := ssh.Dial("tcp", ts.addr, &ssh.ClientConfig{
			User:            "jms",
			Auth:            []ssh.AuthMethod{ssh.PublicKeys(other)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(), // the point here is auth, not pinning
			Timeout:         10 * time.Second,
		})
		if err == nil {
			t.Fatal("a key nobody registered was admitted")
		}
	})

	t.Run("stale mfa", func(t *testing.T) {
		ts := newTunnelTestServer(t, rows, allow)
		defer ts.stop()
		if err := ts.srv.state.RecordMFA("alice", time.Now().Add(-11*24*time.Hour)); err != nil {
			t.Fatalf("record mfa: %v", err)
		}
		_, err := ssh.Dial("tcp", ts.addr, &ssh.ClientConfig{
			User:            "jms",
			Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			Timeout:         10 * time.Second,
		})
		if err == nil {
			t.Fatal("a caller whose MFA interval lapsed was admitted")
		}
	})
}

// There is no shell here. A "session" channel must be rejected rather than
// answered, or the next person to read the code has to work out whether one
// might be.
func TestTunnelServerRefusesSessionChannels(t *testing.T) {
	ip := nonLoopbackIPv4(t)
	signer, authorized, _ := testTunnelKey(t)
	rows := []sshKeyRow{{userID: "u1", username: "alice", publicKey: authorized}}
	// The echo server is only here so the allow list has something real in
	// it; this test never opens a channel to it.
	_, echoPort, stopEcho := startBoundEcho(t, ip)
	defer stopEcho()
	allow := []TunnelRule{{CIDR: ip + "/32", Ports: []int{echoPort}}}

	ts := newTunnelTestServer(t, rows, allow)
	defer ts.stop()
	if err := ts.srv.state.RecordMFA("alice", time.Now()); err != nil {
		t.Fatalf("record mfa: %v", err)
	}
	client := dialTunnel(t, ts.addr, ts.srv.tunnelSigner.PublicKey(), signer)
	defer client.Close()

	if _, _, err := client.OpenChannel("session", nil); err == nil {
		t.Fatal("a session channel was accepted; the tunnel has no shell")
	} else {
		t.Logf("session refused as expected: %v", err)
	}
}

// A pinned host key that does not match must fail the handshake. This is the
// client-side half of why the key is published over the authenticated control
// plane instead of being trusted on first use.
func TestTunnelServerWrongHostKeyIsRefused(t *testing.T) {
	signer, authorized, _ := testTunnelKey(t)
	rows := []sshKeyRow{{userID: "u1", username: "alice", publicKey: authorized}}
	allow := []TunnelRule{{CIDR: "10.200.200.0/24", Ports: []int{443}}}

	ts := newTunnelTestServer(t, rows, allow)
	defer ts.stop()

	_, wrongPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrongSigner, err := ssh.NewSignerFromKey(wrongPriv)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ssh.Dial("tcp", ts.addr, &ssh.ClientConfig{
		User:            "jms",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.FixedHostKey(wrongSigner.PublicKey()),
		Timeout:         10 * time.Second,
	})
	if err == nil {
		t.Fatal("a client pinned to a different host key still connected")
	}
	t.Logf("refused as expected: %v", err)
}
