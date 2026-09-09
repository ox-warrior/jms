// jmsd's tunnel: a second listener, speaking SSH, whose only job is to open
// direct-tcpip channels from jmsd's own host to destinations the caller is
// allowed to reach (README "隧道"). It exists because a laptop cannot route
// into the datacenter and Koko only forwards from an asset it has already
// logged into, while jmsd already sits where the traffic needs to originate.
//
// Why SSH rather than a hand-rolled framing over the control plane's port:
//
//   - The authentication is the protocol's, not ours. A client proves it holds
//     the private key through the SSH handshake, so there is no timestamp
//     window to get right and no nonce to remember -- replay is the key
//     exchange's problem. The identity it proves is the SAME one the control
//     plane uses: a fingerprint resolved against userDirectory (keycache.go),
//     the index built from JumpServer's own registered keys.
//   - Multiplexing comes free. One connection carries a channel per stream,
//     so a browser opening thirty of them handshakes once.
//   - The client already links golang.org/x/crypto/ssh to reach Koko, and the
//     server side of it is a mature implementation -- unlike whatever we
//     would write to hijack an HTTP connection, which is also the thing a TLS
//     proxy in front of the control plane would break.
//
// What this deliberately does NOT do: proxy anything itself. No SOCKS5 server,
// no HTTP proxy, no unix sockets. A channel is a destination and two io.Copys;
// SOCKS5 belongs on the client's loopback, where the browser can find it.
//
// The audit consequence has to be stated plainly: Core records nothing about a
// tunnel stream. There is no ticket, no session, no command log -- the caller
// authenticates to the destination with their own credentials, which is the
// point of a tunnel and also why the log lines below are the only record that
// a given stream ever existed. Everything that bounds the damage lives in
// tunnelpolicy.go and in the two re-checks in serveTunnelConn.
package jmsd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	// tunnelChannelType is the only channel type accepted. "session" is
	// rejected on purpose: there is no shell here, and answering one would
	// invite the assumption that there might be.
	tunnelChannelType = "direct-tcpip"

	// tunnelRecheckInterval is how often a live connection re-verifies that
	// its caller is still allowed to hold it. It matches keyIndexTTL, which
	// bounds the other half of the same window: a key deactivated in
	// JumpServer leaves the index within keyIndexTTL, and the connection
	// holding it dies within one tick after that.
	tunnelRecheckInterval = 30 * time.Second

	// tunnelHandshakeTimeout bounds how long an unauthenticated peer may hold
	// a connection. Without it, opening sockets and never speaking is enough
	// to exhaust jmsd's descriptors.
	tunnelHandshakeTimeout = 15 * time.Second

	tunnelDialTimeout = 10 * time.Second

	// TunnelMFARequiredReason is the channel-rejection message a client may
	// act on by prompting for a TOTP code and reconnecting. It is a fixed
	// string because the client branches on it (internal/tunnel): a lapsed MFA
	// interval is recoverable, while a revoked key or a refused destination
	// is not, and re-prompting for the latter would loop forever.
	//
	// Note the asymmetry with the HTTP path, which answers 401 with
	// {"mfa_required": true}: there, MFA lapsing mid-session is normal, and
	// the client is expected to recover. Here it is nearly unreachable --
	// jms clears MFA on the control plane immediately before dialling -- so
	// the string exists for the long-lived connection whose interval expires
	// underneath it, not for the connect path.
	//
	// Exported because internal/tunnel branches on it and a literal copied
	// into the client would be a protocol agreed by coincidence.
	TunnelMFARequiredReason = "mfa_required"
)

// pipeStream copies between an accepted channel and the dialled connection
// until both directions are done, and reports the byte counts for the audit
// line (up = client to destination).
//
// Each direction is half-closed as it finishes, because a client that has
// stopped reading may still be sending and tearing the channel down on the
// first EOF truncates requests that were mid-flight.
//
// Waiting for both directions is what the half-close costs, and getting that
// wait wrong is how goroutines leak. This first used a WaitGroup, which
// deadlocked on exactly the path this design exists for: revocation closes the
// ssh.ServerConn, the channel-side Copy returns immediately, and the goroutine
// blocked reading an idle destination -- a held-open database connection, say
// -- never wakes, so the Wait never returns and dst is never closed. The
// connection was revoked and the socket outlived it anyway.
//
// Closing both ends once either direction finishes fixes it: that unblocks the
// other Read, so both Copys always return.
func pipeStream(ch ssh.Channel, dst net.Conn) (up, down int64) {
	done := make(chan struct{})
	go func() {
		down, _ = io.Copy(ch, dst)
		_ = ch.CloseWrite()
		close(done)
	}()
	up, _ = io.Copy(dst, ch)
	if tcp, ok := dst.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
	// Closing both ends before waiting is the whole point: the goroutine may
	// be blocked on a Read that will never see an EOF (an idle destination,
	// or a connection revoked out from under it), and only a close wakes it.
	// Waiting first is what deadlocked.
	_ = ch.Close()
	_ = dst.Close()
	<-done
	return up, down
}

