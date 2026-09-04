// Tests for prompt.go: the ":" and "/" box and the keys around it.

package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// Reopening "/" prefills the active query so it refines instead of restarting.
func TestSlashResumesActiveQuery(t *testing.T) {
	w, _ := newTestWorkspace(t, []ResourceRow{
		{ID: "1", Name: "web-01", Category: "host"},
	})
	w.startCommand("/")
	w.command.SetText("web")
	w.finishCommand(tcell.KeyEnter)

	w.startCommand("/")
	if got := w.command.GetText(); got != "web" {
		t.Errorf("reopened search field = %q, want \"web\" prefilled", got)
	}
	// ":" is a fresh command every time, never prefilled with the search.
	w.finishCommand(tcell.KeyEnter)
	w.startCommand(":")
	if got := w.command.GetText(); got != "" {
		t.Errorf("command field = %q, want empty", got)
	}
}

// A committed filter must stay visible in the footer -- otherwise the plain
// hint line reads as "filter dismissed" while the table is still filtered.
func TestFooterShowsActiveFilter(t *testing.T) {
	w, _ := newTestWorkspace(t, []ResourceRow{
		{ID: "1", Name: "web-01", Category: "host"},
		{ID: "2", Name: "db-01", Category: "host"},
	})
	if got := w.footer.GetText(true); strings.Contains(got, "/web") {
		t.Fatalf("footer shows a filter before one is set: %q", got)
	}

	w.filterCurrent("web")
	got := w.footer.GetText(true)
	if !strings.Contains(got, "/web") {
		t.Errorf("footer = %q, want it to show the active /web filter", got)
	}
	if !strings.Contains(got, "1/2") {
		t.Errorf("footer = %q, want a 1/2 match count", got)
	}

	w.resetFilters()
	if got := w.footer.GetText(true); strings.Contains(got, "/web") {
		t.Errorf("footer = %q, want the filter indicator gone after reset", got)
	}
}

// promptWorkspace is starWorkspace's rows with the simulation screen kept, so
// the prompt can be drawn repeatedly. Application.SetScreen finalises the
// screen it replaces, so a fresh one per draw panics on the second call.
func promptWorkspace(t *testing.T) (*Workspace, tcell.SimulationScreen) {
	t.Helper()
	return newTestWorkspace(t, []ResourceRow{
		{ID: "h1", Name: "web-01", Address: "10.0.0.1", Category: "host"},
		{ID: "h2", Name: "web-02", Address: "10.0.0.2", Category: "host"},
	})
}

// drawWorkspace renders the whole workspace and returns it as lines, so the
// prompt can be asserted on as the user sees it rather than as field values.
func drawWorkspace(t *testing.T, w *Workspace, screen tcell.SimulationScreen, cols, rows int) []string {
	t.Helper()
	screen.SetSize(cols, rows)
	screen.Clear()
	w.root.SetRect(0, 0, cols, rows)
	w.root.Draw(screen)
	screen.Show()

	cells, width, height := screen.GetContents()
	out := make([]string, 0, height)
	for y := range height {
		var line strings.Builder
		for x := 0; x < width; x++ {
			text := string(cells[y*width+x].Bytes)
			line.WriteString(text)
			// A double-width rune owns the next cell too, and the simulation
			// screen leaves a filler rune in it. Copying that filler out would
			// put a stray character after every emoji that a real terminal
			// never shows.
			if tview.TaggedStringWidth(text) == 2 {
				x++
			}
		}
		out = append(out, strings.TrimRight(line.String(), " "))
	}
	return out
}

