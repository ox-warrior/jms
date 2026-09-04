// Tests for resources.go: which assets are on screen and how the columns
// share a narrow terminal.

package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
)

// The kind table itself is tested in internal/jmsd, where it lives. What
// belongs here is only what the TUI adds on top of it -- see
// TestSourceTypeLabelAgreesWithKindFilter.

// copyWorkspace is a workspace whose clipboard writes are recorded instead of
// performed, so these tests never touch the real one.
func copyWorkspace(t *testing.T, err error) (*Workspace, *[]string) {
	t.Helper()
	var copied []string
	w := NewWorkspace(tview.NewApplication(), "tester", WorkspaceActions{
		CopyName: func(name string) error {
			copied = append(copied, name)
			return err
		},
	})
	return w, &copied
}

// The value has to come from the row data, not the screen. A narrow terminal
// caps both NAME and ADDRESS (columnCaps), so the drawn text can be an
// ellipsised prefix -- and `jms -c "ec2 <name>"` matches exactly, so a prefix
// connects to nothing.
func TestDoubleClickCopiesTheWholeValueNotWhatFitsOnScreen(t *testing.T) {
	const (
		longName    = "prod-web-api-with-a-very-long-name"
		longAddress = "prod-mysql.cluster-abc123.eu-central-1.rds.amazonaws.com"
	)
	w, copied := copyWorkspace(t, nil)
	w.applyResources([]ResourceRow{{ID: "1", Name: longName, Address: longAddress, Category: "host"}})
	// Narrow enough that both columns are certainly capped.
	w.fitColumns(30)

	// The column decides which field: aiming at ADDRESS and getting the name
	// back is what makes the gesture feel arbitrary.
	w.copyCell(1, addressColumn)
	w.copyCell(1, nameColumn)
	// SOURCE TYPE holds one word from a fixed vocabulary; copying "ec2" back
	// to yourself helps nobody, so it copies the name like the rest of the row.
	w.copyCell(1, sourceTypeColumn)

	want := []string{longAddress, longName, longName}
	if len(*copied) != len(want) {
		t.Fatalf("copied %v, want %v", *copied, want)
	}
	for i, wantValue := range want {
		if (*copied)[i] != wantValue {
			t.Errorf("copy %d = %q, want %q", i, (*copied)[i], wantValue)
		}
	}
	if got := w.status; !strings.Contains(got, "copied "+longName) {
		t.Errorf("status %q does not confirm what was copied", got)
	}
	if w.statusColor != theme.Hint {
		t.Errorf("confirmation drawn in %v, want the Hint colour", w.statusColor)
	}
}

// An asset with no address still has a name, so the ADDRESS column must not
// copy an empty string over whatever was on the clipboard before.
func TestDoubleClickOnAnEmptyAddressFallsBackToTheName(t *testing.T) {
	w, copied := copyWorkspace(t, nil)
	w.applyResources([]ResourceRow{{ID: "1", Name: "cluster", Address: "", Category: "cloud", Type: "k8s"}})

	w.copyCell(1, addressColumn)

	if len(*copied) != 1 || (*copied)[0] != "cluster" {
		t.Fatalf("copied %v, want the name as a fallback", *copied)
	}
}

// Telling someone "copied" when nothing was copied sends them off to paste
// whatever was on the clipboard before into a connect command.
func TestDoubleClickReportsAFailedCopyAsAFailure(t *testing.T) {
	w, _ := copyWorkspace(t, errors.New("pbcopy: not found"))
	w.applyResources([]ResourceRow{{ID: "1", Name: "ng0", Category: "host"}})

	w.copyCell(1, nameColumn)

	if got := w.status; !strings.Contains(got, "copy failed") || !strings.Contains(got, "pbcopy") {
		t.Errorf("status %q does not report the failure and its reason", got)
	}
	if w.statusColor != theme.Danger {
		t.Errorf("failure drawn in %v, want the Danger colour", w.statusColor)
	}
}

// The header row and a click below the last row both land inside the table.
func TestDoubleClickOutsideTheRowsCopiesNothing(t *testing.T) {
	w, copied := copyWorkspace(t, nil)
	w.applyResources([]ResourceRow{{ID: "1", Name: "ng0", Category: "host"}})

	for _, row := range []int{0, 2, 99, -1} {
		w.copyCell(row, nameColumn)
	}
	if len(*copied) != 0 {
		t.Errorf("copied %v from rows that hold no resource", *copied)
	}
}

