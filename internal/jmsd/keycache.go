package jmsd

import (
	"fmt"
	"slices"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// keyIndexTTL is how long a built SSH key index is served before Core's
// database is read again.
//
// The number trades two things off. Long is cheap: a forged request costs a
// map lookup instead of a full authentication_sshkey scan. Short bounds
// staleness, and staleness cuts both ways -- a key registered a moment ago
// won't be accepted yet, and, the direction that actually matters, a key
// deactivated a moment ago is still accepted. 30 seconds keeps that window
// smaller than the nonce/timestamp window an attacker already has to work
// inside, while making the scan effectively per-minute rather than
// per-request.
const keyIndexTTL = 30 * time.Second

// sshKeyIndex is a snapshot of every active SSH key in the install with the
// fingerprint work already done: parsing a stored key and hashing it are the
// per-request costs this exists to remove, so they happen once per refresh
// instead of once per caller.
type sshKeyIndex struct {
	// rows is the snapshot the records are assembled from, kept so
	// collectUser can gather every key belonging to the matched user.
	rows []sshKeyRow
	// owners maps a fingerprint to the DISTINCT user ids that registered that
	// key. A slice, not a single id, because more than one entry is the case
	// that must be refused rather than resolved -- see lookupUserID.
	owners map[string][]string
}

// buildSSHKeyIndex precomputes the fingerprint of every parseable key in rows.
//
// A key that will not parse is skipped rather than fatal: one malformed stored
// key must not make everybody else unidentifiable.
//
// Owners are deduplicated per fingerprint, so a user who registered the same
// key twice stays one owner. JumpServer places no uniqueness constraint on
// public_key (impossible anyway given AES-GCM's random nonce, which makes
// every ciphertext for the same plaintext look different -- see decryptField),
// so two DIFFERENT users can end up holding the same key, by mistake or
// otherwise. That case has to survive the build intact for lookupUserID to
// refuse it.
func buildSSHKeyIndex(rows []sshKeyRow) *sshKeyIndex {
	index := &sshKeyIndex{rows: rows, owners: make(map[string][]string)}
	for _, row := range rows {
		pubKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(row.publicKey))
		if err != nil {
			continue // one unparseable stored key shouldn't fail the whole lookup
		}
		fingerprint := ssh.FingerprintSHA256(pubKey)
		if !slices.Contains(index.owners[fingerprint], row.userID) {
			index.owners[fingerprint] = append(index.owners[fingerprint], row.userID)
		}
	}
	return index
}

// lookupUserID resolves a fingerprint to the one user who registered it.
//
// It deliberately refuses a fingerprint held by more than one distinct user
// instead of picking one: choosing arbitrarily would authenticate a caller as
// whichever of them happened to sort first.
func (i *sshKeyIndex) lookupUserID(fingerprint string) (string, error) {
	owners := i.owners[fingerprint]
	switch len(owners) {
	case 0:
		return "", fmt.Errorf("no JumpServer user has this SSH key registered")
	case 1:
		return owners[0], nil
	default:
		return "", fmt.Errorf("this SSH key is registered to more than one JumpServer user")
	}
}

// userDirectory answers "whose key is this?" from a cached index, refreshing it
// from Core's database at most once per ttl.
//
// The cache is the point, not an optimisation. Identifying a caller requires
// the full scan in activeSSHKeys, and that scan used to run BEFORE the
// signature was checked -- there is no way around that ordering, since you
// cannot verify a signature without first knowing whose public keys to try.
// The consequence was that anyone who could reach the port could make jmsd
// run a full-table query plus a per-row AES-GCM decrypt against Core's
// production database by sending a request with a garbage signature: a cheap
// request buying an expensive one, aimed at the database JumpServer itself
// depends on. With the index cached and its fingerprints precomputed, a
// forged request costs one map lookup and the flood has nothing to amplify.
type userDirectory struct {
	// load is activeSSHKeys in production, injected so the caching and
	// single-flight behaviour can be tested without a database.
	load func() ([]sshKeyRow, error)
	ttl  time.Duration
	// now is time.Now in production, injected so expiry is testable without
	// sleeping.
	now func() time.Time

	// mu guards index/builtAt AND is held across the refresh, which is what
	// makes the refresh single-flight: a burst arriving on a cold or expired
	// cache produces one query with the rest waiting on it, rather than one
	// query per request -- exactly the stampede this type exists to prevent.
	mu      sync.Mutex
	index   *sshKeyIndex
	builtAt time.Time
}

func newUserDirectory(load func() ([]sshKeyRow, error), ttl time.Duration) *userDirectory {
	return &userDirectory{load: load, ttl: ttl, now: time.Now}
}

// Lookup resolves a caller's SSH key fingerprint to their JumpServer user
// record. fingerprint is the OpenSSH SHA256 form (ssh.FingerprintSHA256),
// which is what the client sends: a hash of a public key, not a secret, so
// nothing sensitive crosses the wire in the clear.
func (d *userDirectory) Lookup(fingerprint string) (*userRecord, error) {
	index, err := d.currentIndex()
	if err != nil {
		return nil, err
	}
	userID, err := index.lookupUserID(fingerprint)
	if err != nil {
		return nil, err
	}
	return collectUser(index.rows, userID), nil
}

// currentIndex returns the cached index, rebuilding it when it has expired.
//
// A failed refresh is returned to the caller rather than papered over with the
// previous index: serving a stale snapshot through a database outage would
// keep deactivated keys working for as long as the outage lasted, with nothing
// bounding it. Failing is the same behaviour a lookup had before any of this
// was cached.
func (d *userDirectory) currentIndex() (*sshKeyIndex, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.index != nil && d.now().Sub(d.builtAt) < d.ttl {
		return d.index, nil
	}
	started := d.now()
	rows, err := d.load()
	if err != nil {
		return nil, err
	}
	d.index = buildSSHKeyIndex(rows)
	d.builtAt = d.now()
	// Logged because it is the one place the cache is observable: "is jmsd
	// still seeing the key I just added" is answered by this line's timestamp
	// and count, and nothing else reveals either.
	debugf("ssh key index: refreshed %d keys (%d distinct fingerprints) in %s",
		len(rows), len(d.index.owners), d.builtAt.Sub(started).Round(time.Millisecond))
	return d.index, nil
}
