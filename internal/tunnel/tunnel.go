// Package tunnel is jms's "-w PORT" mode: a SOCKS5 proxy on the laptop's
// loopback whose streams are carried, one SSH channel each, to jmsd, which
// dials them from inside the datacenter (see internal/jmsd/tunnel.go for the
// server half and what it refuses).
//
// The MFA experience is the reason this package talks to two planes instead of
// one. Every prompt happens on the control plane, where connectflow.WithMFA
// and HandleMFA already know how to ask, retry a wrong code, and give up on a
// lockout -- so a tunnel asks for a TOTP code with the same wording, the same
// retry behaviour and the same lockout message as everything else in jms. The
// SSH side never prompts: it either works, because MFA was just cleared, or it
// fails opaquely, which is the right answer for someone dialling it directly
// with a stolen key.
//
// That is also why reconnecting always re-runs the control-plane call first
// (see link.reconnect). A connection that jmsd dropped because the caller's
// MFA interval lapsed would otherwise come back as "ssh: unable to
// authenticate", which tells the user nothing they can act on; going through
// /v1/tunnel-config turns it into the ordinary code prompt.
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/leiyupeng/jumpserver-cli/internal/connectflow"
	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
	"github.com/leiyupeng/jumpserver-cli/internal/jmsdclient"
	"github.com/leiyupeng/jumpserver-cli/internal/jmslog"
)

// ErrDestinationRefused is what a policy refusal from jmsd looks like on this
// side: the destination is not in tunnel.allow, or not an asset the caller may
// reach. Distinguished from every other failure because it is the one no
// retry, reconnect or fresh TOTP code can change.
var ErrDestinationRefused = errors.New("destination not permitted by jmsd")

// tunnelSSHUser is sent as the SSH username. jmsd ignores it entirely --
// identity comes from the key -- so this is only what shows up in a packet
// capture or a puzzled operator's log.
const tunnelSSHUser = "jms"

// tunnelDialTimeout bounds the dial and the handshake. Ten seconds is long
// enough for a datacenter round trip and short enough that a firewalled port
// reports itself instead of hanging.
const tunnelDialTimeout = 10 * time.Second

// Options is what Run needs. Gate carries the jmsd client, the context and
// the TOTP prompt; Signer is the same private key that identifies this user
// everywhere else (sshclient.LoadIdentity).
type Options struct {
	LocalPort int
	Signer    ssh.Signer
	Gate      *connectflow.Gate
}

// stderr is a variable so a test can capture what Run prints. Nothing else in
// this package writes to the terminal: the rest goes to jmslog, because a TUI
// may own the screen when a stream fails.
var stderr io.Writer = os.Stderr

// Run serves SOCKS5 on 127.0.0.1:LocalPort until ctx is cancelled.
func Run(ctx context.Context, opts Options) error {
	l := &link{gate: opts.Gate, signer: opts.Signer}
	if err := l.reconnect(0); err != nil {
		return err
	}
	defer l.close()

	// Loopback only, never a configurable bind address: this listener needs no
	// credential of its own (see socksHandshake), so exposing it on a LAN
	// interface would hand the tunnel to the network.
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(opts.LocalPort))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer ln.Close()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	fmt.Fprintf(stderr, "jms: SOCKS5 on %s -> %s (as %s); Ctrl-C to stop\n", addr, l.addr(), opts.Gate.Username)
	jmslog.Printf("tunnel: socks5 on %s via %s as %s", addr, l.addr(), opts.Gate.Username)

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Never fatal on one bad accept. mfmc's equivalent calls
			// log.Fatalf here, so a single failed connection kills the whole
			// client and every stream still running through it.
			jmslog.Printf("tunnel: accept: %v", err)
			continue
		}
		go func() {
			if err := l.serve(conn); err != nil {
				jmslog.Debugf("tunnel: stream ended: %v", err)
			}
		}()
	}
}

// link owns the single SSH connection every stream is multiplexed over, and
// the redial that replaces it.
//
// gen exists so that N streams failing at once cause one reconnect, not N. A
// caller records the generation it was using before it failed; reconnect is a
// no-op if someone else has already replaced that generation.
type link struct {
	gate   *connectflow.Gate
	signer ssh.Signer

	mu     sync.Mutex
	client *ssh.Client
	cfg    jmsd.TunnelConfigResponse
	gen    uint64
}

func (l *link) addr() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cfg.Addr
}

func (l *link) get() (*ssh.Client, uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.client == nil {
		return nil, l.gen, errors.New("tunnel not connected")
	}
	return l.client, l.gen, nil
}

func (l *link) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.client != nil {
		_ = l.client.Close()
		l.client = nil
	}
}

