package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/rivo/tview"
	"golang.org/x/crypto/ssh"

	"github.com/leiyupeng/jumpserver-cli/internal/config"
	"github.com/leiyupeng/jumpserver-cli/internal/connectflow"
	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
	"github.com/leiyupeng/jumpserver-cli/internal/jmsdclient"
	"github.com/leiyupeng/jumpserver-cli/internal/jmslog"
	"github.com/leiyupeng/jumpserver-cli/internal/k8sclient"
	"github.com/leiyupeng/jumpserver-cli/internal/sshclient"
)

// connectResource is the OpenResource action (docs/PLAN.md §4 阶段3): it asks
// jmsd which accounts/protocols the caller may use on the selected asset,
// resolves both (auto-picking when there's only one option, prompting via
// pickFrom otherwise), mints a connection ticket, and dials Koko with it.
// Runs on a background goroutine -- like loadWorkspace -- since it does
// network I/O and blocks on modal prompts.
func (u *UI) connectResource(selection ResourceSelection, username string, jc *jmsdclient.Client, endpoints config.Endpoints, hostKey ssh.HostKeyCallback, workspace *Workspace) {
	asset := selection.Resource
	// Cancellable so the "Connecting…" modal's Esc can abort at any stage:
	// jmsdclient's postSigned/getSigned use http.NewRequestWithContext, and
	// sshclient.dialAndHandshake watches ctx.Done() to tear down the dial.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	u.showConnectingAsync(asset.Name, cancel)
	// Covers every exit path below -- success, cancel, or any error -- so a
	// later Enter can start a new attempt (openSelectedResource's guard).
	defer u.app.QueueUpdateDraw(func() {
		u.hideConnecting()
		workspace.endConnect()
	})
	// report is for progress ("connecting to X…"), reportFailed for a dead
	// end. Only the second one reaches the log unconditionally: the status bar
	// wants both, the log wants a list of what went wrong.
	report := func(level statusLevel, status string) {
		jmslog.Debugf("connect %s: %s", asset.Name, status)
		u.setWorkspaceStatus(workspace, username, level, status)
	}
	reportFailed := func(status string) {
		jmslog.Printf("connect %s: %s", asset.Name, status)
		u.setWorkspaceStatus(workspace, username, statusFailed, status)
	}
	jmslog.Debugf("connect %s (id=%s category=%s type=%s): start", asset.Name, asset.ID, asset.Category, asset.Type)

	// Anything that asks the user something needs the connecting modal out of
	// the way first and back afterwards -- the account picker, the protocol
	// picker, and the MFA prompt that either signed call below can trigger.
	withoutModal := func(ask func()) {
		u.hideConnectingAsync()
		defer u.showConnectingAsync(asset.Name, cancel)
		ask()
	}
	gate := u.gate(ctx, jc, username)
	gate.Around = func(prompt func() error) (err error) {
		withoutModal(func() { err = prompt() })
		return err
	}

	detail, err := connectflow.FetchDetail(gate, asset.ID, asset.Name)
	if err != nil {
		reportFailed(tview.Escape(err.Error()))
		return
	}

	var protocol jmsd.ProtocolOption
	if connectflow.IsK8sAsset(asset.Category, asset.Type) {
		protocol, err = connectflow.RequireK8sProtocol(detail.Protocols)
	} else {
		withoutModal(func() { protocol, err = u.pickProtocol(ctx, asset.Name, detail.Protocols) })
	}
	if err != nil {
		reportFailed(tview.Escape(err.Error()))
		return
	}
	var account jmsd.AccountOption
	withoutModal(func() { account, err = u.pickAccount(ctx, asset.Name, detail.Accounts) })
	if err != nil {
		// Was "connect cancelled" regardless of err, which erased the
		// difference between Esc on the picker (errPromptCancelled), Esc on
		// the connecting modal (ctx.Err()), and a deadline. The status bar
		// keeps the short phrase -- it has no room for more -- but the log
		// now records which one it actually was.
		jmslog.Printf("connect %s: account selection ended: %v", asset.Name, err)
		report(statusBusy, "connect cancelled")
		return
	}
	jmslog.Debugf("connect %s: account=%s protocol=%s k8s=%t",
		asset.Name, account.Alias, protocol.Name, connectflow.IsK8sProtocol(protocol.Name))

	// The office network's k8s proxy address lives in jmsd's own
	// config-jmsd.yaml, not in a client flag or file -- one operator-set
	// place instead of a setting every laptop has to carry
	// (docs/K9S_LOCAL_PROXY_PLAN.md). Only fetched for k8s assets: it's a
	// no-op round-trip for everything else.
	isK8s := connectflow.IsK8sProtocol(protocol.Name)
	var k8sPrep *connectflow.K8sPrep
	if isK8s {
		if k8sPrep, err = connectflow.PrepareK8s(gate, u.opts.K9sPath); err != nil {
			reportFailed(tview.Escape(err.Error()))
			return
		}
	}

	ticket, err := connectflow.MintTicket(gate, asset.ID, asset.Name, account.Alias, protocol.Name)
	if err != nil {
		reportFailed(tview.Escape(err.Error()))
		return
	}

	report(statusBusy, "connecting to "+asset.Name+"…")
	assetClient, err := connectflow.DialWithTicket(ctx, endpoints, hostKey, ticket, asset.Name)
	if err != nil {
		reportFailed(tview.Escape(err.Error()))
		return
	}
	defer assetClient.Close()

	// Core writes the authoritative terminal_session row for this connection
	// itself -- asset, account, date_start, date_end, is_finished -- which is
	// both what the Recent scope reads and what the audit trail is. These two
	// lines are the local copy: jmsd only ever sees the ticket being minted, so
	// without them nothing on this side records that a session ended, or when.
	jmslog.Printf("connect %s: session open (account=%s protocol=%s ticket=%s)",
		asset.Name, account.Alias, protocol.Name, ticket.ID)
	sessionStart := time.Now()
	defer func() {
		jmslog.Printf("connect %s: session closed after %s (ticket=%s)",
			asset.Name, time.Since(sessionStart).Round(time.Second), ticket.ID)
	}()

	var sessionErr error
	if isK8s {
		// The modal deliberately stays up here: for k8s the SSH handshake is
		// not the last wait but roughly the halfway point (runK9s's own doc
		// comment). runK9s drops it itself, immediately before k9s takes the
		// terminal.
		sessionErr = u.runK9s(ctx, assetClient, asset.Name, k8sPrep.K9sPath, k8sPrep.Config)
	} else {
		// The terminal is about to take over the screen -- the modal must not
		// linger behind it and reappear when the shell session ends.
		u.hideConnectingUpFront()
		u.app.Suspend(func() { sessionErr = sshclient.RunInteractiveShell(assetClient) })
	}
	// Core has just logged this session, so the Recent scope is now one entry
	// out of date -- pick the new one up rather than wait for a relaunch.
	// Runs on either exit path: the session happened regardless.
	go u.loadMarks(username, jc, workspace)
	if sessionErr != nil {
		// The status bar gets two words and the modal gets the full text, but
		// the modal is gone the moment OK is pressed -- this is the only copy
		// that survives the session.
		jmslog.Printf("connect %s: session error: %v", asset.Name, sessionErr)
		reportFailed("session error")
		u.showErrorAsync("Session error", sessionErr.Error())
		return
	}
	report(statusOK, "session ended")
}

