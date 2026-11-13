package connectflow

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/crypto/ssh"

	"github.com/leiyupeng/jumpserver-cli/internal/config"
	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
	"github.com/leiyupeng/jumpserver-cli/internal/jmslog"
	"github.com/leiyupeng/jumpserver-cli/internal/k8sclient"
	"github.com/leiyupeng/jumpserver-cli/internal/sshclient"
)

// FetchDetail asks jmsd which accounts and Koko-compatible protocols the
// caller may use on assetID, and validates the answer is usable at all: an
// asset jmsd will not name a protocol or an account for cannot be connected
// to, whichever picking policy the caller applies next.
func FetchDetail(gate *Gate, assetID, assetName string) (jmsd.AssetDetail, error) {
	donePhase := jmslog.Phase("connect %s: asset detail", assetName)
	detail, err := WithMFA(gate, func() (jmsd.AssetDetail, error) {
		return gate.JC.AssetDetail(gate.Ctx, assetID)
	})
	donePhase(err)
	if err != nil {
		return jmsd.AssetDetail{}, fmt.Errorf("load asset detail failed: %w", err)
	}
	if len(detail.Protocols) == 0 {
		return jmsd.AssetDetail{}, fmt.Errorf("no compatible protocol for %s", assetName)
	}
	if len(detail.Accounts) == 0 {
		return jmsd.AssetDetail{}, fmt.Errorf("no permitted account for %s", assetName)
	}
	return detail, nil
}

// K8sPrep is what PrepareK8s resolves before any ticket is minted.
type K8sPrep struct {
	K9sPath string
	Config  jmsd.K8sConfigResponse
}

// PrepareK8s resolves everything a k8s connect needs that isn't the ticket
// itself: the local k9s binary and jmsd's proxy configuration.
//
// k9s is located FIRST, before any network call. Once a mint has happened Koko
// has opened a session Core will audit, so a laptop without k9s installed used
// to burn a real ticket and leave an audit entry before failing on something
// knowable up front. Folding the check in here is what stops a future caller
// getting that ordering wrong by forgetting to call Locate.
func PrepareK8s(gate *Gate, k9sPathOverride string) (*K8sPrep, error) {
	k9sPath, err := k8sclient.Locate(k9sPathOverride)
	if err != nil {
		return nil, err
	}
	// Through the gate like every other signed call: /v1/k8s-config sits behind
	// requireSignature too, so an MFA lapse landing between the asset-detail
	// call and this one must prompt rather than dead-end the connect.
	cfg, err := WithMFA(gate, func() (jmsd.K8sConfigResponse, error) {
		return gate.JC.K8sConfig(gate.Ctx)
	})
	if err != nil {
		return nil, fmt.Errorf("load k8s config failed: %w", err)
	}
	if cfg.ProxyURL == "" {
		return nil, errors.New("jmsd has no k8s.proxy_url configured")
	}
	return &K8sPrep{K9sPath: k9sPath, Config: cfg}, nil
}

// MintTicket mints a connection ticket for one asset+account+protocol
// (docs/PLAN.md §4 阶段3).
func MintTicket(gate *Gate, assetID, assetName, account, protocol, command string) (jmsd.ConnectionTicket, error) {
	donePhase := jmslog.Phase("connect %s: mint ticket", assetName)
	ticket, err := WithMFA(gate, func() (jmsd.ConnectionTicket, error) {
		return gate.JC.Connect(gate.Ctx, assetID, account, protocol, command)
	})
	donePhase(err)
	if err != nil {
		return jmsd.ConnectionTicket{}, fmt.Errorf("mint connection ticket failed: %w", err)
	}
	return ticket, nil
}

