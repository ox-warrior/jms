// Package tui is jms's terminal interface.
//
// Workspace, the post-login screen, is split across four files by what a
// reader is looking for:
//
//   - workspace.go  -- the type, its layout, focus, header and load plumbing
//   - resources.go  -- the asset table: what is shown, and how wide
//   - scopetree.go  -- the left-hand tree and what each node contains
//   - prompt.go     -- the ":" and "/" input box, and the keys around it
//
// The "d" panel is a view of its own rather than part of the workspace, and
// splits the same way:
//
//   - describe.go     -- the view: its keys, its search box, its clipboard
//   - describetext.go -- HostInfo to text, all pure functions

package tui

import (
	"context"
	"fmt"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
)

// ResourceQuery identifies one generation of the loaded collection. It
// carries no scope/kind/search: one fetch pulls every permitted asset and all
// three are applied locally (see renderResources). The scope tree has to tally
// counts across the WHOLE tree to be useful for navigation, which already
// requires the full set -- fetching a node's subset on top of that would just
// be a redundant round-trip that also breaks those counts.
type ResourceQuery struct {
	Context    context.Context
	Generation uint64
	// Refresh is set only by the explicit "r" action, and means "ignore the
	// local cache and re-read from Core". Startup leaves it false so a cached
	// listing can serve the launch without touching the network.
	Refresh bool
}

// ResourceSelection is the stable connection-layer input for a selected row.
//
// It is a struct around a single field on purpose: it is the boundary between
// the table and the connect flow, and the two have already had to grow a
// second thing to carry across it once.
type ResourceSelection struct {
	Resource ResourceRow
}

// WorkspaceActions connects the UI to asynchronous API and session layers.
type WorkspaceActions struct {
	Load         func(query ResourceQuery)
	OpenResource func(selection ResourceSelection)
	// Describe fetches and shows the selected machine's cloud metadata ("d").
	// Optional -- nil makes the key a no-op rather than an error, which is
	// what keeps the workspace tests from needing a jmsd.
	Describe func(selection ResourceSelection)
	// ShowDescribe opens the panel with metadata already in hand, for the row
	// whose listing said it has none. Separate from Describe so that answer
	// costs no request.
	ShowDescribe func(resource ResourceRow, host *jmsd.HostInfo)
	// SetFavorite records a star change with jmsd, which writes it to Core so
	// it is shared with the web console. Called after the UI has already
	// updated optimistically; report failure by calling back into
	// RevertFavorite. Optional -- nil keeps stars in memory for the session.
	SetFavorite func(assetID string, starred bool)
	// CopyName puts an asset name on the system clipboard, for the
	// double-click gesture on the resource table. Optional -- nil disables the
	// gesture entirely rather than failing at the moment it is used, which is
	// also what keeps "go test" from touching a real clipboard.
	CopyName func(name string) error
}

