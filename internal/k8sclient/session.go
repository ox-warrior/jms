package k8sclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// marker delimits the kubeconfig payload inside the shell's raw PTY output
// so it can be located regardless of banner/prompt noise around it. Sent
// twice, once on each side of the `cat`, so extractKubeconfig can slice out
// exactly what sits between the two occurrences.
const marker = "__jms_k8s_kubeconfig__"

// markerLiteral is how remoteReadCommand spells marker: split across two
// adjacent shell strings, which the shell concatenates back into marker when
// it *runs* the command but which does not contain marker as a substring when
// the command is merely echoed.
//
// That distinction is the whole reason this is not just marker. The command
// gets echoed twice before it ever runs, so a plain spelling puts three pairs
// of markers on the wire, not one:
//
//  1. The tty driver echoes it the moment it is written. jms writes as soon
//     as it sees bannerText, and koko prints that banner at line 12 of
//     init-kubectl.sh -- some forty lines and a dozen process spawns before
//     `exec su` at line 57 starts the interactive shell. Nothing is reading
//     stdin yet, so the bytes sit in the tty buffer with ECHO still on.
//  2. Once bash does start, readline reads that queued line and echoes it
//     again, this time after its own prompt.
//  3. Only then does the command actually run and emit the real pair.
//
// Slicing between "the last two markers seen so far" is then a race with how
// the PTY output happens to split across reads: stopping at four yields the
// echoed command text, at five yields just a newline (the empty-kubeconfig
// failure), and only at six the real payload. Making the echoes contribute no
// markers at all removes the race instead of guessing a count -- exactly two
// occurrences ever appear, and they are always the right two.
var markerLiteral = `"` + marker[:5] + `""` + marker[5:] + `"`

// remoteReadCommand is what a human would type by hand, give or take
// markerLiteral's quoting. init-kubectl.sh (koko v4.10.16
// utils/init-kubectl.sh) drops the session into a plain interactive bash as
// jms_k8s_user with HOME=/nonexistent, having already run `kubectl config
// set-*` to populate ~/.kube/config -- so this is the one command needed to
// read it back out. Those set-* calls (lines 30-42) run before `exec su`
// (line 57), so by the time bash reaches this command the file is there.
var remoteReadCommand = fmt.Sprintf("echo %s; cat ~/.kube/config; echo %s\n", markerLiteral, markerLiteral)

// bannerText is what init-kubectl.sh prints (WELCOME_BANNER env var) once
// Koko has established the k8s connection -- koko v4.10.16
// pkg/config/config.go's KubectlBanner. Waiting for it before sending
// remoteReadCommand is required, not optional: connecting first goes through
// pkg/proxy/server.go's sendConnectingMsg, which writes and redraws
// "Connecting to Kubernetes <addr>  <elapsed>" in place -- via backspace
// characters, not a fresh line -- for as long as Koko's own connection to
// the REAL cluster takes to establish (it allows up to 5 minutes). Nothing
// can be read back before that succeeds.
//
// It does NOT mean the interactive shell is up. init-kubectl.sh prints this
// at its line 12 and only reaches `exec su` at line 57, so writing on the
// banner puts the command in the tty's input buffer, where it waits (and is
// echoed by the tty driver, and again by bash's readline) until bash gets to
// it. That is harmless -- the buffered bytes do reach bash, confirmed
// against a live Koko -- but it is why markerLiteral exists.
const bannerText = "Welcome to JumpServer kubectl, try kubectl --help."

// connectErrorMarker is the separator in Koko's own connection-failure
// message (pkg/proxy/server.go's sendConnectErrorMsg: "<connect msg> error:
// <readable error>"), e.g. "Connecting to Kubernetes https://... error:
// invalid token" when the real cluster rejects the token
// (conn_k8s.go's IsValidK8sUserToken check). Seeing this before the banner
// means the connection failed on Koko's side, before any shell -- or
// kubeconfig -- ever existed. Surfacing Koko's own message here is far more
// useful than the generic errors below, which fire in this case only if
// this check is somehow missed.
const connectErrorMarker = " error: "