// The prompt is a bordered box that takes over the whole bottom row, not an
// extra field tacked onto the hint line. The old version left the hint's
// wording in place next to a bare ":" and read as "the footer grew a field".
func TestPromptOpensAsABorderedBox(t *testing.T) {
	for _, c := range []struct{ prefix, label, title string }{
		{":", commandPromptLabel, commandPromptTitle},
		{"/", searchPromptLabel, searchPromptTitle},
	} {
		t.Run(c.prefix, func(t *testing.T) {
			w, screen := promptWorkspace(t)
			before := drawWorkspace(t, w, screen, 110, 14)
			if !strings.Contains(before[len(before)-1], "y star") {
				t.Fatalf("setup: last line is %q, want the hint footer", before[len(before)-1])
			}

			w.startCommand(c.prefix)
			lines := drawWorkspace(t, w, screen, 110, 14)
			box := lines[len(lines)-3:] // top border, prompt, bottom border

			prompt := strings.TrimPrefix(strings.TrimSpace(box[1]), "║")
			if !strings.HasPrefix(prompt, strings.TrimSpace(c.label)) {
				t.Errorf("prompt line is %q, want it to start with %q", box[1], c.label)
			}
			if !strings.Contains(box[0], strings.TrimSpace(c.title)) {
				t.Errorf("top border is %q, want the hint %q in it", box[0], c.title)
			}
			// The hint footer is gone, not merely pushed sideways.
			for _, line := range box {
				if strings.Contains(line, "y star") {
					t.Errorf("the hint footer is still on screen next to the prompt: %q", line)
				}
			}
		})
	}
}

// Both boxes have to name a way out on their border. A modal text field the
// user cannot see their way out of is the worst kind, and ":" used to spend
// its whole border on the command list without mentioning Esc.
func TestBothPromptsAdvertiseEscape(t *testing.T) {
	for _, c := range []struct{ prefix, title string }{
		{":", commandPromptTitle}, {"/", searchPromptTitle},
	} {
		if !strings.Contains(c.title, "Esc") {
			t.Errorf("%q prompt border is %q, want it to name Esc", c.prefix, c.title)
		}
		if !strings.Contains(c.title, "Enter") {
			t.Errorf("%q prompt border is %q, want it to name Enter", c.prefix, c.title)
		}
	}
}

// And Esc has to actually close them, from either mode, leaving focus back on
// a panel the user can navigate.
func TestEscapeClosesBothPrompts(t *testing.T) {
	for _, prefix := range []string{":", "/"} {
		t.Run(prefix, func(t *testing.T) {
			w := starWorkspace(t)
			w.app.SetFocus(w.table)
			pressRoot(w, tcell.KeyRune, []rune(prefix)[0])
			for _, r := range "web" {
				pressRoot(w, tcell.KeyRune, r)
			}
			if !w.commandActive {
				t.Fatalf("setup: %q did not open the prompt", prefix)
			}

			pressRoot(w, tcell.KeyEscape, 0)
			if w.commandActive {
				t.Errorf("Esc left the %q prompt open", prefix)
			}
			if w.commandPrefix != "" {
				t.Errorf("commandPrefix = %q after Esc, want it cleared", w.commandPrefix)
			}
			if got := w.app.GetFocus(); got == tview.Primitive(w.command) {
				t.Error("focus is still in the closed prompt field")
			}
			// Esc cancels rather than commits: no filter is left behind.
			if w.searchQuery != "" {
				t.Errorf("searchQuery = %q after Esc, want it cleared", w.searchQuery)
			}
		})
	}
}

// The command list is a placeholder, so it shows in the empty ":" box and gets
// out of the way as soon as anything is typed.
func TestCommandListIsAPlaceholder(t *testing.T) {
	w, screen := promptWorkspace(t)
	w.startCommand(":")
	empty := strings.Join(drawWorkspace(t, w, screen, 92, 11), "\n")
	if !strings.Contains(empty, "redis") {
		t.Error("the empty \":\" box does not list the commands")
	}
	w.command.SetText("k8")
	typed := strings.Join(drawWorkspace(t, w, screen, 92, 11), "\n")
	if strings.Contains(typed, "redis") {
		t.Error("the command list is still on screen after typing; it must be a placeholder")
	}
}

