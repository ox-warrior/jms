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
// columnRole says what a table column is, independently of where it sits.
//
// Position used to be identity -- nameColumn = 0 and so on -- which stopped
// working the moment a per-kind column had to sit BETWEEN two fixed ones
// (OWNER between NAME and ADDRESS). The role is what the rest of the file
// actually cares about: which cell the double-click copies from, which two
// columns absorb the spare width.
type columnRole int

const (
	roleName columnRole = iota
	roleAddress
	roleSourceType
	// roleExtra is a per-kind column: it draws its text from value and is
	// present only for the kinds it lists.
	roleExtra
)

// column is one column of the resources table.
type column struct {
	role    columnRole
	heading string
	// reserve is the display width held back so tview cannot drop this column
	// off the right edge -- it lays columns out left to right and simply
	// discards what no longer fits, so without a reserve the LAST column is
	// the one that vanishes on a narrow terminal. Zero means "takes the spare
	// width instead", which is NAME and ADDRESS.
	//
	// The numbers come from the widest value in production, not from
	// guesswork: instance types run to 18 characters, team names to 12
	// ("Payments"), modules to 7 ("Monitor").
	reserve int
	// kinds are the resource kinds this column applies to; nil means every
	// kind. ResourceAll is deliberately absent from the per-kind ones: the
	// mixed view would show columns that are blank for most of their rows,
	// and the width they cost comes out of NAME and ADDRESS, which every row
	// uses.
	kinds []ResourceKind
	// value renders the cell, for roleExtra only. The other roles have their
	// own rendering (a star prefix, a source-type palette).
	value func(resource ResourceRow) string
}

// hostKinds are the kinds whose rows are machines, and so the ones with a
// shape, an owning team and a module.
var hostKinds = []ResourceKind{ResourceEC2}

// tableColumns is the column order, top to bottom of this slice, left to right
// on screen. Adding a column is one entry here plus one field on
// jmsd.AssetRow -- and the second half is the expensive one, see
// jmsd.cloudBrief for the bar it has to clear.
var tableColumns = []column{
	{role: roleName, heading: "NAME"},
	{role: roleExtra, heading: "OWNER", reserve: 12, kinds: hostKinds,
		value: func(r ResourceRow) string { return r.OwnerTeam }},
	{role: roleAddress, heading: "ADDRESS"},
	{role: roleExtra, heading: "INSTANCE TYPE", reserve: 18, kinds: hostKinds,
		value: func(r ResourceRow) string { return r.InstanceType }},
	{role: roleExtra, heading: "MODULE", reserve: 7, kinds: hostKinds,
		value: func(r ResourceRow) string { return r.AppModule }},
	{role: roleSourceType, heading: sourceTypeHeading, reserve: sourceTypeReserve},
}

// activeColumns is the layout for one resource kind, in table order.
//
// Width plays no part. An earlier version dropped columns to fit and that was
// a mistake twice over: the cells are written before any width is known, so
// changing the SET meant rebuilding the table from inside its own draw
// function -- and the only safe way to defer that, QueueUpdate, blocks until
// the event loop runs it, which during a draw is the event loop waiting for
// itself. Widths are capped instead (see columnWidths); every column stays.
func activeColumns(kind ResourceKind) []column {
	out := make([]column, 0, len(tableColumns))
	for _, c := range tableColumns {
		if c.kinds == nil || slices.Contains(c.kinds, kind) {
			out = append(out, c)
		}
	}
	return out
}

// columnIndex finds where a role sits in a layout, or -1.
func columnIndex(layout []column, role columnRole) int {
	for i, c := range layout {
		if c.role == role {
			return i
		}
	}
	return -1
}

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
	ID      string
	Name    string
	Address string
	// CloudKey addresses this machine's cloud metadata, empty when it has
	// none. The listing carries the key rather than the metadata, so "d"
	// fetches the one row it needs -- and an empty key answers "d" with no
	// request at all.
	CloudKey string
	// InstanceType, OwnerTeam and AppModule are shown as per-kind columns
	// under ":ec2" -- see extraColumns. Only hosts have them.
	InstanceType string
	OwnerTeam    string
	AppModule    string
	Category     string // JumpServer asset category: host / database / cloud / ...
	Type         string // JumpServer asset type within category, e.g. redis / k8s
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
func matchesFilter(resource ResourceRow, kind ResourceKind, query string, layout []column) bool {
	if !jmsd.MatchesKind(kind, resource.Category, resource.Type) {
		return false
	}
	if query == "" {
		return true
	}
	return strings.Contains(searchableText(resource, layout), query)
}

