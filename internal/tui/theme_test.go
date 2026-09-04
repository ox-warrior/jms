package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

func TestResolveThemeUnknownNameListsChoices(t *testing.T) {
	// DefaultTheme is a hand-written string that has to key into the palette
	// map -- the one part of theme resolution a typo can break. (Checking the
	// rest of ThemeNames() would be circular: it returns that map's keys.)
	if _, err := resolveTheme(DefaultTheme); err != nil {
		t.Fatalf("default theme %q does not resolve: %v", DefaultTheme, err)
	}

	_, err := resolveTheme("bogus")
	if err == nil {
		t.Fatal("expected an error for an unknown theme name")
	}
	for _, name := range ThemeNames() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not mention available theme %q", err, name)
		}
	}
}

func TestTag(t *testing.T) {
	// ColorDefault is not a "valid" tcell colour and its Hex() is -1, so it
	// must degrade to tview's reset tag rather than a bogus hex string.
	if got := tag(tcell.ColorDefault); got != "[-]" {
		t.Errorf("tag(ColorDefault) = %q, want %q", got, "[-]")
	}
	if got := tag(tcell.NewHexColor(0x1e90ff)); got != "[#1e90ff]" {
		t.Errorf("tag(0x1e90ff) = %q, want %q", got, "[#1e90ff]")
	}
}

// statusLevel.color() is not tested directly: it is a switch that restates
// itself, and the mappings that matter are pinned where they are observable --
// TestStatusColourIgnoresTheText below, and the copy tests in
// resources_test.go for statusHint.

// The old status line was classified by scanning the finished text for words
// like "failed" and "connecting". That could not tell jms's own vocabulary
// from an asset's name, so an ordinary "starred …" went red for any machine
// with "error" or "failover" in its name. Levels are now stated by the call
// site; this pins down that the text is never consulted again.
func TestStatusColourIgnoresTheText(t *testing.T) {
	theme = themes[DefaultTheme]
	w, _ := newTestWorkspace(t, []ResourceRow{
		{ID: "1", Name: "db-failover-error", Address: "10.0.0.1", Category: "host"},
	})

	w.setStatus("alice", statusOK, "starred db-failover-error")
	if w.statusColor != theme.OK {
		t.Errorf("statusColor = %v, want theme.OK -- the asset's name must not be read as a verdict", w.statusColor)
	}
	w.setStatus("alice", statusFailed, "ready")
	if w.statusColor != theme.Danger {
		t.Errorf("statusColor = %v, want theme.Danger -- the level decides, not the wording", w.statusColor)
	}
}
