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
// to use (see resolveAccount), and how sign-in is announced (stderr, not a
// modal).
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

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
	// Command is what followed "--". Non-empty runs it instead of opening a
	// shell, and makes the whole run non-interactive (see signIn).
	Command string
	// Timeout bounds Command; 0 means none.
	Timeout time.Duration
}

// maxCommandOutput caps what one remote command may print, stdout and stderr
// together. Enough for any log excerpt worth reading; small enough that an
// unbounded cat cannot flood Koko or an AI's context.
const maxCommandOutput = 1 << 20

// headlessDeps is everything runHeadless needs that isn't specific to one
// asset.
type headlessDeps struct {
	Endpoints config.Endpoints
	JmsdAddr  string
	Identity  string
	K9sPath   string
	CacheTTL  time.Duration
}

// errMFANeedsLogin is what a run that cannot prompt reports when MFA has
// lapsed. It names the fix rather than the condition: the caller is usually
// an AI agent or a script, and the only thing either can do is hand this
// sentence to a human.
var errMFANeedsLogin = errors.New("not signed in, or the 10-day sign-in has lapsed, and this run does not prompt -- " +
	"ask a human to run `jms login`, then retry")

// errUnregisteredKey is what an explicit -i key gets instead of a sign-in.
// Such a key is a service key: jmsd knows it from service_keys or not at all.
// Signing it in with Google would bind it to whoever clicked through -- the
// agent would then act, and be audited, as that person.
var errUnregisteredKey = errors.New("this -i key is not in jmsd's service_keys -- " +
	"ask the jmsd admin to register its public key; `jms login` does not apply to -i keys")

// signIn resolves which JumpServer user this key belongs to, and returns the
// gate every later jmsd call should go through.
//
// interactive decides what a lapsed sign-in does: open the browser, or fail
// with errMFANeedsLogin. An agent such as Claude Code cannot click through a
// browser, so a sign-in there would hang until the tool call times out rather
// than fail.
func signIn(ctx context.Context, identityPath, jmsdAddr string, interactive bool) (*connectflow.Gate, jmsdclient.Identity, error) {
	signer, _, err := sshclient.LoadIdentity(identityPath)
	if err != nil {
		return nil, jmsdclient.Identity{}, resolveErr(err)
	}
	jc := jmsdclient.New(jmsdAddr, signer)
	gate := newGate(ctx, jc, identityPath != "", interactive)

	identity, err := connectflow.WithMFA(gate, func() (jmsdclient.Identity, error) { return jc.Whoami(ctx) })
	if err != nil {
		return nil, identity, resolveErr(fmt.Errorf("resolve identity: %w", err))
	}
	// Known from here on, so later prompts show a name instead of falling
	// back to whatever MFARequiredError happens to carry -- same as the TUI
	// re-building its gate with a username once identify() learns one.
	gate.Username = identity.Username
	return gate, identity, nil
}

