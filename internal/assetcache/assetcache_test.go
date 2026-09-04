package assetcache

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
)

// isolateCacheDir points os.UserCacheDir at a temp directory. Both vars are
// set because the lookup differs by platform: macOS derives it from HOME,
// other Unixes read XDG_CACHE_HOME.
func isolateCacheDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "Library", "Caches"))
}

var (
	testNodes  = []jmsd.NodeTree{{ID: "n1", Name: "DEFAULT"}}
	testAssets = []jmsd.AssetRow{{ID: "a1", Name: "web-01", Address: "10.0.0.1",
		Category: "host", NodeIDs: []string{"n1"}}}
)

func TestSaveThenLoad(t *testing.T) {
	isolateCacheDir(t)
	if err := Save("srv", "alice", testNodes, testAssets); err != nil {
		t.Fatal(err)
	}
	entry := Load("srv", "alice", DefaultTTL)
	if entry == nil {
		t.Fatal("Load returned nil right after Save")
	}
	if len(entry.Assets) != 1 || entry.Assets[0].Name != "web-01" {
		t.Errorf("assets = %+v", entry.Assets)
	}
	if len(entry.Nodes) != 1 || entry.Nodes[0].Name != "DEFAULT" {
		t.Errorf("nodes = %+v", entry.Nodes)
	}
	if entry.Age() > time.Minute {
		t.Errorf("Age = %v, want ~0", entry.Age())
	}
}

func TestLoadMissIsNotAnError(t *testing.T) {
	isolateCacheDir(t)
	if entry := Load("srv", "nobody", DefaultTTL); entry != nil {
		t.Errorf("Load on an empty cache dir = %+v, want nil", entry)
	}
}

// Past the TTL the entry must be ignored, so "r"-less startups still pick up
// permission changes eventually.
func TestLoadRespectsTTL(t *testing.T) {
	isolateCacheDir(t)
	if err := Save("srv", "alice", testNodes, testAssets); err != nil {
		t.Fatal(err)
	}
	if entry := Load("srv", "alice", time.Nanosecond); entry != nil {
		t.Error("an entry older than the TTL was served")
	}
	if entry := Load("srv", "alice", time.Hour); entry == nil {
		t.Error("a fresh entry was rejected")
	}
}

// A zero or negative TTL is the off switch.
func TestZeroTTLDisablesCache(t *testing.T) {
	isolateCacheDir(t)
	if err := Save("srv", "alice", testNodes, testAssets); err != nil {
		t.Fatal(err)
	}
	if entry := Load("srv", "alice", 0); entry != nil {
		t.Error("TTL 0 should disable the cache")
	}
	if entry := Load("srv", "alice", -time.Hour); entry != nil {
		t.Error("a negative TTL should disable the cache")
	}
}

// The security-relevant one: two users on one machine have different
// permitted assets, so one must never be served the other's listing.
func TestUsersDoNotShareCache(t *testing.T) {
	isolateCacheDir(t)
	if err := Save("srv", "alice", testNodes, testAssets); err != nil {
		t.Fatal(err)
	}
	if entry := Load("srv", "bob", DefaultTTL); entry != nil {
		t.Fatalf("bob was served alice's cached inventory: %+v", entry.Assets)
	}

	bobAssets := []jmsd.AssetRow{{ID: "b1", Name: "bob-only"}}
	if err := Save("srv", "bob", nil, bobAssets); err != nil {
		t.Fatal(err)
	}
	alice := Load("srv", "alice", DefaultTTL)
	if alice == nil || len(alice.Assets) != 1 || alice.Assets[0].Name != "web-01" {
		t.Errorf("alice's entry was clobbered by bob's: %+v", alice)
	}
}

// The same person on two JumpServers has two different inventories, and asset
// IDs from one mean nothing on the other.
// The source is the jmsd address: aiming --jmsd-addr at a local build while
// leaving --server alone used to hand production's inventory to the dev
// daemon's user and back again, because only --server was in the key.
func TestSourcesDoNotShareCache(t *testing.T) {
	isolateCacheDir(t)
	if err := Save("alpha", "alice", testNodes, testAssets); err != nil {
		t.Fatal(err)
	}
	if entry := Load("beta", "alice", DefaultTTL); entry != nil {
		t.Errorf("server beta was served alpha's cache: %+v", entry.Assets)
	}
}

// A file whose contents don't match the requested identity must be rejected,
// not trusted on the strength of its filename alone.
func TestLoadRejectsMismatchedIdentity(t *testing.T) {
	isolateCacheDir(t)
	if err := Save("srv", "alice", testNodes, testAssets); err != nil {
		t.Fatal(err)
	}
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected exactly one cache file, got %v (err %v)", entries, err)
	}
	path := filepath.Join(dir, entries[0].Name())

	// Rewrite the body as if it belonged to somebody else.
	body := `{"source":"srv","username":"mallory","saved_at":"` +
		time.Now().Format(time.RFC3339) + `","nodes":[],"assets":[]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if entry := Load("srv", "alice", DefaultTTL); entry != nil {
		t.Error("a cache file naming a different user was accepted")
	}
}

func TestLoadToleratesCorruptFile(t *testing.T) {
	isolateCacheDir(t)
	if err := Save("srv", "alice", testNodes, testAssets); err != nil {
		t.Fatal(err)
	}
	dir, _ := Dir()
	entries, _ := os.ReadDir(dir)
	path := filepath.Join(dir, entries[0].Name())
	if err := os.WriteFile(path, []byte("{{{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if entry := Load("srv", "alice", DefaultTTL); entry != nil {
		t.Error("a corrupt cache file was served")
	}
	// And a later Save must be able to replace it.
	if err := Save("srv", "alice", testNodes, testAssets); err != nil {
		t.Errorf("Save over a corrupt file failed: %v", err)
	}
	if Load("srv", "alice", DefaultTTL) == nil {
		t.Error("cache did not recover after being overwritten")
	}
}

func TestSaveIsPrivateAndLeavesNoTempFile(t *testing.T) {
	isolateCacheDir(t)
	if err := Save("srv", "alice", testNodes, testAssets); err != nil {
		t.Fatal(err)
	}
	dir, _ := Dir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("cache dir holds %v, want a single file (a .tmp means the rename didn't happen)", names)
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	// The file lists every machine this person can reach; keep it to them.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("cache file mode = %o, want 600", perm)
	}
}

// Saving twice replaces the entry rather than accumulating files.
func TestSaveOverwrites(t *testing.T) {
	isolateCacheDir(t)
	if err := Save("srv", "alice", testNodes, testAssets); err != nil {
		t.Fatal(err)
	}
	updated := []jmsd.AssetRow{{ID: "a2", Name: "web-02"}}
	if err := Save("srv", "alice", testNodes, updated); err != nil {
		t.Fatal(err)
	}
	entry := Load("srv", "alice", DefaultTTL)
	if entry == nil || len(entry.Assets) != 1 || entry.Assets[0].Name != "web-02" {
		t.Errorf("entry = %+v, want the second save to win", entry)
	}
	dir, _ := Dir()
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("cache dir holds %d files, want 1", len(entries))
	}
}
