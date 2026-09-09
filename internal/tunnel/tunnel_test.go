package tunnel

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
)

// The four answers classify can give lead to four different behaviours, so a
// misclassification is user-visible: a stream jmsd refused on policy would be
// retried forever (a browser issues dozens of those at once), a lapsed MFA
// interval would surface as an opaque SSH error instead of a code prompt, and
// a destination that is simply down would be reported as the tunnel being
// broken -- sending the user to jmsd when the server is the problem.
func TestClassifyStreamOutcome(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want streamOutcome
	}{
		{
			name: "policy refusal",
			err: &ssh.OpenChannelError{
				Reason: ssh.Prohibited, Message: "destination not permitted",
			},
			want: outcomeRefused,
		},
		{
			name: "revoked key is a refusal too",
			err: &ssh.OpenChannelError{
				Reason: ssh.Prohibited, Message: "key no longer valid",
			},
			want: outcomeRefused,
		},
		{
			name: "lapsed MFA is recoverable",
			err: &ssh.OpenChannelError{
				Reason: ssh.Prohibited, Message: jmsd.TunnelMFARequiredReason,
			},
			want: outcomeMFA,
		},
		{
			name: "destination down",
			err: &ssh.OpenChannelError{
				Reason: ssh.ConnectionFailed, Message: "dial failed",
			},
			want: outcomeFatal,
		},
		{
			name: "out of stream slots",
			err: &ssh.OpenChannelError{
				Reason: ssh.ResourceShortage, Message: "too many concurrent streams",
			},
			want: outcomeFatal,
		},
		{
			name: "connection gone",
			err:  errors.New("ssh: connection lost"),
			want: outcomeBroken,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.err); got != tc.want {
				t.Fatalf("classify(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// Only a refused destination maps onto ErrDestinationRefused, which is what
// decides whether the failure is worth printing on the terminal and whether a
// SOCKS client gets "not allowed by ruleset" instead of a generic error.
func TestRefusedWrapsOnlyPolicyRefusals(t *testing.T) {
	refused := &ssh.OpenChannelError{Reason: ssh.Prohibited, Message: "destination not permitted"}
	if got := classify(refused); got != outcomeRefused {
		t.Fatalf("classify = %d, want outcomeRefused", got)
	}
	mfa := &ssh.OpenChannelError{Reason: ssh.Prohibited, Message: jmsd.TunnelMFARequiredReason}
	if classify(mfa) == outcomeRefused {
		t.Error("an MFA lapse classified as a refusal: the client would never prompt again")
	}
}

// deadConn is a reader that blocks forever, standing in for the case that
// deadlocked: an idle peer that will never send an EOF -- a held-open database
// connection, a browser's keep-alive socket. Closing it unblocks the Read.
type deadConn struct {
	closed chan struct{}
	once   sync.Once
}

func newDeadConn() *deadConn { return &deadConn{closed: make(chan struct{})} }

func (d *deadConn) Read([]byte) (int, error) {
	<-d.closed
	return 0, io.EOF
}
func (d *deadConn) Write(p []byte) (int, error) { return len(p), nil }
func (d *deadConn) Close() error {
	d.once.Do(func() { close(d.closed) })
	return nil
}

// eofConn reads EOF at once and swallows writes: the side that dies when a
// revocation closes the SSH connection.
type eofConn struct{ closed atomic.Bool }

func (e *eofConn) Read([]byte) (int, error) { return 0, io.EOF }
func (e *eofConn) Write(p []byte) (int, error) {
	return len(p), nil
}
func (e *eofConn) Close() error { e.closed.Store(true); return nil }

// The regression: pipe used a WaitGroup, so when one side returned EOF while
// the other sat on a Read that would never see one, the wait never returned
// and NEITHER end was closed -- a leaked goroutine plus a leaked socket, on
// exactly the revocation path this design exists for (jmsd closes the SSH
// connection; the destination socket outlives it anyway).
func TestPipeDoesNotDeadlockWhenOneSideNeverSendsEOF(t *testing.T) {
	idle := newDeadConn()
	dead := &eofConn{}

	done := make(chan struct{})
	go func() { pipe(dead, idle); close(done) }()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pipe did not return: one direction is blocked on a Read that will never see EOF")
	}
	// Both ends closed is what releases the descriptors; returning without
	// closing would leak them just as surely as never returning.
	select {
	case <-idle.closed:
	default:
		t.Error("the idle end was left open")
	}
	if !dead.closed.Load() {
		t.Error("the finished end was left open")
	}
}

// The ordinary case still has to work: bytes cross, and a clean EOF ends it.
func TestPipeCopiesBothDirections(t *testing.T) {
	left, leftPeer := net.Pipe()
	right, rightPeer := net.Pipe()

	done := make(chan struct{})
	go func() { pipe(left, right); close(done) }()

	// Write into one peer, read it out of the other: left <-> right is what
	// pipe joins, so a byte written to leftPeer must appear on rightPeer.
	go func() {
		_, _ = leftPeer.Write([]byte("ping"))
		_ = leftPeer.Close()
	}()
	buf := make([]byte, 4)
	_ = rightPeer.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(rightPeer, buf); err != nil {
		t.Fatalf("payload did not cross: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("got %q, want \"ping\"", buf)
	}
	_ = rightPeer.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pipe did not return after both ends closed")
	}
}
