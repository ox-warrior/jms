package tui

import (
	"context"
	"errors"
	"sync"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/leiyupeng/jumpserver-cli/internal/connectflow"
	"github.com/leiyupeng/jumpserver-cli/internal/jmsdclient"
)

// errPromptCancelled is what every modal below returns when the user presses
// Esc. Callers only ever test it for non-nil -- what they report is phrased
// in terms of what was being attempted, not of the prompt.
var errPromptCancelled = errors.New("selection cancelled")

// promptSpec is the chrome of a blocking modal: everything except the widget
// the user actually interacts with.
type promptSpec struct {
	// pageName is the tview.Pages key. Reusing one name per kind of prompt is
	// what keeps a stray second prompt from stacking up invisibly.
	pageName      string
	width, height int
	title         string
	hint          string
	hintColor     tcell.Color
}

// modalPrompt shows a bordered, centred title/body/hint panel as its own page
// and blocks until body reports a result, the user presses Esc, or ctx is
// cancelled. The page is removed on all three paths.
//
// body builds the interactive widget and is handed the finish callback to
// report through. That callback is guarded by a sync.Once: there are three
// ways to reach the exit and the page must be removed exactly once, since a
// second removal would take down whichever page had been added after it.
//
// Esc is caught on the panel rather than the widget. SetInputCapture on a Box
// is consulted whenever that Box HasFocus(), which includes focus held
// transitively by a descendant -- so it fires no matter which child is
// focused, and it runs before the child ever sees the key.
func modalPrompt[T any](u *UI, ctx context.Context, spec promptSpec, body func(finish func(T, error)) tview.Primitive) (T, error) {
	type result struct {
		value T
		err   error
	}
	done := make(chan result, 1)
	u.app.QueueUpdateDraw(func() {
		var once sync.Once
		finish := func(value T, err error) {
			once.Do(func() {
				u.removePage(spec.pageName)
				done <- result{value: value, err: err}
			})
		}
		title := tview.NewTextView().SetTextAlign(tview.AlignCenter).
			SetTextColor(theme.Header).SetText(spec.title)
		hint := tview.NewTextView().SetTextAlign(tview.AlignCenter).
			SetTextColor(spec.hintColor).SetText(spec.hint)
		content := body(finish)

		panel := tview.NewGrid().SetRows(1, 0, 1).SetColumns(0)
		panel.SetBorder(true).SetBorderColor(theme.BorderHi)
		panel.AddItem(title, 0, 0, 1, 1, 0, 0, false)
		panel.AddItem(content, 1, 0, 1, 1, 0, 0, true)
		panel.AddItem(hint, 2, 0, 1, 1, 0, 0, false)
		panel.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
			if event.Key() == tcell.KeyEscape {
				var zero T
				finish(zero, errPromptCancelled)
				return nil
			}
			return event
		})
		u.pages.AddPage(spec.pageName, center(spec.width, spec.height, panel), true, true)
		u.app.SetFocus(content)
	})

	select {
	case r := <-done:
		return r.value, r.err
	case <-ctx.Done():
		u.app.QueueUpdateDraw(func() { u.removePage(spec.pageName) })
		var zero T
		return zero, ctx.Err()
	}
}

// pickFrom returns the only option without asking, and otherwise shows a
// scrollable list titled with what is being chosen.
func pickFrom[T any](u *UI, ctx context.Context, title string, options []T, label func(T) string) (T, error) {
	if len(options) == 1 {
		return options[0], nil
	}
	index, err := modalPrompt(u, ctx, promptSpec{
		pageName:  "picker",
		width:     60,
		height:    min(len(options)+4, 20),
		title:     title,
		hint:      "Enter to select · Esc to cancel",
		hintColor: theme.Muted,
	}, func(finish func(int, error)) tview.Primitive {
		list := tview.NewList().ShowSecondaryText(false)
		list.SetMainTextColor(theme.Fg).SetSelectedStyle(selectionStyle(theme))
		for _, option := range options {
			list.AddItem(label(option), "", 0, nil)
		}
		list.SetSelectedFunc(func(index int, _, _ string, _ rune) { finish(index, nil) })
		return list
	})
	if err != nil {
		var zero T
		return zero, err
	}
	return options[index], nil
}

// gate builds a connectflow.Gate wired to this UI's sign-in modal. The retry
// mechanics live in internal/connectflow, shared with the CLI's headless
// connect path -- see that package's doc comment for why.
func (u *UI) gate(ctx context.Context, jc *jmsdclient.Client, username string) *connectflow.Gate {
	return &connectflow.Gate{JC: jc, Ctx: ctx, Username: username,
		Authenticate: func(ctx context.Context, _ string) error { return u.googleSignIn(ctx, jc) }}
}