// Workspace is the screen displayed after Koko authentication succeeds.
// It owns presentation state only; API calls and connections remain outside it.
type Workspace struct {
	app     *tview.Application
	actions WorkspaceActions

	root       *tview.Flex
	header     *tview.TextView
	tree       *tview.TreeView
	table      *tview.Table
	footer     *tview.TextView
	command    *tview.InputField
	commandRow *tview.Flex

	username string
	// status is the last status line shown and statusColor the verdict its
	// call site's wording earned (see setStatus). Both are held so a header
	// repaint with no status of its own (SetUpdateNotice) does not blank it,
	// and so the colour is not re-derived from text that by then includes
	// interpolated data.
	status         string
	statusColor    tcell.Color
	connecting     bool
	connectingName string
	// updateNotice is the version-mismatch line shown at the end of the
	// header, empty when this jms matches the jmsd it is talking to.
	updateNotice  string
	kind          ResourceKind
	scopeID       string
	resources     []ResourceRow
	visible       []ResourceRow
	selectedID    string
	generation    uint64
	loadCancel    context.CancelFunc
	focusTree     bool
	commandActive bool
	// commandPrefix is ":" or "/" -- which prompt is open. Held here because
	// the prompt's on-screen label is a symbol now, so it can no longer double
	// as the record of which mode this is.
	commandPrefix string
	// scopeNodes is the last tree the API returned, kept so the tree can be
	// rebuilt whenever the filter changes without refetching it.
	// scopeSubtrees is its unfiltered node-to-assets index, computed lazily --
	// see subtrees.
	scopeNodes    []ScopeNode
	scopeSubtrees map[string]map[string]struct{}
	// searchQuery is the active "/" filter, lower-cased and trimmed. Held so
	// tree rebuilds triggered by anything other than the search box still
	// tally against the same filter the table is showing.
	searchQuery string
	// scopeBeforeSearch is the node the tree cursor was on when the current
	// search started, held so clearing the search puts it back. Empty when no
	// search is running, or when the scope it started from was a virtual one.
	scopeBeforeSearch string
	// searchScopePicked records that the user has chosen a tree node SINCE
	// the current search began, which turns the search back into a
	// within-that-subtree search (see scopeMembers). False again as soon as
	// the search ends.
	searchScopePicked bool
	// naturalName and naturalAddress are the widths those two columns would
	// take if nothing constrained them, measured while the rows are built.
	// fitWidth is the table width the current caps were computed for, and
	// fitDirty says the rows changed underneath them.
	naturalName    int
	naturalAddress int
	fitWidth       int
	fitDirty       bool
	// favorites is the starred asset IDs, and recent the most-recently
	// connected ones, newest first. Both come from Core via jmsd's /v1/marks
	// (see QueueMarks) -- favorites is written back through actions.SetFavorite
	// on toggle; recent is read-only here, Core derives it from session history.
	favorites map[string]struct{}
	recent    []string
}

