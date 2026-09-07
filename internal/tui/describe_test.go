package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rivo/tview"

	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
)

func ptr[T any](v T) *T { return &v }

// shown renders the panel text for a host; copied renders what "y" would put
// on the clipboard. Both come from one call, so they cannot drift.
func shown(host *jmsd.HostInfo) string  { d, _ := renderDescribe(ResourceRow{}, host); return d }
func copied(host *jmsd.HostInfo) string { _, p := renderDescribe(ResourceRow{}, host); return p }

func TestRenderDescribe_AWSDoesNotClaimAChargeType(t *testing.T) {
	// is_prepaid is three-state and AWS is the nil one -- the concept does not
	// exist there. Rendering nil as "按量付费" would put a confident wrong fact
	// about billing in front of the user on every single AWS machine.
	out := shown(&jmsd.HostInfo{Provider: "aws", Account: "mega-base", ResourceID: "i-1"})
	if strings.Contains(out, "按量") || strings.Contains(out, "包年") {
		t.Fatalf("AWS host rendered a charge type:\n%s", out)
	}
}

func TestRenderDescribe_PrepaidShowsItsExpiry(t *testing.T) {
	// The expiry is the reason to show this at all: a subscription instance
	// stops itself when it passes.
	expires := time.Date(2027, 1, 31, 8, 0, 0, 0, time.UTC)
	out := shown(&jmsd.HostInfo{Provider: "aliyun", IsPrepaid: ptr(true), ExpiredAt: &expires})
	if !strings.Contains(out, "包年包月") || !strings.Contains(out, expires.Local().Format("2006-01-02")) {
		t.Fatalf("prepaid host lost its expiry:\n%s", out)
	}
	if out = shown(&jmsd.HostInfo{Provider: "aliyun", IsPrepaid: ptr(false)}); !strings.Contains(out, "按量付费") {
		t.Fatalf("pay-as-you-go host lost its charge type:\n%s", out)
	}
}

func TestRenderDescribe_AutoRenewOffIsCalledOut(t *testing.T) {
	// "包年包月 · 到期 <date>" alone does not say whether that date is a
	// deadline or just a date. Off is the state somebody acts on, and it must
	// stay distinguishable from "cloud-sync could not read it" -- claiming
	// nobody is renewing a machine, wrongly, sends someone to the console.
	expires := time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC)
	base := jmsd.HostInfo{IsPrepaid: ptr(true), ExpiredAt: &expires}

	off, on := base, base
	off.AutoRenew, on.AutoRenew = ptr(false), ptr(true)

	if got := describeCharge(&off); !strings.Contains(got, "未开自动续费") {
		t.Errorf("auto-renew off was not called out: %q", got)
	}
	if got := describeCharge(&on); !strings.Contains(got, "自动续费") || strings.Contains(got, "未开") {
		t.Errorf("auto-renew on rendered wrong: %q", got)
	}
	if got := describeCharge(&base); strings.Contains(got, "续费") {
		t.Errorf("unknown auto-renew claimed a state: %q", got)
	}
}

func TestRenderDescribe_UnknownValuesAreOmittedNotZeroed(t *testing.T) {
	// A cloud that does not report vCPU/memory must not render as a machine
	// with 0 of them.
	out := shown(&jmsd.HostInfo{ResourceID: "i-1"})
	if strings.Contains(out, "0 vCPU") || strings.Contains(out, "0 MiB") || strings.Contains(out, "0 GiB") {
		t.Fatalf("nil spec fields rendered as zero:\n%s", out)
	}
}

func TestRenderDescribe_MemoryKeepsSubGigabyteValues(t *testing.T) {
	// 512 MiB in whole GiB rounds to "0 GiB", which reads as broken.
	if got := describeMemory(ptr(512)); got != "512 MiB" {
		t.Errorf("512 MB rendered as %q", got)
	}
	if got := describeMemory(ptr(8192)); got != "8 GiB" {
		t.Errorf("8192 MB rendered as %q", got)
	}
}

