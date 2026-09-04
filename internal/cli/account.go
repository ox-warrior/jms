package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
)

// accountPrompt asks which of several permitted accounts to connect as and
// returns the chosen index. A nil prompt means the caller cannot ask, which
// turns an ambiguous account into an error rather than a guess.
type accountPrompt func(assetName string, options []jmsd.AccountOption) (int, error)

// resolveAccount decides which account a headless run connects as.
//
// With --account: an exact Username match, refused rather than guessed at when
// it hits zero or several. Without it: a lone permitted account is used
// directly, and anything more is asked about.
//
// Never "the first one". jmsd does not promise a stable order for this list,
// so a positional default is a coin flip between a routine account and a
// privileged one -- resolvable differently on different days for the very same
// command, which is the one thing an audited connection must not be.
func resolveAccount(assetName string, options []jmsd.AccountOption, username string, choose accountPrompt) (jmsd.AccountOption, error) {
	if username != "" {
		account, err := matchAccountByUsername(options, username)
		if err != nil {
			return jmsd.AccountOption{}, fmt.Errorf("--account %s: %w (candidates: %s)",
				username, err, describeAccounts(options))
		}
		return account, nil
	}
	if len(options) == 1 {
		return options[0], nil
	}
	if choose == nil {
		return jmsd.AccountOption{}, ambiguousAccountErr(assetName, options)
	}
	i, err := choose(assetName, options)
	if err != nil {
		return jmsd.AccountOption{}, err
	}
	if i < 0 || i >= len(options) {
		return jmsd.AccountOption{}, fmt.Errorf("account choice %d is out of range", i)
	}
	return options[i], nil
}

func ambiguousAccountErr(assetName string, options []jmsd.AccountOption) error {
	return fmt.Errorf("%s has %d permitted accounts and there is no terminal to choose on -- pass --account (candidates: %s)",
		assetName, len(options), describeAccounts(options))
}

// chooseAccountFromTTY lists the permitted accounts and reads a number back on
// the controlling terminal, bypassing stdin/stdout so it behaves the same
// whether or not those are redirected. No controlling terminal at all -- cron,
// CI, a systemd unit -- is not an error case to prompt through: it becomes the
// same "pass --account" refusal a nil prompt gives.
//
// There is deliberately no default choice: Enter meaning "the first one" would
// reintroduce exactly the unstable-ordering coin flip this exists to remove.
func chooseAccountFromTTY(assetName string, options []jmsd.AccountOption) (int, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return 0, ambiguousAccountErr(assetName, options)
	}
	defer tty.Close()
	return promptAccountChoice(tty, tty, assetName, options)
}

// promptAccountChoice is the terminal-independent half of
// chooseAccountFromTTY: it prints the menu to out and reads a 1-based choice
// from in, re-asking until it gets one. Split out so the loop is testable
// without a controlling terminal.
func promptAccountChoice(in io.Reader, out io.Writer, assetName string, options []jmsd.AccountOption) (int, error) {
	fmt.Fprintf(out, "%s · %d permitted accounts:\n", assetName, len(options))
	for i, a := range options {
		fmt.Fprintf(out, "  %d) %s\n", i+1, describeAccount(a))
	}

	scanner := bufio.NewScanner(in)
	for {
		fmt.Fprintf(out, "account [1-%d]: ", len(options))
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return 0, fmt.Errorf("read account choice: %w", err)
			}
			// EOF -- Ctrl+D, or the terminal went away mid-prompt.
			fmt.Fprintln(out)
			return 0, errors.New("account selection cancelled")
		}
		choice, err := strconv.Atoi(strings.TrimSpace(scanner.Text()))
		if err != nil || choice < 1 || choice > len(options) {
			fmt.Fprintf(out, "  not a choice between 1 and %d\n", len(options))
			continue
		}
		return choice - 1, nil
	}
}