// NewWorkspace creates the first production layout for the post-login screen.
// The caller should pass the same tview.Application used by the login screen.
func NewWorkspace(app *tview.Application, username string, actions WorkspaceActions) *Workspace {
	w := &Workspace{
		app:       app,
		actions:   actions,
		kind:      ResourceAll,
		scopeID:   scopeAll,
		focusTree: true,
		favorites: map[string]struct{}{},
	}

	w.header = tview.NewTextView().SetDynamicColors(true)
	w.setStatus(username, statusBusy, "starting…")

	w.tree = tview.NewTreeView()
	w.tree.SetBorder(true)
	w.tree.SetTitle(" Scope ")
	w.tree.SetTitleAlign(tview.AlignLeft)
	w.tree.SetGraphics(true)
	w.tree.SetPrefixes([]string{"▸ ", "▾ "})
	w.tree.SetTopLevel(1)
	w.tree.SetSelectedFunc(w.selectTreeNode)
	w.tree.SetChangedFunc(w.changeTreeNode)
	w.tree.SetFocusFunc(func() {
		w.focusTree = true
		w.updateFocusStyle()
	})

	w.table = tview.NewTable()
	w.table.SetBorder(true)
	w.table.SetTitle(" Resources ")
	w.table.SetTitleAlign(tview.AlignLeft)
	w.table.SetFixed(1, 0)
	w.table.SetSelectable(true, false)
	w.table.SetSelectedStyle(selectionStyle(theme))
	w.table.SetSelectionChangedFunc(func(row, _ int) {
		// len(w.visible), not len(w.resources): the table only ever holds the
		// filtered rows, so a non-empty w.resources with an empty w.visible
		// (any ":" kind or "/" filter that matches nothing) would otherwise
		// bounce the selection onto row 1, which doesn't exist.
		if row == 0 && len(w.visible) > 0 {
			w.table.Select(1, 0)
			return
		}
		if row > 0 && row <= len(w.visible) {
			w.selectedID = w.visible[row-1].ID
		}
	})
	w.table.SetSelectedFunc(w.openSelectedResource)
	// Double-click copies the cell. Nothing is being overridden: tview's
	// Table handles only MouseLeftDown, MouseLeftClick and the two scroll
	// actions (table.go's MouseHandler), so MouseLeftDoubleClick is unclaimed
	// -- and connecting stays on Enter, which is why double-clicking cannot
	// start a session by accident. Everything else is returned untouched so
	// click-to-select and the scroll wheel keep working exactly as before.
	w.table.SetMouseCapture(func(action tview.MouseAction, event *tcell.EventMouse) (tview.MouseAction, *tcell.EventMouse) {
		if action != tview.MouseLeftDoubleClick || event == nil {
			return action, event
		}
		w.copyCell(w.table.CellAt(event.Position()))
		return action, nil
	})
	// The only place the table's own width is known. tview hands the draw
	// function the outer rect and takes the inner one back, so this returns
	// exactly what the default path would and re-fits the columns on the way
	// through -- the caps land in the same pass, with no frame of stale
	// layout after a resize.
	w.table.SetDrawFunc(func(_ tcell.Screen, _, _, _, _ int) (int, int, int, int) {
		x, y, width, height := w.table.GetInnerRect()
		w.fitColumns(width)
		return x, y, width, height
	})
	w.table.SetFocusFunc(func() {
		w.focusTree = false
		w.updateFocusStyle()
	})

	w.footer = tview.NewTextView().SetDynamicColors(true)

	w.command = tview.NewInputField()
	w.command.SetFieldBackgroundColor(theme.Bg)
	w.command.SetFieldTextColor(theme.Fg)
	w.command.SetLabelColor(theme.Accent)
	w.command.SetPlaceholderTextColor(theme.Muted)
	w.command.SetPlaceholderStyle(tcell.StyleDefault.Background(theme.Bg).Foreground(theme.Muted))
	w.command.SetChangedFunc(func(value string) {
		if w.commandActive && w.commandPrefix == "/" {
			w.filterCurrent(value)
		}
	})
	w.command.SetDoneFunc(w.finishCommand)
	// A click on a result row or a tree node takes focus with it: tview's
	// Table and TreeView call setFocus from their own mouse handlers, which
	// never passes through root's input capture. Without this the box stays
	// open with commandActive still set while the keyboard belongs to the
	// table, so captureInput keeps routing everything into
	// captureSearchInput, which forwards it to a Table that binds none of it
	// -- Esc, y, r, Tab and ":" all die silently until the box is clicked
	// back into by hand.
	//
	// Closing counts as a commit, not a cancel: the click landed on a row the
	// filter found, so the filter stays exactly as Enter would have left it.
	w.command.SetBlurFunc(func() {
		if !w.commandActive {
			return
		}
		w.closeCommand()
	})

	// One row tall showing the footer, three rows tall as a bordered prompt
	// box -- the border needs a row above and below the text. startCommand and
	// finishCommand switch it between the two.
	w.commandRow = tview.NewFlex().SetDirection(tview.FlexColumn)
	w.commandRow.SetTitleColor(theme.Muted).SetTitleAlign(tview.AlignRight)
	w.commandRow.AddItem(w.footer, 0, 1, false)

	body := tview.NewFlex().SetDirection(tview.FlexColumn)
	body.AddItem(w.tree, 30, 0, true)
	body.AddItem(w.table, 0, 1, false)

	w.root = tview.NewFlex().SetDirection(tview.FlexRow)
	w.root.AddItem(w.header, 1, 0, false)
	w.root.AddItem(body, 0, 1, true)
	w.root.AddItem(w.commandRow, footerHeight, 0, false)
	w.root.SetInputCapture(w.captureInput)

	w.setScopeNodes(nil)
	w.applyResources(nil)
	return w
}

