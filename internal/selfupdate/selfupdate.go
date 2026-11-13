// Package selfupdate finds the newest published jms at launch and replaces
// the running binary with it.
//
// It exists because nothing in this story is a package manager. jms is
// installed by curling a binary out of a CDN (scripts/install.sh), so a laptop
// keeps whatever build it was handed until somebody re-runs that command by
// hand -- and nobody does. The skew surfaces much later as jms behaving unlike
// the jmsd it is talking to.
//
// The CDN, not jmsd, is the authority on "is there a newer jms". jmsd's
// /v1/whoami version answers a different question -- whether this client and
// that daemon came from one build -- and using it here would offer a
// *downgrade* every time the machine room lags a release behind. That
// comparison stays where it is, as the TUI's passive header notice
// (tui.updateNotice); this is a separate signal with a separate source.
//
// A failure to *find* a release is silent (no network, no manifest, an
// unparseable version, an unpublished platform): the caller starts anyway. A
// failure to *install* one is the caller's to report -- see cli.forceUpdate.
package selfupdate

import (
	"fmt"
	"strconv"
	"strings"
)

// Release is the published build this platform would move to.
type Release struct {
	// Version is the manifest's version, already established as newer than
	// this build's.
	Version string
	// URL is the binary for this GOOS/GOARCH.
	URL string
	// SHA256 is that binary's expected digest, hex, from the same manifest.
	// Never empty: a manifest entry without one is treated as no entry at all
	// (see parseManifest), because "download an executable and exec it" with
	// nothing but HTTPS behind it is the one part of this worth refusing.
	SHA256 string
	// BaseURL is the publish location this release was found at, carried along
	// so the "binary not writable, update by hand" message can name the right
	// install.sh. It cannot be recovered from URL, which points at the binary
	// inside a per-platform directory, and there is no package-level default to
	// fall back on any more (see DefaultBaseURL).
	BaseURL string
}

// isNewer reports whether published is strictly ahead of local.
//
// Ordering, not inequality, and that distinction is the whole safety story for
// builds made from source. A version that does not parse -- "dev", which is
// what a plain `go build` compiles in, or a bad manifest, or an empty string --
// is an error, which callers read as "offer nothing". A build that does carry a
// real number but is ahead of what has been published (release.sh tagged but
// not yet uploaded) compares as local > published and this returns false. Both
// roads lead to leaving a developer's binary alone.
func isNewer(published, local string) (bool, error) {
	order, err := compareVersions(published, local)
	if err != nil {
		return false, err
	}
	return order > 0, nil
}

// compareVersions returns -1, 0 or +1 as a sorts before, with, or after b.
// Both must be exactly three dot-separated runs of digits: this repo has only
// ever published X.Y.Z (see internal/version.Version and the grep in
// scripts/release.sh that reads it), and accepting looser shapes here would
// mean inventing an ordering for suffixes nothing produces.
func compareVersions(a, b string) (int, error) {
	fieldsA, err := parseVersion(a)
	if err != nil {
		return 0, err
	}
	fieldsB, err := parseVersion(b)
	if err != nil {
		return 0, err
	}
	for i := range fieldsA {
		switch {
		case fieldsA[i] < fieldsB[i]:
			return -1, nil
		case fieldsA[i] > fieldsB[i]:
			return 1, nil
		}
	}
	return 0, nil
}

func parseVersion(v string) ([3]int, error) {
	var out [3]int
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, fmt.Errorf("version %q is not X.Y.Z", v)
	}
	for i, part := range parts {
		// Atoi alone would accept "+1" and "-1", and a negative component
		// would order a malformed version ahead of a real one -- exactly the
		// wrong direction for something that decides whether to overwrite a
		// binary.
		if part == "" || strings.ContainsFunc(part, func(r rune) bool { return r < '0' || r > '9' }) {
			return out, fmt.Errorf("version %q has a non-numeric component %q", v, part)
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return out, fmt.Errorf("version %q: %w", v, err)
		}
		out[i] = n
	}
	return out, nil
}