// pickProtocol prefers ssh whenever it is on offer, which it usually is
// alongside sftp on the same account -- ssh is what Enter means on a host
// asset. Anything else falls through to pickFrom, which asks (auto-selecting
// a lone option is now connectflow.SelectProtocol's second rule, so it never
// reaches the modal in the first place).
func (u *UI) pickProtocol(ctx context.Context, assetName string, options []jmsd.ProtocolOption) (jmsd.ProtocolOption, error) {
	if p, ok := connectflow.SelectProtocol(options); ok {
		return p, nil
	}
	return pickFrom(u, ctx, assetName+" · choose protocol", options,
		func(p jmsd.ProtocolOption) string { return p.Name })
}

func (u *UI) pickAccount(ctx context.Context, assetName string, options []jmsd.AccountOption) (jmsd.AccountOption, error) {
	return pickFrom(u, ctx, assetName+" · choose account", options,
		func(a jmsd.AccountOption) string { return fmt.Sprintf("%s (%s)", a.Name, a.Username) })
}

// runK9s holds the Koko session that owns the proxy, rewrites its kubeconfig,
// and runs k9s on the user's computer -- the shared mechanics live in
// connectflow.RunK9s (session open, kubeconfig rewrite, lost-session watch);
// this wrapper supplies the TUI-specific handover: routing progress to the
// connecting modal's phase text, and -- immediately before k9s takes the
// terminal -- dropping that modal, painting a placeholder over whatever
// scrollback tview's Suspend uncovers (see holdScreen), and running k9s
// inside app.Suspend so tview's event loop stops touching the screen while it
// has it.
//
// The modal deliberately stays up until Exec runs: for k8s the SSH handshake
// is not the last wait but roughly the halfway point, and closing the modal
// any earlier would leave the screen blank with nothing explaining the wait.
func (u *UI) runK9s(ctx context.Context, assetClient *ssh.Client, assetName, k9sPath string, k8sCfg jmsd.K8sConfigResponse) error {
	jmslog.Debugf("k9s %s: using %s, proxy=%s insecure=%t",
		assetName, k9sPath, k8sCfg.ProxyURL, k8sCfg.InsecureSkipVerify)
	return connectflow.RunK9s(ctx, assetClient, connectflow.K9sRunDeps{
		AssetName: assetName,
		K8sCfg:    k8sCfg,
		Phase:     u.setConnectingPhaseAsync,
		Exec: func(runCtx context.Context, kubeconfigPath string) error {
			// k9s takes the terminal from here, so its own stderr is painted
			// over by its first frame and lost -- RunK9s's own jmslog.Phase
			// around this call is what remains of the run.
			cols, rows := u.screenSize()
			u.hideConnectingUpFront()
			var runErr error
			u.app.Suspend(func() {
				holdScreen(assetName, cols, rows)
				runErr = k8sclient.Run(runCtx, k9sPath, kubeconfigPath)
			})
			return runErr
		},
	})
}