// copyCell puts the double-clicked cell's value on the clipboard: the name
// from the NAME column, the address from ADDRESS. SOURCE TYPE copies the name
// too -- copying "ec2" back to yourself is of no use to anyone, and the row is
// what was aimed at.
//
// The value comes from the row data, never from the screen. NAME and ADDRESS
// are both capped on a narrow terminal (columnCaps), so what is drawn can be
// an ellipsised prefix -- and a prefix is exactly what the thing this exists
// to feed, `jms -c "ec2 <name>"`, cannot use, because that match is exact.
//
// A failure is reported rather than swallowed: someone told "copied" who then
// pastes the previous clipboard contents into a connect command has been sent
// somewhere worse than nowhere.
func (w *Workspace) copyCell(row, column int) {
	// Row 0 is the header; rows past the end happen on a click below the last
	// one, which tview still reports as inside the table.
	if row <= 0 || row > len(w.visible) || w.actions.CopyName == nil {
		return
	}
	resource := w.visible[row-1]
	value := resource.Name
	if column == columnIndex(w.columns(), roleAddress) && resource.Address != "" {
		value = resource.Address
	}
	if err := w.actions.CopyName(value); err != nil {
		w.setStatus(w.username, statusFailed, "copy failed: "+tview.Escape(err.Error()))
		return
	}
	w.setStatus(w.username, statusHint, "copied "+tview.Escape(value))
}

// Primitive returns the root primitive to place in the application's Pages.
func (w *Workspace) Primitive() tview.Primitive { return w.root }

// Focus gives keyboard focus to the last active workspace panel.
func (w *Workspace) Focus() { w.applyFocus() }

// SetUpdateNotice sets the "your jms is out of step with jmsd" line carried at
// the end of the header, or clears it when empty.
//
// It lives in the header rather than the status bar because the status bar is
// overwritten by the next thing that happens -- a load finishing, a scope
// change -- and this has to still be there minutes later. The header is
// rebuilt from these fields on every redraw, so it survives all of them for
// free.
func (w *Workspace) SetUpdateNotice(notice string) {
	w.updateNotice = notice
	w.setStatus(w.username, statusOK, "")
}

// statusLevel is how a status line should read at a glance. Every caller of
// setStatus states its own, because the call site is the only place that
// actually knows.
//
// This replaced classifying the finished line by keyword ("failed", "error",
// "connecting", ...). That worked only as long as the line contained nothing
// but wording this code chose -- and it does not: statuses interpolate asset
// names and error text. A machine legitimately named "db-failover" turned an
// ordinary "starred …" red, and "connecting-proxy" turned it amber. There is
// no keyword list that fixes that, because the data and the vocabulary are
// drawn from the same alphabet.
type statusLevel int

const (
	statusOK     statusLevel = iota // finished, idle, nothing outstanding
	statusBusy                      // in flight, or waiting on the user
	statusFailed                    // it did not work
	statusHint                      // a gesture did something, reported in Hint
)

func (l statusLevel) color() tcell.Color {
	switch l {
	case statusBusy:
		return theme.Warn
	case statusFailed:
		return theme.Danger
	case statusHint:
		return theme.Hint
	}
	return theme.OK
}

// setStatus repaints the header with a new status line. An empty status keeps
// whatever is already showing, which is what lets SetUpdateNotice repaint
// without one.
func (w *Workspace) setStatus(username string, level statusLevel, status string) {
	// Remembered so redraws with no username in scope (changeKind) keep it.
	w.username = username
	// Likewise remembered, so SetUpdateNotice can repaint the header without a
	// status to hand and not blank out the one already showing.
	if status != "" {
		w.status, w.statusColor = status, level.color()
	}
	// The status is not escaped here: callers that interpolate error text
	// already escape it themselves, and escaping twice would leak "[]"
	// artifacts.
	header := fmt.Sprintf(" [::b]Assets · %s[-:-:-]   %s   %s%s[-]",
		w.kind, tview.Escape(username), tag(w.statusColor), w.status)
	if w.updateNotice != "" {
		header += fmt.Sprintf("   %s%s[-]", tag(theme.Warn), tview.Escape(w.updateNotice))
	}
	w.header.SetText(header)
}