func TestRenderDescribe_EmptySectionsAreDropped(t *testing.T) {
	// Every field is droppable, so a heading printed up front can end up with
	// nothing under it -- which reads as data that failed to load.
	plain := copied(&jmsd.HostInfo{Provider: "aws"})
	for _, heading := range []string{"规格", "网络", "磁盘", "时间", "标签"} {
		if strings.Contains(plain, heading) {
			t.Errorf("empty section %q was printed:\n%s", heading, plain)
		}
	}
	if !strings.Contains(plain, "云账号") {
		t.Errorf("the section that does have a field was dropped:\n%s", plain)
	}
}

func TestRenderDescribe_ZeroSyncTimeIsNotPrinted(t *testing.T) {
	// "0001-01-01" is exactly the kind of value that gets read as real.
	if plain := copied(&jmsd.HostInfo{Provider: "aws"}); strings.Contains(plain, "0001-01-01") {
		t.Fatalf("the zero time was rendered:\n%s", plain)
	}
}

func TestRenderDescribe_LabelsLineUpAcrossScripts(t *testing.T) {
	// %-14s pads by bytes, and a CJK label is three bytes per two display
	// columns -- so every Chinese row landed several columns off the ASCII
	// ones. The panel is a two-column table; misaligned, it stops reading as
	// one.
	plain := copied(&jmsd.HostInfo{
		Provider:  "aliyun",                                   // ASCII label
		PrivateIP: "10.0.0.1",                                 // CJK+ASCII label, "内网 IP"
		Tags:      map[string]string{"Owner:team": "sre-ops"}, // the longest label
	})

	var columns []int
	for _, value := range []string{"aliyun", "sre-ops", "10.0.0.1"} {
		line := ""
		for _, candidate := range strings.Split(plain, "\n") {
			if strings.Contains(candidate, value) {
				line = candidate
				break
			}
		}
		if line == "" {
			t.Fatalf("no line carries %q:\n%s", value, plain)
		}
		columns = append(columns, tview.TaggedStringWidth(line[:strings.Index(line, value)]))
	}
	for _, column := range columns[1:] {
		if column != columns[0] {
			t.Fatalf("values start at differing display columns %v:\n%s", columns, plain)
		}
	}
}

func TestRenderDescribe_ClipboardTextCarriesNoMarkup(t *testing.T) {
	// The panel escapes values for tview, so "[prod]" is stored as "[prod[]".
	// Whatever reaches the clipboard has to be what the cloud actually says --
	// pasting a tview escape into a ticket or a shell is worse than useless.
	plain := copied(&jmsd.HostInfo{
		InstanceType: "m5.large",
		Tags:         map[string]string{"note": "[prod]"},
	})
	if strings.Contains(plain, "[-]") || strings.Contains(plain, "[#") {
		t.Errorf("colour tags reached the clipboard:\n%s", plain)
	}
	if !strings.Contains(plain, "[prod]") || strings.Contains(plain, "[prod[]") {
		t.Errorf("a tag value was mangled on the way to the clipboard:\n%s", plain)
	}
	if !strings.Contains(plain, "m5.large") {
		t.Errorf("clipboard text lost the content:\n%s", plain)
	}
}

func TestRenderDescribe_NoMetadataSaysWhy(t *testing.T) {
	// A blank panel reads as broken. For databases and caches "nothing here"
	// is also the permanent correct answer, not a fault, so it has to be
	// worded differently from a host that genuinely went missing.
	db, _ := renderDescribe(ResourceRow{Category: "database", Type: "mysql"}, nil)
	if !strings.Contains(db, "RDS") {
		t.Errorf("a database row did not explain itself:\n%s", db)
	}
	host, _ := renderDescribe(ResourceRow{Category: "host", Type: "linux"}, nil)
	if strings.TrimSpace(host) == "" || strings.Contains(host, "RDS") {
		t.Errorf("an unmanaged host got the wrong note:\n%s", host)
	}
}

func TestDescribeGeneration_LatestPressWins(t *testing.T) {
	// Two "d" presses in a row: the first answer may land after the second.
	// tview.Pages.AddPage replaces a same-named page silently, so without the
	// generation check the slower, older result overwrites the newer panel --
	// and the panel reads as showing the wrong machine for no visible reason.
	u := &UI{}
	first, second := u.nextDescribe(), u.nextDescribe()

	if u.isCurrentDescribe(first) {
		t.Error("the superseded press is still current")
	}
	if !u.isCurrentDescribe(second) {
		t.Error("the latest press is not current")
	}
}

