// The "-w PORT" mode: a SOCKS5 proxy on loopback whose streams jmsd dials
// from inside the datacenter (internal/tunnel, internal/jmsd/tunnel.go).
//
// Like runHeadless this never starts the TUI, and for the same reason it
// collects a TOTP code from the tty rather than a modal. It shares that
// prompt, promptTOTPFromTTY, so the two headless modes cannot drift apart in
// how they ask.
package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/leiyupeng/jumpserver-cli/internal/connectflow"
	"github.com/leiyupeng/jumpserver-cli/internal/jmsdclient"
	"github.com/leiyupeng/jumpserver-cli/internal/jmslog"
	"github.com/leiyupeng/jumpserver-cli/internal/sshclient"
	"github.com/leiyupeng/jumpserver-cli/internal/tunnel"
)

// runTunnel resolves the caller's identity on the control plane, then hands
// over to internal/tunnel for the lifetime of the process.
//
// Whoami first, exactly as runHeadless does: it costs one request and buys
// two things -- the username every later MFA prompt is addressed to, and the
// first pass through authorize(), so an expired MFA interval is dealt with
// here, on the plane that can prompt, instead of surfacing as an unexplained
// SSH failure a moment later.
func runTunnel(ctx context.Context, port int, jmsdAddr, identityPath string) error {
	signer, _, err := sshclient.LoadIdentity(identityPath)
	if err != nil {
		return resolveErr(err)
	}
	// Ctrl-C ends the tunnel rather than killing the process mid-copy, so the
	// listener is closed and the SSH connection torn down properly -- which is
	// also what makes jmsd log "tunnel closed" instead of leaving the operator
	// to infer it from silence.
	//
	// Installed BEFORE the Gate is built, because the Gate captures the
	// context it will use for every later control-plane call (including the
	// reconnects inside internal/tunnel). Building it from the outer context
	// and shadowing the variable afterwards left those calls uncancellable:
	// Ctrl-C closed the listener while a hung /v1/tunnel-config request ran on
	// to its own timeout.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	jc := jmsdclient.New(jmsdAddr, signer)
	gate := &connectflow.Gate{JC: jc, Ctx: ctx, Prompt: promptTOTPFromTTY}

	who, err := connectflow.WithMFA(gate, func() (jmsdclient.Identity, error) { return jc.Whoami(ctx) })
	if err != nil {
		return resolveErr(fmt.Errorf("resolve identity: %w", err))
	}
	gate.Username = who.Username
	jmslog.Printf("-w %d: signed in as %s", port, who.Username)

	if err := tunnel.Run(ctx, tunnel.Options{LocalPort: port, Signer: signer, Gate: gate}); err != nil {
		return connectErr(err)
	}
	return nil
}
