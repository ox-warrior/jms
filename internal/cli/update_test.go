package cli

import (
	"strings"
	"testing"

	"github.com/leiyupeng/jumpserver-cli/internal/version"
)

// The key mapping decides, among other things, what a reflexive Enter does to
// the binary the user is running. Getting it wrong in either direction is a real
// failure: Enter meaning nothing makes the feature pointless, and an
// unrecognised key meaning "update" restarts somebody who was reaching for
// Ctrl-C.
func TestPromptForUpdate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		input string
		want  updateChoice
	}{
		{name: "enter takes the update", input: "\n", want: updateNow},
		{name: "y takes the update", input: "y\n", want: updateNow},
		{name: "s skips the version", input: "s\n", want: updateSkip},
		{name: "skip spelled out", input: "SKIP\n", want: updateSkip},
		{name: "n declines for this launch", input: "n\n", want: updateLater},
		{name: "surrounding whitespace is trimmed", input: "  s  \n", want: updateSkip},
		{
			// Far likelier to be a stray keystroke than an answer, so it takes
			// the option that changes nothing and asks again next launch.
			name: "an unrecognised key changes nothing", input: "q\n", want: updateLater,
		},
		{
			// Ctrl-D, or a stdin that is not what we thought. Must not be read
			// as consent to overwrite the binary.
			name: "closed stdin changes nothing", input: "", want: updateLater,
		},
		{
			// A tty hands over a line without its newline when the terminal is
			// closed mid-answer; the answer is still an answer.
			name: "answer without a trailing newline still counts", input: "s", want: updateSkip,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out strings.Builder
			if got := promptForUpdate(strings.NewReader(tc.input), &out, "0.1.3"); got != tc.want {
				t.Errorf("promptForUpdate(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// Both numbers have to appear. "An update is available" without saying from
// what to what leaves the user nothing to check against -- the same reasoning
// the TUI's header notice is held to (see internal/tui/version_test.go).
func TestPromptNamesBothVersions(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	promptForUpdate(strings.NewReader("n\n"), &out, "0.1.3")
	text := out.String()
	for _, want := range []string{"0.1.3", version.Version} {
		if !strings.Contains(text, want) {
			t.Errorf("prompt = %q, want it to name %q", text, want)
		}
	}
	// Every option the prompt accepts has to be visible, or the only
	// discoverable answer is Enter -- which is the one that overwrites the
	// binary.
	for _, want := range []string{"Enter", "s", "n"} {
		if !strings.Contains(text, want) {
			t.Errorf("prompt = %q, want it to offer %q", text, want)
		}
	}
}

// A "-c type name" run is somebody's script: it gets no prompt, ever. A
// question asked where nobody can answer it is a hang, and this one would hang
// with the terminal already handed over.
func TestHeadlessAndDisabledRunsAreNeverPrompted(t *testing.T) {
	t.Parallel()
	if updatesWanted(true, false) {
		t.Error("updatesWanted(headless) = true, want no prompt on a scripted run")
	}
	if updatesWanted(false, true) {
		t.Error("updatesWanted(disabled) = true, want the config file's opt-out honoured")
	}
	if updatesWanted(true, true) {
		t.Error("updatesWanted(headless, disabled) = true")
	}
	// Under `go test` neither stream is a terminal, so the interactive case
	// cannot be exercised here -- but its negative can, and it is the one that
	// would hang: a prompt written where nobody can read it, waiting on an
	// answer that is never coming.
	if updatesWanted(false, false) {
		t.Error("updatesWanted = true with no terminal attached, want no prompt")
	}
}
