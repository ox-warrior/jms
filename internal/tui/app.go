package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"golang.org/x/crypto/ssh"

	"github.com/leiyupeng/jumpserver-cli/internal/assetcache"
	"github.com/leiyupeng/jumpserver-cli/internal/clipboard"
	"github.com/leiyupeng/jumpserver-cli/internal/config"
	"github.com/leiyupeng/jumpserver-cli/internal/connectflow"
	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
	"github.com/leiyupeng/jumpserver-cli/internal/jmsdclient"
	"github.com/leiyupeng/jumpserver-cli/internal/jmslog"
	"github.com/leiyupeng/jumpserver-cli/internal/sshclient"
	"github.com/leiyupeng/jumpserver-cli/internal/version"
)

type Options struct {
	Server   string
	SSHPort  int
	Identity string
	JmsdAddr string
	Theme    string
	// CacheTTL is how long a locally cached asset listing is reused before
	// Core is read again. Zero or less disables the cache; "r" bypasses it
	// regardless.
	CacheTTL time.Duration
	K9sPath  string
	// InitialKind pre-selects a resource kind at launch -- the "-c TYPE" shortcut
	// with no name (docs/PLAN.md §8.1). Empty means the ordinary "all" start.
	InitialKind ResourceKind
}

type UI struct {
	app       *tview.Application
	pages     *tview.Pages
	opts      Options
	attemptMu sync.Mutex
	attemptID uint64
	// connectingPhase is the connecting modal's middle line, held here so a
	// slow connect can say which step it is on without rebuilding the modal.
	// Only ever touched on the tview goroutine (showConnecting, hideConnecting,
	// and setConnectingPhaseAsync's queued func), so it needs no lock.
	connectingPhase *tview.TextView
	// workspace is the active workspace, set in resourcePage. Needed by
	// hideConnecting to restore keyboard focus after the modal is removed --
	// tview's Pages.RemovePage does not restore focus to the underlying page
	// when the removed page held focus, so without this the focus falls to
	// the first focusable element (the tree) instead of the last-used panel.
	workspace *Workspace
}

func Run(opts Options) error {
	if opts.Theme == "" {
		opts.Theme = DefaultTheme
	}
	resolved, err := resolveTheme(opts.Theme)
	if err != nil {
		return err
	}
	// Both must happen before the first primitive is constructed: primitives
	// snapshot tview.Styles at construction time.
	theme = resolved
	applyGlobalStyles(theme)

	u := &UI{app: tview.NewApplication(), pages: tview.NewPages(), opts: opts}
	u.pages.AddPage("starting", u.startingPage(), true, true)
	u.app.SetRoot(u.pages, true).EnableMouse(true)
	return u.app.Run()
}

// startingPage is the very first thing shown. There is no username field: the
// caller is identified by their own SSH key (see internal/jmsdclient's
// package doc), so the only thing to do at startup is resolve that key and
// ask jmsd whose it is. identify() kicks off immediately -- pressing 'r'
// retries it after a failure, since there's no input to correct and retry.
func (u *UI) startingPage() tview.Primitive {
	message := tview.NewTextView().SetDynamicColors(true)
	message.SetTextAlign(tview.AlignCenter).SetText(tag(theme.Warn) + "Resolving identity…[-]")
	hint := tview.NewTextView().SetDynamicColors(true).SetTextAlign(tview.AlignCenter)

	panel := tview.NewGrid().SetRows(1, 1).SetColumns(0)
	// jms's own version, not JumpServer's. This is the first line every user
	// sees, and titling it with the JumpServer release made the tool read as
	// something pinned to that release rather than a client with a life of its
	// own -- the compatibility baseline belongs in `jms version`, not here.
	panel.SetBorder(true).SetTitle(fmt.Sprintf(" jms %s ", version.Version))
	panel.AddItem(message, 0, 0, 1, 1, 0, 0, false)
	panel.AddItem(hint, 1, 0, 1, 1, 0, 0, false)
	panel.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyRune && event.Rune() == 'r' {
			message.SetText(tag(theme.Warn) + "Resolving identity…[-]")
			hint.SetText("")
			attemptID := u.startLoginAttempt()
			go u.identify(attemptID, message, hint)
			return nil
		}
		return event
	})

	attemptID := u.startLoginAttempt()
	go u.identify(attemptID, message, hint)
	return center(72, 7, panel)
}

