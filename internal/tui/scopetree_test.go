// Tests for scopetree.go: node membership, per-node counts, and where the
// cursor goes.

package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// nodeScopedWorkspace mirrors the reported layout: one root with three
// sibling leaves holding host/db/redis assets respectively.
func nodeScopedWorkspace(t *testing.T) (*Workspace, int) {
	t.Helper()
	screen := tcell.NewSimulationScreen("")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	screen.SetSize(120, 40)
	app := tview.NewApplication().SetScreen(screen)

	loads := 0
	w := NewWorkspace(app, "tester", WorkspaceActions{Load: func(ResourceQuery) { loads++ }})
	w.setScopeNodes([]ScopeNode{{
		ID: "default", Name: "DEFAULT",
		Children: []ScopeNode{
			{ID: "host", Name: "host"},
			{ID: "db", Name: "db"},
			{ID: "redis", Name: "redis"},
		},
	}})
	w.applyResources([]ResourceRow{
		{ID: "h1", Name: "web-01", Category: "host", NodeIDs: []string{"host"}},
		{ID: "h2", Name: "web-02", Category: "host", NodeIDs: []string{"host"}},
		{ID: "d1", Name: "pg-01", Category: "database", Type: "postgresql", NodeIDs: []string{"db"}},
		{ID: "r1", Name: "cache-01", Category: "database", Type: "redis", NodeIDs: []string{"redis"}},
	})
	return w, loads
}

func treeCounts(w *Workspace) map[string]int {
	got := make(map[string]int)
	var walk func(n *tview.TreeNode)
	walk = func(n *tview.TreeNode) {
		if ref, ok := n.GetReference().(string); ok && ref != "" {
			got[ref] = 0
		}
		for _, c := range n.GetChildren() {
			walk(c)
		}
	}
	walk(w.tree.GetRoot())
	return got
}

// The reported bug, straight from the screenshots: selecting a leaf used to
// refetch only that node's assets, so every sibling's count went to zero and
// the tree collapsed to the branch under the cursor (DEFAULT 14 -> 10, with
// db and redis gone).
func TestSelectingNodeKeepsSiblingsInTree(t *testing.T) {
	w, _ := nodeScopedWorkspace(t)

	before := treeCounts(w)
	for _, id := range []string{"host", "db", "redis"} {
		if _, ok := before[id]; !ok {
			t.Fatalf("precondition: %q missing from the tree", id)
		}
	}

	w.changeTreeNode(tview.NewTreeNode("host").SetReference("host"))

	after := treeCounts(w)
	for _, id := range []string{"host", "db", "redis", "default"} {
		if _, ok := after[id]; !ok {
			t.Errorf("%q disappeared from the tree after selecting the host node", id)
		}
	}
	// And the table is genuinely narrowed to that node.
	if got := visibleIDs(w.visible); len(got) != 2 || got[0] != "h1" || got[1] != "h2" {
		t.Errorf("visible = %v, want [h1 h2] (only the host node's assets)", got)
	}
}

// Navigating the tree is a local re-filter; it must not cost a round-trip.
func TestSelectingNodeDoesNotRefetch(t *testing.T) {
	w, _ := nodeScopedWorkspace(t)
	loads := 0
	w.actions.Load = func(ResourceQuery) { loads++ }

	w.changeTreeNode(tview.NewTreeNode("db").SetReference("db"))
	w.changeTreeNode(tview.NewTreeNode("redis").SetReference("redis"))
	w.changeTreeNode(tview.NewTreeNode("all").SetReference("__all__"))

	if loads != 0 {
		t.Errorf("tree navigation triggered %d fetch(es), want 0", loads)
	}
}

