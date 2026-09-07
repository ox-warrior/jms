// The scope tree: the left-hand panel, the membership of each node, and the
// three virtual scopes this client keeps itself.

package tui

import (
	"fmt"
	"strings"

	"github.com/rivo/tview"
)

// The three virtual scopes. They are this client's own, not JumpServer nodes,
// so they carry no node membership -- each is expressed as a predicate over
// the loaded rows instead (see applyFilters).
const (
	scopeFavorites = "__favorites__"
	scopeRecent    = "__recent__"
	scopeAll       = "__all__"
)

// ScopeNode is a node returned by the current user's permission-tree API.
// ID, instead of Name, is used to preserve selection across refreshes --
// node names are not unique (production has nine nodes named "Platform").
type ScopeNode struct {
	ID       string
	Name     string
	Children []ScopeNode
}

// setScopeNodes installs a freshly-fetched permission tree. The nodes are
// retained so rebuildScopeTree can re-render them as the filter changes.
func (w *Workspace) setScopeNodes(nodes []ScopeNode) {
	w.scopeNodes = nodes
	w.invalidateSubtrees()
	// Rebuild first: a freshly fetched tree can retire the selected node and
	// fall back to another scope, which changes what the table should list.
	// Filtering afterwards means that settles in one pass.
	w.rebuildScopeTree()
	w.applyFilters(w.searchQuery)
}

// rebuildScopeTree redraws the scope panel against the current kind/search
// filter, hiding nodes whose subtree holds nothing and annotating the rest
// with their match count.
//
// Counts are tallied over every permitted asset, never just the current
// scope's: the tree is how you find where things live, so a node has to keep
// showing its total while you stand somewhere else.
//
// The whole tree is rebuilt from scratch on every call, as it always has
// been. One consequence, now that rebuilds happen on every filter change
// rather than once per login: tview.NewTreeNode defaults to expanded, so a
// node the user collapsed by hand pops back open. Preserving that is out of
// scope here -- noting it so it isn't mistaken for a new bug.
func (w *Workspace) rebuildScopeTree() {
	counts := subtreeMatches(w.scopeNodes, w.resources, w.kind, w.searchQuery)

	// The virtual scopes are always shown, even at zero: unlike a JumpServer
	// node, an empty Favorites list is not clutter -- it's how you find out
	// starring exists. Greyed when empty, matching the retained-node styling.
	allPermitted := w.virtualScopeNode("● All permitted", scopeAll, nil)
	root := tview.NewTreeNode("scope").SetReference("").SetSelectable(false)
	root.AddChild(w.virtualScopeNode("★ Favorites", scopeFavorites, w.favorites))
	root.AddChild(w.virtualScopeNode("◷ Recent", scopeRecent, w.recentSet()))
	root.AddChild(allPermitted)
	for _, node := range w.scopeNodes {
		if child := scopeTreeNode(node, counts, w.scopeID); child != nil {
			root.AddChild(child)
		}
	}

	// A missing node means a freshly fetched tree retired the selected scope;
	// a filter change never lands here, since the selected node is kept
	// visible even once it is empty.
	current := findScopeNode(root, w.scopeID)
	if current == nil {
		current = allPermitted
	}
	if reference, ok := current.GetReference().(string); ok {
		w.scopeID = reference
	}
	// A "/" search spans every permitted asset (see scopeMembers), so the
	// cursor moves to "All permitted" to say so. Leaving it on a node would
	// have the tree naming a scope the table is not honouring -- results from
	// all over the estate listed under a heading that claims one subtree.
	//
	// Deliberately after w.scopeID is settled, and not through changeTreeNode:
	// the node the user was standing in stays remembered, and clearing the
	// search puts the cursor straight back on it. SetCurrentNode does not fire
	// the tree's changed callback, so moving the cursor here cannot overwrite
	// that memory.
	//
	// Favorites and Recent are excluded because searching stays inside them --
	// their heading is still accurate. So is a node the user picked DURING the
	// search: that is a scope they chose with the results already in front of
	// them, so the table honours it (scopeMembers) and the cursor has to stay
	// on it -- otherwise every keystroke that refines the query would drag the
	// cursor back to the top of the tree.
	if w.searchQuery != "" && !isVirtualScope(w.scopeID) && !w.searchScopePicked {
		current = allPermitted
	}
	w.tree.SetRoot(root).SetCurrentNode(current)
}