// tunnelChannelData is direct-tcpip's channel payload (RFC 4254 §7.2).
//
// OriginAddr and OriginPort are never read, and dropping them looks like
// tidying up -- but ssh.Unmarshal rejects a payload with bytes left over
// after the last declared field, so a two-field struct fails to parse EVERY
// channel open and the tunnel refuses all traffic with "bad direct-tcpip
// payload". Declaring the whole wire layout is what makes the parse succeed.
type tunnelChannelData struct {
	DestAddr   string
	DestPort   uint32
	OriginAddr string
	OriginPort uint32
}

// tunnelSession is one authenticated connection's state. It carries the
// caller's identity from the handshake to each channel so a channel handler
// never has to re-derive who it is serving, and it holds the permitted-asset
// set that require_permitted_asset checks against.
type tunnelSession struct {
	fingerprint string
	username    string
	userID      string
	conn        *ssh.ServerConn
	remote      string

	// channels is the CURRENT number of open streams, which max_channels caps
	// and which returns to zero as they close. streamsOpened is the running
	// total, which is what the close line reports -- "carried 83 streams" and
	// "had 8 open at once" answer different questions, and the cap needs the
	// second one.
	channels      atomic.Int64
	streamsOpened atomic.Int64

	// mu guards addrs, which the recheck ticker replaces wholesale while
	// channel handlers read it.
	mu    sync.RWMutex
	addrs map[string]bool
}

func (t *tunnelSession) permits(ip net.IP) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.addrs[ip.String()]
}

func (t *tunnelSession) setAddrs(addrs map[string]bool) {
	t.mu.Lock()
	t.addrs = addrs
	t.mu.Unlock()
}

// loadOrCreateTunnelHostKey reads the tunnel's host key, generating an ed25519
// key on first start.
//
// Persisted rather than generated per run because clients pin it: the
// fingerprint travels to them over the already-authenticated control plane
// (/v1/tunnel-config), which is what removes the trust-on-first-use step and
// with it the InsecureIgnoreHostKey that every hand-rolled SSH tunnel seems to
// end up with. A key that changed on restart would make every client refuse to
// connect, correctly, and the obvious "fix" is the insecure one.
func loadOrCreateTunnelHostKey(path string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			return nil, fmt.Errorf("parse tunnel host key %s: %w", path, err)
		}
		return signer, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read tunnel host key %s: %w", path, err)
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate tunnel host key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, fmt.Errorf("marshal tunnel host key: %w", err)
	}
	encoded := pem.EncodeToMemory(block)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create tunnel host key dir: %w", err)
	}
	// 0600 and O_EXCL: the file is a private key, and two jmsd processes
	// starting together must not race into writing different keys, one of
	// which would then be pinned by clients that never see it again.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create tunnel host key %s: %w", path, err)
	}
	if _, err := f.Write(encoded); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("write tunnel host key %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("write tunnel host key %s: %w", path, err)
	}
	log.Printf("jmsd: generated tunnel host key %s", path)
	// From the key in hand, not by parsing back what was just written.
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, fmt.Errorf("sign with generated tunnel host key: %w", err)
	}
	return signer, nil
}

// ServeTunnel runs the tunnel listener until ctx is cancelled. Returns nil on
// a clean shutdown so cmd/jmsd can treat cancellation as success.
func (s *Server) ServeTunnel(ctx context.Context) error {
	if s.tunnelSigner == nil || s.tunnelPolicy == nil {
		return errors.New("tunnel not configured")
	}
	ln, err := net.Listen("tcp", s.cfg.Tunnel.Listen)
	if err != nil {
		return err
	}
	return s.serveTunnelListener(ctx, ln)
}