// Moving the tree cursor must keep the selection highlight visible on screen.
//
// GetCurrentNode() is not enough here: the logical cursor stayed correct
// while the rendered highlight disappeared entirely. tview fires the tree's
// changed callback from inside TreeView.Draw, so rebuilding the tree there
// swapped the root mid-pass and left the draw comparing stale nodes against a
// current node from the new tree -- no match, no highlight.
func TestTreeCursorStaysHighlightedWhileMoving(t *testing.T) {
	screen := tcell.NewSimulationScreen("")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	screen.SetSize(120, 40)
	app := tview.NewApplication().SetScreen(screen)

	w := NewWorkspace(app, "tester", WorkspaceActions{Load: func(ResourceQuery) {}})
	w.setScopeNodes([]ScopeNode{{
		ID: "default", Name: "DEFAULT",
		Children: []ScopeNode{
			{ID: "host", Name: "host"},
			{ID: "db", Name: "db"},
			{ID: "redis", Name: "redis"},
		},
	}})
	w.applyResources([]ResourceRow{
		{ID: "h1", Name: "web-01", Category: "host", NodeIDs: []string{"host"}},
		{ID: "d1", Name: "pg-01", Category: "database", Type: "postgresql", NodeIDs: []string{"db"}},
		{ID: "r1", Name: "cache-01", Category: "database", Type: "redis", NodeIDs: []string{"redis"}},
	})

	pages := tview.NewPages()
	pages.AddPage("resources", w.Primitive(), true, true)
	app.SetRoot(pages, true)
	app.SetFocus(w.tree)

	// Scan the tree pane (columns 0..29) for a line drawn with a selection
	// background -- that is the highlight the user actually sees.
	highlighted := func() string {
		cells, width, height := screen.GetContents()
		for y := range height {
			var line []rune
			marked := false
			for x := 0; x < 30 && x < width; x++ {
				c := cells[y*width+x]
				if _, bg, _ := c.Style.Decompose(); bg != tcell.ColorDefault && bg != tcell.ColorBlack {
					marked = true
				}
				line = append(line, c.Runes...)
			}
			if marked {
				return strings.TrimSpace(string(line))
			}
		}
		return ""
	}
	draw := func() {
		screen.Clear()
		pages.Draw(screen)
		screen.Show()
	}

	draw()
	for i, want := range []string{"DEFAULT", "host", "db", "redis"} {
		w.root.InputHandler()(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone),
			func(p tview.Primitive) { app.SetFocus(p) })
		draw()

		got := highlighted()
		if got == "" {
			t.Fatalf("Down #%d: nothing is highlighted -- the cursor is invisible "+
				"(logical scope is %q)", i+1, w.scopeID)
		}
		if !strings.Contains(got, want) {
			t.Errorf("Down #%d: highlighted %q, want it to contain %q", i+1, got, want)
		}
	}
}

// Scope composes with kind and search rather than replacing them.
func TestScopeComposesWithKind(t *testing.T) {
	w, _ := nodeScopedWorkspace(t)

	// A kind that the scoped node holds nothing of yields an empty table --
	// without disturbing the scope.
	w.changeTreeNode(tview.NewTreeNode("host").SetReference("host"))
	w.changeKind(ResourceRedis)
	if len(w.visible) != 0 {
		t.Errorf("visible = %v, want empty (host node holds no redis)", visibleIDs(w.visible))
	}
	if w.scopeID != "host" {
		t.Errorf("scopeID = %q, want it unchanged at \"host\"", w.scopeID)
	}
}

// A "/" search is not limited by the tree cursor. The cursor narrows the scope
// on every keypress, so anyone who arrowed through the tree on their way to
// the search box was searching one subtree without having chosen it -- and
// what they were looking for was usually in a different one. "No matching
// resources" has to mean the asset does not exist.
func TestSearchIsNotLimitedByTheSelectedNode(t *testing.T) {
	w, _ := nodeScopedWorkspace(t)
	w.changeTreeNode(tview.NewTreeNode("host").SetReference("host"))
	if got := visibleIDs(w.visible); len(got) != 2 {
		t.Fatalf("setup: scoped to host shows %v, want the two host rows", got)
	}

	// cache-01 lives under the "redis" node, nowhere near the cursor.
	w.filterCurrent("cache-01")
	if got := visibleIDs(w.visible); len(got) != 1 || got[0] != "r1" {
		t.Errorf("searching from the host node found %v, want [r1] from the redis node", got)
	}
	// The scope is not destroyed, just held back -- clearing the search puts
	// the user back in the node they were standing in.
	if w.scopeID != "host" {
		t.Errorf("scopeID = %q, want it still \"host\"", w.scopeID)
	}
	w.filterCurrent("")
	if got := visibleIDs(w.visible); len(got) != 2 {
		t.Errorf("after clearing the search visible = %v, want the host node's two rows back", got)
	}
}

