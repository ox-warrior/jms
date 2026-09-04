// Resource table: which of the loaded assets are on screen, what each row
// says, and how the columns share a narrow terminal.

package tui

import (
	"cmp"
	"slices"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
)

// ResourceKind is the resource collection currently displayed by Workspace.
//
// The type, its five constants and the classification logic all live in
// internal/jmsd (jmsd.AssetRow is what is being classified, and the CLI's
// headless "-c" shortcut needs the same mapping without depending on this
// package) -- aliased back here so every existing reference in this package
// keeps compiling unchanged.
type ResourceKind = jmsd.ResourceKind

// Column indices in the resources table. Named because the header, the width
// rule and the double-click copy all refer to them.
const (
	nameColumn       = 0
	addressColumn    = 1
	sourceTypeColumn = 2
)

// Resource table column widths, in cells.
//
// tview lays columns out left to right, giving each one its content width and
// simply dropping whatever no longer fits -- so on a narrow terminal the LAST
// column is the one that vanishes, and SOURCE TYPE is last. Left alone, an RDS
// endpoint (50-odd characters) eats the whole table and the column the user
// added is the one they stop being able to see. NAME and ADDRESS are therefore
// capped so SOURCE TYPE always has its cells.
// The three column headings, named here because two other things are derived
// from them: sourceTypeReserve below, and the starting values the natural
// widths are measured up from (headingWidths).
const (
	nameHeading       = "NAME"
	addressHeading    = "ADDRESS"
	sourceTypeHeading = "SOURCE TYPE"
)

// starPrefix marks a starred row in the NAME column.
const starPrefix = "★ "

// Display widths of the fixed strings the NAME column is measured against.
// Package-level because tview.TaggedStringWidth is not cheap -- it runs the
// string through uniseg's grapheme/word/line-break state machines -- and
// these three never change, so measuring them per render (once for each
// heading, once per starred row) was pure repetition.
var (
	nameHeadingWidth    = tview.TaggedStringWidth(nameHeading)
	addressHeadingWidth = tview.TaggedStringWidth(addressHeading)
	starPrefixWidth     = tview.TaggedStringWidth(starPrefix)
)

const (
	// sourceTypeReserve is what SOURCE TYPE is guaranteed. Its own heading is
	// wider than any label under it, so the heading is the width to hold back.
	sourceTypeReserve = len(sourceTypeHeading)
	// columnGaps is the total separator width tview charges across the three
	// columns -- one cell between each pair, plus one it keeps at the end.
	columnGaps = 3
	// nameFloor and addressFloor are how far each column may be squeezed
	// before the other gives way instead. ADDRESS yields first: it is the
	// longest, the most repetitive (shared domain suffixes), and the one you
	// are least likely to be reading when picking a row.
	nameFloor    = 12
	addressFloor = 16
)

const (
	ResourceAll   = jmsd.ResourceAll
	ResourceEC2   = jmsd.ResourceEC2
	ResourceRDS   = jmsd.ResourceRDS
	ResourceRedis = jmsd.ResourceRedis
	ResourceK8s   = jmsd.ResourceK8s
)

// sourceTypeLabel is what the SOURCE TYPE column shows for a resource.
//
// Anything jmsd.KindOf cannot place falls back to its raw JumpServer category
// rather than a dash: an asset outside the four known kinds is still a real
// asset, and "web" tells you what it is, while "-" reads as missing data and
// sends you to the web console to find out.
func sourceTypeLabel(resource ResourceRow) string {
	if kind := jmsd.KindOf(resource.Category, resource.Type); kind != "" {
		return string(kind)
	}
	if resource.Category != "" {
		return resource.Category
	}
	return "-"
}

// ResourceRow is one row in the resources panel.
type ResourceRow struct {
	ID       string
	Name     string
	Address  string
	Category string // JumpServer asset category: host / database / cloud / ...
	Type     string // JumpServer asset type within category, e.g. redis / k8s
	// NodeIDs is direct node membership only, never ancestors, so subtree
	// tallies have to aggregate down the tree themselves -- see subtreeMatches.
	NodeIDs []string
	// nameWidth and addressWidth are the display widths of Name and Address,
	// measured once in applyResources and carried along by the filtered copy
	// in w.visible.
	//
	// Cached because measuring them is the single most expensive thing a
	// keystroke in the "/" box used to do: applyFilters re-measured every
	// visible row, and tview.TaggedStringWidth walks each string through
	// uniseg's break-state machines -- 70% of a 3.3ms keystroke at production
	// scale (554 assets), against strings that cannot change between
	// keystrokes because the filter only decides which rows are shown, never
	// what they say.
	nameWidth    int
	addressWidth int
}

