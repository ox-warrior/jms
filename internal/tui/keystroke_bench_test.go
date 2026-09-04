package tui

import (
	"fmt"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// Guards the cost of one character typed into the "/" box, which is the
// hottest thing in this UI: it runs on tview's event goroutine, so whatever it
// costs is added to the latency of every redraw while someone is typing.
//
// Scale is production's (docs/PLAN.md §7.3: 554 assets), and the query matches
// every row on purpose -- the expensive case is a filter that narrows nothing,
// since the whole table gets rebuilt. It caught tview.TaggedStringWidth being
// re-run per visible row per keystroke (70% of 3.3ms, now cached on the row --
// see ResourceRow.nameWidth).
func BenchmarkSearchKeystroke(b *testing.B) {
	screen := tcell.NewSimulationScreen("")
	if err := screen.Init(); err != nil {
		b.Fatal(err)
	}
	screen.SetSize(160, 50)
	w := NewWorkspace(tview.NewApplication().SetScreen(screen), "tester", WorkspaceActions{})

	nodes := make([]ScopeNode, 0, 20)
	for i := range 20 {
		children := make([]ScopeNode, 0, 15)
		for j := range 15 {
			children = append(children, ScopeNode{ID: fmt.Sprintf("n-%d-%d", i, j), Name: fmt.Sprintf("leaf-%d-%d", i, j)})
		}
		nodes = append(nodes, ScopeNode{ID: fmt.Sprintf("n-%d", i), Name: fmt.Sprintf("group-%d", i), Children: children})
	}
	rows := make([]ResourceRow, 0, 554)
	for i := range 554 {
		rows = append(rows, ResourceRow{
			ID:       fmt.Sprintf("a-%d", i),
			Name:     fmt.Sprintf("prod-web-%03d.ap-southeast-1.internal", i),
			Address:  fmt.Sprintf("10.%d.%d.%d", i/256, i%256, i%97),
			Category: "host",
			NodeIDs:  []string{fmt.Sprintf("n-%d-%d", i%20, i%15)},
		})
	}
	w.setScopeNodes(nodes)
	w.applyResources(rows)

	// Six distinct queries so no single one gets to benefit from a warm path
	// the others wouldn't have.
	queries := []string{"p", "pr", "pro", "prod", "prod-", "prod-w"}
	for i := 0; b.Loop(); i++ {
		w.renderResources(queries[i%len(queries)])
	}
}