// errNoGoogleClient is what a build without an injected client ID reports
// instead of a sign-in it cannot run.
var errNoGoogleClient = errors.New("this jms was built without a Google client ID and cannot sign in -- build it with make")

// googleSignIn shows a waiting modal while the browser sign-in runs, with Esc
// to give up. Cancelling the context on the way out is what stops the
// sign-in still waiting behind a modal the user has already closed.
func (u *UI) googleSignIn(ctx context.Context, jc *jmsdclient.Client) error {
	if u.opts.Google.ID == "" {
		return errNoGoogleClient
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	_, err := modalPrompt(u, ctx, promptSpec{
		pageName:  "google-signin",
		width:     76,
		height:    5,
		title:     "Google sign-in (every 10 days)",
		hint:      "Esc to cancel",
		hintColor: theme.Muted,
	}, func(finish func(struct{}, error)) tview.Primitive {
		go func() {
			// The URL is not shown here: it is several hundred characters,
			// and on a laptop the browser opens anyway.
			finish(struct{}{}, connectflow.GoogleSignIn(ctx, jc, u.opts.Google, func(string) {}))
		}()
		return tview.NewTextView().SetTextAlign(tview.AlignCenter).
			SetText("Finish the Google sign-in in your browser…")
	})
	if errors.Is(err, errPromptCancelled) {
		return connectflow.ErrCancelled
	}
	return err
}

// showConnecting displays a small modal indicating a connect attempt is in
// flight, with Esc bound to cancel. It has no interactive child, so focus
// goes directly on the panel itself.
//
// The middle line is left empty and filled in later by
// setConnectingPhaseAsync. It exists because a k8s connect is not one wait
// but two, and the long one is Koko's: runK9s has to sit through Koko
// establishing its own connection to the real cluster before any kubeconfig
// exists to read (see internal/k8sclient's bannerText). Without a phase
// line that whole stretch looks like a frozen "Connecting to …".
// Must run on the tview goroutine.
func (u *UI) showConnecting(name string, cancel context.CancelFunc) {
	text := tview.NewTextView().SetTextAlign(tview.AlignCenter).
		SetText("Connecting to " + tview.Escape(name) + "…")
	phase := tview.NewTextView().SetTextAlign(tview.AlignCenter).SetTextColor(theme.Accent)
	hint := tview.NewTextView().SetTextAlign(tview.AlignCenter).SetTextColor(theme.Muted).SetText("Esc to cancel")
	panel := tview.NewGrid().SetRows(1, 1, 1).SetColumns(0)
	panel.SetBorder(true).SetBorderColor(theme.BorderHi)
	panel.AddItem(text, 0, 0, 1, 1, 0, 0, false)
	panel.AddItem(phase, 1, 0, 1, 1, 0, 0, false)
	panel.AddItem(hint, 2, 0, 1, 1, 0, 0, false)
	panel.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			cancel()
			return nil
		}
		return event
	})
	u.connectingPhase = phase
	u.pages.AddPage(connectingPage, center(56, 5, panel), true, true)
	u.app.SetFocus(panel)
}

const connectingPage = "connecting"

// removePage takes a page down and puts keyboard focus back where the user
// left it. Every page removal goes through here.
//
// tview's Pages.RemovePage refocuses the topmost remaining page, and for the
// workspace that means its default child -- always the scope tree, whichever
// panel the user was actually on. So closing any modal moved the cursor to the
// tree. It was easy to miss because the visible symptom is delayed: the picker
// closes, the session starts, and the misplaced focus only shows up when the
// terminal exits and the workspace comes back.
//
// The snapshot has to be taken before RemovePage, not after: that default-child
// focus fires the tree's SetFocusFunc, which sets the very field applyFocus
// consults.
//
// Restoring is conditional on focus having actually landed inside the
// workspace. If some other page is still up and holding focus, this removal
// was not the one that displaced the user, and stealing focus down to the
// table underneath it would be its own bug.
//
// Safe to call for a page that isn't currently shown -- Pages.RemovePage
// no-ops on an unknown name (though it still refocuses, hence the restore).
func (u *UI) removePage(name string) {
	var restore func()
	if u.workspace != nil {
		restore = u.workspace.snapshotFocus()
	}
	u.pages.RemovePage(name)
	if restore != nil && u.workspace.root.HasFocus() {
		restore()
	}
}