// The SOURCE TYPE column and the ":" kind filter must agree: a row labelled
// "redis" has to be one that ":redis" lists. They share kindOf so that holds
// by construction; this pins it down.
func TestSourceTypeLabelAgreesWithKindFilter(t *testing.T) {
	rows := []ResourceRow{
		{ID: "1", Category: "host", Type: "linux"},
		{ID: "2", Category: "host", Type: "windows"},
		{ID: "3", Category: "database", Type: "mysql"},
		{ID: "4", Category: "database", Type: "postgresql"},
		{ID: "5", Category: "database", Type: "redis"},
		{ID: "6", Category: "cloud", Type: "k8s"},
	}
	want := []string{"ec2", "ec2", "rds", "rds", "redis", "k8s"}
	for i, row := range rows {
		got := sourceTypeLabel(row)
		if got != want[i] {
			t.Errorf("sourceTypeLabel(%s/%s) = %q, want %q", row.Category, row.Type, got, want[i])
		}
		if !jmsd.MatchesKind(ResourceKind(got), row.Category, row.Type) {
			t.Errorf("row labelled %q is not listed by :%s", got, got)
		}
	}
}

// A resource outside the four known kinds still gets a useful label. A dash
// would read as missing data and send you to the web console to find out what
// the thing is; its JumpServer category already answers that.
func TestSourceTypeLabelFallsBackToCategory(t *testing.T) {
	if got := sourceTypeLabel(ResourceRow{Category: "web", Type: "website"}); got != "web" {
		t.Errorf("sourceTypeLabel(web/website) = %q, want the raw category %q", got, "web")
	}
	if got := sourceTypeLabel(ResourceRow{Category: "cloud", Type: "public"}); got != "cloud" {
		t.Errorf("sourceTypeLabel(cloud/public) = %q, want the raw category %q", got, "cloud")
	}
	if got := sourceTypeLabel(ResourceRow{}); got != "-" {
		t.Errorf("sourceTypeLabel of a categoryless row = %q, want %q", got, "-")
	}
}

// The column has to be present in the table itself, headed and populated, not
// just computable.
func TestSourceTypeColumnIsRendered(t *testing.T) {
	w, _ := newTestWorkspace(t, []ResourceRow{
		{ID: "h1", Name: "web-01", Address: "10.0.0.1", Category: "host", Type: "linux"},
		{ID: "d1", Name: "pg-01", Address: "10.0.0.2", Category: "database", Type: "postgresql"},
		{ID: "k1", Name: "cluster", Address: "10.0.0.3", Category: "cloud", Type: "k8s"},
	})
	if got := w.table.GetCell(0, sourceTypeColumn).Text; got != "SOURCE TYPE" {
		t.Errorf("column %d heading = %q, want SOURCE TYPE", sourceTypeColumn, got)
	}
	for row, want := range map[int]string{1: "ec2", 2: "rds", 3: "k8s"} {
		if got := w.table.GetCell(row, sourceTypeColumn).Text; got != want {
			t.Errorf("row %d source type = %q, want %q", row, got, want)
		}
	}
	// It is the last column: nothing may be rendered to its right.
	if got := w.table.GetColumnCount(); got != sourceTypeColumn+1 {
		t.Errorf("table has %d columns, want SOURCE TYPE last at index %d", got, sourceTypeColumn)
	}
}

// SOURCE TYPE is the last column, and tview drops trailing columns that no
// longer fit -- so without capping the two before it, shrinking the terminal
// silently removes the column rather than narrowing it. These pin the policy
// down at widths a terminal is awkward to reproduce by hand.
func TestColumnCapsKeepSourceTypeVisible(t *testing.T) {
	// A long RDS endpoint against a moderate name: the case that actually
	// pushes the column off, since address is the column with no natural
	// ceiling.
	const naturalName, naturalAddress = 18, 55
	for _, width := range []int{40, 50, 60, 70, 80, 90, 100, 120, 200} {
		nameMax, addressMax := columnCaps(width, naturalName, naturalAddress)
		if nameMax == 0 && addressMax == 0 {
			// Uncapped is only correct when both columns already fit.
			if naturalName+naturalAddress > width-columnGaps-sourceTypeReserve {
				t.Errorf("width %d: left uncapped, but %d+%d does not leave room for SOURCE TYPE",
					width, naturalName, naturalAddress)
			}
			continue
		}
		used := nameMax + addressMax + sourceTypeReserve + columnGaps
		if used > width {
			t.Errorf("width %d: caps name=%d address=%d need %d cells, SOURCE TYPE gets squeezed out",
				width, nameMax, addressMax, used)
		}
	}
}

