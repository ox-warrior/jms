package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
)

// shortcutTarget is the parsed form of "-c TYPE [NAME]", mfmc's own syntax
// carried over so the muscle memory transfers. Name is "" for the kind-only
// form ("-c ec2"), which pre-filters the TUI (tui.Options.InitialKind) instead
// of connecting headlessly.
type shortcutTarget struct {
	Kind jmsd.ResourceKind
	Name string
}

// parseShortcut parses one "-c" value. Exactly one or two whitespace-separated
// fields, so a third field is not "extra text ignored" but "not this syntax",
// and is refused -- the same call mfmc makes.
func parseShortcut(command string) (shortcutTarget, error) {
	fields := strings.Fields(command)
	if len(fields) != 1 && len(fields) != 2 {
		return shortcutTarget{}, fmt.Errorf("-c %q: expected \"type\" or \"type name\"", command)
	}
	kind, ok := jmsd.ParseResourceKind(fields[0])
	if !ok {
		return shortcutTarget{}, fmt.Errorf("-c: unknown resource type %q (want one of: %s)",
			fields[0], strings.Join(jmsd.ResourceKindTokens(), ", "))
	}
	target := shortcutTarget{Kind: kind}
	if len(fields) == 2 {
		target.Name = fields[1]
	}
	return target, nil
}

// parseCommandFlags validates and parses the "-c"/"--account" flag pair.
// A nil target means no "-c" was given -- the ordinary TUI launch.
//
// --account is rejected outright when it cannot mean anything: with no "-c"
// at all, or with a kind-only "-c" (nothing to connect to, so nothing to pin
// an account on). Silently ignoring it in either case would connect as the
// default account with no sign that --account was even noticed.
func parseCommandFlags(flags rootFlags) (*shortcutTarget, error) {
	if flags.command == "" {
		if flags.account != "" {
			return nil, errors.New("--account requires -c \"type name\"")
		}
		return nil, nil
	}
	target, err := parseShortcut(flags.command)
	if err != nil {
		return nil, err
	}
	if flags.account != "" && target.Name == "" {
		return nil, errors.New("--account requires -c \"type name\" (a name, not just a type)")
	}
	return &target, nil
}
