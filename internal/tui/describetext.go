package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rivo/tview"

	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
)

// This file is the "d" panel's text: pure functions from a HostInfo to the two
// strings describe.go paints and copies. Split from the view because they are
// what every test in describe_test.go actually exercises, and because a
// formatting question ("why is this column ragged", "why does AWS show no
// charge type") is answered here without reading any tview.

// emptyDescribeNote says why a row has no metadata. "Nothing here" on its own
// reads as broken, and for databases and caches it is the permanent, correct
// answer rather than a fault -- they are synced as assets but their cloud
// metadata lives in cloud-sync's other tables.
func emptyDescribeNote(resource ResourceRow) string {
	switch sourceTypeLabel(resource) {
	case string(jmsd.ResourceRDS), string(jmsd.ResourceRedis):
		return "RDS / Redis 的云元数据不在这张表里"
	case string(jmsd.ResourceK8s):
		return "K8s 集群没有云主机元数据"
	}
	return "没有云元数据（不是 cloud-sync 管的资产，或 jmsd 未配置 inventory 库）"
}

// describeLabelWidth is the narrowest the label column gets. Wider labels
// widen it (see labelColumnWidth), so this is a floor, not a size.
const describeLabelWidth = 14

// describeLabelCap stops one pathological key from indenting the whole
// document. Past it that row runs on and the rest stay aligned, which is the
// better of the two failures.
const describeLabelCap = 24

// labelColumnWidth is the column every value starts at: the widest label
// actually present, floored and capped.
//
// Computed rather than fixed because tag keys decide it and they are not
// bounded by anything this code knows -- production carries
// "Application:name" (17 with its colon) and "tag:createdby" (14), both past
// what a fixed 14 could hold, and each one that overflowed pushed its own
// value out to a different column. A two-column table whose second column
// starts in three different places has stopped being a table.
func labelColumnWidth(labels []string) int {
	width := describeLabelWidth
	for _, label := range labels {
		if label == "" {
			continue
		}
		if w := tview.TaggedStringWidth(label) + 1; w > width { // +1 for the colon
			width = w
		}
	}
	return min(width, describeLabelCap)
}

// padLabel renders the label column: the name, a colon, then padding out to
// width DISPLAY columns.
//
// The colon is what makes the two columns read as label and value rather than
// as two lists that happen to be side by side -- it is also what k9s's own
// describe output does. A continuation row (empty label, see list) gets no
// colon: there is nothing there to punctuate.
//
// Padding is by display width, not %-*s. That pads by byte count, which puts
// a CJK label -- three bytes per two columns -- several spaces past where an
// ASCII one lands, and every Chinese row came out misaligned.
func padLabel(label string, width int) string {
	if label != "" {
		label += ":"
	}
	if w := tview.TaggedStringWidth(label); w < width {
		return label + strings.Repeat(" ", width-w)
	}
	return label
}

// renderDescribe turns one row's metadata into the panel's text, and the same
// thing again without colour tags for the clipboard.
//
// Two strings rather than one plus a stripper: the values are run through
// tview.Escape, so "[prod]" in a tag value is stored as "[prod[]" and any
// regex undoing the colour tags would corrupt it. Building both in the same
// pass cannot drift.
//
// Empty fields are dropped rather than shown as "-": most machines have no
// public IP, no expiry and no owner tag, and printing a column of dashes
// buries the four lines that do say something.
func renderDescribe(resource ResourceRow, h *jmsd.HostInfo) (display, plain string) {
	if h == nil {
		note := emptyDescribeNote(resource)
		return tag(theme.Muted) + tview.Escape(note) + "[-]", note
	}

	// Collected first and written afterwards, because the label column is
	// sized from the labels that turn out to be present (labelColumnWidth) --
	// which is not known until the last one has been seen.
	var rows []describeRow
	// A heading is held back until something lands under it. Every field is
	// droppable, so an eagerly-printed heading leaves a bare "规格" with
	// nothing beneath it -- which reads as data that failed to load rather
	// than as a cloud that does not report those fields.
	pending := ""
	section := func(title string) { pending = title }
	field := func(label, value string) {
		if value == "" {
			return
		}
		if pending != "" {
			rows = append(rows, describeRow{heading: pending})
			pending = ""
		}
		rows = append(rows, describeRow{label: label, value: value})
	}

	// list writes the label once and puts the rest under it, label column
	// blank. Repeating "安全组" down four rows spends the eye's attention on
	// the word rather than on the four values, which are what differ.
	list := func(label string, values []string) {
		for i, value := range values {
			if i == 0 {
				field(label, value)
				continue
			}
			field("", value)
		}
	}

	section("云账号")
	field("provider", h.Provider)
	field("账号", describeAccount(h))
	field("region", h.Region)
	field("可用区", h.ZoneID)

	section("规格")
	field("实例", h.ResourceID)
	field("机型", h.InstanceType)
	field("CPU", describeInt(h.VCPU, " vCPU"))
	field("内存", describeMemory(h.MemoryMB))
	field("计费", describeCharge(h))
	field("系统", h.OSName)
	field("镜像", h.ImageID)

	section("网络")
	field("内网 IP", h.PrivateIP)
	field("公网 IP", h.PublicIP)
	field("VPC", h.VpcID)
	field("子网", h.SubnetID)
	groups := make([]string, 0, len(h.SecurityGroups))
	for _, g := range h.SecurityGroups {
		groups = append(groups, strings.TrimSpace(g.Name+" ("+g.ID+")"))
	}
	list("安全组", groups)

	if len(h.Disks) > 0 {
		section("磁盘")
		for _, d := range h.Disks {
			field(describeOr(d.Device, "disk"), describeDisk(d))
		}
	}

	section("时间")
	// Creation and sync only. The last-boot time is deliberately not here:
	// 121 of 174 Aliyun hosts have never been restarted, so it was the same
	// timestamp printed twice, and one line that is usually redundant costs
	// more attention than the 53 machines it occasionally informs.
	//
	// AWS reports no creation time -- HostInfo.CreatedAt is always nil there
	// -- so an AWS host shows the sync time alone. Its LaunchTime is not
	// substituted in: it moves on every stop/start, so labelling it 创建
	// would be wrong for exactly the machines somebody is asking about.
	field("创建", describeTime(h.CreatedAt))
	field("同步", describeTime(&h.SyncedAt))

	if keys := visibleTagKeys(h.Tags); len(keys) > 0 {
		section("标签")
		for _, key := range keys {
			field(key, h.Tags[key])
		}
	}
	return writeDescribeRows(rows)
}