// subtreeMatches reports, per node ID, how many distinct resources match the
// current filter within that node's whole subtree.
//
// Two properties this has to get right, both grounded in production data:
//   - Resources carry DIRECT node membership only, while a parent node in
//     JumpServer owns everything beneath it -- so counts aggregate downward.
//   - One resource can hang off several nodes at once (19 of 554 assets in
//     production do). Summing child totals would double-count those, so this
//     unions resource IDs and takes the set size.
//
// Nodes holding nothing are simply absent from the returned map.
func subtreeMatches(nodes []ScopeNode, resources []ResourceRow, kind ResourceKind, query string) map[string]int {
	counts := make(map[string]int)
	for nodeID, ids := range subtreeAssetIDs(nodes, resources, kind, query) {
		counts[nodeID] = len(ids)
	}
	return counts
}

// subtreeAssetIDs is subtreeMatches' underlying set computation: node ID to
// the set of matching resource IDs anywhere beneath it. Sets rather than
// running totals are what make the dedup above work, and selecting a scope
// node needs the membership itself, not just its size.
func subtreeAssetIDs(nodes []ScopeNode, resources []ResourceRow, kind ResourceKind, query string) map[string]map[string]struct{} {
	query = strings.ToLower(strings.TrimSpace(query))

	// The same set the table matches on (see matching), so a node's count and
	// the rows it opens to are decided by identical criteria -- a tree
	// counting on more columns than the table matches on would advertise rows
	// that are not there.
	cols := activeColumns(kind)

	byNode := make(map[string][]string)
	for _, resource := range resources {
		if !matchesFilter(resource, kind, query, cols) {
			continue
		}
		for _, nodeID := range resource.NodeIDs {
			byNode[nodeID] = append(byNode[nodeID], resource.ID)
		}
	}

	sets := make(map[string]map[string]struct{})
	var walk func(node ScopeNode) map[string]struct{}
	walk = func(node ScopeNode) map[string]struct{} {
		ids := make(map[string]struct{})
		for _, id := range byNode[node.ID] {
			ids[id] = struct{}{}
		}
		for _, child := range node.Children {
			for id := range walk(child) {
				ids[id] = struct{}{}
			}
		}
		if len(ids) > 0 {
			sets[node.ID] = ids
		}
		return ids
	}
	for _, node := range nodes {
		walk(node)
	}
	return sets
}

// subtrees maps each node ID to every asset anywhere beneath it, unfiltered.
//
// Cached because the answer depends only on the loaded assets and the node
// tree, and neither changes while the user is filtering. scopeMembers wants it
// on every applyFilters -- that is once per arrow key in the scope tree and
// once per character typed into "/" -- and rebuilding it costs a map per node
// over every asset each time. invalidateSubtrees drops it wherever either
// input is replaced.
func (w *Workspace) subtrees() map[string]map[string]struct{} {
	if w.scopeSubtrees == nil {
		w.scopeSubtrees = subtreeAssetIDs(w.scopeNodes, w.resources, ResourceAll, "")
	}
	return w.scopeSubtrees
}

func (w *Workspace) invalidateSubtrees() { w.scopeSubtrees = nil }

// isVirtualScope reports whether scopeID is one of the client's own scope
// entries rather than a JumpServer node -- they have no node membership, so
// subtree lookups don't apply to them. Mirrors internal/jmsd.isVirtualScope.
func isVirtualScope(scopeID string) bool {
	return scopeID == "" || (strings.HasPrefix(scopeID, "__") && strings.HasSuffix(scopeID, "__"))
}