// Wide terminals must not be punished for the narrow ones: a cap where there
// was room would truncate text for no reason.
func TestColumnCapsLeaveRoomyTablesAlone(t *testing.T) {
	nameMax, addressMax := columnCaps(200, 18, 55)
	if nameMax != 0 || addressMax != 0 {
		t.Errorf("columnCaps(200, 18, 55) = (%d, %d), want no caps -- everything fits", nameMax, addressMax)
	}
}

// ADDRESS gives way first. It is the longest column, the most repetitive
// (every RDS endpoint ends the same way), and the one you are least likely to
// be reading when picking a row out of a list.
func TestColumnCapsShrinkAddressBeforeName(t *testing.T) {
	// Widths here are the TABLE's inner width, not the terminal's -- the scope
	// tree takes 30 columns before the table sees any. 55 is roughly what an
	// 88-column terminal leaves.
	nameMax, addressMax := columnCaps(55, 18, 55)
	if nameMax < 18 {
		t.Errorf("name capped to %d before address hit its floor (address=%d)", nameMax, addressMax)
	}
	if addressMax >= 55 {
		t.Errorf("address = %d, want it shrunk to make room", addressMax)
	}
	// Narrow enough that address is already at its floor, so name must yield.
	nameMax, addressMax = columnCaps(35, 40, 55)
	if nameMax >= 40 {
		t.Errorf("name = %d at width 60, want it capped once address bottomed out", nameMax)
	}
	if addressMax > nameMax {
		t.Errorf("caps name=%d address=%d: name should hold the larger share", nameMax, addressMax)
	}
}

// Below both floors something has to break; SOURCE TYPE must not be it.
func TestColumnCapsAtAbsurdWidths(t *testing.T) {
	for _, width := range []int{0, 1, 14, 15, 20, 30} {
		nameMax, addressMax := columnCaps(width, 40, 55)
		if addressMax < 0 || nameMax < 0 {
			t.Errorf("width %d: negative cap (name=%d address=%d)", width, nameMax, addressMax)
		}
		if width > columnGaps+sourceTypeReserve && addressMax < 1 {
			t.Errorf("width %d: address column dropped entirely (%d)", width, addressMax)
		}
	}
}

// The end-to-end version: shrink the real table and check the column is still
// there and still readable, not clipped to an ellipsis.
func TestSourceTypeSurvivesNarrowTerminal(t *testing.T) {
	w, screen := newTestWorkspace(t, []ResourceRow{
		{ID: "1", Name: "prod-api-server-01", Address: "10.0.12.34", Category: "host", Type: "linux"},
		{
			ID: "2", Name: "prod-mysql-main", Category: "database", Type: "mysql",
			Address: "prod-mysql.cluster-abc123.eu-central-1.rds.amazonaws.com",
		},
	})
	for _, cols := range []int{60, 70, 80, 90, 100, 120, 160} {
		screen.SetSize(cols, 14)
		screen.Clear()
		w.Primitive().SetRect(0, 0, cols, 14)
		w.Primitive().Draw(screen)
		screen.Show()

		cells, width, _ := screen.GetContents()
		var header, firstRow strings.Builder
		for x := range width {
			header.Write(cells[2*width+x].Bytes)   // header row inside the table border
			firstRow.Write(cells[3*width+x].Bytes) // the ec2 row
		}
		if !strings.Contains(header.String(), "SOURCE TYPE") {
			t.Errorf("%d cols: SOURCE TYPE heading missing or clipped:\n%s", cols, header.String())
		}
		if !strings.Contains(firstRow.String(), "ec2") {
			t.Errorf("%d cols: source type value missing or clipped:\n%s", cols, firstRow.String())
		}
	}
}

// The star prefix goes on the name, not the source type -- otherwise starring
// a row would rewrite the column that says what kind of thing it is.
func TestStarDoesNotDisturbSourceType(t *testing.T) {
	w := starWorkspace(t)
	w.table.Select(1, 0)
	w.captureInput(tcell.NewEventKey(tcell.KeyRune, 'y', tcell.ModNone))
	if got := w.table.GetCell(1, sourceTypeColumn).Text; got != "ec2" {
		t.Errorf("source type of a starred row = %q, want ec2", got)
	}
}