// serveTunnelListener is the accept loop, split out from ServeTunnel so a test
// can hand it a listener on an ephemeral port instead of the configured one.
func (s *Server) serveTunnelListener(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	log.Printf("jmsd: tunnel listening on %s (host key %s, %d allow rules, require_permitted_asset=%t)",
		ln.Addr(), ssh.FingerprintSHA256(s.tunnelSigner.PublicKey()),
		len(s.cfg.Tunnel.Allow), s.cfg.Tunnel.RequirePermittedAsset)

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// One failed accept is not worth taking the listener down; a
			// permanently broken one shows up as a flood of these.
			log.Printf("jmsd: tunnel accept: %v", err)
			continue
		}
		go s.serveTunnelConn(ctx, conn)
	}
}

// tunnelServerConfig is rebuilt per connection because ssh.ServerConfig has no
// per-connection state of its own and the callback needs somewhere to record
// which key authenticated -- ssh.Permissions.Extensions, read back below.
func (s *Server) tunnelServerConfig() *ssh.ServerConfig {
	cfg := &ssh.ServerConfig{
		ServerVersion:     "SSH-2.0-jmsd",
		PublicKeyCallback: s.authTunnelKey,
	}
	cfg.AddHostKey(s.tunnelSigner)
	return cfg
}

// authTunnelKey is the tunnel's whole authentication step. The SSH username is
// ignored: identity comes from the key, exactly as it does on the control
// plane, so there is nothing for a caller to claim.
//
// The MFA check here is a backstop, not the user-facing gate. jms clears MFA
// against the control plane (/v1/tunnel-config runs through authorize) moments
// before it dials, so a legitimate client never trips this. Whoever does trip
// it is dialling the data plane directly with a stolen key, and for them a
// bare "unable to authenticate" -- which is all SSH will report -- is the
// right answer: it distinguishes nothing about why.
func (s *Server) authTunnelKey(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	fingerprint := ssh.FingerprintSHA256(key)
	user, err := s.users.Lookup(fingerprint)
	if err != nil {
		log.Printf("jmsd: tunnel auth refused from %s: %v", conn.RemoteAddr(), err)
		return nil, fmt.Errorf("permission denied")
	}
	fresh, err := s.mfaFresh(user.Username)
	if err != nil {
		// A store we cannot read is not a caller we can admit: unlike the HTTP
		// path, which answers 503 and lets the client retry, there is no way to
		// say "try again later" inside an SSH handshake.
		log.Printf("jmsd: tunnel auth deferred for %s: %v", user.Username, err)
		return nil, fmt.Errorf("permission denied")
	}
	if !fresh {
		log.Printf("jmsd: tunnel auth refused for %s: mfa stale", user.Username)
		return nil, fmt.Errorf("permission denied")
	}
	return &ssh.Permissions{Extensions: map[string]string{
		"fingerprint": fingerprint,
		"username":    user.Username,
		"user-id":     user.ID,
	}}, nil
}

func (s *Server) serveTunnelConn(ctx context.Context, nConn net.Conn) {
	defer nConn.Close()

	// The deadline covers the handshake only and is cleared right after: a
	// tunnel stream may legitimately sit idle for hours, and inheriting a
	// handshake deadline into it is the bug that makes long-lived forwards
	// die at a fixed interval for no visible reason.
	_ = nConn.SetDeadline(time.Now().Add(tunnelHandshakeTimeout))
	sshConn, chans, reqs, err := ssh.NewServerConn(nConn, s.tunnelServerConfig())
	if err != nil {
		// Logged at debug: port scanners and health checks produce these
		// constantly, and a refused authentication is already logged, with a
		// reason, by authTunnelKey.
		debugf("tunnel handshake from %s failed: %v", nConn.RemoteAddr(), err)
		return
	}
	_ = nConn.SetDeadline(time.Time{})
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)

	sess := &tunnelSession{
		fingerprint: sshConn.Permissions.Extensions["fingerprint"],
		username:    sshConn.Permissions.Extensions["username"],
		userID:      sshConn.Permissions.Extensions["user-id"],
		conn:        sshConn,
		remote:      sshConn.RemoteAddr().String(),
	}
	if s.cfg.Tunnel.RequirePermittedAsset {
		if err := s.refreshTunnelAddrs(sess); err != nil {
			log.Printf("jmsd: tunnel refused for %s: %v", sess.username, err)
			return
		}
	}
	// The open/close pair is how "who had a tunnel, and when" gets answered --
	// Core records nothing about any of this. The close line carries the
	// duration and the stream count so the question does not require finding
	// the matching open line and subtracting timestamps by hand, which is
	// exactly what nobody does at 3am.
	opened := time.Now()
	log.Printf("jmsd: tunnel opened for %s from %s", sess.username, sess.remote)
	defer func() {
		log.Printf("jmsd: tunnel closed for %s from %s after %s, %d streams",
			sess.username, sess.remote, time.Since(opened).Round(time.Second), sess.streamsOpened.Load())
	}()

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.watchTunnelSession(connCtx, sess)
	go func() {
		<-connCtx.Done()
		_ = sshConn.Close()
	}()

	for newChan := range chans {
		if newChan.ChannelType() != tunnelChannelType {
			_ = newChan.Reject(ssh.UnknownChannelType, "only "+tunnelChannelType+" is supported")
			continue
		}
		go s.handleTunnelChannel(connCtx, sess, newChan)
	}
}

