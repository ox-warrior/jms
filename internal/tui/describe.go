package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/leiyupeng/jumpserver-cli/internal/clipboard"
	"github.com/leiyupeng/jumpserver-cli/internal/connectflow"
	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
	"github.com/leiyupeng/jumpserver-cli/internal/jmsdclient"
	"github.com/leiyupeng/jumpserver-cli/internal/jmslog"
)

// describeSelected is the "d" action on the resources table: show what the
// cloud knows about the highlighted machine.
//
// A row with no CloudKey is answered here, without a request: the listing
// already said there is nothing to fetch, and a round-trip to be told so is
// one the user would feel for no reason.
//
// No connecting guard, unlike Enter. This is a read that opens a panel;
// pressing it twice costs a wasted request, not a second session racing the
// first -- and the generation counter behind it keeps the older answer from
// landing on top of the newer one.
func (w *Workspace) describeSelected() {
	row, _ := w.table.GetSelection()
	if row < 1 || row > len(w.visible) || w.actions.Describe == nil {
		return
	}
	resource := w.visible[row-1]
	if resource.CloudKey == "" {
		w.actions.ShowDescribe(resource, nil)
		return
	}
	w.setStatus(w.username, statusBusy, "describing "+resource.Name+"…")
	w.actions.Describe(ResourceSelection{Resource: resource})
}

// nextDescribe invalidates any describe already in flight and returns the
// generation identifying this one.
func (u *UI) nextDescribe() uint64 {
	u.describeMu.Lock()
	defer u.describeMu.Unlock()
	u.describeGen++
	return u.describeGen
}

func (u *UI) isCurrentDescribe(gen uint64) bool {
	u.describeMu.Lock()
	defer u.describeMu.Unlock()
	return u.describeGen == gen
}

// describeResource fetches one machine's metadata and shows it.
//
// gen is checked twice, and both are needed. The check before QueueUpdateDraw
// drops work early; the one inside it is the one that decides, because the
// generation can advance while this func sits in the queue.
func (u *UI) describeResource(gen uint64, selection ResourceSelection, username string, jc *jmsdclient.Client, workspace *Workspace) {
	resource := selection.Resource
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	host, err := connectflow.WithMFA(u.gate(ctx, jc, username), func() (jmsd.HostInfo, error) {
		return jc.Describe(ctx, resource.CloudKey)
	})
	if err != nil {
		jmslog.Printf("describe %s: %v", resource.Name, err)
		if u.isCurrentDescribe(gen) {
			u.setWorkspaceStatus(workspace, username, statusFailed, "describe failed: "+tview.Escape(err.Error()))
		}
		return
	}
	if !u.isCurrentDescribe(gen) {
		return
	}
	u.app.QueueUpdateDraw(func() {
		if !u.isCurrentDescribe(gen) {
			return
		}
		workspace.setStatus(username, statusOK, resource.Name)
		u.showDescribe(resource, &host, clipboard.Copy)
	})
}

// describeHint is the panel's bottom line. The keys are written on it because
// the panel has no other way to say what it accepts, and an earlier version
// closed on ANY key -- which reads as "it does not accept anything" right up
// until you want to copy out of it.
// describeView is the "d" screen: a full-height view pushed over the
// workspace, the shape k9s uses for describe rather than a dialog.
//
// A centred box was the first attempt and it was wrong for this content. The
// panel is a document -- forty-odd lines of two-column text, longer with tags
// -- and a floating window both truncates it and reads as an interruption,
// when what the user wants is to read something and go back.
type describeView struct {
	ui   *UI
	body *tview.TextView
	// hint doubles as the status line: a copy confirmation belongs here, not
	// on the workspace's own footer, which is behind this view.
	hint   *tview.TextView
	search *tview.InputField
	// bottom holds hint or search; one row either way, so the body never
	// reflows when the box opens.
	bottom *tview.Flex

	// shown and raw are the same document twice: with colour tags for the
	// screen, without them for the clipboard and for matching. renderDescribe
	// writes both in one pass, one line each, so index i is the same row in
	// both -- which is why filtering can work on raw and paint from shown
	// without a tag-stripping regex that would have to cope with values
	// already escaped for tview.
	shown []string
	raw   []string
	query string // the active filter, empty when none
	copy  func(string) error
}

// describeHint names every key the view takes. It is written out because a
// view with no visible keys reads as one that accepts none -- the earlier
// version closed on ANY key, which looked the same from outside.
const describeHint = " Esc 返回 · / 搜索 · ↑↓/jk 滚动 · g/G 首尾 · y 复制 · 鼠标可框选 "

const describePage = "describe"

// resourcesPage is the workspace's own page name, needed here because the
// describe view REPLACES it rather than floating over it -- see showDescribe.
const resourcesPage = "resources"