func (w *Workspace) applyResources(rows []ResourceRow) {
	w.resources = append(w.resources[:0], rows...)
	// The one place rows enter the workspace, so the one place their widths
	// have to be measured (see ResourceRow.nameWidth).
	for i := range w.resources {
		w.resources[i].nameWidth = tview.TaggedStringWidth(w.resources[i].Name)
		w.resources[i].addressWidth = tview.TaggedStringWidth(w.resources[i].Address)
	}
	w.invalidateSubtrees()
	// The active "/" filter has to survive this. Rows arrive here from a scope
	// switch or a background refresh, neither of which the user asked to
	// dismiss their filter -- and the footer goes on advertising it either way.
	w.renderResources(w.searchQuery)
}

// matchesFilter is the single definition of "this resource is currently
// shown". Both the table and the scope tree's per-node counts go through it,
// so the tree can never claim a node holds rows the table wouldn't list.
// query must already be lower-cased and trimmed.
func matchesFilter(resource ResourceRow, kind ResourceKind, query string) bool {
	if !jmsd.MatchesKind(kind, resource.Category, resource.Type) {
		return false
	}
	if query == "" {
		return true
	}
	searchable := strings.ToLower(strings.Join([]string{
		resource.Name, resource.Address,
	}, " "))
	return strings.Contains(searchable, query)
}

// matching collects every loaded resource that passes the current kind and
// search filter and is admitted by members (nil for no scope restriction),
// reusing dst's storage. It is the one definition of "this row is currently
// shown": the table renders what it returns and the tree's virtual-scope
// counts are its length, so the two cannot drift apart.
func (w *Workspace) matching(dst []ResourceRow, members map[string]struct{}) []ResourceRow {
	dst = dst[:0]
	for _, resource := range w.resources {
		if !matchesFilter(resource, w.kind, w.searchQuery) {
			continue
		}
		if members != nil {
			if _, ok := members[resource.ID]; !ok {
				continue
			}
		}
		dst = append(dst, resource)
	}
	return dst
}

// SetMarks installs the starred and recently-used lists fetched from jmsd
// (which reads both out of Core). Safe to call whenever they are refreshed.
func (w *Workspace) SetMarks(favorites, recent []string) {
	w.favorites = make(map[string]struct{}, len(favorites))
	for _, id := range favorites {
		w.favorites[id] = struct{}{}
	}
	w.recent = append([]string(nil), recent...)
	w.renderResources(w.searchQuery)
}

// QueueMarks is the worker-safe entry point for SetMarks.
func (w *Workspace) QueueMarks(favorites, recent []string) {
	favorites = append([]string(nil), favorites...)
	recent = append([]string(nil), recent...)
	w.app.QueueUpdateDraw(func() { w.SetMarks(favorites, recent) })
}

// toggleFavorite stars or unstars the highlighted row. Bound to "y".
//
// The local set is updated and redrawn before jmsd is told, so the star
// appears instantly rather than after a round-trip. RevertFavorite puts it
// back if the write turns out to have failed.
func (w *Workspace) toggleFavorite() {
	row, _ := w.table.GetSelection()
	if row < 1 || row > len(w.visible) {
		return
	}
	resource := w.visible[row-1]
	_, starred := w.favorites[resource.ID]
	w.applyFavorite(resource.ID, !starred)

	if !starred {
		w.setStatus(w.username, statusOK, "starred "+resource.Name)
	} else {
		w.setStatus(w.username, statusOK, "unstarred "+resource.Name)
	}
	if w.actions.SetFavorite != nil {
		w.actions.SetFavorite(resource.ID, !starred)
	}
}

// applyFavorite sets one asset's starred state locally and redraws, so the
// row's star and the Favorites count update -- and unstarring while inside
// the Favorites scope drops the row immediately.
func (w *Workspace) applyFavorite(assetID string, starred bool) {
	if starred {
		w.favorites[assetID] = struct{}{}
	} else {
		delete(w.favorites, assetID)
	}
	w.renderResources(w.searchQuery)
}

// RevertFavorite undoes an optimistic star change that jmsd rejected, so the
// UI cannot keep showing a star that was never actually saved. Worker-safe.
func (w *Workspace) RevertFavorite(assetID string, starred bool, reason string) {
	w.app.QueueUpdateDraw(func() {
		w.applyFavorite(assetID, starred)
		w.setStatus(w.username, statusFailed, "star failed: "+reason)
	})
}