// watchTunnelSession is the revocation half of the design. Authentication
// happened once, at the handshake, so without this a connection outlives
// everything that authorised it: a key deactivated in JumpServer, a user
// disabled, an MFA interval expiring.
//
// It closes the whole connection rather than merely refusing new channels. A
// database session held open through the tunnel would otherwise survive
// revocation indefinitely, which is precisely the access that was revoked.
func (s *Server) watchTunnelSession(ctx context.Context, sess *tunnelSession) {
	ticker := time.NewTicker(tunnelRecheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.recheckTunnelSession(sess); err != nil {
				log.Printf("jmsd: tunnel revoked for %s from %s: %v", sess.username, sess.remote, err)
				_ = sess.conn.Close()
				return
			}
		}
	}
}

func (s *Server) recheckTunnelSession(sess *tunnelSession) error {
	user, err := s.users.Lookup(sess.fingerprint)
	if err != nil {
		return fmt.Errorf("key no longer resolves: %w", err)
	}
	fresh, err := s.mfaFresh(user.Username)
	if err != nil {
		// Deliberately NOT fatal to the connection: an unreachable state store
		// is jmsd's problem, and killing every live tunnel over it would turn a
		// Redis blip into an outage. The next tick tries again.
		debugf("tunnel recheck for %s: state store unavailable: %v", sess.username, err)
		return nil
	}
	if !fresh {
		return errors.New("mfa interval lapsed")
	}
	if s.cfg.Tunnel.RequirePermittedAsset {
		// Same rule as the state store above, and it has to be: this one is an
		// HTTP call to Core, so it is the MORE likely of the two to fail
		// transiently. Treating that as a revocation meant one Core blip
		// dropped every live tunnel at once, while a local Redis error -- far
		// less likely -- was forgiven. The previously loaded address set stays
		// in effect until a refresh succeeds; it is at most one tick stale,
		// which is the same staleness it already carries by design.
		if err := s.refreshTunnelAddrs(sess); err != nil {
			debugf("tunnel recheck for %s: keeping the previous permitted-asset set: %v", sess.username, err)
		}
	}
	return nil
}

// refreshTunnelAddrs reloads the caller's permitted asset addresses. This is
// what ties the tunnel to JumpServer's permission model: without it the allow
// list is a flat network grant identical for everyone who can authenticate.
func (s *Server) refreshTunnelAddrs(sess *tunnelSession) error {
	done := timed("core api: userPermAssets for tunnel user=%s", sess.userID)
	assets, err := s.userPermAssets(sess.userID)
	done()
	if err != nil {
		return fmt.Errorf("load permitted assets: %w", err)
	}
	addrs := make(map[string]bool, len(assets))
	for _, a := range assets {
		// Only literal addresses are usable here. An asset whose address is a
		// hostname cannot be compared against a resolved destination IP
		// without resolving it too, which would put an unbounded number of DNS
		// lookups on this path and make the check depend on records jmsd does
		// not control. Such assets are simply not reachable in this mode.
		if ip := net.ParseIP(strings.TrimSpace(a.Address)); ip != nil {
			addrs[ip.String()] = true
		}
	}
	if len(addrs) == 0 {
		return errors.New("no permitted asset has a literal IP address")
	}
	sess.setAddrs(addrs)
	return nil
}