// newGate builds the gate for a terminal run. A service key (-i) never signs
// in; otherwise interactive signs in through the browser, and a run that
// cannot prompt fails with errMFANeedsLogin.
func newGate(ctx context.Context, jc *jmsdclient.Client, serviceKey, interactive bool) *connectflow.Gate {
	gate := &connectflow.Gate{JC: jc, Ctx: ctx}
	gate.Authenticate = func(context.Context, string) error { return errMFANeedsLogin }
	switch {
	case serviceKey:
		gate.Authenticate = func(context.Context, string) error { return errUnregisteredKey }
	case interactive:
		gate.Authenticate = func(ctx context.Context, _ string) error {
			if googleClientID == "" {
				return errNoGoogleClient
			}
			return connectflow.GoogleSignIn(ctx, jc, googleClient(), func(url string) {
				fmt.Fprintf(os.Stderr, "jms: sign in to Google in the browser that just opened (needed every 10 days).\n"+
					"     If none opened, visit:\n     %s\n", url)
			})
		}
	}
	return gate
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
	runCommand := target.Command != ""
	gate, identity, err := signIn(ctx, deps.Identity, deps.JmsdAddr, !runCommand)
	if err != nil {
		return err
	}
	jmslog.Printf("-c %s %s: signed in as %s", target.Kind, target.Name, identity.Username)

	assets, cached, err := loadAssets(gate, deps.JmsdAddr, identity.Username, deps.CacheTTL, false)
	if err != nil {
		return resolveErr(err)
	}
	asset, candidates, err := matchAsset(assets, target.Kind, target.Name)
	// A cached listing predates any asset created since it was written, so
	// "not found" from the cache is only worth believing once Core agrees.
	if errors.Is(err, ErrAssetNotFound) && cached {
		if assets, _, err = loadAssets(gate, deps.JmsdAddr, identity.Username, deps.CacheTTL, true); err != nil {
			return resolveErr(err)
		}
		asset, candidates, err = matchAsset(assets, target.Kind, target.Name)
	}
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
	// Checked before the mint, not left to Koko: Koko answers a command on a
	// non-ssh ticket by printing "not ssh asset connection token" to stdout
	// and exiting 0, which a caller would take for the command's output.
	isQuery := slices.Contains(jmsd.QueryProtocols, protocol.Name)
	if runCommand && protocol.Name != "ssh" && !isQuery {
		return resolveErr(fmt.Errorf("%s is reached over %s; running a command needs ssh or one of %s",
			asset.Name, protocol.Name, strings.Join(jmsd.QueryProtocols, ", ")))
	}

	choose := chooseAccountFromTTY
	if runCommand {
		choose = nil // ambiguity becomes "pass --account", never a prompt -- see signIn
	}
	account, err := resolveAccount(asset.Name, detail.Accounts, target.Account, choose)
	if err != nil {
		return resolveErr(err)
	}
	// Echoed even after an interactive choice: this line and the log entry
	// below are the only local record of which identity the session ran as.
	fmt.Fprintf(os.Stderr, "jms: connecting to %s as %s\n", asset.Name, describeAccount(account))
	jmslog.Printf("-c %s: using account=%s (alias=%s) protocol=%s", asset.Name, account.Username, account.Alias, protocol.Name)

	if runCommand && isQuery {
		return runQuery(gate, asset, account, protocol.Name, target.Command)
	}

	isK8s := connectflow.IsK8sProtocol(protocol.Name)
	var k8sPrep *connectflow.K8sPrep
	if isK8s {
		if k8sPrep, err = connectflow.PrepareK8s(gate, deps.K9sPath); err != nil {
			return resolveErr(err)
		}
	}

	ticket, err := connectflow.MintTicket(gate, asset.ID, asset.Name, account.Alias, protocol.Name, target.Command)
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

	if runCommand {
		// The command text is not logged: it can carry a password, and Koko
		// already records it in Core's command log next to this ticket.
		status, silent, err := sshclient.RunCommand(assetClient, target.Command, target.Timeout, maxCommandOutput)
		if err != nil {
			jmslog.Printf("-c %s: command error: %v", asset.Name, err)
			return connectErr(err)
		}
		jmslog.Printf("-c %s: command exited %d (ticket=%s)", asset.Name, status, ticket.ID)
		if status != 0 {
			return remoteExit(status)
		}
		if silent {
			fmt.Fprintln(os.Stderr, "jms: the command printed nothing; if output was expected, "+
				"a JumpServer command-filter rule may have rejected it (Koko reports that as success)")
		}
		return nil
	}

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

// runQuery is "-- command" for a mysql or redis asset: jmsd runs it (Koko
// cannot) and the rows come back as tab-separated text -- `mysql -B` with its
// header line, or one value per line for Redis -- so they pipe into cut/awk
// like any other output.
func runQuery(gate *connectflow.Gate, asset jmsd.AssetRow, account jmsd.AccountOption, protocol, query string) error {
	res, err := connectflow.WithMFA(gate, func() (jmsd.QueryResult, error) {
		return gate.JC.Query(gate.Ctx, asset.ID, account.Alias, protocol, query)
	})
	if err != nil {
		return connectErr(err)
	}
	if res.Error != "" {
		fmt.Fprintln(os.Stderr, "ERROR:", res.Error)
		return remoteExit(1) // what the mysql client exits with
	}
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	if len(res.Columns) > 0 { // Redis replies have none: one value per line, as redis-cli --raw
		fmt.Fprintln(w, strings.Join(res.Columns, "\t"))
	}
	for _, row := range res.Rows {
		cells := make([]string, len(row))
		for i, v := range row {
			cells[i] = "NULL"
			if v != nil {
				cells[i] = tsvEscaper.Replace(*v)
			}
		}
		fmt.Fprintln(w, strings.Join(cells, "\t"))
	}
	if res.Truncated {
		fmt.Fprintf(os.Stderr, "jms: result truncated at %d rows; add a LIMIT or narrow the WHERE\n", len(res.Rows))
	}
	return nil
}

// tsvEscaper keeps one row per line and one cell per tab, as mysql -B does.
var tsvEscaper = strings.NewReplacer("\\", "\\\\", "\t", "\\t", "\n", "\\n")
