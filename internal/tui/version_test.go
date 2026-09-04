package tui

import (
	"strings"
	"testing"

	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
	"github.com/leiyupeng/jumpserver-cli/internal/version"
)

func TestUpdateNotice(t *testing.T) {
	t.Parallel()
	matched := buildInfo{version: "0.1.1", jumpServerBaseline: "v4.10.16"}

	for _, tc := range []struct {
		name          string
		local, remote buildInfo
		// wantNamed are the strings the notice must contain; empty means the
		// notice itself must be empty.
		wantNamed []string
	}{
		{
			name:  "same build says nothing",
			local: matched, remote: matched,
		},
		{
			name:   "stale client is told to update",
			local:  matched,
			remote: buildInfo{version: "0.2.0", jumpServerBaseline: "v4.10.16"},
			// Both numbers: "please update" without saying from what to what
			// leaves the user nothing to check against.
			wantNamed: []string{"0.1.1", "0.2.0"},
		},
		{
			// jmsd is the one lagging here. Still worth saying: the two came
			// from different builds either way, and which side needs moving is
			// obvious from the two numbers shown.
			name:      "client ahead of jmsd still reports the mismatch",
			local:     buildInfo{version: "0.2.0", jumpServerBaseline: "v4.10.16"},
			remote:    matched,
			wantNamed: []string{"0.2.0", "0.1.1"},
		},
		{
			name:      "jumpserver baseline drift is reported on its own",
			local:     matched,
			remote:    buildInfo{version: "0.1.1", jumpServerBaseline: "v4.12.0"},
			wantNamed: []string{"JumpServer", "v4.10.16", "v4.12.0"},
		},
		{
			// Both at once: one notice carrying both facts, build version
			// first because it is the actionable one.
			name:      "both mismatches land in one notice",
			local:     matched,
			remote:    buildInfo{version: "0.2.0", jumpServerBaseline: "v4.12.0"},
			wantNamed: []string{"0.2.0", "v4.12.0", "·"},
		},
		{
			// The case that must NOT warn: a jmsd from before these fields
			// existed sends nothing, and "nothing" is not evidence of a
			// mismatch. Warning here would nag on every single launch against
			// an older daemon, which is exactly how a notice gets ignored.
			name:  "jmsd too old to report anything stays quiet",
			local: matched, remote: buildInfo{},
		},
		{
			// Half-old jmsd: reports its build but not its baseline. The
			// unreported field is skipped, the reported one still checked.
			name:      "missing baseline alone does not suppress the version check",
			local:     matched,
			remote:    buildInfo{version: "0.2.0"},
			wantNamed: []string{"0.2.0"},
		},
		{
			name:   "missing baseline with matching version stays quiet",
			local:  matched,
			remote: buildInfo{version: "0.1.1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := updateNotice(tc.local, tc.remote)
			if len(tc.wantNamed) == 0 {
				if got != "" {
					t.Fatalf("updateNotice(%+v, %+v) = %q, want no notice", tc.local, tc.remote, got)
				}
				return
			}
			if got == "" {
				t.Fatalf("updateNotice(%+v, %+v) = \"\", want a notice", tc.local, tc.remote)
			}
			for _, want := range tc.wantNamed {
				if !strings.Contains(got, want) {
					t.Errorf("notice = %q, want it to name %q", got, want)
				}
			}
		})
	}
}

// The baseline check only means anything if the two sides can actually hold
// different values. A single shared constant could never disagree with itself,
// so this pins that they are declared separately.
func TestJumpServerBaselinesAreDeclaredIndependently(t *testing.T) {
	t.Parallel()
	// Same value today -- built from one commit, as they should be. The point
	// is that updateNotice reads them from two places, which the table above
	// exercises by feeding them different values.
	if version.JumpServerBaseline != jmsd.JumpServerBaseline {
		t.Errorf("baselines have drifted: jms %q, jmsd %q -- bump both or neither",
			version.JumpServerBaseline, jmsd.JumpServerBaseline)
	}
}

func TestDescribeServerVersionNamesTheUnreportedCase(t *testing.T) {
	t.Parallel()
	if got := describeServerVersion("0.2.0"); got != "0.2.0" {
		t.Errorf("describeServerVersion(0.2.0) = %q, want it passed through", got)
	}
	if got := describeServerVersion(""); got == "" {
		t.Error("describeServerVersion(\"\") = \"\", want it to say the version was not reported")
	}
}

// The notice rides in the header, so it has to survive the status updates that
// keep landing there -- a load finishing, a scope change. If it did not, it
// would be gone seconds after login, which is the failure the status bar
// already has and the reason this is not in the status bar.
func TestUpdateNoticeSurvivesStatusChanges(t *testing.T) {
	w, _ := newTestWorkspace(t, nil)
	w.SetUpdateNotice(updateNotice(buildInfo{version: "0.1.0", jumpServerBaseline: "v4.10.16"}, buildInfo{version: "0.2.0", jumpServerBaseline: "v4.10.16"}))

	for _, c := range []struct {
		level  statusLevel
		status string
	}{
		{statusBusy, "loading…"},
		{statusOK, "ready"},
		{statusBusy, "connect cancelled"},
		{statusOK, "ready"},
	} {
		w.setStatus("alice@corp.com", c.level, c.status)
		if text := w.header.GetText(true); !strings.Contains(text, "需更新") {
			t.Fatalf("header lost the update notice after setStatus(%q): %q", c.status, text)
		}
	}
}

// Repainting for the notice must not blank the status already showing.
func TestSetUpdateNoticeKeepsCurrentStatus(t *testing.T) {
	w, _ := newTestWorkspace(t, nil)
	w.setStatus("alice@corp.com", statusOK, "ready")
	w.SetUpdateNotice(updateNotice(buildInfo{version: "0.1.0", jumpServerBaseline: "v4.10.16"}, buildInfo{version: "0.2.0", jumpServerBaseline: "v4.10.16"}))

	text := w.header.GetText(true)
	if !strings.Contains(text, "ready") {
		t.Errorf("header = %q, want the existing status kept", text)
	}
	if !strings.Contains(text, "alice@corp.com") {
		t.Errorf("header = %q, want the username kept", text)
	}
}

// Matching versions must leave the header exactly as it was.
func TestNoNoticeLeavesHeaderClean(t *testing.T) {
	w, _ := newTestWorkspace(t, nil)
	w.SetUpdateNotice("")
	w.setStatus("alice@corp.com", statusOK, "ready")

	if text := w.header.GetText(true); strings.Contains(text, "需更新") {
		t.Errorf("header = %q, want no update notice", text)
	}
}