// Regression test for the freeze on switching to a kind with zero matches.
// Table.Draw walks selectedRow forward past the last row when every cell is
// NotSelectable; a subsequent arrow key then spins forever inside tview's
// forward()/backwards(). renderResources must disable selection while empty.
func TestNavigatingEmptyFilterResultDoesNotHang(t *testing.T) {
	// Host-only inventory, so :rds / :redis / :k8s all yield zero rows.
	w, screen := newTestWorkspace(t, []ResourceRow{
		{ID: "1", Name: "host-1", Address: "10.0.0.1", Category: "host", Type: "linux"},
		{ID: "2", Name: "host-2", Address: "10.0.0.2", Category: "host", Type: "linux"},
	})

	for _, kind := range []ResourceKind{ResourceRDS, ResourceRedis, ResourceK8s} {
		t.Run(string(kind), func(t *testing.T) {
			w.changeKind(ResourceAll)
			w.changeKind(kind)
			w.table.Draw(screen)

			if row, _ := w.table.GetSelection(); row >= w.table.GetRowCount() {
				t.Errorf("selection %d is past the last row (%d rows) -- navigation will hang",
					row, w.table.GetRowCount())
			}
			pressKey(t, w, tcell.KeyDown, 0)
			pressKey(t, w, tcell.KeyUp, 0)
			pressKey(t, w, tcell.KeyEnter, 0)
		})
	}
}

// A "/" filter matching nothing must behave the same as an empty kind filter.
func TestNavigatingEmptySearchResultDoesNotHang(t *testing.T) {
	w, screen := newTestWorkspace(t, []ResourceRow{
		{ID: "1", Name: "host-1", Address: "10.0.0.1", Category: "host", Type: "linux"},
	})
	w.filterCurrent("nothing-matches-this")
	w.table.Draw(screen)
	pressKey(t, w, tcell.KeyDown, 0)
}

// Selection must come back once rows reappear, or the table would be dead
// after the first empty filter.
func TestSelectionRecoversAfterEmptyResult(t *testing.T) {
	w, screen := newTestWorkspace(t, []ResourceRow{
		{ID: "1", Name: "host-1", Address: "10.0.0.1", Category: "host", Type: "linux"},
		{ID: "2", Name: "host-2", Address: "10.0.0.2", Category: "host", Type: "linux"},
	})

	w.changeKind(ResourceRDS) // empty
	w.table.Draw(screen)
	pressKey(t, w, tcell.KeyDown, 0)

	w.changeKind(ResourceEC2) // rows are back
	w.table.Draw(screen)

	row, _ := w.table.GetSelection()
	if row < 1 || row > len(w.visible) {
		t.Fatalf("selection %d out of range after recovery (%d visible rows)", row, len(w.visible))
	}
	pressKey(t, w, tcell.KeyDown, 0)
	if row, _ := w.table.GetSelection(); row < 1 || row > len(w.visible) {
		t.Errorf("selection %d out of range after navigating (%d visible rows)", row, len(w.visible))
	}
}

// The reported bug: switching scope nodes reloads rows through
// applyResources, which used to re-render with a hardcoded empty query --
// silently dropping the filter while the footer still advertised it.
func TestSearchSurvivesScopeReload(t *testing.T) {
	w, _ := newTestWorkspace(t, []ResourceRow{
		{ID: "1", Name: "web-01", Address: "10.0.0.1", Category: "host"},
		{ID: "2", Name: "db-01", Address: "10.0.0.2", Category: "host"},
	})
	w.filterCurrent("web")
	if got := visibleIDs(w.visible); len(got) != 1 || got[0] != "1" {
		t.Fatalf("visible after search = %v, want [1]", got)
	}

	// A different scope node's assets land here via QueueLoadResult.
	w.applyResources([]ResourceRow{
		{ID: "3", Name: "web-02", Address: "10.1.0.1", Category: "host"},
		{ID: "4", Name: "cache-01", Address: "10.1.0.2", Category: "host"},
	})
	if got := visibleIDs(w.visible); len(got) != 1 || got[0] != "3" {
		t.Errorf("visible after scope reload = %v, want [3] -- the filter must survive", got)
	}
	if w.searchQuery != "web" {
		t.Errorf("searchQuery = %q, want \"web\"", w.searchQuery)
	}
}

