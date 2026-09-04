package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// The comparison is the whole safety story: it is what decides whether jms
// overwrites the binary the user is running. The cases that matter are the ones
// where the answer must be "leave it alone" -- an equal version, a local build
// that is ahead, and anything that does not parse.
func TestIsNewer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name             string
		published, local string
		want             bool
		wantErr          bool
	}{
		{name: "published ahead", published: "0.1.3", local: "0.1.2", want: true},
		{name: "equal offers nothing", published: "0.1.2", local: "0.1.2"},
		{
			// A build made from source after the version was bumped for the
			// next release. This is the case that must never offer to overwrite
			// a developer's binary with the older published one.
			name: "local ahead offers nothing", published: "0.1.2", local: "0.1.3",
		},
		{name: "minor beats patch", published: "0.2.0", local: "0.1.9", want: true},
		{name: "major beats minor", published: "1.0.0", local: "0.9.9", want: true},
		{
			// Ordering is per component, not lexicographic: "0.1.10" sorts
			// before "0.1.9" as text and after it as a version.
			name: "double digits compare numerically", published: "0.1.10", local: "0.1.9", want: true,
		},
		{name: "prerelease suffix does not parse", published: "0.2.0-dev", local: "0.1.2", wantErr: true},
		{name: "two components do not parse", published: "0.2", local: "0.1.2", wantErr: true},
		{name: "empty published does not parse", published: "", local: "0.1.2", wantErr: true},
		{
			// Atoi would take "+1" and "-1", and a negative component would
			// sort a malformed version ahead of a real one -- offering an
			// overwrite off the back of a typo.
			name: "signed component does not parse", published: "0.1.+3", local: "0.1.2", wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := isNewer(tc.published, tc.local)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("isNewer(%q, %q) = %v, want an error", tc.published, tc.local, got)
				}
				// An error must never also claim "newer": callers read the
				// bool, and a true here would be an update offered on the
				// strength of a version nobody could parse.
				if got {
					t.Errorf("isNewer(%q, %q) reported newer alongside its error", tc.published, tc.local)
				}
				return
			}
			if err != nil {
				t.Fatalf("isNewer(%q, %q): %v", tc.published, tc.local, err)
			}
			if got != tc.want {
				t.Errorf("isNewer(%q, %q) = %v, want %v", tc.published, tc.local, got, tc.want)
			}
		})
	}
}

