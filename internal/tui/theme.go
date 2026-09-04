package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// Theme is the whole colour surface of the TUI. Fields are named for what
// they mean, not what colour they are, so a palette swap never requires
// touching a call site.
type Theme struct {
	Bg       tcell.Color // page background; ColorDefault keeps the terminal's own
	Fg       tcell.Color // primary text
	Border   tcell.Color // pane border + title, unfocused
	BorderHi tcell.Color // pane border + title, focused
	Header   tcell.Color // table header row
	Accent   tcell.Color // input labels, footer key glyphs
	Muted    tcell.Color // hints, empty state, secondary text
	// Hint marks what is neither a keypress nor a status: the mouse gesture on
	// the hint line, and the confirmation it produces. It is a colour of its
	// own precisely so it does not read as another key glyph sitting among the
	// Accent-coloured ones.
	Hint     tcell.Color
	SelBg    tcell.Color // selected row background
	SelFg    tcell.Color // selected row text
	OnAccent tcell.Color // text drawn on a BorderHi/Accent fill (buttons)
	OK       tcell.Color // status: succeeded / idle
	Warn     tcell.Color // status: in flight / needs input
	Danger   tcell.Color // status: failed
}

// DefaultTheme is used when --theme is not given.
const DefaultTheme = "k9s"

// themes are the built-in palettes. Every background is ColorDefault so the
// terminal's own background shows through -- tview's built-in default would
// otherwise paint everything black, which looks wrong on a light terminal.
//
// The k9s palette mirrors K9s's stock skin: lightskyblue body, orange accent,
// aqua headers.
//
// SelBg/SelFg are a plain inverse of the body -- Fg becomes the background and
// the terminal's own background becomes the text -- because that is measurably
// what k9s draws: sampling a k9s screenshot gives a selected row of body
// colour with background-coloured text on it. It is worth stating because the
// skin file suggests otherwise. Its views.table.cursorBgColor says "aqua",
// which renders as a cyan bar nothing like what k9s puts on screen; that
// value was tried here and immediately looked wrong.
//
// Border and BorderHi deliberately do NOT copy k9s, which uses dodgerblue and
// lightskyblue there: two blues a few shades apart, a contrast ratio of 1.27.
// Whichever pane had focus was legible in a screenshot but not at a glance.
// They are separated by brightness here instead -- see the contrast tests.
var themes = map[string]Theme{
	"k9s": {
		Bg:       tcell.ColorDefault,
		Fg:       tcell.NewHexColor(0x87cefa), // lightskyblue
		Border:   tcell.NewHexColor(0x4a4f57), // dim neutral, unfocused
		BorderHi: tcell.NewHexColor(0x7fb3ff), // bright blue, focused
		Header:   tcell.NewHexColor(0x66ffff), // aqua
		Accent:   tcell.NewHexColor(0xffa500), // orange
		Muted:    tcell.NewHexColor(0x778899), // lightslategray
		Hint:     tcell.NewHexColor(0xbd93f9), // purple
		SelBg:    tcell.NewHexColor(0x87cefa), // lightskyblue -- Fg, inverted
		SelFg:    tcell.NewHexColor(0x000000),
		OnAccent: tcell.NewHexColor(0x0b0e14),
		OK:       tcell.NewHexColor(0x32cd32), // limegreen
		Warn:     tcell.NewHexColor(0xffa500), // orange
		Danger:   tcell.NewHexColor(0xcd5c5c), // indianred
	},
	"light": {
		Bg:       tcell.ColorDefault,
		Fg:       tcell.NewHexColor(0x1c1c1c),
		Border:   tcell.NewHexColor(0xb8b8b8),
		BorderHi: tcell.NewHexColor(0x005f87),
		Header:   tcell.NewHexColor(0x005f87),
		Accent:   tcell.NewHexColor(0xaf5f00),
		Muted:    tcell.NewHexColor(0x6c6c6c),
		Hint:     tcell.NewHexColor(0x6f42c1), // purple, dark enough for a light background
		SelBg:    tcell.NewHexColor(0xd0d7de),
		SelFg:    tcell.NewHexColor(0x1c1c1c),
		OnAccent: tcell.NewHexColor(0xffffff),
		OK:       tcell.NewHexColor(0x006400),
		Warn:     tcell.NewHexColor(0xaf5f00),
		Danger:   tcell.NewHexColor(0xaf0000),
	},
	// mono leans on the terminal's own palette and bold attributes only, for
	// terminals with a custom colour scheme or no truecolor support.
	"mono": {
		Bg:       tcell.ColorDefault,
		Fg:       tcell.ColorDefault,
		Border:   tcell.ColorGray,
		BorderHi: tcell.ColorWhite,
		Header:   tcell.ColorWhite,
		Accent:   tcell.ColorWhite,
		Muted:    tcell.ColorGray,
		// No purple here on purpose: mono exists for terminals with their own
		// scheme or no truecolor, and a hard-coded hue is the one thing it
		// promises not to introduce.
		Hint:     tcell.ColorDefault,
		SelBg:    tcell.ColorWhite,
		SelFg:    tcell.ColorBlack,
		OnAccent: tcell.ColorBlack,
		OK:       tcell.ColorDefault,
		Warn:     tcell.ColorGray,
		Danger:   tcell.ColorWhite,
	},
}