// bannerWait bounds how long to wait for bannerText. Generous relative to
// how long a healthy cluster auth check should take, but short of Koko's
// own 5-minute ceiling -- ctx (cancellable via the connect modal's Esc) is
// the user's own way out before this elapses.
const bannerWait = 30 * time.Second

// readWait bounds phase 2 -- the wait for remoteReadCommand's output after
// the banner has arrived. It needs its own deadline: bannerWait is a one-shot
// time.After, so once the command has been sent it can never fire again, and
// without this a shell that reached its banner but then never ran the command
// (bash wedged, ~/.kube/config unreadable, `exec su` hung) would block
// readKubeconfig forever with the connect modal stuck on "waiting for koko's
// kubectl session…". Only Esc got out of that.
//
// Much shorter than bannerWait because the work involved is one local `cat`
// of a file Koko has already written -- the slow part (Koko's own connection
// to the real cluster) is over by the time the banner prints.
const readWait = 15 * time.Second

// keepaliveInterval is how often Open's background session pings the SSH
// connection to keep it alive. golang.org/x/crypto/ssh sends no keepalives
// of its own, and a k9s run can sit idle (from the SSH connection's point of
// view -- k9s itself talks to :5002 over a separate HTTPS connection) for
// long enough that a NAT gateway or firewall along the path drops it for
// inactivity. There is no separate expiry on Koko's side beyond this
// connection staying up (see Session's doc comment), so losing it silently
// would end the k9s session's access without any explicit signal.
const keepaliveInterval = 30 * time.Second

// Session holds a Koko SSH session open in the background for the lifetime
// of a local k9s run. Closing it ends the Koko-side proxy: the reverse-proxy
// token, the per-session kubectl proxy subprocess, and the Unix socket are
// all torn down by Koko when the SSH connection drops
// (pkg/srvconn/conn_k8s_proxy.go's Close(), wired to session teardown) --
// there is no separate "disconnect" call jmsd or jms needs to make, and no
// calendar expiry on the kubeconfig beyond this connection staying open.
type Session struct {
	session *ssh.Session
	done    chan error
	stop    chan struct{}
}

// Open starts an interactive PTY session over client -- already
// authenticated with the k8s connection ticket via
// sshclient.ConnectGatewayWithToken -- waits for Koko to actually establish
// the k8s connection and drop into its restricted shell, then reads back
// the kubeconfig it wrote. The SSH session is left running afterward: k9s
// talks to Koko's :5002 proxy over its own HTTPS connection, not through
// this session, but the session must stay open for that proxy to keep
// working (Session's own doc comment).
func Open(ctx context.Context, client *ssh.Client) (sess *Session, kubeconfigYAML []byte, err error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, nil, fmt.Errorf("open koko session: %w", err)
	}
	modes := ssh.TerminalModes{ssh.ECHO: 0, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}
	if err := session.RequestPty("xterm-256color", 40, 200, modes); err != nil {
		_ = session.Close()
		return nil, nil, fmt.Errorf("request pty: %w", err)
	}

	stdin, err := session.StdinPipe()
	if err != nil {
		_ = session.Close()
		return nil, nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		_ = session.Close()
		return nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := session.Shell(); err != nil {
		_ = session.Close()
		return nil, nil, fmt.Errorf("start shell: %w", err)
	}

	cfg, err := readKubeconfig(ctx, stdin, stdout, bannerWait, readWait)
	if err != nil {
		_ = session.Close()
		return nil, nil, err
	}

	// Keeps draining for the rest of the session: an unread SSH channel fills
	// its window and stalls the remote. readKubeconfig's own reader may still
	// be parked in a Read at this point and so briefly shares the stream --
	// harmless, since both sides discard from here on, and it exits as soon as
	// that Read returns (see readKubeconfig's quit channel).
	done := make(chan error, 1)
	go func() {
		_, _ = io.Copy(io.Discard, stdout)
		done <- session.Wait()
	}()

	stop := make(chan struct{})
	go keepAlive(sshPing(client), keepaliveInterval, stop, done)

	return &Session{session: session, done: done, stop: stop}, cfg, nil
}

