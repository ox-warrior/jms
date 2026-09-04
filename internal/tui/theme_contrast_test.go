package tui

import (
	"math"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// The bug these tests exist for: the focused and unfocused pane borders were
// steelblue and dodgerblue, which scored a contrast ratio of 1.26 -- legible
// in a screenshot held still, invisible while actually using the thing. A
// palette edit that puts two colours that close back into a pair that is
// supposed to distinguish a state is the failure mode being pinned here, and
// it is not one that shows up in any other test.

// relativeLuminance is the WCAG 2.x definition, which is what makes the
// ratios below comparable to published thresholds.
func relativeLuminance(c tcell.Color) float64 {
	hex := c.Hex()
	channel := func(shift uint) float64 {
		v := float64((hex>>shift)&0xff) / 255
		if v <= 0.04045 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*channel(16) + 0.7152*channel(8) + 0.0722*channel(0)
}

// contrastRatio returns WCAG contrast, 1 (identical) to 21 (black on white).
func contrastRatio(a, b tcell.Color) float64 {
	la, lb := relativeLuminance(a), relativeLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// measurable reports whether every colour has a real RGB value. ColorDefault
// is the terminal's own choice and has no measurable luminance (Hex() is -1),
// so pairs involving it are skipped rather than guessed at -- that is the
// whole point of the mono palette.
func measurable(colors ...tcell.Color) bool {
	for _, c := range colors {
		if c.Hex() < 0 {
			return false
		}
	}
	return true
}

func TestFocusedAndUnfocusedBordersAreDistinguishable(t *testing.T) {
	// 2.0 is well under the 3.0 WCAG asks of UI components -- a border is a
	// hint, not text -- but far enough above the 1.26 that caused the
	// complaint to catch a repeat.
	const minRatio = 2.0
	for name, theme := range themes {
		if !measurable(theme.Border, theme.BorderHi) {
			continue
		}
		if got := contrastRatio(theme.Border, theme.BorderHi); got < minRatio {
			t.Errorf("theme %q: Border vs BorderHi contrast %.2f, want >= %.1f -- "+
				"which pane has focus has to be visible at a glance, not on inspection",
				name, got, minRatio)
		}
	}
}

func TestSelectedRowTextStaysReadable(t *testing.T) {
	// A selected row repaints its text in SelFg, so this is the whole check.
	// The bar is a solid fill, which is what k9s does and what makes it the
	// loudest thing on screen -- all the more reason the text on it has to be
	// unambiguous rather than merely legible.
	const minRatio = 4.5
	for name, theme := range themes {
		if !measurable(theme.SelBg, theme.SelFg) {
			continue
		}
		if got := contrastRatio(theme.SelBg, theme.SelFg); got < minRatio {
			t.Errorf("theme %q: SelFg on SelBg contrast %.2f, want >= %.1f",
				name, got, minRatio)
		}
	}
}

func TestOnAccentIsReadableOnEveryFillItLandsOn(t *testing.T) {
	// OnAccent exists because SelFg used to serve both the selection bar and
	// the button fills, which now want opposite ends of the scale. These are
	// the two fills it is actually drawn on: showError's button background
	// (BorderHi) and its activated style (Accent).
	const minRatio = 4.0
	for name, theme := range themes {
		for _, fill := range []struct {
			color tcell.Color
			role  string
		}{{theme.BorderHi, "BorderHi"}, {theme.Accent, "Accent"}} {
			if !measurable(fill.color, theme.OnAccent) {
				continue
			}
			if got := contrastRatio(fill.color, theme.OnAccent); got < minRatio {
				t.Errorf("theme %q: OnAccent on %s contrast %.2f, want >= %.1f -- button text",
					name, fill.role, got, minRatio)
			}
		}
	}
}

// Hint is what a copy confirmation is drawn in, and it lands in the same
// status slot as OK, Warn and Danger -- beside Fg. Collapsing it into any of
// them would make "copied ng0" indistinguishable from a success, a wait or a
// failure, which is the entire distinction it was added to carry.
//
// Deliberately an equality check rather than a contrast ratio. WCAG contrast
// measures luminance alone, and this palette's purple against its orange
// scores 1.22 -- which says nothing useful about two hues that far apart, and
// would only push a future edit toward a brightness the design does not want.
func TestHintDoesNotCollapseIntoTheOtherStatusColours(t *testing.T) {
	for name, th := range themes {
		// mono keeps Hint at ColorDefault on purpose -- it promises not to
		// introduce a hue, and has no distinct status colours to begin with.
		if !measurable(th.Hint) {
			continue
		}
		for _, neighbour := range []struct {
			color tcell.Color
			role  string
		}{{th.OK, "OK"}, {th.Warn, "Warn"}, {th.Danger, "Danger"}, {th.Fg, "Fg"}} {
			if th.Hint == neighbour.color {
				t.Errorf("theme %q: Hint is the same colour as %s -- both appear in the "+
					"status line, so the two states would read identically", name, neighbour.role)
			}
		}
	}
}

// The tree and the table have to highlight identically. They did not before:
// each was left to a different tview default, so the same keypress produced
// two different blues depending on which pane it landed in.
func TestTreeAndTableShareOneSelectionStyle(t *testing.T) {
	theme = themes[DefaultTheme]

	node := scopeNode("aws-prod (67)", "n1", false)
	treeFg, treeBg, _ := node.GetSelectedTextStyle().Decompose()
	tableFg, tableBg, _ := selectionStyle(theme).Decompose()

	if treeFg != tableFg || treeBg != tableBg {
		t.Errorf("tree selection fg=%v bg=%v, table selection fg=%v bg=%v -- the two panes must agree",
			treeFg, treeBg, tableFg, tableBg)
	}
}