// theme is the resolved palette for this process. Run sets it once, before
// any primitive exists, and nothing writes it afterwards -- the theme is
// fixed for the lifetime of the process, so threading it through every
// constructor would be noise.
var theme = themes[DefaultTheme]

// ThemeNames returns the built-in theme names, sorted, for flag help and
// error messages.
func ThemeNames() []string {
	names := make([]string, 0, len(themes))
	for name := range themes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// resolveTheme looks up a palette by name. An unknown name is an error
// rather than a silent fallback: failing at startup with the valid names is
// friendlier than launching into a palette the caller did not ask for.
func resolveTheme(name string) (Theme, error) {
	t, ok := themes[name]
	if !ok {
		return Theme{}, fmt.Errorf("unknown theme %q (available: %s)", name, strings.Join(ThemeNames(), ", "))
	}
	return t, nil
}

// applyGlobalStyles pushes t into tview's global Theme, which every
// primitive reads at construction time for anything not explicitly styled --
// login panel border, modal panels, list selection, input field backgrounds,
// tree graphics. Must run before the first primitive is built.
func applyGlobalStyles(t Theme) {
	tview.Styles = tview.Theme{
		PrimitiveBackgroundColor:    t.Bg,
		ContrastBackgroundColor:     t.SelBg,
		MoreContrastBackgroundColor: t.Accent,
		BorderColor:                 t.Border,
		TitleColor:                  t.BorderHi,
		GraphicsColor:               t.Border,
		PrimaryTextColor:            t.Fg,
		SecondaryTextColor:          t.Accent,
		TertiaryTextColor:           t.Muted,
		// These two are not interchangeable, whatever their names suggest.
		// InverseTextColor is text drawn ON PrimaryTextColor (button.go's
		// activatedStyle), so it answers to Fg; ContrastSecondaryTextColor is
		// text drawn on ContrastBackgroundColor -- i.e. on SelBg -- which is
		// what SelFg already means.
		InverseTextColor:           t.OnAccent,
		ContrastSecondaryTextColor: t.SelFg,
	}
}

// tag renders c as a tview colour tag for use in dynamic-colour text.
// tcell.ColorDefault is not a "valid" colour and its Hex() is -1, so it maps
// to tview's reset tag instead of a bogus [#ffffffff].
func tag(c tcell.Color) string {
	hex := c.Hex()
	if hex < 0 {
		return "[-]"
	}
	return fmt.Sprintf("[#%06x]", hex)
}

// selectionStyle is the shared style for the selected row/item in the resource
// table, the scope tree and the modal lists. All three state it explicitly:
// tview's per-widget defaults disagree with each other -- Table falls back to
// inverting the cell's own colours, TreeView to inverting the node against the
// page background -- which is why the tree and the table used to highlight in
// visibly different blues.
func selectionStyle(t Theme) tcell.Style {
	return tcell.StyleDefault.Background(t.SelBg).Foreground(t.SelFg).Bold(true)
}
