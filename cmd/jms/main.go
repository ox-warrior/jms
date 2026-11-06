package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/leiyupeng/jumpserver-cli/internal/cli"
)

func main() {
	err := cli.Execute()
	if err == nil {
		return
	}
	// A headless "-c" run reports a specific code (docs/PLAN.md §8.1) so a
	// script can tell "the target never resolved" from "it resolved but the
	// connection failed" without parsing this line. Every other error --
	// which is every error the TUI path can produce -- falls back to 1.
	//
	// An ExitError with no Err is a remote command's own status passed
	// through: it has already printed whatever it had to say.
	var exitErr *cli.ExitError
	if errors.As(err, &exitErr) {
		if exitErr.Err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(exitErr.Code)
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