// Search and command have to be told apart at a glance: once the box is open
// the ":" or "/" that opened it is no longer on screen, so the prompt symbol
// is the only thing left carrying that information.
func TestSearchAndCommandPromptsDiffer(t *testing.T) {
	if commandPromptLabel == searchPromptLabel {
		t.Fatalf("both prompts render as %q", commandPromptLabel)
	}
	// Both are emoji, which are two cells wide. tview measures the label with
	// the same function tcell renders it with, so a mismatch here would push
	// the typed text one cell off the prompt.
	for _, label := range []string{commandPromptLabel, searchPromptLabel} {
		if got := tview.TaggedStringWidth(label); got != len([]rune(label))+1 {
			t.Errorf("label %q measures %d cells, want %d -- the emoji is not double width here",
				label, got, len([]rune(label))+1)
		}
	}
}

// Closing the prompt puts the hint footer back and shrinks the row, so the
// table gets its two lines back.
func TestClosingPromptRestoresTheFooter(t *testing.T) {
	w, screen := promptWorkspace(t)
	w.startCommand("/")
	opened := drawWorkspace(t, w, screen, 110, 14)
	w.finishCommand(tcell.KeyEscape)
	closed := drawWorkspace(t, w, screen, 110, 14)

	if !strings.Contains(closed[len(closed)-1], "y star") {
		t.Errorf("last line after closing is %q, want the hint footer back", closed[len(closed)-1])
	}
	if strings.Contains(strings.Join(closed, "\n"), searchPromptLabel) {
		t.Error("the prompt symbol is still on screen after the box closed")
	}
	// The box was taller than the footer, so the table regains rows.
	countTableRows := func(lines []string) (n int) {
		for _, line := range lines {
			if strings.Contains(line, "│") || strings.Contains(line, "║") {
				n++
			}
		}
		return n
	}
	if countTableRows(closed) <= countTableRows(opened) {
		t.Errorf("panel rows opened=%d closed=%d, want the table taller once the box is gone",
			countTableRows(opened), countTableRows(closed))
	}
}

// runningWorkspace starts a real tview event loop and returns the workspace
// plus a channel that closes when the application stops. Quitting is only
// observable end to end -- Stop() on an application that was never Run is a
// silent no-op, so a test that does not run the loop cannot tell ":q" from a
// command that was never wired up at all.
func runningWorkspace(t *testing.T) (*Workspace, <-chan error) {
	t.Helper()
	screen := tcell.NewSimulationScreen("")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	screen.SetSize(120, 40)
	app := tview.NewApplication().SetScreen(screen)
	w := NewWorkspace(app, "tester", WorkspaceActions{})
	w.applyResources([]ResourceRow{{ID: "h1", Name: "web-01", Category: "host"}})
	app.SetRoot(w.Primitive(), true)

	done := make(chan error, 1)
	go func() { done <- app.Run() }()
	t.Cleanup(app.Stop)
	return w, done
}

// ":q" quits, the same way Ctrl+C does -- tview's event loop calls Stop() on
// Ctrl+C, so routing the command to that same call makes the two equivalent
// rather than merely similar.
func TestQuitCommandStopsTheApplication(t *testing.T) {
	for _, command := range []string{"q", "quit", "exit", "Q", "QUIT"} {
		t.Run(command, func(t *testing.T) {
			w, done := runningWorkspace(t)
			w.app.QueueUpdateDraw(func() { w.executeCommand(command) })
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatalf(":%s did not stop the application", command)
			}
		})
	}
}