// publishedTarget is the CDN directory this test machine's build would fetch
// from, so the served manifest covers the platform the test is running on.
func publishedTarget(t *testing.T) string {
	t.Helper()
	dir, err := targetDir()
	if err != nil {
		t.Skipf("no published build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return dir
}

// serveManifest stands in for the CDN, returning body for manifest.json and 404
// for everything else.
func serveManifest(t *testing.T, body string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+manifestName {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// isolateCacheDir points os.UserCacheDir at a fresh directory, so the skip file
// this test writes is not the developer's own.
func isolateCacheDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
}

func TestCheck(t *testing.T) {
	target := publishedTarget(t)
	manifestFor := func(version, sha string) string {
		return fmt.Sprintf(`{"version":%q,"targets":{%q:{"sha256":%q}}}`, version, target, sha)
	}
	const sha = "aa" // never downloaded in these cases, only carried through.

	for _, tc := range []struct {
		name     string
		manifest string
		local    string
		want     string // the version Check should offer; "" for nothing.
	}{
		{name: "newer is offered", manifest: manifestFor("0.1.3", sha), local: "0.1.2", want: "0.1.3"},
		{name: "same version offers nothing", manifest: manifestFor("0.1.2", sha), local: "0.1.2"},
		{name: "older CDN offers nothing", manifest: manifestFor("0.1.1", sha), local: "0.1.2"},
		{
			// The refusal that matters most: this file is about to be executed
			// as the user, and HTTPS attests to the CDN rather than to the
			// build. No digest means no update, not an unverified one.
			name: "target without a digest offers nothing", manifest: manifestFor("0.1.3", ""), local: "0.1.2",
		},
		{
			// A bucket published before manifests existed. Every launch would
			// hit this, so it has to be quiet rather than an error the user
			// sees.
			name: "manifest naming no version offers nothing", manifest: `{"targets":{}}`, local: "0.1.2",
		},
		{name: "unparseable manifest offers nothing", manifest: `not json`, local: "0.1.2"},
		{
			name:     "manifest missing this platform offers nothing",
			manifest: `{"version":"0.1.3","targets":{"solaris-vax":{"sha256":"aa"}}}`,
			local:    "0.1.2",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateCacheDir(t)
			base := serveManifest(t, tc.manifest)
			got, err := Check(context.Background(), base, tc.local)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("Check offered %+v, want nothing (err=%v)", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if got == nil {
				t.Fatal("Check offered nothing, want " + tc.want)
			}
			if got.Version != tc.want {
				t.Errorf("Check offered %q, want %q", got.Version, tc.want)
			}
			if !strings.HasSuffix(got.URL, "/"+target+"/jms") {
				t.Errorf("URL = %q, want it to end in /%s/jms", got.URL, target)
			}
		})
	}
}

// A skip has to survive the launch that recorded it and no further than the
// version it named. Silencing the next release too would opt the user out of
// updates permanently, which is not what the key they pressed promised.
func TestSkipSilencesOnlyTheSkippedVersion(t *testing.T) {
	isolateCacheDir(t)
	target := publishedTarget(t)
	manifest := fmt.Sprintf(`{"version":"0.1.3","targets":{%q:{"sha256":"aa"}}}`, target)
	base := serveManifest(t, manifest)

	release, err := Check(context.Background(), base, "0.1.2")
	if err != nil || release == nil {
		t.Fatalf("Check offered nothing before the skip (err=%v)", err)
	}
	if err := Skip(release.Version); err != nil {
		t.Fatalf("Skip: %v", err)
	}
	if got, _ := Check(context.Background(), base, "0.1.2"); got != nil {
		t.Fatalf("Check offered %+v after it was skipped", got)
	}

	// The next release must get through.
	next := fmt.Sprintf(`{"version":"0.1.4","targets":{%q:{"sha256":"aa"}}}`, target)
	if got, err := Check(context.Background(), serveManifest(t, next), "0.1.2"); got == nil {
		t.Fatalf("skipping 0.1.3 also silenced 0.1.4 (err=%v)", err)
	}
}

// serveBinary returns a base URL serving body at /<target>/jms, plus body's
// digest.
func serveBinary(t *testing.T, target, body string) (base, sha string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+target+"/jms" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	sum := sha256.Sum256([]byte(body))
	return server.URL, hex.EncodeToString(sum[:])
}

func TestApplyReplacesTheBinary(t *testing.T) {
	target := publishedTarget(t)
	base, sha := serveBinary(t, target, "the new jms")

	dir := t.TempDir()
	exe := filepath.Join(dir, "jms")
	if err := os.WriteFile(exe, []byte("the old jms"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := apply(context.Background(), &Release{
		Version: "0.1.3", URL: base + "/" + target + "/jms", SHA256: sha,
	}, exe)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got != exe {
		t.Errorf("apply returned %q, want the replaced path %q", got, exe)
	}
	content, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "the new jms" {
		t.Errorf("binary = %q, want it replaced", content)
	}
	// Downloaded 0600 by CreateTemp; it has to be executable to be worth
	// renaming into place.
	info, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("mode = %v, want the replacement executable", info.Mode().Perm())
	}
	assertNoLeftovers(t, dir)
}

// The failure that must not cost the user their jms. A digest that does not
// match means the download is discarded and the working binary is still there
// -- anything else and a corrupted transfer takes away the tool needed to fix
// it.
func TestApplyRefusesAWrongDigestAndKeepsTheOldBinary(t *testing.T) {
	target := publishedTarget(t)
	base, _ := serveBinary(t, target, "not what the manifest describes")

	dir := t.TempDir()
	exe := filepath.Join(dir, "jms")
	if err := os.WriteFile(exe, []byte("the old jms"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := apply(context.Background(), &Release{
		Version: "0.1.3",
		URL:     base + "/" + target + "/jms",
		SHA256:  strings.Repeat("00", sha256.Size),
	}, exe)
	if err == nil {
		t.Fatal("apply accepted a binary whose digest did not match the manifest")
	}
	content, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("the old binary is gone: %v", err)
	}
	if string(content) != "the old jms" {
		t.Errorf("binary = %q, want the old one untouched", content)
	}
	assertNoLeftovers(t, dir)
}

// A refused or failed update must not leave a half-downloaded file next to the
// binary. They accumulate silently -- 13MB at a time, in a directory nobody
// lists.
func assertNoLeftovers(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".jms-update-") {
			t.Errorf("left a partial download behind: %s", entry.Name())
		}
	}
}

// The loop this breaks is the worst failure the package can produce: a manifest
// naming a version the published binary does not actually report leaves update,
// restart and re-offer repeating forever, and jms can never be launched again.
// The error has to be distinguishable, because it is the one check failure the
// user has to be told about rather than have swallowed.
func TestCheckRefusesToOfferTheVersionItJustInstalled(t *testing.T) {
	isolateCacheDir(t)
	target := publishedTarget(t)
	base := serveManifest(t, fmt.Sprintf(`{"version":"0.1.3","targets":{%q:{"sha256":"aa"}}}`, target))

	t.Setenv(envUpdatedTo, "0.1.3")
	got, err := Check(context.Background(), base, "0.1.2")
	if got != nil {
		t.Fatalf("Check offered %+v again after installing it", got)
	}
	if !errors.Is(err, ErrAlreadyUpdated) {
		t.Fatalf("err = %v, want ErrAlreadyUpdated so the caller can say so out loud", err)
	}

	// A stale variable left in a long-lived shell must not silence a genuinely
	// different version.
	next := fmt.Sprintf(`{"version":"0.1.4","targets":{%q:{"sha256":"aa"}}}`, target)
	if got, err := Check(context.Background(), serveManifest(t, next), "0.1.2"); got == nil {
		t.Fatalf("a stale %s silenced 0.1.4 (err=%v)", envUpdatedTo, err)
	}
}

// Restart hands the installed version to the process it execs into, which is
// the only channel the loop-breaker above has. Two entries would leave which
// one os.Getenv resolves to up to the platform.
func TestWithUpdatedToReplacesRatherThanAccumulates(t *testing.T) {
	t.Parallel()
	env := withUpdatedTo([]string{"PATH=/bin", envUpdatedTo + "=0.1.2", "HOME=/tmp"}, "0.1.3")

	var seen []string
	for _, entry := range env {
		if strings.HasPrefix(entry, envUpdatedTo+"=") {
			seen = append(seen, entry)
		}
	}
	if len(seen) != 1 || seen[0] != envUpdatedTo+"=0.1.3" {
		t.Errorf("%s entries = %v, want exactly [%s=0.1.3]", envUpdatedTo, seen, envUpdatedTo)
	}
	// The rest of the environment has to survive: this is what the restarted
	// process inherits, and jms reads HOME for its keys and its cache.
	for _, want := range []string{"PATH=/bin", "HOME=/tmp"} {
		if !slices.Contains(env, want) {
			t.Errorf("env = %v, want it to keep %q", env, want)
		}
	}
}