func (u *UI) startLoginAttempt() uint64 {
	u.attemptMu.Lock()
	defer u.attemptMu.Unlock()
	u.attemptID++
	return u.attemptID
}

func (u *UI) isCurrentAttempt(attemptID uint64) bool {
	u.attemptMu.Lock()
	defer u.attemptMu.Unlock()
	return u.attemptID == attemptID
}

// identify resolves everything needed to talk to jmsd and, later, to Koko --
// but does not itself open an SSH session. jms used to log into Koko
// directly at this point (triggering Koko's own login-time MFA challenge),
// but nothing past login ever used that session: asset browsing goes
// through jmsd over HTTP, and connecting to a specific asset dials Koko
// fresh with a jmsd-minted ticket (docs/PLAN.md §5.A.1-2, which bypasses MFA
// entirely). Keeping the login-time Koko connection around only bought a
// second, redundant MFA prompt for no functional benefit, so it's gone --
// see docs/PLAN.md §11.
//
// The username isn't known yet at this point -- only the caller's SSH key
// is. jmsd resolves that key's fingerprint to a JumpServer username via
// Whoami, which is itself a signed request and so can require MFA on the
// very first call, same as any other endpoint. The gate below is created
// with no username for exactly that reason: it takes the one jmsd hands back
// on the 401.
func (u *UI) identify(attemptID uint64, message, hint *tview.TextView) {
	endpoints, err := config.ResolveServer(u.opts.Server, u.opts.SSHPort)
	if err != nil {
		jmslog.Printf("startup: resolve server: %v", err)
		u.setAttemptMessage(attemptID, message, hint, err)
		return
	}
	hostKey, err := sshclient.KnownHostsCallback()
	if err != nil {
		jmslog.Printf("startup: known_hosts: %v", err)
		u.setAttemptMessage(attemptID, message, hint, err)
		return
	}
	signer, keyPath, err := sshclient.LoadIdentity(u.opts.Identity)
	if err != nil {
		jmslog.Printf("startup: load identity: %v", err)
		u.setAttemptMessage(attemptID, message, hint, err)
		return
	}
	// The fingerprint is how jmsd identifies this caller, so it is the first
	// thing to compare against jmsd's log when a login is refused. It is a
	// hash of a public key, not a secret.
	jmslog.Debugf("startup: identity %s (%s), koko=%s",
		keyPath, ssh.FingerprintSHA256(signer.PublicKey()), endpoints.KokoAddr)

	ctx := context.Background()
	jc := jmsdclient.New(u.opts.JmsdAddr, signer)
	identity, err := connectflow.WithMFA(u.gate(ctx, jc, ""), func() (jmsdclient.Identity, error) { return jc.Whoami(ctx) })
	if err != nil {
		jmslog.Printf("startup: whoami: %v", err)
		u.setAttemptMessage(attemptID, message, hint, err)
		return
	}
	username := identity.Username
	remote := buildInfo{
		version:            identity.ServerVersion,
		jumpServerBaseline: identity.JumpServerBaseline,
	}
	jmslog.Printf("startup: signed in as %s (jms %s / JumpServer %s, jmsd %s / JumpServer %s)",
		username, version.Version, version.JumpServerBaseline,
		describeServerVersion(remote.version), describeServerVersion(remote.jumpServerBaseline))

	notice := updateNotice(localBuild(), remote)
	if notice != "" {
		// Logged as well as shown: the header line is one row of a TUI that
		// the user may never look twice at, and "which version was I actually
		// running" is the first question any report about odd behaviour needs
		// answered.
		jmslog.Printf("startup: %s", notice)
	}

	u.app.QueueUpdateDraw(func() {
		if !u.isCurrentAttempt(attemptID) {
			return
		}
		u.pages.AddAndSwitchToPage("resources", u.resourcePage(username, jc, endpoints, hostKey, notice), true)
	})
}

// buildInfo is what one side of the connection declares about itself.
type buildInfo struct {
	// version is the jms/jmsd build version.
	version string
	// jumpServerBaseline is the JumpServer release that side was verified
	// against. Declared independently on each side (see
	// jmsd.JumpServerBaseline) precisely so the two can disagree.
	jumpServerBaseline string
}

// localBuild is what this jms declares about itself.
func localBuild() buildInfo {
	return buildInfo{version: version.Version, jumpServerBaseline: version.JumpServerBaseline}
}