// Kind and search compose; changing one must not drop the other.
func TestSearchSurvivesKindChange(t *testing.T) {
	w, _ := newTestWorkspace(t, []ResourceRow{
		{ID: "1", Name: "web-01", Category: "host"},
		{ID: "2", Name: "web-db", Category: "database", Type: "mysql"},
		{ID: "3", Name: "other", Category: "database", Type: "mysql"},
	})
	w.filterCurrent("web")
	w.changeKind(ResourceRDS)
	if got := visibleIDs(w.visible); len(got) != 1 || got[0] != "2" {
		t.Errorf("visible = %v, want [2] (kind AND search applied)", got)
	}
}

// Esc used to route through changeKind, which no-ops when the kind is already
// :all -- leaving an active filter with no keyboard way to clear it.
func TestEscapeClearsSearchWhenKindIsAlreadyAll(t *testing.T) {
	w, _ := newTestWorkspace(t, []ResourceRow{
		{ID: "1", Name: "web-01", Category: "host"},
		{ID: "2", Name: "db-01", Category: "host"},
	})
	w.filterCurrent("web")
	if w.kind != ResourceAll {
		t.Fatalf("precondition: kind = %q, want all", w.kind)
	}

	w.captureInput(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone))
	if w.searchQuery != "" {
		t.Errorf("searchQuery = %q, want empty after Esc", w.searchQuery)
	}
	if len(w.visible) != 2 {
		t.Errorf("visible = %v, want both rows back after Esc", visibleIDs(w.visible))
	}
}

func TestEscapeClearsBothKindAndSearch(t *testing.T) {
	w, _ := newTestWorkspace(t, []ResourceRow{
		{ID: "1", Name: "web-01", Category: "host"},
		{ID: "2", Name: "web-db", Category: "database", Type: "mysql"},
	})
	w.changeKind(ResourceRDS)
	w.filterCurrent("web")

	w.captureInput(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone))
	if w.kind != ResourceAll || w.searchQuery != "" {
		t.Errorf("after Esc: kind=%q search=%q, want all/empty", w.kind, w.searchQuery)
	}
	if len(w.visible) != 2 {
		t.Errorf("visible = %v, want both rows", visibleIDs(w.visible))
	}
}

// "y" stars the highlighted row and stars are what the Favorites scope lists.
func TestToggleFavoriteAndFavoritesScope(t *testing.T) {
	w := starWorkspace(t)
	w.table.Select(1, 0) // web-01

	w.captureInput(tcell.NewEventKey(tcell.KeyRune, 'y', tcell.ModNone))
	if _, ok := w.favorites["h1"]; !ok {
		t.Fatalf("y did not star the selected row; favorites=%v", w.favorites)
	}

	w.changeTreeNode(tview.NewTreeNode("fav").SetReference(scopeFavorites))
	if got := visibleIDs(w.visible); len(got) != 1 || got[0] != "h1" {
		t.Errorf("Favorites scope shows %v, want [h1]", got)
	}

	// Pressing y again unstars, and the row leaves the Favorites scope at once.
	w.table.Select(1, 0)
	w.captureInput(tcell.NewEventKey(tcell.KeyRune, 'y', tcell.ModNone))
	if len(w.visible) != 0 {
		t.Errorf("unstarred row still listed under Favorites: %v", visibleIDs(w.visible))
	}
}

// Starring must survive a reload -- the rows are replaced, the stars are not.
func TestFavoritesSurviveReload(t *testing.T) {
	w := starWorkspace(t)
	w.table.Select(1, 0)
	w.captureInput(tcell.NewEventKey(tcell.KeyRune, 'y', tcell.ModNone))

	w.applyResources([]ResourceRow{
		{ID: "h1", Name: "web-01", Address: "10.0.0.1", Category: "host"},
		{ID: "h9", Name: "new-host", Address: "10.0.0.9", Category: "host"},
	})
	w.changeTreeNode(tview.NewTreeNode("fav").SetReference(scopeFavorites))
	if got := visibleIDs(w.visible); len(got) != 1 || got[0] != "h1" {
		t.Errorf("after reload Favorites shows %v, want [h1]", got)
	}
}

// A star on an asset the user can no longer see must not produce a phantom
// row -- scopes filter the loaded set, they never invent rows.
func TestFavoriteForUnavailableAssetIsIgnored(t *testing.T) {
	w := starWorkspace(t)
	w.SetMarks([]string{"gone-forever"}, nil)
	w.changeTreeNode(tview.NewTreeNode("fav").SetReference(scopeFavorites))
	if len(w.visible) != 0 {
		t.Errorf("visible = %v, want empty", visibleIDs(w.visible))
	}
}