// sshPing is keepAlive's ping for a real connection: a global request the
// server is free to reject -- what matters is only whether the round trip
// completes at all, not what it answers.
func sshPing(client *ssh.Client) func() error {
	return func() error {
		_, _, err := client.SendRequest("keepalive@jumpserver-cli", true, nil)
		return err
	}
}

// keepAlive pings the connection every interval until stop is closed, and
// reports on done the moment a ping does not come back -- the connection is
// gone, so the Koko-side proxy k9s is talking to is gone with it (Session's
// doc comment).
//
// Reporting is the whole point, and it is not redundant with the io.Copy in
// Open: that read sits on an SSH channel with no deadline anywhere in
// golang.org/x/crypto/ssh, so when the connection dies it can block forever
// and Done() never fires. Observed in practice -- a dead Koko session left
// k9s running against a dead proxy for over two hours, with jms suspended
// behind it and no way back to the TUI. This ping is the only bounded signal
// available.
//
// ping runs on its own goroutine so a connection that is dead but not yet
// reset (a NAT or firewall dropping the flow without an RST) cannot wedge the
// loop: the send would block there until the kernel gives up, which is exactly
// the case this is meant to catch. That goroutine may outlive this one, but
// only until the connection finally errors out, and only for a session already
// being torn down.
func keepAlive(ping func() error, interval time.Duration, stop <-chan struct{}, done chan<- error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lost := func(err error) {
		// done is buffered for one value and Open's io.Copy goroutine may have
		// beaten us to it; either way the caller learns the session is over.
		select {
		case done <- err:
		default:
		}
	}
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			reply := make(chan error, 1)
			go func() { reply <- ping() }()
			select {
			case <-stop:
				return
			case err := <-reply:
				if err != nil {
					lost(fmt.Errorf("koko session keepalive failed: %w", err))
					return
				}
			case <-time.After(interval):
				lost(errors.New("koko session stopped answering keepalives"))
				return
			}
		}
	}
}

// Close ends the Koko session, which is what actually tears down the
// server-side k8s proxy (see Session's doc comment), and stops the keepalive
// loop.
func (s *Session) Close() error {
	close(s.stop)
	return s.session.Close()
}

// Done reports when the underlying SSH session exits on its own (Koko
// restarting, network drop, idle timeout) so the caller can stop k9s and
// clean up rather than leave it pointed at a dead proxy.
func (s *Session) Done() <-chan error { return s.done }

type chunkOrErr struct {
	chunk []byte
	err   error
}

