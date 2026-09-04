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
	fmt.Fprintln(os.Stderr, err)
	// A headless "-c" run reports a specific code (docs/PLAN.md §8.1) so a
	// script can tell "the target never resolved" from "it resolved but the
	// connection failed" without parsing this line. Every other error --
	// which is every error the TUI path can produce -- falls back to 1.
	var exitErr *cli.ExitError
	if errors.As(err, &exitErr) {
		os.Exit(exitErr.Code)
	}
	os.Exit(1)
}