// reconnect fetches the tunnel's address and host key from the control plane
// (prompting for MFA if that call says so) and dials a fresh SSH connection.
//
// staleGen is the generation the caller was holding when it failed; passing 0
// forces the initial connect. Fetching the config every time is not
// redundant: it is both the MFA gate and how a rotated host key or a moved
// listener is picked up, and it costs one request per reconnect, not per
// stream.
func (l *link) reconnect(staleGen uint64) error {
	// Held for the WHOLE reconnect, prompt and dial included -- that is what
	// makes it single-flight. Releasing it around those let concurrent callers
	// all pass the staleGen check: N prompts on one tty, and every ssh.Client
	// but the last one leaked as a live connection.
	l.mu.Lock()
	defer l.mu.Unlock()
	if staleGen != 0 && staleGen != l.gen {
		return nil // someone else already replaced this connection
	}
	if l.client != nil {
		_ = l.client.Close()
		l.client = nil
	}

	cfg, err := connectflow.WithMFA(l.gate, func() (jmsd.TunnelConfigResponse, error) {
		return l.gate.JC.TunnelConfig(l.gate.Ctx)
	})
	if err != nil {
		// A 404 here means the jmsd this laptop is pointed at predates the
		// endpoint, which is what a user hits when only half the fleet has
		// been upgraded. Its own message ("404 page not found") says nothing
		// about the fix, and the fix is not theirs.
		var status *jmsdclient.StatusError
		if errors.As(err, &status) && status.Code == http.StatusNotFound {
			return errors.New("this jmsd does not serve /v1/tunnel-config -- it is older than the tunnel " +
				"feature and needs upgrading (see the release notes; jmsd is upgraded before jms)")
		}
		return fmt.Errorf("load tunnel config: %w", err)
	}
	if cfg.Addr == "" || cfg.HostKey == "" {
		return errors.New("jmsd has no tunnel configured (set tunnel.enabled in its config)")
	}
	hostKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(cfg.HostKey))
	if err != nil {
		return fmt.Errorf("parse tunnel host key: %w", err)
	}

	client, err := dialTunnel(l.gate.Ctx, cfg.Addr, &ssh.ClientConfig{
		User: tunnelSSHUser,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(l.signer)},
		// Pinned to what the authenticated control plane just told us. This
		// is the line that must never become InsecureIgnoreHostKey: whoever
		// answers on that address otherwise sees every byte of every stream.
		HostKeyCallback: ssh.FixedHostKey(hostKey),
	})
	if err != nil {
		return fmt.Errorf("connect tunnel %s: %w", cfg.Addr, err)
	}

	l.client, l.cfg, l.gen = client, cfg, l.gen+1
	jmslog.Printf("tunnel: connected to %s (host key %s)", cfg.Addr, ssh.FingerprintSHA256(hostKey))
	return nil
}

// dialTunnel dials and handshakes, honouring ctx and a deadline.
//
// ssh.Dial does neither: it blocks until the OS gives up. A tunnel port that
// is firewalled rather than closed drops the SYN silently, so "jms -w" hung
// with nothing on screen and Ctrl-C did nothing -- the signal cancelled a
// context ssh.Dial was not watching. Same shape as
// sshclient.dialAndHandshake, which solved this for the Koko dial.
func dialTunnel(ctx context.Context, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	dialer := net.Dialer{Timeout: tunnelDialTimeout}
	raw, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	// The handshake itself can hang too (a port that accepts and then says
	// nothing), and it takes no context -- closing the conn is what unblocks it.
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = raw.Close()
		case <-stop:
		}
	}()
	_ = raw.SetDeadline(time.Now().Add(tunnelDialTimeout))
	conn, chans, reqs, err := ssh.NewClientConn(raw, addr, cfg)
	close(stop)
	if err != nil {
		_ = raw.Close()
		// Closing raw above is how the cancellation was delivered, so the
		// handshake blames itself: "read tcp ...: use of closed network
		// connection", which reads like a network fault the user should go
		// investigate. It was their own Ctrl-C. Report the cause, not the
		// symptom.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	// Cleared once up: a tunnel stream may sit idle for hours.
	_ = raw.SetDeadline(time.Time{})
	return ssh.NewClient(conn, chans, reqs), nil
}

// streamOutcome is how a failed open should be treated. It is its own type
// because the three answers lead to three different behaviours and getting one
// wrong is visible: refusing a stream that was only transiently broken, or
// redialling the tunnel on every blocked request a browser makes.
type streamOutcome int

const (
	// outcomeRefused: jmsd's policy said no. No reconnect, no prompt, no
	// retry will change it.
	outcomeRefused streamOutcome = iota
	// outcomeMFA: the caller's interval lapsed mid-tunnel. Ask for a code on
	// the control plane, reconnect, and replay the stream.
	outcomeMFA
	// outcomeBroken: the connection is gone -- jmsd restarted, the laptop
	// slept, the session was revoked. Reconnecting may fix it, and if the key
	// is gone the fresh handshake will say so properly.
	outcomeBroken
	// outcomeFatal: nothing a reconnect can do. The destination is down, or
	// jmsd is out of stream slots. Report it and stop.
	outcomeFatal
)

