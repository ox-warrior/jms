// The launch-time update offer.
//
// It lives here, ahead of tui.Run, rather than as a modal inside the TUI, and
// that placement is the design:
//
//   - Accepting means restarting, and at this point in the launch there is
//     nothing to restart *out of* -- no login, no ticket, no live SSH session.
//     The replacement and the exec cost the user one keystroke and land them in
//     the same place they were headed. From inside the TUI the same choice
//     would have to tear down the screen mid-session.
//   - A tview modal appears whenever the CDN answers, which is a moment nobody
//     chose. Stealing the keyboard while somebody is typing a filter is its own
//     bug, and this repo already carries regression tests for that whole family
//     (see internal/tui/prompt_test.go).
//
// The passive header notice in the TUI is a different signal with a different
// source -- jmsd's build versus this one -- and stays where it is.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/leiyupeng/jumpserver-cli/internal/jmslog"
	"github.com/leiyupeng/jumpserver-cli/internal/selfupdate"
	"github.com/leiyupeng/jumpserver-cli/internal/version"
)

// updateCheckBudget bounds the whole check -- DNS, connect, TLS, one small GET.
// It is the only thing standing between a CDN that has gone dark and every jms
// launch pausing on it, so it is deliberately shorter than a user would notice
// and shorter than they would tolerate.
//
// The check is started early and joined just before the TUI opens, so in the
// ordinary case the round trip overlaps the rest of the launch and costs
// nothing at all.
const updateCheckBudget = 2 * time.Second

// startUpdateCheck runs the check in the background and returns the channel its
// one result arrives on. A nil Release -- up to date, skipped, offline,
// unpublished platform, anything -- is the normal answer and says nothing.
func startUpdateCheck(baseURL string) <-chan *selfupdate.Release {
	out := make(chan *selfupdate.Release, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), updateCheckBudget)
		defer cancel()
		release, err := selfupdate.Check(ctx, baseURL, version.Version)
		switch {
		case errors.Is(err, selfupdate.ErrAlreadyUpdated):
			// The one check failure the user has to see. Silence here reads as
			// "I pressed update and nothing happened", with nothing pointing at
			// the broken release that actually caused it.
			fmt.Fprintf(os.Stderr, "jms: 更新装上了但版本号没变，发布可能有问题：%v\n", err)
			jmslog.Printf("update check: %v", err)
		case err != nil:
			// Debug, not info. A laptop off the office network fails this on
			// every launch, and an info line per launch is how a log stops
			// being read.
			jmslog.Debugf("update check: %v", err)
		}
		out <- release
	}()
	return out
}

// updateChoice is what the user answered.
type updateChoice int

const (
	// updateLater leaves everything alone and asks again next launch.
	updateLater updateChoice = iota
	updateNow
	updateSkip
)

// offerUpdate presents the release and acts on the answer. It returns only when
// jms should carry on launching -- accepting the update ends this process
// instead (see selfupdate.Restart), so on that path there is nothing to return
// to.
//
// Everything it writes goes to stderr, beside the "logging to ..." line and for
// the same reason: stdout belongs to the TUI that is about to take the screen.
func offerUpdate(ctx context.Context, release *selfupdate.Release) {
	jmslog.Printf("update: offering %s (running %s)", release.Version, version.Version)

	switch promptForUpdate(os.Stdin, os.Stderr, release.Version) {
	case updateSkip:
		if err := selfupdate.Skip(release.Version); err != nil {
			// Worth saying out loud: the user pressed a key that promised the
			// question would stop coming back, and it is about to come back.
			fmt.Fprintf(os.Stderr, "jms: 跳过没记下来，下次还会问：%v\n", err)
			jmslog.Printf("update: recording skip of %s failed: %v", release.Version, err)
			return
		}
		jmslog.Printf("update: %s skipped by user", release.Version)
	case updateNow:
		applyUpdate(ctx, release)
	case updateLater:
		jmslog.Printf("update: %s declined for this launch", release.Version)
	}
}

// promptForUpdate asks once and reads one line.
//
// Once, not in a retry loop: this sits between the user and the thing they
// actually typed the command for, and an unrecognised key is far more likely to
// be a stray keystroke than a serious attempt to answer. So anything
// unexpected -- including a closed stdin -- means updateLater, which changes
// nothing and asks again next time. Enter is the update, because that is the
// answer that is right nearly every time it is asked.
func promptForUpdate(in io.Reader, out io.Writer, newVersion string) updateChoice {
	fmt.Fprintf(out, "\njms %s 可用（当前 %s）\n", newVersion, version.Version)
	fmt.Fprintf(out, "  [Enter] 更新并重启    [s] 跳过该版本    [n] 这次不用\n> ")

	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(out)
		return updateLater
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return updateNow
	case "s", "skip":
		return updateSkip
	default:
		return updateLater
	}
}

// applyUpdate downloads and swaps in the new binary, then restarts into it.
func applyUpdate(ctx context.Context, release *selfupdate.Release) {
	fmt.Fprintf(os.Stderr, "jms: 下载 %s …\n", release.Version)
	path, err := selfupdate.Apply(ctx, release)
	if err != nil {
		if errors.Is(err, selfupdate.ErrNotWritable) {
			// The installer is the answer here, not sudo from inside jms. One
			// copy-pasteable line beats explaining the permission model.
			// Apply reports no path when it never got far enough to have one,
			// so the directory is asked for separately rather than read out of
			// a return value that is empty on exactly this path.
			// The base URL can be empty (no update_url configured), in which
			// case there is no install.sh to point at and a formatted line
			// would read "curl -fsSL /install.sh". Name the setting instead.
			if base := release.BaseURL; base != "" {
				fmt.Fprintf(os.Stderr, "jms: %s 不可写，无法自动更新。手动更新：\n"+
					"  curl -fsSL %s/install.sh | bash\n", selfupdate.InstallDir(), base)
			} else {
				fmt.Fprintf(os.Stderr, "jms: %s 不可写，无法自动更新。"+
					"请手动替换二进制，或在配置文件里设 update_url。\n", selfupdate.InstallDir())
			}
		} else {
			fmt.Fprintf(os.Stderr, "jms: 更新失败，继续用当前版本：%v\n", err)
		}
		jmslog.Printf("update: applying %s failed: %v", release.Version, err)
		return
	}
	jmslog.Printf("update: replaced binary with %s, restarting", release.Version)

	// Closed explicitly because Restart never returns to run a deferred close,
	// and the new process opens this same file for append the moment it starts.
	_ = jmslog.Close()
	err = selfupdate.Restart(path, release.Version)

	// Only reachable if exec failed. The binary on disk is already the new one,
	// so the honest thing is to say so and carry on with the code still running
	// in this process.
	fmt.Fprintf(os.Stderr, "jms: 已更新到 %s，但重启失败（%v）——重新运行 jms 生效。\n",
		release.Version, err)
}

// updatesWanted reports whether this launch should look for an update at all.
//
// Both conditions are about not getting in the way of something else:
//
//   - A headless "-c type name" run is somebody's script. A prompt there would
//     hang it, and a restart mid-flow would be worse.
//   - stdin not being a terminal is the same story arriving by a different
//     route: cron, a pipe, a CI step. There is nobody there to answer, and a
//     question asked of nobody is a hang.
//
// Both ends are checked, not just stdin. `jms 2>log` leaves stdin a terminal
// while the question itself goes into the file -- which is the worst of the
// three outcomes: jms waits for an answer to something the user never saw.
func updatesWanted(headless, disabled bool) bool {
	if headless || disabled {
		return false
	}
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}
