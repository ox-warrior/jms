// The command line: the ":" and "/" box that replaces the footer, the keys
// that reach it, and the hint line it displaces.

package tui

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// The two command-line prompts. They are deliberately different symbols
// rather than the ":" and "/" a user types: once the box is open the typed
// character is gone from the screen, and "am I filtering or am I running a
// command" is the one thing the box has to answer at a glance. k9s does the
// same with its own mascot prompt.
const (
	commandPromptLabel = "⚡ > "
	searchPromptLabel  = "🔍 > "
)

// What each prompt puts in its border. Both name the two keys that close the
// box: a modal input the user cannot see their way out of is the worst kind,
// and ":" used to spend its whole border on the command list without saying
// how to leave.
const (
	commandPromptTitle = " Enter run · Esc cancel "
	searchPromptTitle  = " ↑↓ move · Enter done · Esc cancel "
)

// What each prompt shows in the empty field. The command list lives here
// rather than in the border because it is long, it is only useful before you
// have typed anything, and a placeholder gets out of the way by itself.
const (
	commandPromptPlaceholder = "all · ec2 · rds · redis · k8s · q"
	searchPromptPlaceholder  = "type to filter"
)

// Heights of the bottom row in its two states: the hint footer on its own
// line, and the prompt box that replaces it (one row of input between two
// border rows).
const (
	footerHeight     = 1
	commandBoxHeight = 3
)

// renderFooter redraws the hint line, appending the active "/" filter when
// there is one. Committing a search with Enter hands focus back to the
// browse panes and puts the plain hint line back, which on its own reads as
// "the filter is gone" -- while the table is still filtered. The indicator is
// what makes a committed filter visible, and it names the key that clears it.
func (w *Workspace) renderFooter() {
	key := tag(theme.Accent) + "[::b]"
	text := fmt.Sprintf(" %s:[-:-:-] resource   %s/[-:-:-] filter   %sTab[-:-:-] focus   %sEnter[-:-:-] open   %sy[-:-:-] star   %sr[-:-:-] refresh   %sEsc[-:-:-] clear",
		key, key, key, key, key, key, key)
	if w.searchQuery != "" {
		text += fmt.Sprintf("   %s[::b]/%s[-:-:-] %s(%d/%d)[-]",
			tag(theme.Warn), tview.Escape(w.searchQuery),
			tag(theme.Muted), len(w.visible), len(w.resources))
	}
	w.footer.SetText(text)
}

func (w *Workspace) captureInput(event *tcell.EventKey) *tcell.EventKey {
	if w.commandActive {
		return w.captureSearchInput(event)
	}
	switch event.Key() {
	case tcell.KeyTAB:
		w.focusTree = !w.focusTree
		w.applyFocus()
		return nil
	case tcell.KeyBacktab:
		w.focusTree = !w.focusTree
		w.applyFocus()
		return nil
	case tcell.KeyEscape:
		w.resetFilters()
		return nil
	case tcell.KeyRune:
		switch event.Rune() {
		case ':':
			w.startCommand(":")
			return nil
		case '/':
			w.startCommand("/")
			return nil
		case 'r':
			w.requestRefresh()
			w.setStatus(w.username, statusBusy, "refreshing…")
			return nil
		case 'y':
			w.toggleFavorite()
			return nil
		case 'j':
			return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
		case 'k':
			return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
		}
	}
	return event
}

// captureSearchInput handles the keys that stay meaningful while the "/"
// prompt is open. Everything else falls through to the input field as text.
//
// "/" filters live, so the table below the prompt is already showing the rows
// being narrowed down -- but until this existed the prompt owned every key, so
// the selection was stuck on the first row and "y" was typed into the query
// instead of starring. Finding a resource by name and then starring it, which
// is the whole point of searching for it, meant committing with Enter first,
// and nothing on screen said so.
//
// No letter is bound here, starring included. Starring is "y" everywhere in
// this app, and a letter cannot be both text and a command in the same box:
// binding "y" would break typing "mysql", "proxy" or "yum". Enter closes the
// box and keeps the filter, and "y" works from the row it lands on -- so the
// key stays one key, and the only place it is unavailable is mid-word.
//
// Arrows are bound because they are not text. They let you pick the match to
// act on before committing, which is what makes the Enter-then-"y" flow one
// pass rather than a round trip.
func (w *Workspace) captureSearchInput(event *tcell.EventKey) *tcell.EventKey {
	// ":" takes a command name, not a query, and leaves the table untouched --
	// there is no live selection there for these keys to act on.
	if w.commandPrefix != "/" {
		return event
	}
	switch event.Key() {
	case tcell.KeyDown:
		w.moveSelection(1)
		return nil
	case tcell.KeyUp:
		w.moveSelection(-1)
		return nil
	}
	return event
}