// A typo in the ":" prompt must be inert. ":q" is one keystroke from ":qq" and
// from the kind commands, so an unknown word quietly doing nothing is what
// keeps a slip from ending the session.
func TestUnknownCommandNeitherQuitsNorFilters(t *testing.T) {
	w, done := runningWorkspace(t)
	before := visibleIDs(w.visible)
	for _, command := range []string{"", "qq", "quti", ":q", "q!", "kill"} {
		w.app.QueueUpdateDraw(func() { w.executeCommand(command) })
	}
	// Give the loop a chance to process all of them and stop if it were going to.
	select {
	case <-done:
		t.Fatal("an unknown command stopped the application")
	case <-time.After(300 * time.Millisecond):
	}
	if w.kind != ResourceAll {
		t.Errorf("kind = %q, want it unchanged", w.kind)
	}
	if got := visibleIDs(w.visible); !slices.Equal(got, before) {
		t.Errorf("visible = %v, want it unchanged at %v", got, before)
	}
}

// "/" starts a search from either panel. It is bound on the workspace root, so
// which pane holds focus does not matter.
func TestSlashSearchesFromEitherPane(t *testing.T) {
	for _, startOnTree := range []bool{false, true} {
		w := starWorkspace(t)
		if startOnTree {
			w.app.SetFocus(w.tree)
		} else {
			w.app.SetFocus(w.table)
		}
		pressRoot(w, tcell.KeyRune, '/')
		if !w.commandActive || w.commandPrefix != "/" {
			t.Errorf("startOnTree=%v: \"/\" did not open the search prompt", startOnTree)
		}
	}
}

// Committing a search leaves the user on the table, wherever "/" was pressed
// from. Coming back to the scope tree meant the arrow keys walked nodes while
// the matches sat one Tab away -- you searched, and could not reach the result.
func TestCommittedSearchLandsOnTheResults(t *testing.T) {
	for _, startOnTree := range []bool{false, true} {
		w := starWorkspace(t)
		if startOnTree {
			w.app.SetFocus(w.tree)
		} else {
			w.app.SetFocus(w.table)
		}
		pressRoot(w, tcell.KeyRune, '/')
		for _, r := range "web" {
			pressRoot(w, tcell.KeyRune, r)
		}
		pressRoot(w, tcell.KeyEnter, 0)

		if w.focusTree {
			t.Errorf("startOnTree=%v: focus went back to the scope tree after a search", startOnTree)
		}
		if got := w.app.GetFocus(); got != tview.Primitive(w.table) {
			t.Errorf("startOnTree=%v: focused %T after a search, want the resources table", startOnTree, got)
		}
		// The point of landing there: the arrows now walk the matches.
		before, _ := w.table.GetSelection()
		pressRoot(w, tcell.KeyDown, 0)
		if after, _ := w.table.GetSelection(); after == before {
			t.Errorf("startOnTree=%v: Down did not move through the results (row stuck at %d)",
				startOnTree, before)
		}
	}
}

// Esc is a cancel, not a search: nothing was found, so the user goes back to
// the panel they came from rather than being moved to the table.
func TestCancelledSearchLeavesFocusAlone(t *testing.T) {
	w := starWorkspace(t)
	w.app.SetFocus(w.tree)

	pressRoot(w, tcell.KeyRune, '/')
	for _, r := range "web" {
		pressRoot(w, tcell.KeyRune, r)
	}
	pressRoot(w, tcell.KeyEscape, 0)

	if !w.focusTree {
		t.Error("Esc out of a search moved focus off the scope tree")
	}
	if w.searchQuery != "" {
		t.Errorf("searchQuery = %q after Esc, want it cleared", w.searchQuery)
	}
}

// Reopening with "/" carries the previous query, so narrowing a search never
// means retyping it -- or reaching for the mouse.
func TestSlashReopensWithPreviousQuery(t *testing.T) {
	w := starWorkspace(t)
	pressRoot(w, tcell.KeyRune, '/')
	for _, r := range "web" {
		pressRoot(w, tcell.KeyRune, r)
	}
	pressRoot(w, tcell.KeyEnter, 0)

	pressRoot(w, tcell.KeyRune, '/')
	if got := w.command.GetText(); got != "web" {
		t.Errorf("reopened search prompt shows %q, want the previous query %q", got, "web")
	}
}