// renderResources re-filters the table and rebuilds the scope tree. Callers
// reached from the input phase (kind change, "/" filter, a fresh load) want
// both.
//
// Anything invoked from inside tview's tree "changed" callback must call
// applyFilters instead -- see changeTreeNode.
func (w *Workspace) renderResources(query string) {
	w.applyFilters(query)
	// Safe here, but not from a tree callback: the rebuild swaps the tree's
	// root out. It cannot retire the current scope (the selected node is kept
	// even when empty), so w.visible stays valid and needs no second pass.
	w.rebuildScopeTree()
}

// applyFilters recomputes the visible rows for the current scope+kind+search
// and repaints the table and footer. It deliberately does NOT touch the tree,
// which makes it the only variant safe to call while the tree is drawing.
//
// Mutating the table and footer mid-draw is fine: both are drawn after the
// tree in the layout, so they pick up the new content in the same pass.
func (w *Workspace) applyFilters(query string) {
	previousQuery := w.searchQuery
	w.searchQuery = strings.ToLower(strings.TrimSpace(query))
	w.trackSearchScope(previousQuery)

	w.visible = w.matching(w.visible, w.scopeMembers())
	if w.scopeID == scopeRecent {
		// Recent is the one scope with a meaningful order of its own.
		order := make(map[string]int, len(w.recent))
		for i, id := range w.recent {
			order[id] = i
		}
		slices.SortStableFunc(w.visible, func(a, b ResourceRow) int {
			return cmp.Compare(order[a.ID], order[b.ID])
		})
	}
	// After w.visible is settled, so the indicator's match count is accurate,
	// and before the early return on the empty branch.
	w.renderFooter()
	w.table.Clear()
	w.setTableHeader()
	if len(w.visible) == 0 {
		message := "No resources · configure API access or refresh"
		if len(w.resources) > 0 {
			message = "No matching resources"
		}
		cell := tview.NewTableCell(message)
		cell.SetTextColor(theme.Muted)
		cell.SetSelectable(false)
		cell.SetExpansion(1)
		w.table.SetCell(1, 0, cell)
		// setTableHeader above built fresh cells, so whatever caps fitColumns
		// had applied went with the old ones -- exactly as in the populated
		// branch below. Without this the heading keeps its natural width until
		// the next resize.
		w.fitDirty = true
		// Selection must be off while every cell is NotSelectable, or the
		// next navigation key hangs the whole app. Table.Draw walks
		// selectedRow forward looking for a selectable cell and leaves it at
		// rowCount when there is none; tview's down()/up() then spin forever
		// in forward()/backwards() looking for a cell that cannot exist,
		// inside the event-loop goroutine -- so even Ctrl+C stops working.
		w.table.SetSelectable(false, false)
		w.table.Select(0, 0)
		return
	}
	// Accumulated here rather than by re-walking the table later: this loop
	// already knows every row that goes into it, star prefix included. The
	// per-row widths come pre-measured (see ResourceRow.nameWidth); only the
	// star's own width is added here, since starring is not a property of the
	// row's text.
	w.naturalName, w.naturalAddress = nameHeadingWidth, addressHeadingWidth
	for i, resource := range w.visible {
		row := i + 1
		name, nameWidth := resource.Name, resource.nameWidth
		if _, starred := w.favorites[resource.ID]; starred {
			name, nameWidth = starPrefix+name, nameWidth+starPrefixWidth
		}
		w.naturalName = max(w.naturalName, nameWidth)
		w.naturalAddress = max(w.naturalAddress, resource.addressWidth)
		w.table.SetCell(row, 0, resourceCell(name, resource.ID))
		w.table.SetCell(row, 1, resourceCell(resource.Address, resource.ID))
		w.table.SetCell(row, 2, sourceTypeCell(sourceTypeLabel(resource), resource.ID))
	}
	// The cells above are new, so whatever caps were applied to the old ones
	// are gone with them.
	w.fitDirty = true
	selectedRow := 1
	for index, resource := range w.visible {
		if resource.ID == w.selectedID {
			selectedRow = index + 1
			break
		}
	}
	// Re-arm selection (it may have been switched off by a previous empty
	// render) before pointing it at a row that now exists.
	w.table.SetSelectable(true, false)
	w.table.Select(selectedRow, 0)
}

