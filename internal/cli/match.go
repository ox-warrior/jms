package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
)

// ErrAssetNotFound and ErrAssetAmbiguous are matchAsset's two failure modes,
// distinct so a caller can report each one usefully (candidates on
// ambiguous, "try a refresh" on not-found).
var (
	ErrAssetNotFound  = errors.New("no asset with that name")
	ErrAssetAmbiguous = errors.New("multiple assets with that name")
)

// matchAsset finds the one asset matching kind and name exactly.
// docs/PLAN.md §8.1 requires exact matching, case-sensitive, with no
// fuzzy/prefix fallback: a headless "-c" run has nobody watching to catch a
// wrong guess before it mints a real, audited connection ticket.
//
// JumpServer allows the same asset name to appear more than once (different
// nodes, different orgs), so more than one exact match is a real outcome, not
// a bug -- reported as ErrAssetAmbiguous rather than picked between.
func matchAsset(assets []jmsd.AssetRow, kind jmsd.ResourceKind, name string) (jmsd.AssetRow, []jmsd.AssetRow, error) {
	var matches []jmsd.AssetRow
	for _, a := range assets {
		if jmsd.MatchesKind(kind, a.Category, a.Type) && a.Name == name {
			matches = append(matches, a)
		}
	}
	switch len(matches) {
	case 0:
		return jmsd.AssetRow{}, nil, ErrAssetNotFound
	case 1:
		return matches[0], matches, nil
	default:
		return jmsd.AssetRow{}, matches, ErrAssetAmbiguous
	}
}

// ErrAccountNotFound and ErrAccountAmbiguous are matchAccountByUsername's two
// failure modes.
var (
	ErrAccountNotFound  = errors.New("no account with that username")
	ErrAccountAmbiguous = errors.New("multiple accounts with that username")
)

// matchAccountByUsername finds the account whose Username equals username
// exactly. Username, not Alias: Alias is what jmsd's ticket-minting call
// actually takes (docs/PLAN.md §5), but nothing in the TUI ever shows it to a
// human, so nobody types it by that name -- "root" means the Username.
//
// A JumpServer asset can carry two accounts with the same Username (one
// manually added, one auto-discovered) -- reported as ErrAccountAmbiguous
// rather than picked between, the same call docs/PLAN.md §8.1 makes for asset
// names.
func matchAccountByUsername(options []jmsd.AccountOption, username string) (jmsd.AccountOption, error) {
	var matches []jmsd.AccountOption
	for _, a := range options {
		if a.Username == username {
			matches = append(matches, a)
		}
	}
	switch len(matches) {
	case 0:
		return jmsd.AccountOption{}, ErrAccountNotFound
	case 1:
		return matches[0], nil
	default:
		return jmsd.AccountOption{}, ErrAccountAmbiguous
	}
}

// describeAccount renders one candidate account. Username leads because
// --account matches on Username -- the reader is being shown what to type.
func describeAccount(a jmsd.AccountOption) string {
	if a.Name == "" || a.Name == a.Username {
		return a.Username
	}
	return fmt.Sprintf("%s (%s)", a.Username, a.Name)
}

// describeAccounts renders every candidate account for an error message, e.g.
// when --account matched none or several.
func describeAccounts(options []jmsd.AccountOption) string {
	names := make([]string, len(options))
	for i, a := range options {
		names[i] = describeAccount(a)
	}
	return strings.Join(names, ", ")
}

// describeAssets renders every candidate asset for an error message, e.g.
// when a "-c" name matched more than one asset.
func describeAssets(assets []jmsd.AssetRow) string {
	names := make([]string, len(assets))
	for i, a := range assets {
		names[i] = fmt.Sprintf("%s (id=%s)", a.Name, a.ID)
	}
	return strings.Join(names, ", ")
}