// showDescribe pushes the view. Must run on the tview goroutine.
//
// copy is clipboard.Copy in production, injected so nothing in the test binary
// can reach the real pasteboard -- the same reason WorkspaceActions.CopyName
// is a field. nil disables the key.
// Returns the view so a test can drive its exits; production callers ignore it.
func (u *UI) showDescribe(resource ResourceRow, host *jmsd.HostInfo, copy func(string) error) *describeView {
	display, plain := renderDescribe(resource, host)
	v := &describeView{
		ui:    u,
		shown: strings.Split(strings.TrimRight(display, "\n"), "\n"),
		raw:   strings.Split(strings.TrimRight(plain, "\n"), "\n"),
		copy:  copy,
	}

	v.body = tview.NewTextView().SetDynamicColors(true).SetWrap(false)
	v.body.SetBackgroundColor(theme.Bg)

	v.hint = tview.NewTextView().SetDynamicColors(true)
	v.hint.SetBackgroundColor(theme.Bg)

	v.search = tview.NewInputField().
		SetLabel(searchPromptLabel).
		SetFieldBackgroundColor(theme.Bg).
		SetFieldTextColor(theme.Fg)
	v.search.SetBackgroundColor(theme.Bg)

	v.bottom = tview.NewFlex()
	v.bottom.SetBackgroundColor(theme.Bg)

	// The frame goes around the whole view rather than around the text, so the
	// hint line lives inside it instead of floating underneath -- and the
	// padding is charged once, to the box, rather than baked into the
	// rendered text where it would also land in the clipboard.
	//
	// A row of air at the top and two cells either side: the first version
	// put the section headings hard against the border, which reads as text
	// that overflowed its container rather than as a document.
	root := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(v.body, 0, 1, true).
		AddItem(v.bottom, 1, 0, false)
	root.SetBackgroundColor(theme.Bg)
	root.SetBorder(true).
		SetBorderColor(theme.BorderHi).
		SetTitleColor(theme.BorderHi).
		SetTitle(" "+tview.Escape(resource.Name)+" ").
		SetTitleAlign(tview.AlignLeft).
		SetBorderPadding(1, 0, 2, 2)

	v.body.SetInputCapture(v.captureBody)
	v.search.SetChangedFunc(func(text string) { v.applyFilter(text) })
	v.search.SetDoneFunc(func(key tcell.Key) {
		// Escape abandons the search and puts the document back; Enter keeps
		// the filter and hands the keyboard to the body so it can be scrolled
		// -- the same split the workspace's own "/" box uses.
		if key == tcell.KeyEscape {
			v.applyFilter("")
		}
		v.closeSearch()
	})

	v.applyFilter("")

	// Mouse capture goes back to the terminal for as long as this view is up,
	// which is what makes dragging a selection over the text work: with tcell
	// holding the mouse there is no native selection to make, and the usual
	// workaround (hold Shift) is a thing users have to be told. Nothing
	// underneath needs it -- the wheel and double-click gestures belong to
	// the table this replaces -- and every exit runs through close().
	u.app.EnableMouse(false)
	// SWITCHES to the page rather than stacking one on top, which is both what
	// k9s does with describe and the only thing that actually works here:
	// tview.Flex sets dontClear (flex.go), so it never paints its own
	// background, and a Flex laid over the workspace let the scope tree and
	// the panel titles show straight through the text.
	u.pages.AddAndSwitchToPage(describePage, root, true)
	u.app.SetFocus(v.body)
	return v
}

func (v *describeView) close() {
	v.ui.app.EnableMouse(true)
	// Invalidates any describe still in flight, so a slow answer cannot
	// reopen the view the user just left.
	v.ui.nextDescribe()

	// Snapshot BEFORE the switch, restore after. SwitchToPage focuses the
	// page's default child, and the workspace's is the scope tree -- whose
	// SetFocusFunc writes focusTree, so by the time anything downstream reads
	// it the answer is already "the tree". That is why pressing "d" from the
	// table and Escaping came back on the left pane. snapshotFocus exists for
	// exactly this (hideConnecting hit it first); calling applyFocus instead
	// just re-applied the clobbered value.
	var restore func()
	if v.ui.workspace != nil {
		restore = v.ui.workspace.snapshotFocus()
	}
	// Switching away hid the workspace, so removing this page is not enough
	// to bring it back -- without the switch the screen is left blank.
	v.ui.pages.RemovePage(describePage)
	v.ui.pages.SwitchToPage(resourcesPage)
	if restore != nil {
		restore()
	}
}