// describeRow is one line of the panel: a section heading, or a label/value
// pair. Continuation rows (see list) carry an empty label.
type describeRow struct {
	heading string
	label   string
	value   string
}

// writeDescribeRows renders the collected rows twice -- with colour tags for
// the screen, without them for the clipboard -- in one pass, so index i is
// the same line in both. The describe view relies on that: it filters on the
// untagged copy and paints from the tagged one.
func writeDescribeRows(rows []describeRow) (display, plain string) {
	labels := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.heading == "" {
			labels = append(labels, row.label)
		}
	}
	width := labelColumnWidth(labels)

	var b, raw strings.Builder
	for _, row := range rows {
		if row.heading != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
				raw.WriteString("\n")
			}
			fmt.Fprintf(&b, "%s%s[-]\n", tag(theme.BorderHi), row.heading)
			raw.WriteString(row.heading + "\n")
			continue
		}
		fmt.Fprintf(&b, "  %s%s[-] %s\n", tag(theme.Muted), padLabel(row.label, width), tview.Escape(row.value))
		fmt.Fprintf(&raw, "  %s %s\n", padLabel(row.label, width), row.value)
	}
	return b.String(), raw.String()
}

// platformTagPrefixes are the prefixes both clouds reserve for tags THEY set
// -- "aws:" and Alibaba's "acs:". They are bookkeeping, not description:
// launch-template ids, CloudFormation stack ids, autoscaling group names, the
// account that ran the API call. On a production host they are a third of the
// list, and none of them says anything about the machine.
//
// A prefix rule rather than a list of keys: the list would need editing every
// time somebody used a new AWS feature, and it would be edited late -- after
// the noise showed up in the panel.
var platformTagPrefixes = []string{"aws:", "acs:"}

// visibleTagKeys is the tags worth showing, sorted.
//
// Keys are shown EXACTLY as the cloud stores them. An earlier version dropped
// the namespace prefix -- "Application:module" as "module" -- and that was
// wrong: the prefix is usually the meaning. "ecs: on" says nothing;
// "Monitor:ecs: on" says monitoring is on for ecs. Same for
// "Application:domain" and "acs:tag:createdby", both of which came out
// unrecognisable. A tag you cannot paste back into the cloud console to find
// again is not much use.
func visibleTagKeys(tags map[string]string) []string {
	keys := make([]string, 0, len(tags))
	for key := range tags {
		if hasAnyPrefix(key, platformTagPrefixes) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func hasAnyPrefix(value string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

// describeAccount shows the alias next to the account name when they differ.
// The alias is what the scope tree labels this account with, so it is how the
// user thinks of it; the name is what cloud-sync's config calls it, which is
// the string they would grep for.
func describeAccount(h *jmsd.HostInfo) string {
	if h.AccountAlias == "" || h.AccountAlias == h.Account {
		return h.Account
	}
	return h.AccountAlias + " (" + h.Account + ")"
}

func describeInt(v *int, unit string) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%d%s", *v, unit)
}

func describeMemory(mb *int) string {
	if mb == nil {
		return ""
	}
	// Whole GiB below a GiB would round to "0 GiB", so small instances keep
	// their MB.
	if *mb >= 1024 && *mb%1024 == 0 {
		return fmt.Sprintf("%d GiB", *mb/1024)
	}
	return fmt.Sprintf("%d MiB", *mb)
}

// describeCharge renders the three states of IsPrepaid. nil is AWS, where the
// concept does not exist, so it prints nothing at all rather than claiming
// pay-as-you-go.
//
// The expiry is the point of showing any of this: a subscription instance
// stops itself when it passes. Auto-renew is what says whether that date is a
// deadline or just a date -- and its nil state is kept distinct from "off",
// because "未开自动续费" is a line somebody acts on.
func describeCharge(h *jmsd.HostInfo) string {
	if h.IsPrepaid == nil {
		return ""
	}
	if !*h.IsPrepaid {
		return "按量付费"
	}
	parts := []string{"包年包月"}
	if h.AutoRenew != nil {
		if *h.AutoRenew {
			parts = append(parts, "自动续费")
		} else {
			parts = append(parts, "未开自动续费")
		}
	}
	if expires := describeTime(h.ExpiredAt); expires != "" {
		parts = append(parts, "到期 "+expires)
	}
	return strings.Join(parts, " · ")
}

func describeTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04")
}

func describeDisk(d jmsd.DiskRef) string {
	parts := make([]string, 0, 4)
	if d.SizeGB > 0 {
		parts = append(parts, fmt.Sprintf("%d GB", d.SizeGB))
	}
	if d.Type != "" {
		parts = append(parts, d.Type)
	}
	if d.IOPS > 0 {
		parts = append(parts, fmt.Sprintf("%d IOPS", d.IOPS))
	}
	if d.VolumeID != "" {
		parts = append(parts, d.VolumeID)
	}
	return strings.Join(parts, " · ")
}

func describeOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