// scopeMembers returns the set of asset IDs the current scope admits. nil
// means "no restriction", and only a virtual scope ever produces it.
// Favorites and Recent are sets this client keeps itself; a real node
// resolves to its whole subtree.
//
// Subtree membership is computed unfiltered on purpose: which node an asset
// hangs off doesn't depend on the kind or search in force.
func (w *Workspace) scopeMembers() map[string]struct{} {
	switch w.scopeID {
	case scopeFavorites:
		return w.favorites
	case scopeRecent:
		return w.recentSet()
	}
	if isVirtualScope(w.scopeID) {
		return nil
	}
	// A "/" search spans every permitted asset, whatever node the tree cursor
	// is sitting on. That cursor narrows the scope on every keypress (the tree
	// uses SetChangedFunc, not SetSelectedFunc), so anyone who arrowed through
	// the tree on their way to the search box was searching one subtree
	// without having chosen it -- and the machine being looked for is usually
	// in a different one. "No matching resources" now means it does not exist,
	// not that you were standing in the wrong place.
	//
	// Favorites and Recent return above and are deliberately not covered by
	// this: those are scopes you click into on purpose, and searching within
	// them is a real thing to want.
	//
	// The tree's own per-node counts stay filtered by the search, so they go
	// on saying WHERE the matches are -- which is the thing this scope was
	// silently answering wrong.
	//
	// searchScopePicked is what draws the line between "the cursor happened to
	// be here" and "I clicked here": the counts tell the user where the
	// matches are, so clicking that node is how they follow the tree to them.
	// Ignoring the click made the whole panel read as dead mid-search.
	if w.searchQuery != "" && !w.searchScopePicked {
		return nil
	}
	members := w.subtrees()[w.scopeID]
	if members == nil {
		// subtreeAssetIDs omits nodes holding nothing, and a node that is
		// selected but empty is a state the tree deliberately allows (see
		// scopeTreeNode). Handing that missing entry straight back would read
		// as nil, i.e. no scope at all, and show every permitted asset under a
		// node that contains none of them.
		return map[string]struct{}{}
	}
	return members
}

// trackSearchScope remembers which node the tree cursor was on when a search
// starts, and puts it back when the search ends. previousQuery is the search
// that was running before this call.
//
// It exists because moving the cursor to "All permitted" for the duration of a
// search costs the node otherwise. TreeView fires its changed callback from
// Draw, not from SetCurrentNode -- so the repaint after the cursor moves calls
// changeTreeNode with "All permitted" and writes that into scopeID. Without a
// copy held here, clearing the search would strand the user at the top of the
// tree instead of back in the node they were working in.
//
// Virtual scopes are skipped: searching stays inside Favorites and Recent, so
// their cursor never moves and there is nothing to restore.
func (w *Workspace) trackSearchScope(previousQuery string) {
	switch {
	case previousQuery == "" && w.searchQuery != "":
		// A fresh search always starts estate-wide, whatever node the cursor
		// was left on -- that is the whole point of scopeBeforeSearch.
		w.searchScopePicked = false
		if !isVirtualScope(w.scopeID) {
			w.scopeBeforeSearch = w.scopeID
		}
	case previousQuery != "" && w.searchQuery == "":
		// A node picked during the search outranks the one the search started
		// from: it is the more recent, and the more deliberate, of the two. So
		// the search ends where the user last pointed, not where they were
		// standing several keystrokes ago.
		if w.scopeBeforeSearch != "" && !w.searchScopePicked {
			w.scopeID = w.scopeBeforeSearch
		}
		w.scopeBeforeSearch, w.searchScopePicked = "", false
	}
}

func (w *Workspace) selectTreeNode(node *tview.TreeNode) {
	if len(node.GetChildren()) > 0 {
		node.SetExpanded(!node.IsExpanded())
	}
}

