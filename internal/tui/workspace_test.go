// Shared fixtures for the Workspace tests. The per-area tests live beside
// the code they cover: resources_test.go, scopetree_test.go, prompt_test.go.

package tui

import (
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// newTestWorkspace builds a Workspace wired to a headless simulation screen,
// so tests can drive real tview input handlers and Draw passes.
func newTestWorkspace(t *testing.T, rows []ResourceRow) (*Workspace, tcell.SimulationScreen) {
	t.Helper()
	screen := tcell.NewSimulationScreen("")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	screen.SetSize(120, 40)

	app := tview.NewApplication().SetScreen(screen)
	w := NewWorkspace(app, "tester", WorkspaceActions{})
	w.applyResources(rows)

	pages := tview.NewPages()
	pages.AddPage("resources", w.Primitive(), true, true)
	app.SetRoot(pages, true)
	app.SetFocus(w.table)
	return w, screen
}

// pressKey drives the table's real input handler, failing the test if it
// doesn't return promptly. A hang here is the production symptom: the handler
// runs on the event-loop goroutine, so spinning there freezes the whole app
// including Ctrl+C.
func pressKey(t *testing.T, w *Workspace, key tcell.Key, r rune) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.table.InputHandler()(tcell.NewEventKey(key, r, tcell.ModNone), func(tview.Primitive) {})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("table input handler hung -- selection is likely out of range on an unselectable table")
	}
}

func visibleIDs(rows []ResourceRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

func starWorkspace(t *testing.T) *Workspace {
	t.Helper()
	w, _ := newTestWorkspace(t, []ResourceRow{
		{ID: "h1", Name: "web-01", Address: "10.0.0.1", Category: "host"},
		{ID: "h2", Name: "web-02", Address: "10.0.0.2", Category: "host"},
		{ID: "d1", Name: "pg-01", Address: "10.0.0.3", Category: "database", Type: "postgresql"},
	})
	return w
}

// pressRoot drives the real event path -- root's input capture, then the
// focused primitive -- rather than calling captureInput directly, so these
// exercise what actually reaches the app from a keypress.
func pressRoot(w *Workspace, key tcell.Key, r rune) {
	w.root.InputHandler()(tcell.NewEventKey(key, r, tcell.ModNone), func(p tview.Primitive) {
		w.app.SetFocus(p)
	})
}