// readKubeconfig drives the whole exchange over one continuous read loop --
// necessary because waiting for the banner and then reading the command's
// output are not separable reads without risking losing whatever arrives in
// between (an io.Reader's bytes are gone once read).
//
// Phase 1 (before remoteReadCommand is sent): accumulate output looking for
// bannerText (the shell is ready) or connectErrorMarker (Koko's own
// connection attempt failed -- surfaced directly, see its doc comment).
// bannerWait bounds this phase.
//
// Phase 2 (after sending remoteReadCommand): wait for marker to appear
// twice. Exactly two occurrences ever appear, however many times the shell
// echoes the command back, because remoteReadCommand spells the marker so
// that only running it produces one -- see markerLiteral. Two is therefore
// not a lower bound to be second-guessed with a grace period, it is the
// whole payload, and the read can stop the moment the second one lands.
func readKubeconfig(ctx context.Context, stdin io.Writer, stdout io.Reader, bannerWait, readWait time.Duration) ([]byte, error) {
	ch := make(chan chunkOrErr)
	// stdout has no deadline (golang.org/x/crypto/ssh sets none anywhere), so
	// the reader below cannot be unblocked from here -- it can only be told to
	// stop delivering. Without that it would sit on an unbuffered send forever
	// once this function returns, holding a goroutine and its buffer for the
	// life of the process, and Open's drain would then be a second, permanent
	// reader of the same channel.
	quit := make(chan struct{})
	defer close(quit)
	go func() {
		tmp := make([]byte, 4096)
		for {
			n, err := stdout.Read(tmp)
			if n > 0 {
				select {
				case ch <- chunkOrErr{chunk: append([]byte(nil), tmp[:n]...)}:
				case <-quit:
					return
				}
			}
			if err != nil {
				select {
				case ch <- chunkOrErr{err: err}:
				case <-quit:
				}
				return
			}
		}
	}()

	var buf bytes.Buffer
	sentCommand := false
	bannerDeadline := time.After(bannerWait)
	// nil until the command is sent: a nil channel blocks forever in a select,
	// which is exactly the "phase 2 hasn't started" behaviour wanted.
	var readDeadline <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-bannerDeadline:
			if !sentCommand {
				return nil, fmt.Errorf("koko never reached its kubectl shell within %s (got %d bytes) -- is the k8s connection itself failing?", bannerWait, buf.Len())
			}
			// Phase 2 is already running under readDeadline; this arm has
			// nothing left to enforce. bannerDeadline is one-shot, so it will
			// not be selected again.
		case <-readDeadline:
			return nil, fmt.Errorf("koko reached its kubectl shell but never returned the kubeconfig within %s (got %d bytes, %d of 2 markers) -- the remote shell is not running the read command",
				readWait, buf.Len(), bytes.Count(buf.Bytes(), []byte(marker)))
		case item := <-ch:
			if item.err != nil {
				if errors.Is(item.err, io.EOF) && sentCommand && bytes.Count(buf.Bytes(), []byte(marker)) >= 2 {
					return extractKubeconfig(buf.Bytes(), marker)
				}
				return nil, fmt.Errorf("koko shell output ended unexpectedly (got %d bytes): %w", buf.Len(), item.err)
			}
			buf.Write(item.chunk)

			if !sentCommand {
				if idx := bytes.Index(buf.Bytes(), []byte(connectErrorMarker)); idx >= 0 {
					rest := buf.Bytes()[idx+len(connectErrorMarker):]
					// Wait for the rest of the line: at the moment the marker
					// itself first appears, the error text after it may not
					// have fully arrived yet.
					if nl := bytes.IndexAny(rest, "\r\n"); nl >= 0 {
						return nil, fmt.Errorf("koko: %s", strings.TrimSpace(string(rest[:nl])))
					}
					continue
				}
				if !bytes.Contains(buf.Bytes(), []byte(bannerText)) {
					continue
				}
				sentCommand = true
				readDeadline = time.After(readWait)
				if _, err := io.WriteString(stdin, remoteReadCommand); err != nil {
					return nil, fmt.Errorf("send read command: %w", err)
				}
				continue
			}

			if bytes.Count(buf.Bytes(), []byte(marker)) >= 2 {
				return extractKubeconfig(buf.Bytes(), marker)
			}
		}
	}
}

// extractKubeconfig pulls the payload between the LAST two occurrences of
// marker out of raw shell output.
//
// With markerLiteral in place there is only ever one pair, so first and last
// are the same thing -- the echoes of the command contribute no markers at
// all, which is that variable's whole purpose. Last is kept because it is the
// safe end to be wrong at: any future change that reintroduces a spurious
// early pair (a different quoting trick, a shell that expands it differently)
// degrades into reading the real payload rather than the echoed command text.
func extractKubeconfig(raw []byte, marker string) ([]byte, error) {
	m := []byte(marker)
	last := bytes.LastIndex(raw, m)
	if last < 0 {
		return nil, fmt.Errorf("marker not found in koko shell output (got %d bytes)", len(raw))
	}
	secondLast := bytes.LastIndex(raw[:last], m)
	if secondLast < 0 {
		return nil, fmt.Errorf("only one marker occurrence found in koko shell output (got %d bytes)", len(raw))
	}
	payload := bytes.TrimSpace(raw[secondLast+len(m) : last])
	if len(payload) == 0 {
		return nil, errors.New("koko returned an empty kubeconfig -- is this really a k8s cluster-level asset?")
	}
	return payload, nil
}
