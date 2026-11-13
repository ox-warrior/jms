// Package connectflow holds the connection steps shared between jms's TUI
// (internal/tui/connect.go) and its headless "-c" shortcut
// (internal/cli/headless.go): asset detail, ticket minting, the Koko dial, the
// k8s session, and the MFA retry any signed jmsd call can trigger along the
// way. Each caller sequences those steps itself; having one copy of the steps
// is what stops two connection paths leaving different audit trails for what
// looks like the same action.
//
// It knows nothing about tview: a caller that owns a display supplies an
// Authenticate and gets plain errors back.
package connectflow

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/leiyupeng/jumpserver-cli/internal/googlelogin"
	"github.com/leiyupeng/jumpserver-cli/internal/jmsdclient"
)

// ErrCancelled is what WithMFA returns when the user gave up the sign-in
// rather than jmsd refusing it. Callers prefix it with what
// they were attempting: "load scope failed: MFA required · cancelled".
var ErrCancelled = errors.New("MFA required · cancelled")

// Gate carries the jmsd client and context every call in this package runs
// against, plus what WithMFA needs to sign the key in again when jmsd no longer knows it. One
// is built per operation, since Ctx and Username differ per call. Holding both
// here is what keeps the step functions from taking a second, separately
// passed copy that could disagree with it.
type Gate struct {
	JC  *jmsdclient.Client
	Ctx context.Context
	// Username is only for display in the prompt. Empty is legitimate before
	// the caller has learned its own name -- see WithMFA.
	Username string
	// Around wraps the prompt so a caller that already owns the screen can
	// get out of its way first (the TUI hides its "Connecting…" modal while
	// asking). Nil prompts directly -- what a plain terminal prompt needs.
	Around func(ask func() error) error
	// Authenticate gets the key signed in again: a Google sign-in (see
	// GoogleSignIn) where a human is present, an explanatory error where not.
	Authenticate func(ctx context.Context, username string) error
}

// authMu and authDone serialise Authenticate across the whole process. The
// TUI loads the asset list and the marks concurrently, and right after MFA
// lapses both come back mfa_required at once; without this, each would open
// its own browser tab. The second caller waits, sees the first one has just
// succeeded, and goes straight to retrying its call.
var (
	authMu   sync.Mutex
	authDone time.Time
)

func (g *Gate) ask(username string) error {
	handle := func() error {
		authMu.Lock()
		defer authMu.Unlock()
		if time.Since(authDone) < time.Minute {
			return nil
		}
		if err := g.Authenticate(g.Ctx, username); err != nil {
			return err
		}
		authDone = time.Now()
		return nil
	}
	if g.Around == nil {
		return handle()
	}
	return g.Around(handle)
}

// GoogleSignIn signs the key in with Google: the browser flow for an ID token,
// then the token to jmsd, which binds the key to the JumpServer user with
// that email. show receives the sign-in URL, for when no browser opens.
func GoogleSignIn(ctx context.Context, jc *jmsdclient.Client, client googlelogin.Client, show func(url string)) error {
	nonce, err := jmsdclient.NewNonce()
	if err != nil {
		return err
	}
	idToken, err := client.SignIn(ctx, nonce, show)
	if err != nil {
		return err
	}
	return jc.VerifyGoogle(ctx, nonce, idToken)
}

// WithMFA runs call and, if jmsd reports the key is not signed in (never, or
// the interval lapsed), signs in and runs it exactly once more. Every jmsd call
// in this package's pipeline goes through it: MFA can lapse between any two
// requests, so handling it at only some call sites would leave the rest
// failing with an error the user can do nothing about.
//
// Once, not in a loop: a second failure means the sign-in was accepted and
// the call still failed -- a real error, not something re-signing-in fixes.
func WithMFA[T any](g *Gate, call func() (T, error)) (T, error) {
	out, err := call()
	var mfaErr *jmsdclient.MFARequiredError
	if !errors.As(err, &mfaErr) { // the name predates Google sign-in: it means "not signed in"
		return out, err
	}
	if err := g.ask(g.Username); err != nil {
		var zero T
		return zero, err
	}
	return call()
}
