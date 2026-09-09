package tunnel

import (
	"errors"
	"net"
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

// The regression: pipe first waited for both directions before closing
// anything, which deadlocked whenever one side died without an EOF -- the
// revocation path this design exists for. A blocked Read plus an unclosed
// socket, forever.
//
// net.Pipe's ends never see an EOF on their own, so leaving one peer untouched
// reproduces it exactly.
func TestPipeClosesBothEndsWhenOneSideDies(t *testing.T) {
	a, aPeer := net.Pipe()
	b, _ := net.Pipe() // b's peer is deliberately never read or closed

	done := make(chan struct{})
	go func() { pipe(a, b); close(done) }()

	_ = aPeer.Close() // one direction ends; the other is stuck on a Read

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pipe did not return: a direction is blocked on a Read that will never see EOF")
	}
	// Returning without closing would leak the descriptors just as surely.
	if err := b.SetDeadline(time.Now()); err == nil {
		t.Error("the stuck end was left open")
	}
}