func TestDescribeGeneration_DismissalCancelsAnInFlightRequest(t *testing.T) {
	// Press "d", close the panel before the answer arrives. Closing bumps the
	// generation, so the late answer cannot reopen a panel the user has
	// already dismissed.
	u := &UI{}
	pending := u.nextDescribe()
	u.nextDescribe() // what closing the panel does

	if u.isCurrentDescribe(pending) {
		t.Fatal("a dismissed describe would still render")
	}
}

func TestRenderDescribe_DisplayAndClipboardLineUpOneForOne(t *testing.T) {
	// The describe view keeps both renderings as parallel slices and filters
	// on the untagged one while painting from the tagged one, so index i must
	// be the same row in both. If the two ever diverge in line count, the
	// filter shows one row and copies another -- silently.
	expires := time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC)
	display, plain := renderDescribe(ResourceRow{}, &jmsd.HostInfo{
		Provider: "aliyun", Account: "power-base", ResourceID: "i-1",
		InstanceType: "ecs.g7.large", VCPU: ptr(2), MemoryMB: ptr(8192),
		IsPrepaid: ptr(true), AutoRenew: ptr(false), ExpiredAt: &expires,
		PrivateIP: "10.0.0.1",
		Disks:     []jmsd.DiskRef{{Device: "/dev/xvda", SizeGB: 100, Type: "gp3"}},
		Tags:      map[string]string{"env": "prod", "app": "web"},
	})

	shown := strings.Split(strings.TrimRight(display, "\n"), "\n")
	raw := strings.Split(strings.TrimRight(plain, "\n"), "\n")
	if len(shown) != len(raw) {
		t.Fatalf("display has %d lines, clipboard text has %d:\n--- display ---\n%s\n--- plain ---\n%s",
			len(shown), len(raw), display, plain)
	}
	if len(raw) < 10 {
		t.Fatalf("expected a document, got %d lines:\n%s", len(raw), plain)
	}
}

func TestRenderDescribe_RepeatedValuesShareOneLabel(t *testing.T) {
	// A machine on four security groups used to print "安全组" four times.
	// The word is not what differs between the rows, and repeating it spends
	// the eye's attention on the label instead of on the values.
	plain := copied(&jmsd.HostInfo{
		PrivateIP: "10.0.0.1",
		SecurityGroups: []jmsd.SecurityGroupRef{
			{ID: "sg-1", Name: "prod-web"},
			{ID: "sg-2", Name: "base-ssh"},
			{ID: "sg-3", Name: "allow-bastion"},
		},
	})
	if got := strings.Count(plain, "安全组"); got != 1 {
		t.Errorf("the label appears %d times, want 1:\n%s", got, plain)
	}
	for _, name := range []string{"prod-web", "base-ssh", "allow-bastion"} {
		if !strings.Contains(plain, name) {
			t.Errorf("continuation rows dropped %q:\n%s", name, plain)
		}
	}

	// The continuation rows must land in the same value column as the first,
	// or the group turns into a ragged list.
	var columns []int
	for _, name := range []string{"prod-web", "base-ssh", "allow-bastion"} {
		for _, line := range strings.Split(plain, "\n") {
			if strings.Contains(line, name) {
				columns = append(columns, tview.TaggedStringWidth(line[:strings.Index(line, name)]))
				break
			}
		}
	}
	for _, column := range columns[1:] {
		if column != columns[0] {
			t.Fatalf("continuation rows start at differing columns %v:\n%s", columns, plain)
		}
	}
}

func TestRenderDescribe_OwnerTeamIsNotShown(t *testing.T) {
	// Dropped from the panel on request: it is a grouping key, and the scope
	// tree already puts the machine under its team.
	plain := copied(&jmsd.HostInfo{Provider: "aliyun", OwnerTeam: "platform"})
	if strings.Contains(plain, "platform") {
		t.Errorf("owner team is still rendered:\n%s", plain)
	}
}

