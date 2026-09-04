// Headless connect: the "-c TYPE NAME" shortcut (docs/PLAN.md §8.1).
//
// runHeadless and the TUI's connectResource are two orchestrations over one
// set of steps -- internal/connectflow -- not one shared function. Every step
// that talks to jmsd or Koko lives there, so both leave identical tickets,
// logs, and audit trails; the sequencing around them is each caller's own, and
// a new step has to be added to both.
//
// What differs here is only where a human would otherwise be asked something:
// which asset a name means (exact match or refuse, never guess), which account
// to use (see resolveAccount), and how an MFA code is collected (a tty read,
// not a modal).
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/leiyupeng/jumpserver-cli/internal/config"
	"github.com/leiyupeng/jumpserver-cli/internal/connectflow"
	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
	"github.com/leiyupeng/jumpserver-cli/internal/jmsdclient"
	"github.com/leiyupeng/jumpserver-cli/internal/jmslog"
	"github.com/leiyupeng/jumpserver-cli/internal/k8sclient"
	"github.com/leiyupeng/jumpserver-cli/internal/sshclient"
)

// headlessTarget is what "-c TYPE NAME" plus an optional --account resolved
// to.
type headlessTarget struct {
	Kind    jmsd.ResourceKind
	Name    string
	Account string
}

// headlessDeps is everything runHeadless needs that isn't specific to one
// asset.
type headlessDeps struct {
	Endpoints config.Endpoints
	JmsdAddr  string
	Identity  string
	K9sPath   string
}

// runHeadless resolves target to exactly one asset+account+protocol,
// connects, and runs the session -- no TUI is ever started. Every step past
// "which asset" reuses internal/connectflow, the same pipeline
// connectResource drives.
func runHeadless(ctx context.Context, target headlessTarget, deps headlessDeps) error {
	hostKey, err := sshclient.KnownHostsCallback()
	if err != nil {
		return resolveErr(err)
	}
	signer, _, err := sshclient.LoadIdentity(deps.Identity)
	if err != nil {
		return resolveErr(err)
	}
	jc := jmsdclient.New(deps.JmsdAddr, signer)

	gate := &connectflow.Gate{JC: jc, Ctx: ctx, Prompt: promptTOTPFromTTY}

	identity, err := connectflow.WithMFA(gate, func() (jmsdclient.Identity, error) { return jc.Whoami(ctx) })
	if err != nil {
		return resolveErr(fmt.Errorf("resolve identity: %w", err))
	}
	// Known from here on, so later prompts show a name instead of falling
	// back to whatever MFARequiredError happens to carry -- same as the TUI
	// re-building its gate with a username once identify() learns one.
	gate.Username = identity.Username
	jmslog.Printf("-c %s %s: signed in as %s", target.Kind, target.Name, identity.Username)

	assets, err := connectflow.WithMFA(gate, func() ([]jmsd.AssetRow, error) { return jc.Assets(ctx) })
	if err != nil {
		return resolveErr(fmt.Errorf("load assets: %w", err))
	}
	asset, candidates, err := matchAsset(assets, target.Kind, target.Name)
	if err != nil {
		return resolveErr(describeAssetMatchFailure(err, target, candidates))
	}
	jmslog.Debugf("-c %s: matched asset id=%s category=%s type=%s", target.Name, asset.ID, asset.Category, asset.Type)

	detail, err := connectflow.FetchDetail(gate, asset.ID, asset.Name)
	if err != nil {
		return resolveErr(err)
	}

	var protocol jmsd.ProtocolOption
	if connectflow.IsK8sAsset(asset.Category, asset.Type) {
		protocol, err = connectflow.RequireK8sProtocol(detail.Protocols)
	} else if p, ok := connectflow.SelectProtocol(detail.Protocols); ok {
		protocol, err = p, nil
	} else {
		err = fmt.Errorf("ambiguous protocol for %s -- pass one explicitly is not supported yet (options: %s)",
			asset.Name, describeProtocols(detail.Protocols))
	}
	if err != nil {
		return resolveErr(err)
	}

	account, err := resolveAccount(asset.Name, detail.Accounts, target.Account, chooseAccountFromTTY)
	if err != nil {
		return resolveErr(err)
	}
	// Echoed even after an interactive choice: this line and the log entry
	// below are the only local record of which identity the session ran as.
	fmt.Fprintf(os.Stderr, "jms: connecting to %s as %s\n", asset.Name, describeAccount(account))
	jmslog.Printf("-c %s: using account=%s (alias=%s) protocol=%s", asset.Name, account.Username, account.Alias, protocol.Name)

	isK8s := connectflow.IsK8sProtocol(protocol.Name)
	var k8sPrep *connectflow.K8sPrep
	if isK8s {
		if k8sPrep, err = connectflow.PrepareK8s(gate, deps.K9sPath); err != nil {
			return resolveErr(err)
		}
	}

	ticket, err := connectflow.MintTicket(gate, asset.ID, asset.Name, account.Alias, protocol.Name)
	if err != nil {
		return connectErr(err)
	}
	assetClient, err := connectflow.DialWithTicket(ctx, deps.Endpoints, hostKey, ticket, asset.Name)
	if err != nil {
		return connectErr(err)
	}
	defer assetClient.Close()

	// Same pair of lines connectResource logs around a session, and for the
	// same reason: jmsd only ever sees the ticket minted, so without these
	// nothing on this side records that a session ended, or when.
	jmslog.Printf("-c %s: session open (account=%s protocol=%s ticket=%s)", asset.Name, account.Alias, protocol.Name, ticket.ID)
	defer jmslog.Printf("-c %s: session closed (ticket=%s)", asset.Name, ticket.ID)

	var sessionErr error
	if isK8s {
		sessionErr = connectflow.RunK9s(ctx, assetClient, connectflow.K9sRunDeps{
			AssetName: asset.Name,
			K8sCfg:    k8sPrep.Config,
			Phase:     func(text string) { fmt.Fprintln(os.Stderr, "jms:", text) },
			Exec: func(runCtx context.Context, kubeconfigPath string) error {
				return k8sclient.Run(runCtx, k8sPrep.K9sPath, kubeconfigPath)
			},
		})
	} else {
		sessionErr = sshclient.RunInteractiveShell(assetClient)
	}
	if sessionErr != nil {
		jmslog.Printf("-c %s: session error: %v", asset.Name, sessionErr)
		return connectErr(sessionErr)
	}
	return nil
}