// treeCursorWorkspace is nodeScopedWorkspace with the tree cursor driven the
// way a user drives it. Calling changeTreeNode directly does not survive: the
// TreeView fires its changed callback from Draw, so the next repaint reports
// whatever node the cursor is actually on and overwrites the scope.
func treeCursorWorkspace(t *testing.T, downs int) (*Workspace, tcell.SimulationScreen) {
	t.Helper()
	screen := tcell.NewSimulationScreen("")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	screen.SetSize(100, 20)
	app := tview.NewApplication().SetScreen(screen)
	w := NewWorkspace(app, "tester", WorkspaceActions{})
	w.setScopeNodes([]ScopeNode{{ID: "default", Name: "DEFAULT", Children: []ScopeNode{
		{ID: "host", Name: "host"}, {ID: "db", Name: "db"}, {ID: "redis", Name: "redis"},
	}}})
	w.applyResources([]ResourceRow{
		{ID: "h1", Name: "web-01", Category: "host", NodeIDs: []string{"host"}},
		{ID: "h2", Name: "web-02", Category: "host", NodeIDs: []string{"host"}},
		{ID: "d1", Name: "pg-01", Category: "database", Type: "postgresql", NodeIDs: []string{"db"}},
		{ID: "r1", Name: "cache-01", Category: "database", Type: "redis", NodeIDs: []string{"redis"}},
	})
	app.SetFocus(w.tree)
	w.root.SetRect(0, 0, 100, 20)
	w.root.Draw(screen)
	for range downs {
		w.root.InputHandler()(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone),
			func(p tview.Primitive) { app.SetFocus(p) })
		w.root.Draw(screen)
	}
	return w, screen
}

func treeCursor(w *Workspace) string {
	ref, _ := w.tree.GetCurrentNode().GetReference().(string)
	return ref
}

// While a search is running the tree cursor sits on "All permitted", because
// that is the scope the table is actually showing. Leaving it on a node had
// the tree naming one subtree over results drawn from the whole estate.
func TestSearchMovesTreeCursorToAllPermitted(t *testing.T) {
	w, screen := treeCursorWorkspace(t, 2) // DEFAULT, then host
	if got := treeCursor(w); got != "host" {
		t.Fatalf("setup: cursor on %q, want host", got)
	}

	w.renderResources("cache-01")
	w.root.Draw(screen)
	if got := treeCursor(w); got != scopeAll {
		t.Errorf("cursor on %q during a search, want %q", got, scopeAll)
	}
}

// And clearing the search puts it back on the node the user was working in --
// a search is a detour, not a way of losing your place.
func TestClearingSearchReturnsToTheNode(t *testing.T) {
	w, screen := treeCursorWorkspace(t, 2)

	w.renderResources("cache-01")
	w.root.Draw(screen)
	w.renderResources("")
	w.root.Draw(screen)

	if got := treeCursor(w); got != "host" {
		t.Errorf("cursor on %q after clearing the search, want host", got)
	}
	if got := visibleIDs(w.visible); len(got) != 2 {
		t.Errorf("visible = %v, want the host node's two rows back", got)
	}
}

// Esc clears kind and search together, and lands in the same place.
func TestEscapeFromSearchReturnsToTheNode(t *testing.T) {
	w, screen := treeCursorWorkspace(t, 2)

	w.renderResources("cache-01")
	w.root.Draw(screen)
	w.resetFilters()
	w.root.Draw(screen)

	if got := treeCursor(w); got != "host" {
		t.Errorf("cursor on %q after Esc, want host", got)
	}
}