// ApplyLoad installs a tree and its assets together. Must run on the tview
// goroutine; the caller owns the queueing, so it can put the workspace on
// screen in the SAME update (see UI.applyWorkspaceLoad).
//
// Together is the point. Installed separately the tree went in while
// w.resources was still empty, so every node scored zero matches and rendered
// in its nothing-here state, and only the next frame showed the real tree --
// and the counts are computed from the assets precisely so the two agree,
// which they cannot do a frame apart.
//
// Reports whether it applied: a result from a superseded generation is
// dropped, and the caller must not reveal anything on the strength of it.
func (w *Workspace) ApplyLoad(query ResourceQuery, nodes []ScopeNode, rows []ResourceRow, username string, level statusLevel, status string) bool {
	if query.Generation != w.generation {
		return false
	}
	// Assets first: setScopeNodes rebuilds the tree, and the per-node counts
	// it renders come from w.resources.
	if nodes != nil {
		w.applyResources(rows)
		w.setScopeNodes(cloneScopeNodes(nodes))
	}
	w.setStatus(username, level, status)
	return true
}

// openSelectedResource is the Enter-key action on the resources table. It
// guards against a second connect attempt firing while one is already in
// flight -- connectResource runs asynchronously with no modal covering every
// stage, so without this a stray Enter (or a click elsewhere followed by
// Enter) can launch a second concurrent connectResource for a different
// asset, racing the first over the shared "picker"/"google-signin" page names.
func (w *Workspace) openSelectedResource(row, _ int) {
	if row < 1 || row > len(w.visible) || w.actions.OpenResource == nil {
		return
	}
	if w.connecting {
		w.setStatus(w.username, statusBusy, "still connecting to "+w.connectingName+"…")
		return
	}
	resource := w.visible[row-1]
	w.connecting = true
	w.connectingName = resource.Name
	w.actions.OpenResource(ResourceSelection{Resource: resource})
}

// endConnect clears the guard set by openSelectedResource. Must be called
// (via QueueUpdateDraw, since it touches tview-adjacent state) once the
// OpenResource action settles on every exit path, so a later Enter can start
// a new attempt.
func (w *Workspace) endConnect() {
	w.connecting = false
	w.connectingName = ""
}

// requestLoad loads the listing, cache permitted. Only startup and an
// explicit "r" reach here -- scope, kind and search are all applied to the
// loaded set locally, so navigating never triggers I/O.
func (w *Workspace) requestLoad() { w.load(false) }

// requestRefresh is the "r" action: same load, but the local cache is
// bypassed so Core is genuinely re-read.
func (w *Workspace) requestRefresh() { w.load(true) }

func (w *Workspace) load(refresh bool) {
	if w.loadCancel != nil {
		w.loadCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.loadCancel = cancel
	w.generation++
	if w.actions.Load != nil {
		w.actions.Load(ResourceQuery{Context: ctx, Generation: w.generation, Refresh: refresh})
	}
}

func (w *Workspace) applyFocus() {
	if w.focusTree {
		w.app.SetFocus(w.tree)
		return
	}
	w.app.SetFocus(w.table)
}

// snapshotFocus captures which panel is active and returns a function that
// puts focus back there. It exists for hideConnecting: removing a modal page
// makes tview focus the underlying page's default child, which clobbers
// focusTree via the panels' SetFocusFunc before applyFocus can read it. Take
// the snapshot first, remove the page, then call the returned function.
func (w *Workspace) snapshotFocus() func() {
	wasTree := w.focusTree
	return func() {
		w.focusTree = wasTree
		w.applyFocus()
	}
}

func (w *Workspace) updateFocusStyle() {
	muted := theme.Border
	active := theme.BorderHi
	if w.focusTree {
		w.tree.SetBorderColor(active).SetTitleColor(active)
		w.table.SetBorderColor(muted).SetTitleColor(muted)
		return
	}
	w.tree.SetBorderColor(muted).SetTitleColor(muted)
	w.table.SetBorderColor(active).SetTitleColor(active)
}
