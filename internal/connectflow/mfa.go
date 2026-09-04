// Package connectflow holds the connection steps shared between jms's TUI
// (internal/tui/connect.go) and its headless "-c" shortcut
// (internal/cli/headless.go): asset detail, ticket minting, the Koko dial, the
// k8s session, and the MFA retry any signed jmsd call can trigger along the
// way. Each caller sequences those steps itself; having one copy of the steps
// is what stops two connection paths leaving different audit trails for what
// looks like the same action.
//
// It knows nothing about tview: a caller that owns a display supplies a Prompt
// and gets plain errors back.
package connectflow

import (
	"context"
	"errors"
	"net/http"

	"github.com/leiyupeng/jumpserver-cli/internal/jmsdclient"
)

// Prompt asks the user for a fresh TOTP code, or errors if they gave up (Esc
// in the TUI's modal; Ctrl+D/Ctrl+C on the CLI's tty read). errMsg is the
// previous attempt's rejection reason, or "" on the first try.
type Prompt func(ctx context.Context, username, errMsg string) (code string, err error)

// ErrCancelled is what HandleMFA and WithMFA return when Prompt reports the
// user gave up rather than jmsd refusing a code. Callers prefix it with what
// they were attempting: "load scope failed: MFA required · cancelled".
var ErrCancelled = errors.New("MFA required · cancelled")

// Gate carries the jmsd client and context every call in this package runs
// against, plus what WithMFA needs to recover from a lapsed MFA interval. One
// is built per operation, since Ctx and Username differ per call. Holding both
// here is what keeps the step functions from taking a second, separately
// passed copy that could disagree with it.
type Gate struct {
	JC  *jmsdclient.Client
	Ctx context.Context
	// Username is only for display in the prompt. Empty is legitimate before
	// the caller has learned its own name -- see WithMFA.
	Username string
	Prompt   Prompt
	// Around wraps the prompt so a caller that already owns the screen can
	// get out of its way first (the TUI hides its "Connecting…" modal while
	// asking). Nil prompts directly -- what a plain terminal prompt needs.
	Around func(ask func() error) error
}

func (g *Gate) ask(username string) error {
	handle := func() error { return HandleMFA(g.Ctx, g.JC, username, g.Prompt) }
	if g.Around == nil {
		return handle()
	}
	return g.Around(handle)
}

// WithMFA runs call and, if jmsd reports the caller's MFA interval has
// lapsed, prompts for a code and runs it exactly once more. Every jmsd call
// in this package's pipeline goes through it: MFA can lapse between any two
// requests, so handling it at only some call sites would leave the rest
// failing with an error the user can do nothing about.
//
// Once, not in a loop: HandleMFA already retries a wrong code, so a second
// failure here means MFA was accepted and the call still failed -- which is a
// real error, not something re-prompting can fix.
func WithMFA[T any](g *Gate, call func() (T, error)) (T, error) {
	out, err := call()
	var mfaErr *jmsdclient.MFARequiredError
	if !errors.As(err, &mfaErr) {
		return out, err
	}
	// jmsd resolves the caller's fingerprint to a user before checking MFA
	// freshness, so even the first request comes back naming them -- which is
	// what lets a startup prompt show a username, not a fingerprint.
	username := g.Username
	if username == "" {
		username = mfaErr.Username
	}
	if err := g.ask(username); err != nil {
		var zero T
		return zero, err
	}
	return call()
}

// HandleMFA asks for a TOTP code and submits it to jmsd, retrying on a wrong
// code until the user succeeds, cancels, or hits something no further code can
// satisfy. It returns nil once jmsd accepts a code.
//
// That last case is why this inspects the rejection reason rather than just
// "did it fail": a locked-out account (429, for fifteen minutes) and one with
// no enrolled device (400) are refused with errors that stay true whatever is
// typed next, so retrying them reopens the prompt forever with cancelling the
// only way out.
func HandleMFA(ctx context.Context, jc *jmsdclient.Client, username string, prompt Prompt) error {
	errMsg := ""
	for {
		code, err := prompt(ctx, username, errMsg)
		if err != nil {
			return ErrCancelled
		}
		err = jc.VerifyMFA(ctx, code)
		if err == nil {
			return nil
		}
		if !MFACodeRejected(err) {
			return err
		}
		errMsg = err.Error()
	}
}

// MFACodeRejected reports whether an error from VerifyMFA means "that code was
// wrong". 401 is the only rejection a fresh code can fix; everything else -- a
// lockout, a missing device, an unreachable state store, a transport failure
// with no status at all -- goes back to the caller instead of re-prompting.
func MFACodeRejected(err error) bool {
	var status *jmsdclient.StatusError
	return errors.As(err, &status) && status.Code == http.StatusUnauthorized
}