// Favorites keeps its own cursor: the search stays inside it, so the heading
// is still accurate and there is nothing to move to.
func TestSearchLeavesVirtualScopeCursorAlone(t *testing.T) {
	w, screen := treeCursorWorkspace(t, 2)
	w.applyFavorite("h1", true)
	w.changeTreeNode(tview.NewTreeNode("fav").SetReference(scopeFavorites))
	w.renderResources("")
	w.root.Draw(screen)
	if got := treeCursor(w); got != scopeFavorites {
		t.Fatalf("setup: cursor on %q, want Favorites", got)
	}

	w.renderResources("web")
	w.root.Draw(screen)
	if got := treeCursor(w); got != scopeFavorites {
		t.Errorf("cursor moved to %q, want it to stay on Favorites", got)
	}
	if got := visibleIDs(w.visible); len(got) != 1 || got[0] != "h1" {
		t.Errorf("visible = %v, want only the starred [h1]", got)
	}
}

// The tree keeps counting matches per node while a search is on, which is what
// tells you which node the thing you found actually lives in.
func TestTreeStillShowsWhereMatchesAreDuringSearch(t *testing.T) {
	w, _ := nodeScopedWorkspace(t)
	w.changeTreeNode(tview.NewTreeNode("host").SetReference("host"))
	w.renderResources("cache-01")

	onTree := treeCounts(w) // presence, keyed by node reference
	if _, listed := onTree["redis"]; !listed {
		t.Error("the redis node dropped off the tree, so nothing says where the match lives")
	}
	if _, listed := onTree["db"]; listed {
		t.Error("db node is still on the tree with no matches under it")
	}
}

// Favorites and Recent are scopes you click into on purpose, so searching
// inside them stays scoped -- "find one of my starred boxes" is a real thing
// to want, and it is the one case where a narrower search is the request.
func TestSearchStaysInsideFavourites(t *testing.T) {
	w, _ := nodeScopedWorkspace(t)
	w.applyFavorite("h1", true)
	w.changeTreeNode(tview.NewTreeNode("fav").SetReference(scopeFavorites))

	w.filterCurrent("web")
	if got := visibleIDs(w.visible); len(got) != 1 || got[0] != "h1" {
		t.Errorf("searching inside Favorites found %v, want only the starred [h1]", got)
	}
}

// A virtual scope means "everything permitted" -- it must not restrict rows.
func TestVirtualScopeDoesNotRestrict(t *testing.T) {
	w, _ := nodeScopedWorkspace(t)
	w.changeTreeNode(tview.NewTreeNode("host").SetReference("host"))
	w.changeTreeNode(tview.NewTreeNode("all").SetReference("__all__"))
	if len(w.visible) != 4 {
		t.Errorf("visible = %v, want all 4 rows back under __all__", visibleIDs(w.visible))
	}
}

// isVirtualScope is not tested directly -- it is a lookup over three magic
// IDs, and what actually matters is the behaviour it gates, which
// TestSearchLeavesVirtualScopeCursorAlone and TestVirtualScopeDoesNotRestrict
// already pin.

// Recent lists most-recently-connected first, in the order jmsd supplies (it
// comes from Core's session history, newest first).
func TestRecentScopeKeepsServerOrder(t *testing.T) {
	w := starWorkspace(t)
	w.SetMarks(nil, []string{"h1", "h2", "d1"})

	w.changeTreeNode(tview.NewTreeNode("recent").SetReference(scopeRecent))
	if got := visibleIDs(w.visible); !slices.Equal(got, []string{"h1", "h2", "d1"}) {
		t.Errorf("Recent shows %v, want [h1 h2 d1] in server order", got)
	}
}

// Recent composes with kind and search like any other scope.
func TestRecentScopeComposesWithKind(t *testing.T) {
	w := starWorkspace(t)
	w.SetMarks(nil, []string{"h1", "d1"})

	w.changeTreeNode(tview.NewTreeNode("recent").SetReference(scopeRecent))
	w.changeKind(ResourceRDS)
	if got := visibleIDs(w.visible); !slices.Equal(got, []string{"d1"}) {
		t.Errorf("Recent+:rds shows %v, want [d1]", got)
	}
}