// captureBody handles the keys the document itself takes. Unknown keys are
// swallowed rather than treated as "close": closing on a stray key would make
// "y" a coin flip between copying and dismissing the thing being copied.
func (v *describeView) captureBody(event *tcell.EventKey) *tcell.EventKey {
	switch event.Key() {
	// The TextView's own scrolling. A machine with many tags does not fit on
	// one screen, and a view you cannot scroll silently truncates.
	case tcell.KeyUp, tcell.KeyDown, tcell.KeyPgUp, tcell.KeyPgDn, tcell.KeyHome, tcell.KeyEnd:
		return event
	case tcell.KeyEscape:
		// Escape clears an active filter before it leaves, so the key that
		// undoes the narrowing is the same one that goes back -- pressing it
		// once should not both drop the filter and close the view.
		if v.query != "" {
			v.applyFilter("")
			return nil
		}
		v.close()
		return nil
	// Ctrl+F / Ctrl+B are page forward and back, the vi and less binding --
	// not "find", which is a GUI habit. This view already speaks vi (j/k to
	// move, "/" to search), so anyone reaching for Ctrl+F here means the
	// next page.
	case tcell.KeyCtrlF:
		return tcell.NewEventKey(tcell.KeyPgDn, 0, tcell.ModNone)
	case tcell.KeyCtrlB:
		return tcell.NewEventKey(tcell.KeyPgUp, 0, tcell.ModNone)
	case tcell.KeyRune:
		switch event.Rune() {
		case 'q':
			v.close()
			return nil
		case '/':
			v.openSearch()
			return nil
		// j/k and g/G are TextView's own bindings (textview.go) -- passed
		// through rather than reimplemented. They were being swallowed by the
		// catch-all below, which is why "G" stopped jumping to the end.
		case 'j', 'k', 'g', 'G':
			return event
		case 'y':
			v.copyShown()
			return nil
		}
	}
	return nil
}

func (v *describeView) openSearch() {
	v.bottom.Clear()
	v.bottom.AddItem(v.search, 0, 1, true)
	// Reopens on top of the active filter so it refines rather than restarts,
	// matching the workspace's "/" box.
	v.search.SetText(v.query)
	v.ui.app.SetFocus(v.search)
}

func (v *describeView) closeSearch() {
	v.bottom.Clear()
	v.bottom.AddItem(v.hint, 0, 1, false)
	v.ui.app.SetFocus(v.body)
}

// applyFilter narrows the document to the lines matching query, and repaints
// the hint line with the count.
//
// It filters rather than highlights because the document is a list of
// name/value rows: "which disk is the 500GB one" is answered by the matching
// rows alone, and dropping the rest is what makes a long tag list readable.
// Section headings go with their contents -- a heading kept above an empty
// section would read as a section whose rows failed to render.
func (v *describeView) applyFilter(query string) {
	v.query = query
	if query == "" {
		v.body.SetText(strings.Join(v.shown, "\n"))
		v.hint.SetText(tag(theme.Muted) + describeHint + "[-]")
		v.body.ScrollToBeginning()
		return
	}

	matched := v.match()
	lines := make([]string, 0, len(matched))
	for _, i := range matched {
		lines = append(lines, v.shown[i])
	}
	v.body.SetText(strings.Join(lines, "\n"))
	v.body.ScrollToBeginning()
	v.hint.SetText(fmt.Sprintf("%s[::b]/%s[-:-:-] %s%d/%d 行 · Esc 清除[-]",
		tag(theme.Warn), tview.Escape(query), tag(theme.Muted), len(matched), len(v.shown)))
}

// copyShown puts what is on screen on the clipboard -- the filtered lines
// when a filter is up, the whole document otherwise. Copying the full text
// out from under an active filter would hand back something the user is not
// looking at.
// match returns the indices of the lines the active filter keeps, matched
// against the untagged text so a search for "gp3" is not thrown off by the
// colour codes around it. An empty query keeps everything.
func (v *describeView) match() []int {
	needle := strings.ToLower(v.query)
	out := make([]int, 0, len(v.raw))
	for i, line := range v.raw {
		if v.query == "" || strings.Contains(strings.ToLower(line), needle) {
			out = append(out, i)
		}
	}
	return out
}

func (v *describeView) copyShown() {
	if v.copy == nil {
		return
	}
	matched := v.match()
	lines := make([]string, 0, len(matched))
	for _, i := range matched {
		lines = append(lines, v.raw[i])
	}
	if err := v.copy(strings.Join(lines, "\n")); err != nil {
		v.hint.SetText(tag(theme.Danger) + " 复制失败：" + tview.Escape(err.Error()) + "[-]")
		return
	}
	v.hint.SetText(fmt.Sprintf("%s 已复制 %d 行[-]", tag(theme.Hint), len(lines)))
}