// Starring is "y" everywhere, including out of a search: type enough to find
// the row, arrow to it, Enter to close the box, "y". The only thing the box
// takes away is the ability to press it mid-word, which no text field can give
// back -- "y" is a letter, and "mysql" needs it to be one.
func TestSearchThenStarUsesTheSameLowercaseY(t *testing.T) {
	w := starWorkspace(t)

	pressRoot(w, tcell.KeyRune, '/')
	for _, r := range "web" {
		pressRoot(w, tcell.KeyRune, r)
	}
	pressRoot(w, tcell.KeyDown, 0) // second match
	pressRoot(w, tcell.KeyEnter, 0)

	pressRoot(w, tcell.KeyRune, 'y')
	if _, ok := w.favorites["h2"]; !ok {
		t.Errorf("y did not star the row the search landed on; favorites=%v", w.favorites)
	}
	// And the filter is still on, so "y" acted inside the search, not after it.
	if got := visibleIDs(w.visible); len(got) != 2 {
		t.Errorf("visible = %v, want the search still filtering", got)
	}
}

// The arrow keys move the table cursor while the prompt is open, so the row
// Enter lands on is the one you picked, not always the first match.
func TestArrowsMoveSelectionWhileSearching(t *testing.T) {
	w := starWorkspace(t)
	w.startCommand("/")
	w.command.SetText("web")

	if event := w.captureInput(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)); event != nil {
		t.Error("Down was passed through to the query field")
	}
	if row, _ := w.table.GetSelection(); row != 2 {
		t.Fatalf("selection = row %d, want row 2 after Down", row)
	}
	if w.selectedID != "h2" {
		t.Errorf("selectedID = %q after Down, want h2", w.selectedID)
	}
}

// Selection must clamp to the matches. A cursor past w.visible sits on a row
// that toggleFavorite and openSelectedResource both decline to act on, so the
// keys would go quiet rather than obviously break.
func TestSearchSelectionClampsToMatches(t *testing.T) {
	w := starWorkspace(t)
	w.startCommand("/")
	w.command.SetText("web") // two matches

	for range 5 {
		w.captureInput(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	}
	if row, _ := w.table.GetSelection(); row != 2 {
		t.Errorf("selection = row %d after running past the end, want the last match (2)", row)
	}
	for range 5 {
		w.captureInput(tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone))
	}
	if row, _ := w.table.GetSelection(); row != 1 {
		t.Errorf("selection = row %d after running past the top, want the first match (1)", row)
	}
}