// The three virtual scopes must report counts, and stay visible at zero --
// an empty Favorites entry is how the feature is discovered.
func TestVirtualScopeNodesShowCounts(t *testing.T) {
	w := starWorkspace(t)
	w.table.Select(1, 0)
	w.captureInput(tcell.NewEventKey(tcell.KeyRune, 'y', tcell.ModNone))

	labels := map[string]string{}
	for _, child := range w.tree.GetRoot().GetChildren() {
		if ref, ok := child.GetReference().(string); ok {
			labels[ref] = child.GetText()
		}
	}
	if got := labels[scopeFavorites]; !strings.Contains(got, "(1)") {
		t.Errorf("Favorites label = %q, want a (1) count", got)
	}
	if got := labels[scopeAll]; !strings.Contains(got, "(3)") {
		t.Errorf("All permitted label = %q, want a (3) count", got)
	}
	if got, ok := labels[scopeRecent]; !ok || !strings.Contains(got, "(0)") {
		t.Errorf("Recent label = %q, want it present showing (0)", got)
	}
}

// treeFixture mirrors the shape production actually has: a parent whose
// children hold the assets, sibling nodes that share an asset, and two
// distinct nodes with the SAME name (production has nine called "Platform").
func treeFixture() []ScopeNode {
	return []ScopeNode{{
		ID: "root", Name: "DEFAULT",
		Children: []ScopeNode{
			{ID: "ack", Name: "ack"},
			{ID: "db", Name: "db"},
			{ID: "plat-a", Name: "Platform"},
			{ID: "plat-b", Name: "Platform"},
		},
	}}
}

func TestSubtreeMatchesAggregatesDescendants(t *testing.T) {
	resources := []ResourceRow{
		{ID: "k1", Category: "cloud", Type: "k8s", NodeIDs: []string{"ack"}},
		{ID: "k2", Category: "cloud", Type: "k8s", NodeIDs: []string{"ack"}},
		{ID: "h1", Category: "host", NodeIDs: []string{"plat-a"}},
	}
	got := subtreeMatches(treeFixture(), resources, ResourceK8s, "")

	// The parent owns everything beneath it even though no k8s asset is
	// attached to it directly.
	if got["root"] != 2 {
		t.Errorf("root count = %d, want 2 (aggregated from children)", got["root"])
	}
	if got["ack"] != 2 {
		t.Errorf("ack count = %d, want 2", got["ack"])
	}
	// Nodes with nothing matching must be absent, not zero-valued.
	if _, present := got["plat-a"]; present {
		t.Errorf("plat-a holds only a host asset; must be absent under :k8s, got %d", got["plat-a"])
	}
	if _, present := got["db"]; present {
		t.Error("db holds nothing; must be absent from the count map")
	}
}

// The dedup case: an asset attached to two sibling nodes must count once at
// their shared ancestor. 19 of 554 production assets sit in multiple nodes,
// so summing child totals would inflate every parent.
func TestSubtreeMatchesDeduplicatesSharedAsset(t *testing.T) {
	resources := []ResourceRow{
		{ID: "shared", Category: "host", NodeIDs: []string{"ack", "db"}},
	}
	got := subtreeMatches(treeFixture(), resources, ResourceEC2, "")

	if got["ack"] != 1 || got["db"] != 1 {
		t.Errorf("each child should count the shared asset once: ack=%d db=%d", got["ack"], got["db"])
	}
	if got["root"] != 1 {
		t.Errorf("root count = %d, want 1 -- the shared asset must not be double-counted", got["root"])
	}
}

// An asset attached to both a node and that node's own ancestor must also
// count once, not twice.
func TestSubtreeMatchesDeduplicatesAncestorAndDescendant(t *testing.T) {
	resources := []ResourceRow{
		{ID: "both", Category: "host", NodeIDs: []string{"root", "ack"}},
	}
	got := subtreeMatches(treeFixture(), resources, ResourceEC2, "")
	if got["root"] != 1 {
		t.Errorf("root count = %d, want 1", got["root"])
	}
}

// Node names are not unique, so counts must key off IDs alone.
func TestSubtreeMatchesKeysByIDNotName(t *testing.T) {
	resources := []ResourceRow{
		{ID: "h1", Category: "host", NodeIDs: []string{"plat-a"}},
	}
	got := subtreeMatches(treeFixture(), resources, ResourceEC2, "")
	if got["plat-a"] != 1 {
		t.Errorf("plat-a count = %d, want 1", got["plat-a"])
	}
	if _, present := got["plat-b"]; present {
		t.Error("plat-b shares a name with plat-a but holds nothing; it must not inherit the count")
	}
}

