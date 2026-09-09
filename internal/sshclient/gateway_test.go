package sshclient

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// The regression: an asset jms could not reach used to be reported as
// "session ended", because Koko prints why and then exits 0 -- so the only
// thing that separates that from a logout is whether anyone touched the
// keyboard, and the only copy of the reason is what got tapped off stdout.
func TestSessionEndWithoutInputIsDistinguishable(t *testing.T) {
	stdin := &countReader{r: strings.NewReader("")}
	if _, err := io.Copy(io.Discard, stdin); err != nil {
		t.Fatalf("drain stdin: %v", err)
	}
	if got := stdin.n.Load(); got != 0 {
		t.Fatalf("no keystrokes should count as 0, got %d", got)
	}

	typed := &countReader{r: strings.NewReader("exit\n")}
	if _, err := io.Copy(io.Discard, typed); err != nil {
		t.Fatalf("drain stdin: %v", err)
	}
	if got := typed.n.Load(); got != 5 {
		t.Fatalf("typing `exit\\n` should count as 5, got %d", got)
	}
}

func TestHeadTapKeepsTheReasonAndStripsTheTerminalNoise(t *testing.T) {
	tap := &headTap{}
	// Shaped like what Koko actually writes: coloured, \r\n line ends.
	n, err := tap.Write([]byte("\x1b[31mConnect asset failed\x1b[0m\r\ni/o timeout\r\n"))
	if err != nil || n != 44 {
		t.Fatalf("Write must report the full length and no error, got %d, %v", n, err)
	}
	if got := tap.text(); got != "Connect asset failed\ni/o timeout" {
		t.Fatalf("text() = %q", got)
	}
}

// The regression: Koko prints an elapsed-seconds counter that redraws itself
// in place with \r. Deleting the \r instead of honouring it concatenated
// every frame, so the modal showed "0.00.00.10.2…3.3" wedged between the two
// lines that actually said something.
func TestHeadTapDiscardsOverwrittenProgressFrames(t *testing.T) {
	tap := &headTap{}
	tap.Write([]byte("开始连接到 svc-user(svc)@10.0.0.1\r\n"))
	for _, frame := range []string{"0.0", "0.1", "0.2", "1.0", "3.3"} {
		tap.Write([]byte("\r" + frame))
	}
	tap.Write([]byte("\r\n开始连接到 svc-user(svc)@10.0.0.1 error: 网络不通（路由不通）\r\n"))

	// "3.3" survives on purpose: the last frame was followed by \r\n, and on
	// a real terminal that leaves the text sitting on its line. One short
	// line is the faithful rendering; filtering it would mean special-casing
	// "looks like a number", which is fitting to Koko's current output.
	want := "开始连接到 svc-user(svc)@10.0.0.1\n" +
		"3.3\n" +
		"开始连接到 svc-user(svc)@10.0.0.1 error: 网络不通（路由不通）"
	if got := tap.text(); got != want {
		t.Fatalf("text() = %q\nwant %q", got, want)
	}
}

// The real thing, captured off the wire from a relay host with no route to
// it (jms -c "ec2 ..." > raw.bin). Koko puts its elapsed-seconds counter at
// the end of the "开始连接到" line and redraws it with three backspaces per
// frame; dropping \b as a non-graphic character instead of acting on it was
// what wedged "0.00.00.10.2…3.6" into the middle of the modal.
func TestHeadTapRendersKokoBackspaceCounter(t *testing.T) {
	const line = "开始连接到 svc-user(svc)@10.0.0.1"
	tap := &headTap{}
	tap.Write([]byte(line + "  0.0"))
	for _, frame := range []string{"0.0", "0.1", "0.2", "1.0", "2.9", "3.6"} {
		tap.Write([]byte("\b\b\b" + frame))
	}
	tap.Write([]byte("\r\n" + line + " error: 网络不通（路由不通）\r\n"))

	// The repeated prefix is collapsed by text() -- see TestSharedPrefixIsShownOnce.
	want := line + "  3.6\n  error: 网络不通（路由不通）"
	if got := tap.text(); got != want {
		t.Fatalf("text() = %q\nwant %q", got, want)
	}
}

// Backing over a multi-byte character must not leave half of one behind --
// text() would then be invalid UTF-8 on its way into a log line and a modal.
func TestHeadTapBackspaceDropsWholeRunes(t *testing.T) {
	tap := &headTap{}
	tap.Write([]byte("网络不通\b\b是通的\r\n"))
	if got := tap.text(); got != "网络是通的" {
		t.Fatalf("text() = %q", got)
	}
}

