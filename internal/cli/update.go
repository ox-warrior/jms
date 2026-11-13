// The launch-time forced update.
//
// Every launch -- TUI, "-c", "-w" -- asks the CDN for the newest jms before
// doing anything else, and a newer one is installed and restarted into without
// asking: jms and jmsd change their wire format together, and an old client
// left running fails in ways nobody can read. It happens ahead of tui.Run,
// where there is nothing yet to restart out of -- no ticket, no live session.
// A launch that cannot reach the CDN carries on: being offline is not a
// reason to refuse to start.
//
// The passive header notice in the TUI is a different signal with a different
// source -- jmsd's build versus this one -- and stays where it is.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

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
// one result arrives on. A nil Release -- up to date, offline,
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
			fmt.Fprintf(os.Stderr, "jms: 更新装上了但版本号没变，发布可能有问题，请联系 %s：%v\n", updateContact, err)
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

// updateCheckFor is startUpdateCheck for a person and nothing at all for a
// service key (-i): nil, no request to the CDN.
//
// A service key runs inside an image somebody pinned. Forcing an update there
// either fails the launch (read-only binary) or silently swaps the binary the
// image was built with; either way the machine's jms is upgraded by rebuilding
// the image, not by itself. The cost is old clients that stay old, which is why
// jmsd's protocol changes have to stay backward compatible.
func updateCheckFor(identity, baseURL string) <-chan *selfupdate.Release {
	if identity != "" {
		return nil
	}
	return startUpdateCheck(baseURL)
}

// updateContact is who to ask when a forced update cannot complete.
const updateContact = "leiyupeng"

// forceUpdate downloads and swaps in the new binary, then restarts into it with
// the same arguments. There is no "later": an old jms against a newer jmsd is
// how people end up at a sign-in that can never succeed. So it either restarts
// into the new build or returns the error that stops this launch.
func forceUpdate(ctx context.Context, release *selfupdate.Release) error {
	fmt.Fprintf(os.Stderr, "jms: 发现新版本 %s（当前 %s），正在升级…\n", release.Version, version.Version)
	path, err := selfupdate.Apply(ctx, release)
	if err != nil {
		jmslog.Printf("update: applying %s failed: %v", release.Version, err)
		hint := ""
		if errors.Is(err, selfupdate.ErrNotWritable) && release.BaseURL != "" {
			hint = fmt.Sprintf("\n重装一次即可（不要加 sudo，需要时脚本会自己要密码）：curl -fsSL %s/install.sh | bash", release.BaseURL)
		}
		return fmt.Errorf("升级到 %s 失败：%w%s\n请联系 %s", release.Version, err, hint, updateContact)
	}
	jmslog.Printf("update: replaced binary with %s, restarting", release.Version)
	// Closed explicitly because Restart never returns to run a deferred close,
	// and the new process opens this same file for append the moment it starts.
	_ = jmslog.Close()
	err = selfupdate.Restart(path, release.Version)
	// Only reachable if exec failed; the binary on disk is already the new one.
	return fmt.Errorf("已升级到 %s，但重启失败（%v）——请重新运行 jms", release.Version, err)
}