// The tree must tally against the same predicate the table filters by.
func TestSubtreeMatchesHonoursSearchQuery(t *testing.T) {
	resources := []ResourceRow{
		{ID: "h1", Name: "web-01", Address: "10.0.0.1", Category: "host", NodeIDs: []string{"ack"}},
		{ID: "h2", Name: "db-01", Address: "10.0.0.2", Category: "host", NodeIDs: []string{"db"}},
	}
	got := subtreeMatches(treeFixture(), resources, ResourceEC2, "web")

	if got["ack"] != 1 {
		t.Errorf("ack count = %d, want 1 (web-01 matches)", got["ack"])
	}
	if _, present := got["db"]; present {
		t.Error("db-01 does not match the query; db must drop out of the tree")
	}
	// Matching on address, not just name.
	if got := subtreeMatches(treeFixture(), resources, ResourceEC2, "10.0.0.2"); got["db"] != 1 {
		t.Errorf("address search: db count = %d, want 1", got["db"])
	}
}

func TestSubtreeMatchesEmptyInputs(t *testing.T) {
	if got := subtreeMatches(nil, nil, ResourceAll, ""); len(got) != 0 {
		t.Errorf("empty inputs = %v, want empty map", got)
	}
	// A resource pointing at a node that isn't in the tree must not panic or
	// invent an entry.
	orphan := []ResourceRow{{ID: "x", Category: "host", NodeIDs: []string{"not-in-tree"}}}
	got := subtreeMatches(treeFixture(), orphan, ResourceEC2, "")
	if len(got) != 0 {
		t.Errorf("orphan node membership = %v, want empty map", got)
	}
	// A resource with no node membership at all.
	nodeless := []ResourceRow{{ID: "y", Category: "host"}}
	if got := subtreeMatches(treeFixture(), nodeless, ResourceEC2, ""); len(got) != 0 {
		t.Errorf("nodeless resource = %v, want empty map", got)
	}
}

// The selected node stays visible even when empty, so that changing kind
// never moves the cursor -- which would trigger a scope change and refetch.
func TestScopeTreeKeepsSelectedNodeWhenEmpty(t *testing.T) {
	counts := map[string]int{"ack": 3}

	rendered := scopeTreeNode(treeFixture()[0], counts, "db")
	if rendered == nil {
		t.Fatal("root should render")
	}
	var names []string
	for _, child := range rendered.GetChildren() {
		ref, _ := child.GetReference().(string)
		names = append(names, ref)
	}
	if len(names) != 2 || names[0] != "ack" || names[1] != "db" {
		t.Fatalf("children = %v, want [ack db] (db kept because selected)", names)
	}

	// Without the selection exemption, the empty node disappears.
	rendered = scopeTreeNode(treeFixture()[0], counts, "")
	if got := len(rendered.GetChildren()); got != 1 {
		t.Errorf("children = %d, want 1 (only ack)", got)
	}
}

func TestScopeTreeHidesFullyEmptySubtree(t *testing.T) {
	if got := scopeTreeNode(treeFixture()[0], map[string]int{}, ""); got != nil {
		t.Error("a subtree with no matches anywhere must render as nil, not an empty shell")
	}
}

// The selection exemption has to survive an empty ANCESTOR too: pruning the
// parent would otherwise take the retained child down with it, and the
// cursor would jump -- costing an unwanted refetch.
func TestScopeTreeKeepsSelectedNodeUnderEmptyAncestor(t *testing.T) {
	nodes := []ScopeNode{{
		ID: "root", Name: "DEFAULT",
		Children: []ScopeNode{{
			ID: "mid", Name: "mid",
			Children: []ScopeNode{{ID: "leaf", Name: "leaf"}},
		}},
	}}
	// Nothing matches anywhere, but "leaf" is selected.
	rendered := scopeTreeNode(nodes[0], map[string]int{}, "leaf")
	if rendered == nil {
		t.Fatal("root must survive to keep the selected leaf reachable")
	}
	mid := rendered.GetChildren()
	if len(mid) != 1 {
		t.Fatalf("root children = %d, want 1", len(mid))
	}
	leaf := mid[0].GetChildren()
	if len(leaf) != 1 {
		t.Fatalf("mid children = %d, want 1", len(leaf))
	}
	if ref, _ := leaf[0].GetReference().(string); ref != "leaf" {
		t.Errorf("leaf reference = %q, want \"leaf\"", ref)
	}
}