func (s *Server) handleTunnelChannel(ctx context.Context, sess *tunnelSession, newChan ssh.NewChannel) {
	var data tunnelChannelData
	if err := ssh.Unmarshal(newChan.ExtraData(), &data); err != nil {
		_ = newChan.Reject(ssh.ConnectionFailed, "bad direct-tcpip payload")
		return
	}
	port := int(data.DestPort)

	// Re-checked per channel, not only on the ticker: this is the cheap half
	// (two map lookups) and it closes the window where a stream opens in the
	// seconds between a revocation and the next tick. It is also where the
	// client learns a lapsed MFA interval is recoverable -- see
	// TunnelMFARequiredReason.
	user, err := s.users.Lookup(sess.fingerprint)
	if err != nil {
		// ConnectionFailed, not Prohibited: the client reads Prohibited as
		// "policy said no about this destination" and reports it that way,
		// which would send the user to read tunnel.allow when their key has
		// actually been deactivated. Neither is retryable, but they are
		// different problems -- and the recheck ticker is about to close the
		// whole session anyway.
		log.Printf("jmsd: tunnel channel refused for %s: key no longer resolves: %v", sess.username, err)
		_ = newChan.Reject(ssh.ConnectionFailed, "key no longer valid")
		return
	}
	// A store error is not fatal here, for the same reason as in
	// recheckTunnelSession -- but it is logged rather than silently passed
	// over: an unreachable store means this gate is open, and that has to be
	// discoverable afterwards instead of leaving no trace at all.
	fresh, ferr := s.mfaFresh(user.Username)
	if ferr != nil {
		debugf("tunnel channel for %s: mfa gate skipped, state store unavailable: %v", sess.username, ferr)
	} else if !fresh {
		_ = newChan.Reject(ssh.Prohibited, TunnelMFARequiredReason)
		return
	}

	// Reserved by incrementing FIRST, not by comparing and incrementing later.
	// The cap exists so one client cannot exhaust jmsd's descriptors and take
	// the control plane down with it -- and a check-then-act version could not
	// deliver that: every channel gets its own goroutine, so a burst all read
	// the same stale value, and the counter stayed untouched across the DNS
	// lookup and the 10s dial that follow. The number of sockets actually
	// outstanding could run well past the limit.
	if n := sess.channels.Add(1); n > int64(s.cfg.Tunnel.MaxChannels) {
		sess.channels.Add(-1)
		log.Printf("jmsd: tunnel channel refused for %s: %d concurrent streams (max_channels)", sess.username, n-1)
		_ = newChan.Reject(ssh.ResourceShortage, "too many concurrent streams")
		return
	}
	// Released on every path from here on, including the rejections below.
	defer sess.channels.Add(-1)

	ip, err := resolveTunnelTarget(ctx, net.DefaultResolver.LookupIP, s.tunnelPolicy, data.DestAddr, port)
	if err != nil {
		log.Printf("jmsd: tunnel denied for %s: %v", sess.username, err)
		// The client is told only that policy refused it, not which rule or
		// which resolved address -- that detail is in jmsd's log, for the
		// operator. It is enough for the client to stop retrying.
		_ = newChan.Reject(ssh.Prohibited, "destination not permitted")
		return
	}
	if s.cfg.Tunnel.RequirePermittedAsset && !sess.permits(ip) {
		log.Printf("jmsd: tunnel denied for %s: %s:%d is not a permitted asset address", sess.username, ip, port)
		_ = newChan.Reject(ssh.Prohibited, "destination not permitted")
		return
	}

	target := net.JoinHostPort(ip.String(), fmt.Sprint(port))
	dialer := net.Dialer{Timeout: tunnelDialTimeout}
	dst, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		log.Printf("jmsd: tunnel dial %s for %s failed: %v", target, sess.username, err)
		_ = newChan.Reject(ssh.ConnectionFailed, "dial failed")
		return
	}
	ch, reqs, err := newChan.Accept()
	if err != nil {
		_ = dst.Close()
		return
	}
	go ssh.DiscardRequests(reqs)

	sess.streamsOpened.Add(1)

	// The only record this stream will ever have: Core knows nothing about it.
	// Both the requested name and the resolved address are logged, because
	// they can differ and the difference is what a policy question turns on.
	started := time.Now()
	log.Printf("jmsd: tunnel stream open user=%s requested=%s:%d target=%s from=%s",
		sess.username, data.DestAddr, port, target, sess.remote)

	up, down := pipeStream(ch, dst)

	log.Printf("jmsd: tunnel stream close user=%s target=%s up=%dB down=%dB duration=%s",
		sess.username, target, up, down, time.Since(started).Round(time.Millisecond))
}