// moveSelection steps the table cursor by delta rows, clamped to the visible
// rows. Used from the search prompt, where the table does not have focus and
// so cannot move itself.
//
// The clamp is not cosmetic: rows beyond w.visible exist in neither the table
// nor the selection-changed handler's range check, and selecting one leaves
// the cursor on a row that toggleFavorite and openSelectedResource both refuse
// to act on -- keys would silently stop working.
func (w *Workspace) moveSelection(delta int) {
	if len(w.visible) == 0 {
		return
	}
	row, _ := w.table.GetSelection()
	row += delta
	if row < 1 {
		row = 1
	}
	if row > len(w.visible) {
		row = len(w.visible)
	}
	w.table.Select(row, 0)
}

// startCommand opens the prompt box. The footer's one row grows into a
// bordered three-row box holding nothing but the prompt, which is what makes
// the mode unmistakable -- the old version left the hint line's wording in
// place beside a bare ":" and read as "the footer has an extra field in it".
func (w *Workspace) startCommand(prefix string) {
	w.commandActive = true
	w.commandPrefix = prefix

	label, title, placeholder := commandPromptLabel, commandPromptTitle, commandPromptPlaceholder
	// "/" reopens on top of the active filter so it refines rather than
	// restarts -- retyping the whole query to narrow it down is the reason
	// reaching for the mouse felt necessary. ":" always starts blank.
	text := ""
	if prefix == "/" {
		label, title, placeholder, text = searchPromptLabel, searchPromptTitle, searchPromptPlaceholder, w.searchQuery
	}
	w.command.SetLabel(label)
	w.command.SetPlaceholder(placeholder)
	w.command.SetText(text)

	w.commandRow.Clear()
	w.commandRow.AddItem(w.command, 0, 1, true)
	w.commandRow.SetBorder(true).SetBorderColor(theme.BorderHi).SetTitle(title)
	// Three rows: the border takes one above and one below the input.
	w.root.ResizeItem(w.commandRow, commandBoxHeight, 0)
	w.app.SetFocus(w.command)
}

// closeCommand puts the footer back and shrinks the box to its single row.
func (w *Workspace) closeCommand() {
	w.commandActive = false
	w.commandPrefix = ""
	w.commandRow.SetBorder(false).SetTitle("")
	w.commandRow.Clear()
	w.commandRow.AddItem(w.footer, 0, 1, false)
	w.root.ResizeItem(w.commandRow, footerHeight, 0)
}

func (w *Workspace) finishCommand(key tcell.Key) {
	if key == tcell.KeyEnter {
		value := strings.TrimSpace(w.command.GetText())
		if w.commandPrefix == ":" {
			w.executeCommand(value)
		} else {
			w.filterCurrent(value)
			// Committing a search hands over to the table, wherever "/" was
			// pressed from. A search is a hunt for a row, and the rows are in
			// the table -- coming back to the scope tree instead left the
			// arrow keys walking nodes while the matches sat one Tab away,
			// which reads as "I searched and cannot get to the results".
			w.focusTree = false
		}
	} else if w.commandPrefix == "/" {
		// Esc is a cancel: the filter goes, and so does any claim on where the
		// user was. They go back to the panel they came from.
		w.filterCurrent("")
	}
	w.closeCommand()
	w.applyFocus()
}

func (w *Workspace) executeCommand(command string) {
	switch strings.ToLower(command) {
	case "all":
		w.changeKind(ResourceAll)
	case "ec2":
		w.changeKind(ResourceEC2)
	case "rds":
		w.changeKind(ResourceRDS)
	case "redis":
		w.changeKind(ResourceRedis)
	case "k8s", "kubernetes":
		w.changeKind(ResourceK8s)
	case "q", "quit", "exit":
		// Exactly what Ctrl+C does: tview's event loop calls Stop() on it
		// (application.go, "Ctrl-C closes the application"), and Run returns,
		// which unwinds cli.RunE's deferred jmslog.Close the same way. Worth
		// having because ":" is already where this client is driven from, and
		// ":q" is the reflex anyone arriving from vim or k9s brings with them.
		w.app.Stop()
	}
}