// changeTreeNode narrows the table to the highlighted node's subtree. Every
// permitted asset is already loaded, so this is a local re-filter: moving the
// cursor through the tree costs nothing and, crucially, leaves the other
// nodes' counts intact. Refetching a node's own subset used to blank them --
// the tree collapsed to whichever branch you were standing on.
//
// applyFilters, NOT renderResources: tview invokes this from TreeView.process,
// which runs *inside* TreeView.Draw. Rebuilding the tree here would swap in a
// new root mid-draw, leaving the rest of that pass comparing old-generation
// nodes against a current node from the new tree -- nothing matches, so the
// selection highlight vanishes and the cursor looks lost.
//
// Nothing is missed by skipping the rebuild: node visibility and counts
// depend only on kind and search, never on which scope is selected. The one
// scope-dependent detail is that an empty node is kept visible while
// selected; it now lingers until the next kind/search change, which is the
// better behaviour anyway -- nodes shouldn't vanish from under the cursor as
// you arrow past them.
func (w *Workspace) changeTreeNode(node *tview.TreeNode) {
	if reference, ok := node.GetReference().(string); ok && reference != "" {
		if reference == w.scopeID {
			return
		}
		// A real node reached mid-search is a deliberate pick. Nothing else can
		// land here with one: the only cursor move this code makes during a
		// search is rebuildScopeTree's fallback to "All permitted", which is
		// virtual -- and TreeView fires this callback from Draw on every
		// rebuild, so a test on "did the reference change" alone would count
		// that fallback as a pick.
		if w.searchQuery != "" && !isVirtualScope(reference) {
			w.searchScopePicked = true
		}
		w.scopeID = reference
		w.applyFilters(w.searchQuery)
		w.setStatus(w.username, statusOK, "ready")
	}
}

func (w *Workspace) recentSet() map[string]struct{} {
	set := make(map[string]struct{}, len(w.recent))
	for _, id := range w.recent {
		set[id] = struct{}{}
	}
	return set
}

// virtualScopeNode builds one of the three client-side scope entries, labelled
// with how many currently-matching rows it holds. members nil means "every
// permitted asset" (All permitted), which needs no membership test.
func (w *Workspace) virtualScopeNode(label, reference string, members map[string]struct{}) *tview.TreeNode {
	count := len(w.matching(nil, members))
	return scopeNode(fmt.Sprintf("%s (%d)", label, count), reference, count == 0)
}

// scopeNode builds one tree node. dimmed marks a node holding nothing under
// the current filter, kept only so the count still reads as empty (or so the
// selection stays reachable -- see scopeTreeNode).
//
// The selected style is stated rather than left to tview: TreeView's default
// inverts each node against the page background, so a dimmed node highlighted
// in grey while the table highlighted in blue -- the two panes disagreed about
// what "selected" looks like. It has to come after SetColor, which writes the
// selected style's background as a side effect.
func scopeNode(label, reference string, dimmed bool) *tview.TreeNode {
	colour := theme.Fg
	if dimmed {
		colour = theme.Muted
	}
	return tview.NewTreeNode(label).
		SetReference(reference).
		SetColor(colour).
		SetSelectedTextStyle(selectionStyle(theme))
}

// scopeTreeNode renders one node and its descendants, or nil if the node's
// whole subtree is empty under the current filter. selectedID is exempt from
// hiding: yanking the cursor out from under the user would be jarring, and
// moving it elsewhere would trigger a scope change -- i.e. a refetch.
func scopeTreeNode(node ScopeNode, counts map[string]int, selectedID string) *tview.TreeNode {
	// Children are resolved first: a node must survive if it merely holds the
	// retained selection somewhere below it, otherwise pruning an empty
	// ancestor would take the selected node down with it.
	var children []*tview.TreeNode
	for _, child := range node.Children {
		if rendered := scopeTreeNode(child, counts, selectedID); rendered != nil {
			children = append(children, rendered)
		}
	}

	count, matched := counts[node.ID]
	if !matched && node.ID != selectedID && len(children) == 0 {
		return nil
	}

	// !matched means the node is retained only to keep the selection
	// reachable, so it is dimmed and its empty count reads as empty.
	label := fmt.Sprintf("%s (%d)", tview.Escape(node.Name), count)
	result := scopeNode(label, node.ID, !matched)
	for _, child := range children {
		result.AddChild(child)
	}
	return result
}

func cloneScopeNodes(nodes []ScopeNode) []ScopeNode {
	cloned := make([]ScopeNode, len(nodes))
	for i, node := range nodes {
		cloned[i] = ScopeNode{ID: node.ID, Name: node.Name, Children: cloneScopeNodes(node.Children)}
	}
	return cloned
}

func findScopeNode(root *tview.TreeNode, id string) *tview.TreeNode {
	if reference, ok := root.GetReference().(string); ok && reference == id {
		return root
	}
	for _, child := range root.GetChildren() {
		if found := findScopeNode(child, id); found != nil {
			return found
		}
	}
	return nil
}