// End-to-end wiring check: switching kind or typing a search must re-render
// the tree AND must not cost a network round-trip. The no-refetch half is the
// point of keeping the selected node visible when it empties out.
func TestTreeFollowsFilterAndDoesNotRefetch(t *testing.T) {
	screen := tcell.NewSimulationScreen("")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	screen.SetSize(120, 40)
	app := tview.NewApplication().SetScreen(screen)

	loads := 0
	w := NewWorkspace(app, "tester", WorkspaceActions{
		Load: func(ResourceQuery) { loads++ },
	})
	w.setScopeNodes([]ScopeNode{{
		ID: "root", Name: "DEFAULT",
		Children: []ScopeNode{
			{ID: "ack", Name: "ack"},
			{ID: "hosts", Name: "hosts"},
		},
	}})
	w.applyResources([]ResourceRow{
		{ID: "k1", Name: "cluster-a", Category: "cloud", Type: "k8s", NodeIDs: []string{"ack"}},
		{ID: "h1", Name: "web-01", Category: "host", NodeIDs: []string{"hosts"}},
	})

	labels := func() []string {
		var out []string
		for _, c := range w.tree.GetRoot().GetChildren() {
			ref, _ := c.GetReference().(string)
			if ref == "root" {
				for _, g := range c.GetChildren() {
					out = append(out, g.GetText())
				}
			}
		}
		return out
	}

	t.Logf(":all       -> %v", labels())
	before := loads
	w.changeKind(ResourceK8s)
	t.Logf(":k8s       -> %v", labels())
	w.changeKind(ResourceEC2)
	t.Logf(":ec2       -> %v", labels())
	w.changeKind(ResourceAll)
	w.filterCurrent("web")
	t.Logf("/web       -> %v", labels())

	if loads != before {
		t.Errorf("filter changes triggered %d asset refetch(es), want 0", loads-before)
	}
}

// A node that holds nothing must restrict the table to nothing. It is
// reachable as the selected scope (scopeTreeNode keeps the selection visible
// even when it empties out), and subtreeAssetIDs omits empty nodes -- so a
// missing entry passed straight through would read as nil, i.e. "no scope at
// all", and list every permitted asset under a node that contains none.
func TestEmptyNodeScopeShowsNothingRatherThanEverything(t *testing.T) {
	w, _ := nodeScopedWorkspace(t)
	w.setScopeNodes(append(w.scopeNodes, ScopeNode{ID: "empty", Name: "empty"}))

	w.changeTreeNode(tview.NewTreeNode("empty").SetReference("empty"))
	if got := visibleIDs(w.visible); len(got) != 0 {
		t.Errorf("visible under an empty node = %v, want none", got)
	}
	// And an id that isn't in the tree at all gets the same treatment, rather
	// than falling back to "everything".
	w.scopeID = "no-such-node"
	w.applyFilters("")
	if got := visibleIDs(w.visible); len(got) != 0 {
		t.Errorf("visible under an unknown node = %v, want none", got)
	}
}