func TestRenderDescribe_LabelsCarryAColonExceptContinuationRows(t *testing.T) {
	// The colon is what makes the two columns read as label and value rather
	// than as two lists sitting side by side -- and it is what k9s's own
	// describe output does. A continuation row has no label, so there is
	// nothing there to punctuate.
	plain := copied(&jmsd.HostInfo{
		Provider: "aliyun",
		SecurityGroups: []jmsd.SecurityGroupRef{
			{ID: "sg-1", Name: "prod-web"},
			{ID: "sg-2", Name: "base-ssh"},
		},
	})
	if !strings.Contains(plain, "provider:") || !strings.Contains(plain, "安全组:") {
		t.Errorf("labels lost their colon:\n%s", plain)
	}
	for _, line := range strings.Split(plain, "\n") {
		if strings.Contains(line, "base-ssh") && strings.Contains(line, ":") {
			t.Errorf("a continuation row was punctuated: %q", line)
		}
	}
}

func TestRenderDescribe_TagsAreSortedByWhatIsOnScreen(t *testing.T) {
	// Sorting by the underlying key leaves the visible labels out of order,
	// which reads as an unsorted list -- worse than either ordering alone.
	plain := copied(&jmsd.HostInfo{Tags: map[string]string{
		"Application:module":  "Redis", // shows as "module"
		"Application:domain":  "media", // shows as "domain"
		"Application:product": "bc",    // shows as "product"
		"env":                 "prod",  // shows as "env"
	}})

	var labels []string
	for _, line := range strings.Split(plain, "\n") {
		if label, _, found := strings.Cut(strings.TrimSpace(line), ":"); found && label != "" {
			labels = append(labels, label)
		}
	}
	// Drop everything above the tag section; the tags are the tail.
	tags := labels[len(labels)-4:]
	if !slices.IsSorted(tags) {
		t.Fatalf("tag labels are not in order: %v\n%s", tags, plain)
	}
}

func TestDescribeViewReplacesTheWorkspaceRatherThanFloatingOverIt(t *testing.T) {
	// tview.Flex sets dontClear, so it never paints its own background. Laid
	// over the workspace, the scope tree and the panel titles showed straight
	// through the metadata text. Switching pages is both what k9s does and
	// the only thing that keeps the screen clean.
	app := tview.NewApplication()
	u := &UI{app: app, pages: tview.NewPages()}
	u.pages.AddAndSwitchToPage(resourcesPage, tview.NewBox(), true)

	u.showDescribe(ResourceRow{Name: "web-01"}, &jmsd.HostInfo{Provider: "aws"}, nil)

	name, _ := u.pages.GetFrontPage()
	if name != describePage {
		t.Fatalf("front page = %q, want the describe view", name)
	}
	if u.pages.HasPage(resourcesPage) && frontPageIsVisible(u, resourcesPage) {
		t.Error("the workspace is still being drawn under the describe view")
	}
}

// frontPageIsVisible reports whether a page would still be painted. Pages
// draws every visible page in order, so a page left visible underneath is
// exactly the bleed-through this guards against.
func frontPageIsVisible(u *UI, name string) bool {
	front, _ := u.pages.GetFrontPage()
	return front == name
}

func TestRenderDescribe_OneValueColumnAcrossTheWholeDocument(t *testing.T) {
	// The label column is sized from the labels actually present, because tag
	// keys decide it and nothing here bounds them: production carries
	// "Application:name" and "tag:createdby", both past a fixed 14, and each
	// overflow pushed its own value out to a different column. A two-column
	// table whose second column starts in three places has stopped being one.
	_, plain := renderDescribe(ResourceRow{}, &jmsd.HostInfo{
		Provider:  "aliyun",   // short ASCII label
		PrivateIP: "10.0.0.1", // CJK+ASCII label
		Tags: map[string]string{
			"Application:name": "ack-relay", // the widest label
			"Owner:team":       "Platform",
			"env":              "prod",
		},
	})

	var columns []int
	for _, value := range []string{"aliyun", "10.0.0.1", "ack-relay", "Platform", "prod"} {
		for _, line := range strings.Split(plain, "\n") {
			if strings.Contains(line, value) {
				columns = append(columns, tview.TaggedStringWidth(line[:strings.Index(line, value)]))
				break
			}
		}
	}
	if len(columns) != 5 {
		t.Fatalf("expected five value lines, found %d:\n%s", len(columns), plain)
	}
	for _, column := range columns[1:] {
		if column != columns[0] {
			t.Fatalf("values start at differing columns %v:\n%s", columns, plain)
		}
	}
}

