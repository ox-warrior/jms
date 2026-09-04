// Package assetcache keeps a local copy of the two things jms fetches at
// startup -- the permission node tree and the full permitted asset list -- so
// a relaunch inside the TTL opens instantly instead of waiting on Core's
// permission expansion.
//
// This is a cache in the strict sense: every entry is reconstructible by
// asking jmsd again, nothing here is authoritative, and deleting the
// directory only costs one slow launch. Favourites and recent history are
// deliberately NOT cached -- they live in Core and are cheap to read, and a
// stale "recent" list is worse than a slow one.
package assetcache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
	"github.com/leiyupeng/jumpserver-cli/internal/jmslog"
)

// DefaultTTL is how long a cached listing is served before jms re-reads it
// from Core.
const DefaultTTL = 12 * time.Hour

// Entry is one cached listing, for one user on one source.
type Entry struct {
	// Source and Username are stored so a load can verify the file really
	// belongs to this caller rather than trusting the filename alone.
	//
	// Source is the jmsd address, since that is who answered. It was the
	// JumpServer host until it turned out the two are not the same key:
	// aiming --jmsd-addr at a local build without touching --server made both
	// daemons share one file.
	Source   string    `json:"source"`
	Username string    `json:"username"`
	SavedAt  time.Time `json:"saved_at"`

	Nodes  []jmsd.NodeTree `json:"nodes"`
	Assets []jmsd.AssetRow `json:"assets"`
}

// Age reports how long ago the entry was written.
func (e *Entry) Age() time.Duration { return time.Since(e.SavedAt) }

// Dir is where cache files live: <user cache dir>/jms.
func Dir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate cache dir: %w", err)
	}
	return filepath.Join(dir, "jms"), nil
}

// pathFor names the file for one (source, username) pair. Both are hashed
// rather than used directly: a username is an email address here, and
// source/username are attacker-influenced enough that building a path out of
// them invites traversal. The pair is also what makes the key correct --
// two people on one machine have different permitted assets, and the same
// person has different assets on different jmsd instances.
func pathFor(source, username string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(source + "\x00" + username))
	return filepath.Join(dir, "assets-"+hex.EncodeToString(sum[:8])+".json"), nil
}

// Load returns the cached listing for this user if one exists and is younger
// than ttl. A miss -- no file, unreadable, corrupt, expired, or belonging to
// somebody else -- returns nil with no error: the caller's fallback is simply
// to fetch, which is always correct.
//
// A ttl of zero or less disables the cache entirely.
//
// Which of those misses happened is logged rather than returned. The caller
// genuinely does not care -- it fetches either way -- but the six of them are
// not equally benign: an expired entry is the cache working, while a file
// that is permanently unreadable (wrong owner, wrong mode, failing disk)
// presents as nothing but a slow launch, every single time, forever.
func Load(source, username string, ttl time.Duration) *Entry {
	if ttl <= 0 {
		jmslog.Debugf("asset cache: disabled (ttl=%s)", ttl)
		return nil
	}
	path, err := pathFor(source, username)
	if err != nil {
		jmslog.Printf("asset cache: miss, cannot locate cache dir: %v", err)
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			jmslog.Debugf("asset cache: miss, no file at %s", path)
		} else {
			jmslog.Printf("asset cache: miss, %s unreadable: %v", path, err)
		}
		return nil
	}
	var entry Entry
	if err := json.Unmarshal(data, &entry); err != nil {
		jmslog.Printf("asset cache: miss, %s is corrupt: %v", path, err)
		return nil
	}
	// Guards against a hash collision or a hand-edited file being served to
	// the wrong person -- the consequence would be showing someone another
	// user's inventory.
	if entry.Source != source || entry.Username != username {
		jmslog.Printf("asset cache: miss, %s belongs to somebody else", path)
		return nil
	}
	if entry.Age() > ttl {
		jmslog.Debugf("asset cache: miss, %s is %s old (ttl %s)", path, entry.Age().Round(time.Second), ttl)
		return nil
	}
	jmslog.Debugf("asset cache: hit, %d assets %s old", len(entry.Assets), entry.Age().Round(time.Second))
	return &entry
}

// Save writes the listing, replacing any previous one for this user. Errors
// are worth surfacing but never fatal: a cache that fails to write costs a
// slow launch next time and nothing else.
func Save(source, username string, nodes []jmsd.NodeTree, assets []jmsd.AssetRow) error {
	path, err := pathFor(source, username)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}
	data, err := json.Marshal(Entry{
		Source: source, Username: username, SavedAt: time.Now(),
		Nodes: nodes, Assets: assets,
	})
	if err != nil {
		return err
	}
	// Temp file plus rename, so an interrupted write can't leave a truncated
	// file that the next Load throws away as corrupt.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write cache: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace cache: %w", err)
	}
	return nil
}