// updateNotice compares this build against the jmsd answering and returns the
// line to show the user, or "" when there is nothing to say.
//
// Two things are checked, and both land in the same notice because they are
// the same message to the user: the halves of this system are out of step.
//
//  1. The build version. jms and jmsd ship from one repo and are released
//     together, so a difference means the two binaries came from different
//     builds -- in practice a laptop whose jms was never re-copied after jmsd
//     was upgraded. Nothing in this story is a package manager, so nobody
//     notices that on the user's behalf.
//  2. The JumpServer baseline each side was verified against. Listed second
//     because it is the less actionable of the two.
//
// An empty value from jmsd means it predates that field and simply cannot
// answer, which is reported as nothing at all. Warning on the strength of a
// value that was never sent would be a false alarm on every single launch
// against an older daemon -- and a notice that cries wolf every time is one
// nobody reads when it finally matters.
func updateNotice(local, remote buildInfo) string {
	var problems []string
	if remote.version != "" && remote.version != local.version {
		problems = append(problems,
			fmt.Sprintf("jms 需更新:本地 %s / jmsd %s", local.version, remote.version))
	}
	if remote.jumpServerBaseline != "" && remote.jumpServerBaseline != local.jumpServerBaseline {
		problems = append(problems,
			fmt.Sprintf("JumpServer 基线不一致:jms %s / jmsd %s",
				local.jumpServerBaseline, remote.jumpServerBaseline))
	}
	return strings.Join(problems, " · ")
}

// describeServerVersion renders a version for the log, naming the one case an
// empty string could otherwise be mistaken for -- a failed read rather than a
// daemon too old to have the field.
func describeServerVersion(serverVersion string) string {
	if serverVersion == "" {
		return "(version not reported)"
	}
	return serverVersion
}

func (u *UI) resourcePage(username string, jc *jmsdclient.Client, endpoints config.Endpoints, hostKey ssh.HostKeyCallback, updateNotice string) tview.Primitive {
	var workspace *Workspace
	workspace = NewWorkspace(u.app, username, WorkspaceActions{
		Load: func(query ResourceQuery) {
			go u.loadWorkspace(query, username, jc, workspace)
		},
		OpenResource: func(selection ResourceSelection) {
			go u.connectResource(selection, username, jc, endpoints, hostKey, workspace)
		},
		SetFavorite: func(assetID string, starred bool) {
			go u.setFavorite(assetID, starred, username, jc, workspace)
		},
		// Synchronous, unlike the three above: pbcopy returns in single-digit
		// milliseconds, and the status line has to say "copied" or why not
		// before the next redraw rather than land some frames later.
		CopyName: clipboard.Copy,
	})
	u.workspace = workspace
	// Before requestLoad: SetInitialKind's own contract is "before the first
	// load", and nothing here depends on the order relative to the two calls
	// below.
	workspace.SetInitialKind(u.opts.InitialKind)
	// Set before the first setStatus, since that is what paints the header the
	// notice rides in.
	workspace.SetUpdateNotice(updateNotice)
	workspace.setStatus(username, statusBusy, "loading…")
	go u.loadMarks(username, jc, workspace)
	workspace.requestLoad()
	return workspace.Primitive()
}

// loadWorkspace fills the scope tree and the asset table, from the local
// cache when it is still fresh and from Core otherwise.
//
// The two are loaded and cached together on purpose: the tree's per-node
// counts are computed from the asset list, so a tree from one fetch paired
// with assets from another would show counts that don't match the table.
//
// query.Refresh (the "r" key) bypasses the cache. Everything else -- startup
// included -- takes it, which is what makes a relaunch inside the TTL open
// without touching the network at all.
func (u *UI) loadWorkspace(query ResourceQuery, username string, jc *jmsdclient.Client, workspace *Workspace) {
	if !query.Refresh {
		if entry := assetcache.Load(u.cacheKey(), username, u.opts.CacheTTL); entry != nil {
			workspace.QueueScopeNodes(convertScopeNodes(entry.Nodes))
			workspace.QueueLoadResult(query, convertResourceRows(entry.Assets), username, statusOK,
				fmt.Sprintf("ready · cached %s ago", entry.Age().Round(time.Minute)))
			return
		}
	}

	ctx := query.Context
	gate := u.gate(ctx, jc, username)
	nodes, err := connectflow.WithMFA(gate, func() ([]jmsd.NodeTree, error) { return jc.Nodes(ctx) })
	if err != nil {
		jmslog.Printf("workspace: load nodes: %v", err)
		workspace.QueueLoadResult(query, nil, username, statusFailed, "load scope failed: "+tview.Escape(err.Error()))
		return
	}
	assets, err := connectflow.WithMFA(gate, func() ([]jmsd.AssetRow, error) { return jc.Assets(ctx) })
	if err != nil {
		jmslog.Printf("workspace: load assets: %v", err)
		workspace.QueueLoadResult(query, nil, username, statusFailed, "load failed: "+tview.Escape(err.Error()))
		return
	}
	jmslog.Debugf("workspace: loaded %d nodes, %d assets from jmsd (refresh=%t)", len(nodes), len(assets), query.Refresh)

	// Cached only after both succeed, so a half-populated pair is never
	// written. A failure here costs a slow next launch, nothing more.
	level, status := statusOK, "ready"
	if err := assetcache.Save(u.cacheKey(), username, nodes, assets); err != nil {
		jmslog.Printf("asset cache: write failed: %v", err)
		level, status = statusFailed, "ready · cache write failed: "+tview.Escape(err.Error())
	}
	workspace.QueueScopeNodes(convertScopeNodes(nodes))
	workspace.QueueLoadResult(query, convertResourceRows(assets), username, level, status)
}