// screenSize reports the terminal's current size, for code that has to draw
// without tview's help. tview gives the root primitive the whole screen on
// every draw, so its rect is that size; there is no exported accessor for the
// tcell screen itself in v0.42. Read on the tview goroutine because that is
// what writes the rect. Zero before the first draw. Deadlocks if called from
// the tview goroutine -- connectResource's background goroutine is the only
// caller, same as hideConnectingUpFront.
func (u *UI) screenSize() (cols, rows int) {
	done := make(chan struct{})
	u.app.QueueUpdate(func() {
		_, _, cols, rows = u.pages.GetRect()
		close(done)
	})
	<-done
	return cols, rows
}

// holdScreen keeps the wait for k9s's first frame inside jms rather than
// dumping the user back to their shell for it.
//
// tview's Suspend leaves the alternate screen on its way out (tcell's
// disengage sends ExitCA), which uncovers whatever scrollback was there
// before jms started. k9s then spends a while talking to the cluster before
// tcell draws anything, so that scrollback is what the user actually looks at
// during the handover -- and it is not a surface jms can write to without
// leaving a line behind in it permanently.
//
// Re-entering the alternate screen here puts a jms-owned page back in front
// for exactly that gap. k9s's own EnterCA is then a no-op (the mode is
// already set) and its first paint simply overwrites this; on exit k9s
// restores the normal screen and tview's resume re-enters. Nothing reaches
// the scrollback either way.
//
// Written as raw escape sequences on purpose: tview's screen is disengaged by
// the time this runs, so there is no primitive left to render with.
func holdScreen(assetName string, cols, rows int) {
	const (
		enterAlt  = "\x1b[?1049h"
		clear     = "\x1b[H\x1b[2J"
		hideCur   = "\x1b[?25l"
		dim       = "\x1b[2m"
		bold      = "\x1b[1m"
		resetAttr = "\x1b[0m"
	)
	var b strings.Builder
	b.WriteString(enterAlt + clear + hideCur)

	title := "starting k9s · " + assetName
	hint := "first screen appears once k9s finishes talking to the cluster"
	// Display width, not len(): the title carries an asset name, and a name
	// with CJK characters in it measures roughly three bytes per two columns --
	// byte lengths there push the line left of centre and, on a narrow
	// terminal, past the edge.
	titleWidth, hintWidth := tview.TaggedStringWidth(title), tview.TaggedStringWidth(hint)
	// Centre when the size is known; fall back to the top-left corner rather
	// than guessing at a width that might wrap.
	if cols > hintWidth+2 && rows > 2 {
		b.WriteString(fmt.Sprintf("\x1b[%d;%dH", rows/2, max(1, (cols-titleWidth)/2+1)))
		b.WriteString(bold + title + resetAttr)
		b.WriteString(fmt.Sprintf("\x1b[%d;%dH", rows/2+2, max(1, (cols-hintWidth)/2+1)))
		b.WriteString(dim + hint + resetAttr)
	} else {
		b.WriteString(bold + title + resetAttr + "\r\n" + dim + hint + resetAttr)
	}
	_, _ = io.WriteString(os.Stdout, b.String())
}
