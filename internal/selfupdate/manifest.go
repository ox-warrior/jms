package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
)

// DefaultBaseURL is empty in the source tree for the same reason defaultServer
// is (see internal/cli/root.go): a publish location only ever belongs to one
// deployment, and a repository cannot know which. The config file's update_url
// is the single place it gets configured -- deliberately no build-time knob, so
// a deployment that wants self-update has exactly one thing to write down.
//
// Empty means the launch-time update check is simply off (Check returns no
// release and no error), which is the right behaviour for a build nobody has
// pointed at a publish location yet.
var DefaultBaseURL = ""

// manifestName is fetched instead of the plain-text "version" file that
// scripts/install.sh reads. Two reasons it is a second file rather than a
// change to that one: install.sh in the wild still curls "version" and would
// start writing JSON into ~/.jms_version, and the digest has to arrive from the
// same request as the version number -- fetching them separately leaves a
// window where they describe different builds.
const manifestName = "manifest.json"

// manifest is the document scripts/release.sh writes next to the binaries.
//
// Keyed by the CDN's directory name (darwin/amd/linux), not by GOOS/GOARCH:
// those directory names are an inherited convention from mfmc that other tools
// already depend on, so the mapping lives in targetDir rather than being
// tidied up here.
type manifest struct {
	Version string                    `json:"version"`
	Targets map[string]manifestTarget `json:"targets"`
}

type manifestTarget struct {
	SHA256 string `json:"sha256"`
}

// errNoTarget means the manifest is fine and simply does not cover this
// machine -- a Windows machine, say. Distinguished from a parse failure
// only so the log line says which of the two happened.
var errNoTarget = errors.New("no published build for this platform")

// targetDir maps this build's GOOS/GOARCH to the CDN directory holding its
// binary.
//
// This mapping also exists in scripts/install.sh, and a new publish target has
// to be added in three places: TARGETS in scripts/release.sh, the case
// statement in scripts/install.sh, and here. Sharing one copy would mean jms
// fetching its own platform table over the network, which is a lot of moving
// parts for a table that changes about once a year.
func targetDir() (string, error) {
	switch {
	case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
		return "darwin", nil
	case runtime.GOOS == "darwin" && runtime.GOARCH == "amd64":
		return "amd", nil
	case runtime.GOOS == "linux" && runtime.GOARCH == "amd64":
		return "linux", nil
	case runtime.GOOS == "linux" && runtime.GOARCH == "arm64":
		return "linux-arm64", nil
	}
	return "", fmt.Errorf("%w: %s/%s", errNoTarget, runtime.GOOS, runtime.GOARCH)
}

// maxManifestSize caps what we are willing to read from the manifest URL. The
// real document is a couple of hundred bytes; the cap is what keeps a
// misrouted URL that happens to serve something enormous from being read into
// memory at launch.
const maxManifestSize = 64 << 10

// fetchManifest reads and validates the published manifest.
func fetchManifest(ctx context.Context, client *http.Client, baseURL string) (manifest, error) {
	url := strings.TrimSuffix(baseURL, "/") + "/" + manifestName
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return manifest{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return manifest{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// A 404 is the ordinary reading here, not an anomaly: it is what a
		// bucket published before manifests existed returns, and the right
		// response to it is to offer nothing.
		return manifest{}, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	var m manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxManifestSize)).Decode(&m); err != nil {
		return manifest{}, fmt.Errorf("decode %s: %w", url, err)
	}
	if m.Version == "" {
		return manifest{}, fmt.Errorf("%s names no version", url)
	}
	return m, nil
}

// release turns a manifest into the Release for this platform, or reports why
// there isn't one.
func (m manifest) release(baseURL string) (Release, error) {
	dir, err := targetDir()
	if err != nil {
		return Release{}, err
	}
	target, ok := m.Targets[dir]
	if !ok {
		return Release{}, fmt.Errorf("%w: manifest has no %q target", errNoTarget, dir)
	}
	// An entry with no digest is refused rather than downloaded unverified.
	// This is the one place the package chooses to do nothing over doing the
	// convenient thing: the file we are about to fetch gets exec'd as the user
	// moments later, and HTTPS alone attests to the CDN, not to the build.
	if target.SHA256 == "" {
		return Release{}, fmt.Errorf("manifest target %q carries no sha256", dir)
	}
	return Release{
		Version: m.Version,
		URL:     strings.TrimSuffix(baseURL, "/") + "/" + dir + "/jms",
		SHA256:  strings.ToLower(target.SHA256),
	}, nil
}