// hideConnecting removes the connecting modal.
func (u *UI) hideConnecting() {
	u.connectingPhase = nil
	u.removePage(connectingPage)
}

// showConnectingAsync / hideConnectingAsync are the goroutine-safe wrappers,
// for the same reason setWorkspaceStatus needs one: these touch tview state
// from connectResource's background goroutine.
func (u *UI) showConnectingAsync(name string, cancel context.CancelFunc) {
	u.app.QueueUpdateDraw(func() { u.showConnecting(name, cancel) })
}

func (u *UI) hideConnectingAsync() {
	u.app.QueueUpdateDraw(func() { u.hideConnecting() })
}

// setConnectingPhaseAsync updates the connecting modal's phase line. A no-op
// when the modal isn't currently up, so callers don't have to track that.
func (u *UI) setConnectingPhaseAsync(phase string) {
	u.app.QueueUpdateDraw(func() {
		if u.connectingPhase != nil {
			u.connectingPhase.SetText(tview.Escape(phase))
		}
	})
}

// hideConnectingUpFront is hideConnecting for the one caller that needs it to
// have actually happened before it continues: whoever is about to call
// app.Suspend and hand the terminal to a child process. QueueUpdateDraw only
// enqueues, and tview's event loop keeps running while suspended -- a redraw
// landing after screen.Suspend() would paint escape sequences over the child's
// display (tcell's Suspend only disengages; Show() still writes). Deadlocks if
// called from the tview goroutine; connectResource's background goroutine is
// the only caller.
func (u *UI) hideConnectingUpFront() {
	done := make(chan struct{})
	u.app.QueueUpdateDraw(func() {
		u.hideConnecting()
		close(done)
	})
	<-done
}

// showError displays a full error message in a dismissible modal. The
// one-line status bar truncates at the terminal's width, which for anything
// longer than a short phrase means the actual text is simply gone -- this is
// where a real error (a failed session, not routine status) should show up
// instead, in full, the way k9s surfaces its own errors.
//
// tview.NewModal() defaults its background to Styles.ContrastBackgroundColor
// and its text to Styles.PrimaryTextColor, which applyGlobalStyles points at
// theme.SelBg/theme.Fg -- colours meant for a selected table row, not a
// dialog. Left unset, that renders as a solid dodgerblue box with barely-
// readable text.
//
// Modal.SetBackgroundColor only forwards to the modal's inner form/frame; it
// does not touch the embedded *Box, which fills the modal's whole rect
// (border line, title row, and the padding around the frame) from its own
// backgroundColor field first. Left at that field's construction-time
// default -- still Styles.ContrastBackgroundColor -- the frame's black
// interior ends up floating inside a leftover blue band. modal.Box's setter
// is called directly, alongside Modal's own, to clear both.
//
// Chrome (border/title/button) stays in theme.BorderHi, the same accent
// every other panel uses -- k9s's own error dialog
// (internal/ui/dialog/error.go, ShowError) does the same: a solid body on the
// app's normal border colour, with only the message text itself (orange in
// k9s) marked out as an error. theme.Danger is reserved for that message text
// here, not the whole dialog. Must run on the tview goroutine.
func (u *UI) showError(title, message string) {
	const pageName = "error"
	modal := tview.NewModal().
		SetText(tview.Escape(message)).
		SetTextColor(theme.Danger).
		AddButtons([]string{"OK"}).
		SetButtonBackgroundColor(theme.BorderHi).
		SetButtonTextColor(theme.OnAccent).
		SetButtonActivatedStyle(tcell.StyleDefault.Background(theme.Accent).Foreground(theme.OnAccent).Bold(true)).
		SetDoneFunc(func(_ int, _ string) {
			u.removePage(pageName)
		})
	modal.SetBackgroundColor(theme.Bg)
	modal.Box.SetBackgroundColor(theme.Bg)
	modal.SetBorder(true).SetBorderColor(theme.BorderHi).SetTitleColor(theme.BorderHi).SetTitle(" " + title + " ")
	u.pages.AddPage(pageName, modal, true, true)
	u.app.SetFocus(modal)
}

// showErrorAsync is showError's goroutine-safe wrapper, for callers like
// connectResource that run on a background goroutine.
func (u *UI) showErrorAsync(title, message string) {
	u.app.QueueUpdateDraw(func() { u.showError(title, message) })
}

func center(width, height int, p tview.Primitive) tview.Primitive {
	return tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().AddItem(nil, 0, 1, false).AddItem(p, width, 1, true).AddItem(nil, 0, 1, false), height, 1, true).
		AddItem(nil, 0, 1, false)
}