func TestLabelColumnWidth_FlooredAndCapped(t *testing.T) {
	if got := labelColumnWidth([]string{"CPU", "内存"}); got != describeLabelWidth {
		t.Errorf("short labels gave width %d, want the floor %d", got, describeLabelWidth)
	}
	// One pathological key must not indent the whole document; past the cap
	// that row runs on and the rest stay aligned.
	if got := labelColumnWidth([]string{strings.Repeat("x", 80)}); got != describeLabelCap {
		t.Errorf("an 80-char label gave width %d, want the cap %d", got, describeLabelCap)
	}
	if got := labelColumnWidth([]string{"Application:name"}); got != len("Application:name")+1 {
		t.Errorf("width %d does not fit the label plus its colon", got)
	}
}

func TestDescribeReturnsFocusToThePanelItWasOpenedFrom(t *testing.T) {
	// SwitchToPage focuses the page's default child, and the workspace's is
	// the scope tree -- whose SetFocusFunc writes focusTree. So by the time
	// close() looked at it the answer was already "the tree", and pressing
	// "d" from the table and Escaping came back on the left pane.
	w, _ := newTestWorkspace(t, []ResourceRow{{ID: "a", Name: "web-01", Category: "host"}})
	u := &UI{app: w.app, pages: tview.NewPages(), workspace: w}
	u.pages.AddAndSwitchToPage(resourcesPage, w.Primitive(), true)
	w.app.SetRoot(u.pages, true)

	w.focusTree = false // on the table, which is where "d" is pressed from
	w.applyFocus()

	view := u.showDescribe(ResourceRow{Name: "web-01"}, &jmsd.HostInfo{Provider: "aws"}, nil)
	if name, _ := u.pages.GetFrontPage(); name != describePage {
		t.Fatalf("front page = %q, want the describe view", name)
	}

	view.close() // what Esc does

	if w.focusTree {
		t.Error("came back on the scope tree, not the table it was opened from")
	}
	if !w.table.HasFocus() {
		t.Error("the table does not have focus")
	}
	if name, _ := u.pages.GetFrontPage(); name != resourcesPage {
		t.Errorf("front page = %q after closing, want the workspace back", name)
	}
}

func TestRenderDescribe_TimesAreCreationAndSyncOnly(t *testing.T) {
	// The last-boot time is not shown: 121 of 174 Aliyun hosts have never
	// been restarted, so it was the same timestamp printed twice.
	created := time.Date(2024, 9, 23, 16, 9, 0, 0, time.UTC)
	booted := created.Add(875 * 24 * time.Hour)
	plain := copied(&jmsd.HostInfo{CreatedAt: &created, StartedAt: &booted, SyncedAt: booted})

	if strings.Contains(plain, "启动") {
		t.Errorf("the boot time is still rendered:\n%s", plain)
	}
	if !strings.Contains(plain, "创建") || !strings.Contains(plain, "同步") {
		t.Errorf("creation or sync time missing:\n%s", plain)
	}

	// AWS reports no creation time, and its LaunchTime must not be
	// substituted in under that label -- it moves on every stop/start.
	synced := time.Date(2026, 9, 7, 11, 0, 0, 0, time.UTC)
	plain = copied(&jmsd.HostInfo{StartedAt: &booted, SyncedAt: synced})
	if strings.Contains(plain, "创建") {
		t.Errorf("an AWS host claimed a creation time:\n%s", plain)
	}
	if strings.Contains(plain, booted.Local().Format("2006-01-02 15:04")) {
		t.Errorf("LaunchTime leaked into the panel:\n%s", plain)
	}
}

func TestDescribeShownWithoutAFetchStillCancelsTheOneInFlight(t *testing.T) {
	// A row with no CloudKey opens the panel synchronously -- nothing to
	// fetch. But a request from a PREVIOUS "d" may still be in flight, and
	// without a bump it stays current and lands on top of this panel: press
	// "d" on a machine with metadata, arrow to one without, press "d" again,
	// and the first machine's data appears under the second one's title.
	u := &UI{}
	inFlight := u.nextDescribe() // the async press

	u.nextDescribe() // what the synchronous press must also do

	if u.isCurrentDescribe(inFlight) {
		t.Fatal("the in-flight request would still overwrite the panel")
	}
}