// columnCaps returns the MaxWidth to put on the NAME and ADDRESS columns so
// that SOURCE TYPE still fits in a table this wide. Zero means "no cap".
//
// Split out from fitColumns because this is the whole of the resize policy and
// it is worth being able to test at widths a terminal is awkward to produce.
func columnCaps(width, naturalName, naturalAddress int) (nameMax, addressMax int) {
	budget := width - columnGaps - sourceTypeReserve
	if budget <= 0 || naturalName+naturalAddress <= budget {
		// Either everything fits already, or the table is too narrow for any
		// arrangement to help. Capping in the first case would truncate text
		// there was room for; in the second it would only pick which column
		// gets mangled.
		return 0, 0
	}
	// NAME keeps its natural width, but never so much that ADDRESS drops below
	// its floor -- and never less than its own floor, whatever that costs
	// ADDRESS. Below both floors ADDRESS is left with a single cell: an
	// ellipsis keeps the column present, where letting it back to its natural
	// width would push SOURCE TYPE off the edge again.
	nameMax = max(min(naturalName, budget-addressFloor), nameFloor)
	return nameMax, max(budget-nameMax, 1)
}

// fitColumns re-caps the columns for a table of the given inner width. Called
// on every draw, so it does nothing unless the width or the rows changed.
func (w *Workspace) fitColumns(width int) {
	if width == w.fitWidth && !w.fitDirty {
		return
	}
	w.fitWidth = width
	w.fitDirty = false

	nameMax, addressMax := columnCaps(width, w.naturalName, w.naturalAddress)
	// Row 0 is the header, which is capped too -- a heading that outlived its
	// column would be measured into the layout and defeat the cap.
	for row := 0; row <= len(w.visible); row++ {
		w.table.GetCell(row, 0).SetMaxWidth(nameMax)
		w.table.GetCell(row, 1).SetMaxWidth(addressMax)
	}
}

func (w *Workspace) setTableHeader() {
	// Indexed by the column constants so the headings and the indices cannot
	// drift out of order.
	headings := [...]string{nameColumn: nameHeading, addressColumn: addressHeading, sourceTypeColumn: sourceTypeHeading}
	for column, heading := range headings {
		cell := tview.NewTableCell(heading)
		cell.SetTextColor(theme.Header)
		cell.SetAttributes(tcell.AttrBold)
		cell.SetSelectable(false)
		// SOURCE TYPE holds one short word out of a fixed vocabulary, so it
		// takes no share of the spare width -- that all goes to NAME and
		// ADDRESS, which are the columns that actually get truncated.
		if column != sourceTypeColumn {
			cell.SetExpansion(1)
		}
		w.table.SetCell(0, column, cell)
	}
}

// SetInitialKind pre-selects which resource kind the workspace opens showing
// -- the effect of the "-c TYPE" shortcut with no name (docs/PLAN.md §8.1),
// equivalent to typing ":<kind>" once the first load lands. Must be called
// before the first requestLoad, and at most once; empty is a no-op, which
// covers the ordinary launch (no "-c") for free.
//
// This does not go through changeKind: nothing has been rendered yet, so
// there is no already-drawn table to refresh and no status line worth
// announcing "ready" on top of the "loading…" one requestLoad is about to set.
func (w *Workspace) SetInitialKind(kind ResourceKind) {
	if kind != "" {
		w.kind = kind
	}
}

// changeKind switches which cloud resource category is shown. Unlike a scope
// change, the already-loaded asset list carries enough data (category/type)
// to filter locally -- no need to refetch from jmsd.
func (w *Workspace) changeKind(kind ResourceKind) {
	if kind == w.kind {
		return
	}
	w.kind = kind
	// Kind and search are independent filters that compose; switching one
	// must not silently drop the other. Esc (resetFilters) clears both.
	w.renderResources(w.searchQuery)
	w.setStatus(w.username, statusOK, "ready")
}

// resetFilters is the Esc action in browse mode: back to :all with no search.
// It deliberately does not go through changeKind, which no-ops when the kind
// is already :all -- that would leave an active "/" filter stuck on with no
// keyboard way to clear it.
func (w *Workspace) resetFilters() {
	if w.kind == ResourceAll && w.searchQuery == "" {
		return
	}
	w.kind = ResourceAll
	w.renderResources("")
	w.setStatus(w.username, statusOK, "ready")
}

func (w *Workspace) filterCurrent(query string) {
	w.renderResources(query)
}

func resourceCell(text, id string) *tview.TableCell {
	return sourceTypeCell(text, id).SetExpansion(2)
}

// sourceTypeCell is resourceCell without the expansion, so the column stays
// the width of its widest label instead of taking a third of the table for a
// four-character word. Narrow is the only thing different about it: it used
// to be dimmed as well, which read as "this one matters less".
func sourceTypeCell(text, id string) *tview.TableCell {
	cell := tview.NewTableCell(tview.Escape(text))
	cell.SetReference(id)
	return cell
}