// The star must reach jmsd, since Core is where it lives now.
func TestSetFavoriteIsCalledOnStar(t *testing.T) {
	w := starWorkspace(t)
	type call struct {
		assetID string
		starred bool
	}
	var calls []call
	w.actions.SetFavorite = func(assetID string, starred bool) {
		calls = append(calls, call{assetID, starred})
	}

	w.table.Select(1, 0)
	w.captureInput(tcell.NewEventKey(tcell.KeyRune, 'y', tcell.ModNone))
	w.table.Select(1, 0)
	w.captureInput(tcell.NewEventKey(tcell.KeyRune, 'y', tcell.ModNone))

	want := []call{{"h1", true}, {"h1", false}}
	if !slices.Equal(calls, want) {
		t.Errorf("SetFavorite calls = %v, want %v (star then unstar)", calls, want)
	}
}

// The star shows immediately, before jmsd answers -- and is taken back if the
// write turns out to have failed, so the UI can't claim a star Core rejected.
func TestRevertFavoriteUndoesOptimisticStar(t *testing.T) {
	w := starWorkspace(t)
	w.actions.SetFavorite = func(string, bool) {} // never completes

	w.table.Select(1, 0)
	w.captureInput(tcell.NewEventKey(tcell.KeyRune, 'y', tcell.ModNone))
	if _, ok := w.favorites["h1"]; !ok {
		t.Fatal("star should appear immediately, without waiting for jmsd")
	}

	w.applyFavorite("h1", false) // what RevertFavorite does on the UI goroutine
	if _, ok := w.favorites["h1"]; ok {
		t.Error("revert left the star in place")
	}
}

// Starred rows are marked in the table so the state is visible without
// switching scope.
func TestStarredRowIsMarked(t *testing.T) {
	w := starWorkspace(t)
	w.table.Select(1, 0)
	w.captureInput(tcell.NewEventKey(tcell.KeyRune, 'y', tcell.ModNone))

	if got := w.table.GetCell(1, 0).Text; !strings.HasPrefix(got, "★") {
		t.Errorf("starred row renders as %q, want a ★ prefix", got)
	}
	if got := w.table.GetCell(2, 0).Text; strings.HasPrefix(got, "★") {
		t.Errorf("unstarred row renders as %q, want no ★", got)
	}
}

// "y" on an empty table must be a no-op, not a panic.
func TestToggleFavoriteWithNoSelection(t *testing.T) {
	w, _ := newTestWorkspace(t, nil)
	w.captureInput(tcell.NewEventKey(tcell.KeyRune, 'y', tcell.ModNone))
	if len(w.favorites) != 0 {
		t.Errorf("favorites = %v, want empty", w.favorites)
	}
}

// SetInitialKind is the "-c TYPE" shortcut's effect on the TUI (docs/PLAN.md
// §8.1): it must take before the first load, unlike changeKind, which is the
// ":" command's live switch and refuses to no-op silently on an unloaded
// table.
func TestSetInitialKindAppliesBeforeTheFirstLoad(t *testing.T) {
	screen := tcell.NewSimulationScreen("")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	app := tview.NewApplication().SetScreen(screen)
	w := NewWorkspace(app, "tester", WorkspaceActions{})

	w.SetInitialKind(ResourceEC2)
	// Simulates the first load landing, exactly as loadWorkspace's
	// QueueLoadResult -> applyLoadResult -> applyResources chain does.
	w.applyResources([]ResourceRow{
		{ID: "1", Name: "web-1", Category: "host"},
		{ID: "2", Name: "db-1", Category: "database", Type: "mysql"},
	})

	if got := visibleIDs(w.visible); !slices.Equal(got, []string{"1"}) {
		t.Errorf("visible = %v, want only the ec2 asset -- SetInitialKind(ec2) before load did not stick", got)
	}
}

func TestSetInitialKindEmptyIsANoOp(t *testing.T) {
	screen := tcell.NewSimulationScreen("")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	app := tview.NewApplication().SetScreen(screen)
	w := NewWorkspace(app, "tester", WorkspaceActions{})

	w.SetInitialKind("")
	if w.kind != ResourceAll {
		t.Errorf("kind = %q after SetInitialKind(\"\"), want %q -- the ordinary launch (no \"-c\") must be untouched", w.kind, ResourceAll)
	}
}