// Arrowing with nothing matched must not select a row that isn't there --
// that is the state that hangs tview's navigation (see
// TestNavigatingEmptySearchResultDoesNotHang).
func TestSearchSelectionWithNoMatches(t *testing.T) {
	w := starWorkspace(t)
	w.startCommand("/")
	w.command.SetText("nothing-matches-this")

	w.captureInput(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	if row, _ := w.table.GetSelection(); row > len(w.visible) {
		t.Errorf("selection = row %d with no matches, want it left alone", row)
	}
}

// No letter is bound inside the search box -- not one. Binding any of them
// would make every hostname containing it unsearchable, and "y" is the one
// that matters: "mysql", "proxy" and "yum" all need it to be a letter.
func TestSearchKeysStayTextual(t *testing.T) {
	w := starWorkspace(t)
	w.startCommand("/")
	for _, r := range []rune{'y', 'Y', 'r', 'j', 'k', '/', ':'} {
		event := w.captureInput(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
		if event == nil {
			t.Errorf("%q was intercepted; it must reach the query field as text", r)
		}
	}
	if len(w.favorites) != 0 {
		t.Errorf("typing letters starred something: %v", w.favorites)
	}
}

// ":" takes a command name and leaves the table alone, so the search-prompt
// keys have no live selection to act on there.
func TestCommandPromptIgnoresSearchKeys(t *testing.T) {
	w := starWorkspace(t)
	w.startCommand(":")
	for _, key := range []tcell.Key{tcell.KeyDown, tcell.KeyUp} {
		if event := w.captureInput(tcell.NewEventKey(key, 0, tcell.ModNone)); event == nil {
			t.Errorf("key %v was intercepted during a \":\" command", key)
		}
	}
	if len(w.favorites) != 0 {
		t.Errorf("a \":\" command starred something: %v", w.favorites)
	}
}

// clickRow drives the real mouse path for a left click on one of the table's
// rows, focus steal included. The workspace has to have been drawn first, or
// the primitives have no rects to hit-test against.
func clickRow(t *testing.T, w *Workspace, row int) {
	t.Helper()
	x, y, _, _ := w.table.GetInnerRect()
	event := tcell.NewEventMouse(x+1, y+row, tcell.Button1, tcell.ModNone)
	setFocus := func(p tview.Primitive) { w.app.SetFocus(p) }
	for _, action := range []tview.MouseAction{tview.MouseLeftDown, tview.MouseLeftClick, tview.MouseLeftUp} {
		w.root.MouseHandler()(action, event, setFocus)
	}
}

// Clicking a result while the search box is open closes the box and keeps the
// filter, so the browse keys work on the row that was clicked.
//
// tview's Table takes focus from its own mouse handler, which never passes
// through root's input capture -- so commandActive stayed set with the
// keyboard already belonging to the table. captureInput then routed every key
// into captureSearchInput, which hands anything but ↑↓ to a Table that binds
// none of it: Esc, y, r and Tab all vanished, and the only way out was to
// click back into the box.
func TestClickingAResultClosesTheSearchBox(t *testing.T) {
	w, screen := promptWorkspace(t)
	w.applyResources([]ResourceRow{
		{ID: "h1", Name: "web-01", Address: "10.0.0.1", Category: "host"},
		{ID: "h2", Name: "web-02", Address: "10.0.0.2", Category: "host"},
		{ID: "d1", Name: "pg-01", Address: "10.0.0.3", Category: "database", Type: "postgresql"},
	})
	w.startCommand("/")
	w.command.SetText("web")
	drawWorkspace(t, w, screen, 110, 14)

	clickRow(t, w, 2) // the second match
	if w.commandActive {
		t.Error("the prompt is still open after the click took focus to the table")
	}
	if w.searchQuery != "web" {
		t.Errorf("searchQuery = %q, want the filter kept -- a click on a match is a commit, not a cancel", w.searchQuery)
	}
	if w.selectedID != "h2" {
		t.Errorf("selectedID = %q, want the clicked row h2", w.selectedID)
	}
	lines := drawWorkspace(t, w, screen, 110, 14)
	if last := lines[len(lines)-1]; !strings.Contains(last, "y star") {
		t.Errorf("last line is %q, want the hint footer back in place of the prompt box", last)
	}

	// The keys the half-open state used to swallow.
	pressRoot(w, tcell.KeyRune, 'y')
	if _, starred := w.favorites["h2"]; !starred {
		t.Error(`"y" did not star the clicked row`)
	}
	pressRoot(w, tcell.KeyEscape, 0)
	if w.searchQuery != "" {
		t.Errorf("searchQuery = %q, want Esc to have cleared the filter", w.searchQuery)
	}
}

// Same for ":", where losing focus means the typed command was abandoned: the
// box goes away without running it.
func TestClickingAResultCancelsTheCommandBox(t *testing.T) {
	w, screen := promptWorkspace(t)
	w.startCommand(":")
	w.command.SetText("k8s")
	drawWorkspace(t, w, screen, 110, 14)

	clickRow(t, w, 1)
	if w.commandActive {
		t.Error("the command box is still open after the click took focus to the table")
	}
	if w.kind != ResourceAll {
		t.Errorf("kind = %v, want an unrun command to have been dropped, not executed", w.kind)
	}
}
