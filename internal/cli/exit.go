package cli

import (
	"errors"
	"fmt"
)

// ExitError lets a headless "-c" run report a specific process exit code, so a
// script driving it can tell the two failure classes apart without parsing
// stderr. cmd/jms's main checks for this type and falls back to exit 1 for
// every other error, which is every error the TUI path can still produce.
type ExitError struct {
	Code int
	Err  error
}

// Err is nil for remoteExit: the remote command already said whatever it had
// to say on its own stderr, so there is nothing for main to print.
func (e *ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("remote command exited with status %d", e.Code)
	}
	return e.Err.Error()
}
func (e *ExitError) Unwrap() error { return e.Err }

// The line these two codes draw is "was a connection ticket minted", because
// that -- not how far the flow got -- is what decides whether a retry is free
// or leaves a second entry in Core's audit trail.
const (
	// ExitResolveFailed: nothing was minted and nothing reached Core's audit
	// trail. Bad flags, an asset or account that matched zero times or more
	// than once, no unambiguous protocol, a missing k9s binary, or MFA
	// cancelled/refused at any point before the mint.
	ExitResolveFailed = 2
	// ExitConnectFailed: the mint was attempted, so a ticket and an audited
	// session may exist. The mint itself failing, the SSH dial to Koko failing,
	// or the session ending in error.
	ExitConnectFailed = 3
	// ExitCommandFailed replaces both of the above when a remote command was
	// given ("-c ... -- CMD"). There the exit code belongs to the remote
	// command, which can itself return 2 or 3, so jms keeps a single code of
	// its own -- 255, as OpenSSH does -- and the stderr line says which side
	// of the mint it failed on.
	ExitCommandFailed = 255
)

// remoteExit passes a remote command's non-zero exit status through as jms's
// own, with nothing printed.
func remoteExit(status int) error {
	return &ExitError{Code: status}
}

// asCommandFailure moves a jms-side failure onto ExitCommandFailed. A
// remoteExit is left alone: that code is the command's.
func asCommandFailure(err error) error {
	var exitErr *ExitError
	if errors.As(err, &exitErr) && exitErr.Err != nil {
		exitErr.Code = ExitCommandFailed
	}
	return err
}

func resolveErr(err error) error {
	if err == nil {
		return nil
	}
	return &ExitError{Code: ExitResolveFailed, Err: err}
}

func connectErr(err error) error {
	if err == nil {
		return nil
	}
	return &ExitError{Code: ExitConnectFailed, Err: err}
}
