package cli

// ExitError lets a headless "-c" run report a specific process exit code, so a
// script driving it can tell the two failure classes apart without parsing
// stderr. cmd/jms's main checks for this type and falls back to exit 1 for
// every other error, which is every error the TUI path can still produce.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
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
)

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