// classify decides what a failed channel open means.
//
// The distinction it is built around is recoverable-from-here versus not: only
// the first two outcomes deserve a reconnect, and only outcomeMFA deserves a
// prompt. A destination jmsd refused for policy reasons reconnects into the
// same refusal forever; a destination that is simply down would be reported
// as a tunnel failure, which sends the user looking at jmsd when the answer is
// the server.
func classify(err error) streamOutcome {
	var openErr *ssh.OpenChannelError
	if !errors.As(err, &openErr) {
		// Not a channel-level refusal at all: a dropped connection, a write
		// failure, anything that says nothing about the destination.
		return outcomeBroken
	}
	switch {
	case openErr.Reason == ssh.Prohibited && openErr.Message == jmsd.TunnelMFARequiredReason:
		return outcomeMFA
	case openErr.Reason == ssh.Prohibited:
		return outcomeRefused
	default:
		// ConnectionFailed (destination unreachable), ResourceShortage
		// (max_channels), ConnectFailed: all answers about this stream that a
		// new connection would repeat.
		return outcomeFatal
	}
}

// openStream opens one channel, recovering once from a lapsed MFA interval or
// a broken connection.
//
// Once, not in a loop: reconnect has already prompted and retried a wrong code
// via HandleMFA, so a second failure means the code was accepted and the
// stream still failed -- a real error, and re-prompting would only hide it
// behind a prompt that can never be satisfied.
func (l *link) openStream(target socksTarget) (net.Conn, error) {
	client, gen, err := l.get()
	if err != nil {
		// A previous reconnect failed partway (gave up on the TOTP prompt, jmsd
		// briefly unreachable). Retrying here is what stops that one failure
		// from killing the tunnel for the life of the process.
		if rerr := l.reconnect(gen); rerr != nil {
			return nil, rerr
		}
		if client, gen, err = l.get(); err != nil {
			return nil, err
		}
	}
	conn, dialErr := client.Dial("tcp", target.String())
	if dialErr == nil {
		return conn, nil
	}

	// The two outcomes a reconnect cannot improve on are settled here;
	// outcomeMFA and outcomeBroken fall through to it, which is the whole
	// point of distinguishing them.
	switch classify(dialErr) {
	case outcomeRefused:
		return nil, fmt.Errorf("%w: %s", ErrDestinationRefused, target)
	case outcomeFatal:
		return nil, dialErr
	}
	jmslog.Printf("tunnel: reconnecting (%v)", dialErr)
	if rerr := l.reconnect(gen); rerr != nil {
		return nil, rerr
	}
	client, _, err = l.get()
	if err != nil {
		return nil, err
	}
	conn, err = client.Dial("tcp", target.String())
	if err != nil && classify(err) == outcomeRefused {
		return nil, fmt.Errorf("%w: %s", ErrDestinationRefused, target)
	}
	return conn, err
}

func (l *link) serve(local net.Conn) error {
	defer local.Close()

	target, err := socksHandshake(local)
	if err != nil {
		// A failed handshake may not have reached the request stage, in which
		// case there is nothing to reply to; write the code anyway and ignore
		// the outcome -- a client that has gone away does not care.
		_ = socksReply(local, socksFailureCode(err))
		return err
	}
	remote, err := l.openStream(target)
	if err != nil {
		// The reply code carries the reason (socksFailureCode maps a policy
		// refusal to "not allowed by ruleset", which curl and browsers print
		// themselves), so nothing is written to the terminal here.
		//
		// It used to print every refusal, on the theory that a blocked
		// destination is the one failure a user can act on. In egress mode
		// that is unusable: a laptop with DNS-level ad blocking resolves
		// tracker domains to 127.0.0.1, the tunnel correctly refuses loopback,
		// and one browser tab then scrolls dozens of identical lines past a
		// tunnel that is working perfectly well -- which reads exactly like
		// the tunnel being broken.
		return err
	}
	defer remote.Close()
	if err := socksReply(local, socksSuccess); err != nil {
		return err
	}
	pipe(local, remote)
	return nil
}

// pipe copies between the two ends until both directions are done,
// half-closing each as it finishes so the other survives -- a client that has
// sent its whole request still has a response coming.
//
// Closing both ends once EITHER direction finishes is the part that matters.
// Waiting for both first (a WaitGroup, the obvious shape) deadlocks on the
// revocation path this design exists for: jmsd drops the SSH connection, one
// Copy returns at once, and the other sits on a Read of an idle socket that
// will never see an EOF -- so nothing is ever closed.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	_, _ = a.Close(), b.Close()
	<-done
}