// DialWithTicket dials Koko authenticated with an already-minted connection
// ticket. ticket.Value is the credential and never appears in a log line --
// only ticket.ID does, which is also what jmsd logged when it minted this, so
// a client-side failure can still be tied back to jmsd's own line.
func DialWithTicket(ctx context.Context, endpoints config.Endpoints, hostKey ssh.HostKeyCallback, ticket jmsd.ConnectionTicket, assetName string) (*ssh.Client, error) {
	donePhase := jmslog.Phase("connect %s: koko ssh %s ticket=%s", assetName, endpoints.KokoAddr, ticket.ID)
	client, err := sshclient.ConnectGatewayWithToken(ctx, sshclient.TokenGatewayOptions{
		Address: endpoints.KokoAddr, TokenID: ticket.ID, TokenValue: ticket.Value, HostKey: hostKey,
	})
	donePhase(err)
	if err != nil {
		return nil, fmt.Errorf("connect failed: %w", err)
	}
	return client, nil
}

// K9sRunDeps is what RunK9s needs beyond the already-open Koko session.
type K9sRunDeps struct {
	AssetName string
	K8sCfg    jmsd.K8sConfigResponse
	// Phase reports a coarse progress line -- the two waits in a k8s connect
	// that are otherwise indistinguishable from a frozen screen. The TUI routes
	// it to its connecting modal; a headless caller prints it to stderr. Nil is
	// fine when the caller has nowhere to put it.
	Phase func(text string)
	// Exec runs k9s (or a test double) against kubeconfigPath, taking over
	// however much of the terminal the caller owns. The TUI wraps this in
	// app.Suspend plus a placeholder screen; a headless caller, never having
	// taken an alternate screen, calls k8sclient.Run directly.
	Exec func(runCtx context.Context, kubeconfigPath string) error
}

// RunK9s holds the Koko session that owns the k8s proxy, reads back and
// rewrites its kubeconfig, and hands off to deps.Exec. It watches the Koko
// session for the rest of the run: if Koko drops it (restart, idle timeout,
// network blip) the proxy k9s is talking to is already dead, so this cancels
// runCtx rather than leave a hung k9s sitting blank against a dead API server.
func RunK9s(ctx context.Context, sshClient *ssh.Client, deps K9sRunDeps) error {
	// The longest wait in the flow and the one that looks most like a freeze:
	// Koko allows itself up to 5 minutes to reach the real cluster and prints
	// nothing this client can show.
	if deps.Phase != nil {
		deps.Phase("waiting for koko's kubectl session…")
	}
	donePhase := jmslog.Phase("k9s %s: koko kubectl session", deps.AssetName)
	session, rawConfig, err := k8sclient.Open(ctx, sshClient)
	donePhase(err)
	if err != nil {
		return err
	}
	defer session.Close()

	// Length only, never contents: rawConfig holds Koko's real bearer token.
	jmslog.Debugf("k9s %s: kubeconfig %d bytes", deps.AssetName, len(rawConfig))
	path, cleanup, err := k8sclient.WriteTemp(rawConfig, deps.K8sCfg.ProxyURL, deps.AssetName, deps.K8sCfg.InsecureSkipVerify)
	if err != nil {
		// rawConfig must never be written anywhere but WriteTemp's own private,
		// cleaned-up temp dir (docs/K9S_LOCAL_PROXY_PLAN.md §8), so this
		// deliberately does not dump it on failure.
		jmslog.Printf("k9s %s: rewrite kubeconfig: %v", deps.AssetName, err)
		return err
	}
	defer cleanup()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	lost := make(chan error, 1)
	go func() {
		select {
		case err := <-session.Done():
			lost <- err
			cancel()
		case <-runCtx.Done():
		}
	}()

	if deps.Phase != nil {
		deps.Phase("starting k9s…")
	}
	donePhase = jmslog.Phase("k9s %s: run", deps.AssetName)
	runErr := deps.Exec(runCtx, path)
	donePhase(runErr)

	// A session that died out from under k9s is the real error; runErr in that
	// case is just the "signal: killed" from cancelling it, which says nothing
	// about why.
	select {
	case err := <-lost:
		jmslog.Printf("k9s %s: koko session ended first: %v", deps.AssetName, err)
		if err != nil {
			return fmt.Errorf("koko ended the session, so k9s was stopped: %w", err)
		}
		return errors.New("koko ended the session, so k9s was stopped")
	default:
		return runErr
	}
}