// The unfiltered node-to-assets index is cached (see Workspace.subtrees),
// which makes going stale the failure mode worth pinning down. Both of its
// inputs have to drop it: a refresh replacing the assets, and a fresh
// permission tree replacing the nodes.
func TestSubtreeCacheFollowsANewListing(t *testing.T) {
	w, _ := nodeScopedWorkspace(t)
	w.changeTreeNode(scopeNodeFor(t, w, "host"))
	if got := visibleIDs(w.visible); len(got) != 2 {
		t.Fatalf("visible = %v, want the two hosts before the refresh", got)
	}

	// A refresh brings back a host that moved into the node, and drops one.
	w.applyResources([]ResourceRow{
		{ID: "h1", Name: "web-01", Category: "host", NodeIDs: []string{"host"}},
		{ID: "h3", Name: "web-03", Category: "host", NodeIDs: []string{"host"}},
		{ID: "d1", Name: "pg-01", Category: "database", Type: "postgresql", NodeIDs: []string{"db"}},
	})
	if got := visibleIDs(w.visible); !slices.Equal(got, []string{"h1", "h3"}) {
		t.Errorf("visible = %v, want [h1 h3] -- the cached subtree index outlived the assets it was built from", got)
	}

	// And a re-fetched tree that moves the same assets under a different node.
	w.setScopeNodes([]ScopeNode{{ID: "host", Name: "host"}})
	w.applyResources([]ResourceRow{
		{ID: "h9", Name: "web-09", Category: "host", NodeIDs: []string{"host"}},
	})
	w.changeTreeNode(scopeNodeFor(t, w, "host"))
	if got := visibleIDs(w.visible); !slices.Equal(got, []string{"h9"}) {
		t.Errorf("visible = %v, want [h9] after the tree itself was replaced", got)
	}
}

// scopeNodeFor finds the rendered tree node carrying the given scope id, so a
// test can drive changeTreeNode the way TreeView.Draw does.
func scopeNodeFor(t *testing.T, w *Workspace, id string) *tview.TreeNode {
	t.Helper()
	node := findScopeNode(w.tree.GetRoot(), id)
	if node == nil {
		t.Fatalf("no %q node in the scope tree", id)
	}
	return node
}

// Clicking a node while a search is running narrows the results into it. The
// tree's per-node counts stay filtered by the query, so they are what tells
// the user where the matches live -- and following that pointer has to do
// something. It didn't: scopeMembers returned "no restriction" for the whole
// duration of a search, so every node in the panel was inert and the tree read
// as broken.
func TestPickingANodeDuringSearchNarrowsTheTable(t *testing.T) {
	w, screen := treeCursorWorkspace(t, 2) // cursor parked on host

	w.renderResources("01")
	w.root.Draw(screen)
	// The search itself still spans the estate: merely standing on a node is
	// not choosing one.
	if got := visibleIDs(w.visible); !slices.Equal(got, []string{"h1", "d1", "r1"}) {
		t.Fatalf("setup: visible = %v, want every -01 asset in the estate", got)
	}

	node := scopeNodeFor(t, w, "db")
	w.tree.SetCurrentNode(node)
	w.changeTreeNode(node)
	if got := visibleIDs(w.visible); !slices.Equal(got, []string{"d1"}) {
		t.Errorf("visible = %v, want only the db node's match", got)
	}

	// And refining the query afterwards leaves the cursor where the user put
	// it, instead of dragging it back to "All permitted" on every keystroke.
	w.renderResources("pg")
	w.root.Draw(screen)
	if got := treeCursor(w); got != "db" {
		t.Errorf("cursor on %q after refining the query, want db", got)
	}
	if got := visibleIDs(w.visible); !slices.Equal(got, []string{"d1"}) {
		t.Errorf("visible = %v, want the pick still honoured", got)
	}
}

// Clearing the search leaves the user in the node they picked during it, not
// the one they happened to be standing on when they started typing. The pick
// is both the more recent and the more deliberate of the two.
func TestClearingSearchKeepsTheNodePickedDuringIt(t *testing.T) {
	w, screen := treeCursorWorkspace(t, 2) // cursor parked on host

	w.renderResources("01")
	w.root.Draw(screen)
	node := scopeNodeFor(t, w, "redis")
	w.tree.SetCurrentNode(node)
	w.changeTreeNode(node)

	w.renderResources("")
	w.root.Draw(screen)
	if got := treeCursor(w); got != "redis" {
		t.Errorf("cursor on %q after clearing the search, want the picked redis node", got)
	}
	if got := visibleIDs(w.visible); !slices.Equal(got, []string{"r1"}) {
		t.Errorf("visible = %v, want the redis node's rows", got)
	}
	// The pick does not outlive its search: the next one starts estate-wide
	// again from wherever the cursor now is.
	w.renderResources("01")
	w.root.Draw(screen)
	if got := visibleIDs(w.visible); !slices.Equal(got, []string{"h1", "d1", "r1"}) {
		t.Errorf("visible = %v, want the next search to span the estate again", got)
	}
}
