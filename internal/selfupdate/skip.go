package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// state is what jms remembers between launches about updating. One field so
// far, and a JSON object rather than a bare version string so adding the next
// one does not need a format migration.
type state struct {
	// SkippedVersion is the one version the user declined by name. Storing the
	// version rather than a "don't ask again" flag is what makes the prompt's
	// wording true: skipping 0.1.3 must not also silence 0.1.4, or the user
	// has been opted out of updates permanently by a keystroke that promised
	// less than that.
	SkippedVersion string `json:"skipped_version"`
}

// statePath is <user cache dir>/jms/update.json.
//
// The cache directory, not the config directory: the config file is
// hand-written and carries the user's comments, and a program that rewrites it
// to record a keystroke would eat them. This file is regenerable -- losing it
// costs one re-asked question -- which is exactly what UserCacheDir is for,
// and it lands beside the log and the asset cache for the same reason they do.
func statePath() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate cache dir: %w", err)
	}
	return filepath.Join(dir, "jms", "update.json"), nil
}

// skippedVersion returns the version the user declined, or "" for a missing,
// unreadable or corrupt file. A miss is never an error to the caller: the
// fallback is to ask, which is always safe.
func skippedVersion() string {
	path, err := statePath()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var s state
	if err := json.Unmarshal(data, &s); err != nil {
		return ""
	}
	return s.SkippedVersion
}

// Skip records that the user declined this version, so the next launch does
// not ask about it again.
//
// The error is returned rather than swallowed because the caller has something
// honest to say about it: a skip that could not be written means the question
// comes back next launch, and a user who pressed "skip" and got asked again
// should be told why rather than left thinking the key does nothing.
func Skip(version string) error {
	path, err := statePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}
	data, err := json.Marshal(state{SkippedVersion: version})
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