// loadMarks fetches the caller's starred and recently-connected assets. Both
// live in Core, so they are the same set the JumpServer web console shows and
// they follow the user to any machine.
//
// A failure here is not fatal: it costs the Favorites and Recent scopes their
// contents, but every other scope still works, so it reports through the
// status line rather than blocking the workspace.
func (u *UI) loadMarks(username string, jc *jmsdclient.Client, workspace *Workspace) {
	ctx := context.Background()
	marks, err := connectflow.WithMFA(u.gate(ctx, jc, username), func() (jmsd.MarksResponse, error) { return jc.Marks(ctx) })
	if err != nil {
		u.setWorkspaceStatus(workspace, username, statusFailed, "load stars failed: "+tview.Escape(err.Error()))
		return
	}
	workspace.QueueMarks(marks.Favorites, marks.Recent)
}

// setFavorite writes a star change through to Core. The UI has already
// updated optimistically, so the only thing left to do here is put it back if
// the write fails.
func (u *UI) setFavorite(assetID string, starred bool, username string, jc *jmsdclient.Client, workspace *Workspace) {
	ctx := context.Background()
	_, err := connectflow.WithMFA(u.gate(ctx, jc, username), func() (struct{}, error) {
		return struct{}{}, jc.SetFavorite(ctx, assetID, starred)
	})
	if err != nil {
		workspace.RevertFavorite(assetID, !starred, tview.Escape(err.Error()))
	}
}

func convertScopeNodes(nodes []jmsd.NodeTree) []ScopeNode {
	out := make([]ScopeNode, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, ScopeNode{ID: n.ID, Name: n.Name, Children: convertScopeNodes(n.Children)})
	}
	return out
}

func convertResourceRows(assets []jmsd.AssetRow) []ResourceRow {
	out := make([]ResourceRow, 0, len(assets))
	for _, a := range assets {
		out = append(out, ResourceRow{
			ID:       a.ID,
			Name:     a.Name,
			Address:  a.Address,
			Category: a.Category,
			Type:     a.Type,
			NodeIDs:  a.NodeIDs,
		})
	}
	return out
}

func (u *UI) setAttemptMessage(attemptID uint64, view, hint *tview.TextView, err error) {
	u.app.QueueUpdateDraw(func() {
		if u.isCurrentAttempt(attemptID) {
			view.SetText(tag(theme.Danger) + tview.Escape(err.Error()) + "[-]")
			hint.SetText(tag(theme.Muted) + "r to retry[-]")
		}
	})
}

// cacheKey identifies which listing a cache file holds, alongside the
// username. It is the jmsd address rather than --server because jmsd is where
// the assets come from: pointing --jmsd-addr at a local build while leaving
// --server alone -- the exact shape of a debugging session -- used to serve
// production's cached inventory to the dev daemon's user and vice versa.
func (u *UI) cacheKey() string { return u.opts.JmsdAddr }

// setWorkspaceStatus is the goroutine-safe way to update the status line:
// setStatus itself touches tview primitives, so any call from outside the
// tview event goroutine (i.e. everywhere in this file except direct event
// callbacks) must go through QueueUpdateDraw.
func (u *UI) setWorkspaceStatus(workspace *Workspace, username string, level statusLevel, status string) {
	u.app.QueueUpdateDraw(func() { workspace.setStatus(username, level, status) })
}