// Koko announces the same "开始连接到 <user>@<addr>" prefix twice, once with
// its progress counter and once with the failure. In a modal narrow enough to
// wrap that was two copies of the same 45 characters.
func TestSharedPrefixIsShownOnce(t *testing.T) {
	const line = "开始连接到 svc-user(svc)@10.0.0.1"
	tap := &headTap{}
	tap.Write([]byte(line + "  0.0\b\b\b3.4\r\n" + line + " error: 网络不通（路由不通）\r\n"))

	want := line + "  3.4\n  error: 网络不通（路由不通）"
	if got := tap.text(); got != want {
		t.Fatalf("text() = %q\nwant %q", got, want)
	}
}

func TestSharedPrefixLeavesUnrelatedLinesAlone(t *testing.T) {
	// Short coincidental overlap ("网络" is 6 bytes, under minSharedPrefix).
	tap := &headTap{}
	tap.Write([]byte("网络不通\r\n网络也许通了\r\n"))
	if got := tap.text(); got != "网络不通\n网络也许通了" {
		t.Fatalf("text() = %q", got)
	}
}

// One line being wholly the opening of the next used to walk sharedPrefixLen
// off the end of the shorter string.
func TestSharedPrefixHandlesOneLineContainingTheOther(t *testing.T) {
	for _, pair := range [][2]string{
		{"开始连接到 svc-user@10.0.0.1", "开始连接到 svc-user@10.0.0.1 error: 网络不通"},
		{"开始连接到 svc-user@10.0.0.1 error: 网络不通", "开始连接到 svc-user@10.0.0.1"},
	} {
		tap := &headTap{}
		tap.Write([]byte(pair[0] + "\r\n" + pair[1] + "\r\n"))
		if got := tap.text(); !strings.Contains(got, "网络不通") {
			t.Fatalf("text() = %q, lost the reason", got)
		}
	}
}

// The same bug's other half: a redraw that costs cap budget would, on a
// connect that takes long enough, push the actual error out of the buffer.
// A counter must be able to spin indefinitely without consuming any.
func TestSpinningProgressNeverExhaustsTheCap(t *testing.T) {
	tap := &headTap{}
	for i := 0; i < 50*headTapMax; i++ {
		tap.Write([]byte("\r0.1"))
	}
	tap.Write([]byte("\rerror: 网络不通（路由不通）\r\n"))
	if got := tap.text(); got != "error: 网络不通（路由不通）" {
		t.Fatalf("text() = %q", got)
	}
}

func TestHeadTapStopsAtTheCap(t *testing.T) {
	tap := &headTap{}
	// One write that overshoots, then one that should be dropped whole --
	// the cap is what keeps the user's own shell output out of the log.
	if n, _ := tap.Write([]byte(strings.Repeat("a", headTapMax+10))); n != headTapMax+10 {
		t.Fatalf("Write must report the full length even past the cap, got %d", n)
	}
	if n, _ := tap.Write([]byte("bbb")); n != 3 {
		t.Fatalf("Write must report the full length once full, got %d", n)
	}
	if got := tap.text(); got != strings.Repeat("a", headTapMax) {
		t.Fatalf("text() kept %d bytes, want %d", len(got), headTapMax)
	}
}

// The regression: the captured text used to be discarded whenever Wait()
// also returned an error -- `return err` came before it was ever consulted.
// Koko exits 0 on most unreachable assets but not all, and the log has real
// examples of the other kind ("wait: remote command exited without exit
// status or exit signal"), which on its own explains nothing.
func TestSessionOutcome(t *testing.T) {
	waitErr := errors.New("wait: remote command exited without exit status or exit signal")
	for _, tc := range []struct {
		name  string
		wait  error
		typed int64
		out   string
		want  string
	}{
		{"typed, clean exit", nil, 5, "banner", ""},
		{"typed, wait error", waitErr, 5, "banner", waitErr.Error()},
		{"no input, exit 0, text kept", nil, 0, "网络不通", "remote closed the session without any input from us:\n网络不通"},
		{"no input, wait error, text still kept", waitErr, 0, "网络不通", "remote closed the session without any input from us:\n网络不通"},
		{"no input, wait error, nothing captured", waitErr, 0, "", "remote closed the session without any input from us: " + waitErr.Error()},
		{"no input, nothing at all", nil, 0, "", "remote closed the session without any input from us"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := sessionOutcome(tc.wait, tc.typed, tc.out)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("want nil, got %v", err)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				t.Fatalf("got %v\nwant %s", err, tc.want)
			}
		})
	}
	// The wrapped case must stay unwrappable, so a caller can still test for
	// what x/crypto/ssh actually returned.
	if err := sessionOutcome(waitErr, 0, ""); !errors.Is(err, waitErr) {
		t.Fatal("waitErr must stay unwrappable")
	}
}