// searchableText is every cell of the row that is currently on screen, lowered
// and joined.
//
// Scoped to the columns the current kind DECLARES, which is what makes a
// match explainable: the row that came back has the query in one of its cells.
// Searching fields the view does not show at all would make rows appear for no
// visible reason, indistinguishable from a broken filter.
//
// Width is deliberately not part of it -- see the caller. A column trimmed off
// a narrow window is still searched, which is the lesser of two oddities next
// to "resizing the terminal changed my results".
func searchableText(resource ResourceRow, layout []column) string {
	parts := make([]string, 0, len(layout))
	for _, col := range layout {
		switch col.role {
		case roleName:
			parts = append(parts, resource.Name)
		case roleAddress:
			parts = append(parts, resource.Address)
		case roleSourceType:
			parts = append(parts, sourceTypeLabel(resource))
		default:
			parts = append(parts, col.value(resource))
		}
	}
	return strings.ToLower(strings.Join(parts, " "))
}

// matching collects every loaded resource that passes the current kind and
// search filter and is admitted by members (nil for no scope restriction),
// reusing dst's storage. It is the one definition of "this row is currently
// shown": the table renders what it returns and the tree's virtual-scope
// counts are its length, so the two cannot drift apart.
func (w *Workspace) matching(dst []ResourceRow, members map[string]struct{}) []ResourceRow {
	dst = dst[:0]
	// Resolved once, not per row: the layout is the same for every row in a
	// render, and this runs on every keystroke in the "/" box. The scope tree
	// derives the same set (subtreeAssetIDs) so a node's count and the rows
	// it opens to are decided identically.
	cols := w.columns()
	for _, resource := range w.resources {
		if !matchesFilter(resource, w.kind, w.searchQuery, cols) {
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
	layout := w.columns()
	for i, resource := range w.visible {
		row := i + 1
		name, nameWidth := resource.Name, resource.nameWidth
		if _, starred := w.favorites[resource.ID]; starred {
			name, nameWidth = starPrefix+name, nameWidth+starPrefixWidth
		}
		w.naturalName = max(w.naturalName, nameWidth)
		w.naturalAddress = max(w.naturalAddress, resource.addressWidth)
		for index, col := range layout {
			var cell *tview.TableCell
			switch col.role {
			case roleName:
				cell = expandingCell(name, resource.ID)
			case roleAddress:
				cell = expandingCell(resource.Address, resource.ID)
			case roleSourceType:
				cell = narrowCell(sourceTypeLabel(resource), resource.ID)
			default:
				cell = narrowCell(col.value(resource), resource.ID)
			}
			w.table.SetCell(row, index, cell)
		}
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
func columnCaps(width, naturalName, naturalAddress, otherReserve int) (nameMax, addressMax int) {
	budget := width - otherReserve
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

// columnWidths returns the MaxWidth for each column of a table this wide.
// Zero means "no cap".
//
// The order the budget is spent in is the whole policy, and it is ordered by
// what is unusable without:
//
//  1. one cell per column, which tview charges as separators;
//  2. SOURCE TYPE's reserve, and NAME and ADDRESS's floors -- the three
//     columns every row has something in;
//  3. each per-kind column, left to right, up to its reserve. What is left
//     over caps it instead, down to a single cell: an ellipsis keeps the
//     column and its heading on screen, where dropping it would leave the
//     reader wondering whether the field exists at all;
//  4. anything still spare widens NAME and ADDRESS, up to their natural
//     widths.
//
// Nothing is ever removed. tview lays columns out left to right and discards
// what does not fit, so the casualty of overflowing would be the LAST column
// -- and that is SOURCE TYPE, which this package already has a regression
// test for losing.
func columnWidths(cols []column, width, naturalName, naturalAddress int) []int {
	caps := make([]int, len(cols))

	// What is left after the costs nothing can be taken from: one cell per
	// column, which tview charges as separators, and SOURCE TYPE's reserve.
	avail := width - len(cols)
	for _, col := range cols {
		if col.role == roleSourceType {
			avail -= col.reserve
		}
	}

	// Then the per-kind columns, left to right, each yielding to NAME and
	// ADDRESS's floors rather than the other way round.
	for i, col := range cols {
		if col.role != roleExtra {
			continue
		}
		switch spare := avail - nameFloor - addressFloor; {
		case spare >= col.reserve:
			avail -= col.reserve // fits at full width, no cap needed
		case spare > 0:
			caps[i] = spare
			avail -= spare
		default:
			// A single cell renders as an ellipsis, which keeps the column
			// and its heading on screen. Dropping it would leave the reader
			// wondering whether the field exists at all -- and dropping is
			// also what tview does on its own, from the right, where the
			// casualty is SOURCE TYPE.
			caps[i] = 1
			avail--
		}
	}

	nameMax, addressMax := columnCaps(avail, naturalName, naturalAddress, 0)
	if at := columnIndex(cols, roleName); at >= 0 {
		caps[at] = nameMax
	}
	if at := columnIndex(cols, roleAddress); at >= 0 {
		caps[at] = addressMax
	}
	return caps
}

// fitColumns re-caps the columns for a table of the given inner width. Called
// on every draw, so it does nothing unless the width or the rows changed.
//
// Only MaxWidth is touched. This runs inside the table's own SetDrawFunc, so
// it must not add, remove or reselect cells -- see activeColumns.
func (w *Workspace) fitColumns(width int) {
	if width == w.fitWidth && !w.fitDirty {
		return
	}
	w.fitWidth = width
	w.fitDirty = false

	cols := w.columns()
	caps := columnWidths(cols, width, w.naturalName, w.naturalAddress)
	// Row 0 is the header, which is capped too -- a heading that outlived its
	// column would be measured into the layout and defeat the cap.
	for row := 0; row <= len(w.visible); row++ {
		for column := range cols {
			if cell := w.table.GetCell(row, column); cell != nil {
				cell.SetMaxWidth(caps[column])
			}
		}
	}
}

func (w *Workspace) setTableHeader() {
	for index, col := range w.columns() {
		// Only NAME and ADDRESS take a share of the spare width; every other
		// column holds short values out of a small vocabulary, and giving
		// them room would take it from the two that actually get truncated.
		w.table.SetCell(0, index, headerCell(col.heading, col.reserve == 0))
	}
}

// columns is the active column set. Derived on each call rather than cached in
// a field, so it cannot go stale against w.kind.
func (w *Workspace) columns() []column { return activeColumns(w.kind) }

func headerCell(heading string, expand bool) *tview.TableCell {
	cell := tview.NewTableCell(heading)
	cell.SetTextColor(theme.Header)
	cell.SetAttributes(tcell.AttrBold)
	cell.SetSelectable(false)
	if expand {
		cell.SetExpansion(1)
	}
	return cell
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

// expandingCell is a cell in one of the two columns that absorb the spare
// width, NAME and ADDRESS.
func expandingCell(text, id string) *tview.TableCell {
	return narrowCell(text, id).SetExpansion(2)
}

// narrowCell is expandingCell without the expansion, so the column stays the
// width of its widest value instead of taking a share of the table for a
// four-character word. Narrow is the only thing different about it: these
// cells used to be dimmed as well, which read as "this one matters less".
//
// Used by every column except NAME and ADDRESS -- it was called
// sourceTypeCell when SOURCE TYPE was the only one.
func narrowCell(text, id string) *tview.TableCell {
	cell := tview.NewTableCell(tview.Escape(text))
	cell.SetReference(id)
	return cell
}
