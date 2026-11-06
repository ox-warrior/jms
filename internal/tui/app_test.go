package tui

import (
	"context"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// K8s protocol/asset classification is tested where it lives, in
// internal/connectflow -- the TUI now calls it directly rather than through
// package-local wrappers, so there is nothing left here to test separately.

// Regression test for focus landing on the scope tree after a session ends.
// tview's Pages.RemovePage only restores focus when Pages itself has focus,
// which it does not while a modal page holds it -- so dismissing the
// connecting modal left focus on the first focusable primitive (the tree)
// rather than the table the user was navigating.
func TestHidingConnectingModalRestoresTableFocus(t *testing.T) {
	screen := tcell.NewSimulationScreen("")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	screen.SetSize(120, 40)

	app := tview.NewApplication().SetScreen(screen)
	w := NewWorkspace(app, "tester", WorkspaceActions{})
	w.applyResources([]ResourceRow{{ID: "a1", Name: "asset-one", Category: "host"}})

	u := &UI{app: app, pages: tview.NewPages(), workspace: w}
	u.pages.AddPage("resources", w.Primitive(), true, true)
	app.SetRoot(u.pages, true)

	// The user is on the table -- the state Enter is pressed from.
	app.SetFocus(w.table)
	if w.focusTree {
		t.Fatal("setup: expected focus on the table, not the tree")
	}

	u.showConnecting("asset-one", func() {})
	u.hideConnecting()

	if w.focusTree {
		t.Error("focus fell back to the scope tree after the session ended, want the resources table")
	}
	if got := app.GetFocus(); got != tview.Primitive(w.table) {
		t.Errorf("focused primitive = %T, want the resources table", got)
	}
}

// The counterpart: someone who was on the tree when they hit Enter should
// come back to the tree, not be moved to the table.
func TestHidingConnectingModalRestoresTreeFocus(t *testing.T) {
	screen := tcell.NewSimulationScreen("")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	screen.SetSize(120, 40)

	app := tview.NewApplication().SetScreen(screen)
	w := NewWorkspace(app, "tester", WorkspaceActions{})
	w.applyResources([]ResourceRow{{ID: "a1", Name: "asset-one", Category: "host"}})

	u := &UI{app: app, pages: tview.NewPages(), workspace: w}
	u.pages.AddPage("resources", w.Primitive(), true, true)
	app.SetRoot(u.pages, true)
	app.SetFocus(w.tree)

	u.showConnecting("asset-one", func() {})
	u.hideConnecting()

	if !w.focusTree {
		t.Error("focus moved off the scope tree, want it restored there")
	}
	if got := app.GetFocus(); got != tview.Primitive(w.tree) {
		t.Errorf("focused primitive = %T, want the scope tree", got)
	}
}

// focusFixture is the state Enter is pressed from: a workspace page under a
// UI, with the resources table focused.
func focusFixture(t *testing.T) (*UI, *Workspace, *tview.Application) {
	t.Helper()
	screen := tcell.NewSimulationScreen("")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	screen.SetSize(120, 40)

	app := tview.NewApplication().SetScreen(screen)
	w := NewWorkspace(app, "tester", WorkspaceActions{})
	w.applyResources([]ResourceRow{{ID: "a1", Name: "asset-one", Category: "host"}})

	u := &UI{app: app, pages: tview.NewPages(), workspace: w}
	u.pages.AddPage("resources", w.Primitive(), true, true)
	app.SetRoot(u.pages, true)
	app.SetFocus(w.table)
	if w.focusTree {
		t.Fatal("setup: expected focus on the table, not the tree")
	}
	return u, w, app
}

// Every modal page has to put focus back, not just the connecting one. An
// asset with two accounts goes through the picker on its way to the session,
// and the picker's own removal was dropping the cursor on the scope tree --
// which then became the state hideConnecting faithfully restored when the
// session ended. The symptom only showed up on assets that ask something.
func TestClosingPickerRestoresTableFocus(t *testing.T) {
	u, w, app := focusFixture(t)

	// What modalPrompt does: its own page on top, focus handed to the widget.
	list := tview.NewList()
	u.pages.AddPage("picker", list, true, true)
	app.SetFocus(list)

	u.removePage("picker")

	if w.focusTree {
		t.Error("closing the picker left focus on the scope tree, want the table")
	}
	if got := app.GetFocus(); got != tview.Primitive(w.table) {
		t.Errorf("focused primitive = %T, want the resources table", got)
	}
}

// The full sequence an asset with a picker actually goes through, in order:
// connecting modal up, down for the question, question answered, connecting
// modal back up, session, connecting modal down. Focus must be on the table at
// the end -- this is the path the bug was reported from.
func TestFocusSurvivesPickerThenSession(t *testing.T) {
	u, w, app := focusFixture(t)

	u.showConnecting("asset-one", func() {})
	u.hideConnecting() // withoutModal: get out of the way to ask

	list := tview.NewList()
	u.pages.AddPage("picker", list, true, true)
	app.SetFocus(list)
	u.removePage("picker") // the user picked an account

	u.showConnecting("asset-one", func() {}) // withoutModal's deferred restore
	u.hideConnecting()                       // session over

	if w.focusTree {
		t.Error("focus ended on the scope tree after a session, want the resources table")
	}
	if got := app.GetFocus(); got != tview.Primitive(w.table) {
		t.Errorf("focused primitive = %T, want the resources table", got)
	}
}

// Dismissing the error modal is the other way a session ends.
func TestClosingErrorModalRestoresTableFocus(t *testing.T) {
	u, w, app := focusFixture(t)

	modal := tview.NewModal()
	u.pages.AddPage("error", modal, true, true)
	app.SetFocus(modal)

	u.removePage("error")

	if got := app.GetFocus(); got != tview.Primitive(w.table) {
		t.Errorf("focused primitive = %T, want the resources table", got)
	}
}

// Someone who was on the tree when they answered a picker belongs back on the
// tree -- the fix restores the last-used panel, it does not force the table.
func TestClosingPickerRestoresTreeFocus(t *testing.T) {
	u, w, app := focusFixture(t)
	app.SetFocus(w.tree)

	list := tview.NewList()
	u.pages.AddPage("picker", list, true, true)
	app.SetFocus(list)

	u.removePage("picker")

	if got := app.GetFocus(); got != tview.Primitive(w.tree) {
		t.Errorf("focused primitive = %T, want the scope tree", got)
	}
}

// hideConnecting is called on paths where no workspace exists yet (a failure
// during startup, before resourcePage runs), so the nil guard has to hold.
func TestHidingConnectingModalWithoutWorkspace(t *testing.T) {
	screen := tcell.NewSimulationScreen("")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	app := tview.NewApplication().SetScreen(screen)
	u := &UI{app: app, pages: tview.NewPages()}
	app.SetRoot(u.pages, true)

	u.showConnecting("asset-one", context.CancelFunc(func() {}))
	u.hideConnecting()
}