func describeAssetMatchFailure(err error, target headlessTarget, candidates []jmsd.AssetRow) error {
	if errors.Is(err, ErrAssetAmbiguous) {
		return fmt.Errorf("-c %s %s: %w -- candidates: %s", target.Kind, target.Name, err, describeAssets(candidates))
	}
	return fmt.Errorf("-c %s %s: %w", target.Kind, target.Name, err)
}

func describeProtocols(options []jmsd.ProtocolOption) string {
	names := make([]string, len(options))
	for i, p := range options {
		names[i] = p.Name
	}
	return strings.Join(names, ", ")
}

// promptTOTPFromTTY asks for a TOTP code directly on the controlling
// terminal, bypassing stdin/stdout so it works the same whether or not those
// are redirected. Unix only (/dev/tty) -- jms has no Windows build target
// today.
//
// There is deliberately no --totp flag: a code is only valid for the ~30
// seconds around when it was generated, so one written into a command line
// would need to be typed and run within that window anyway, while also
// landing in shell history -- worse on both counts than typing it here.
func promptTOTPFromTTY(_ context.Context, username, errMsg string) (string, error) {
	if errMsg != "" {
		fmt.Fprintln(os.Stderr, "jms:", errMsg)
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("open terminal for MFA prompt: %w", err)
	}
	defer tty.Close()

	fmt.Fprintf(tty, "%s · MFA code: ", username)
	code, err := term.ReadPassword(int(tty.Fd()))
	fmt.Fprintln(tty)
	if err != nil {
		return "", fmt.Errorf("read MFA code: %w", err)
	}
	return strings.TrimSpace(string(code)), nil
}
