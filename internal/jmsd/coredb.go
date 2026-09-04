package jmsd

import (
	"database/sql"
	"fmt"

	_ "github.com/lib/pq"
)

// CoreDB is a read-only connection to JumpServer Core's PostgreSQL database.
// jmsd never writes to it. It exists because the REST API can't answer the
// two questions hop① needs -- see docs/PLAN.md §7.2.
type CoreDB struct {
	db  *sql.DB
	key []byte // derived from Config.SecretKey, see crypto.go
}

// userRecord is one users_user row plus that user's active SSH keys, with
// every EncryptField column already decrypted.
type userRecord struct {
	ID       string
	Username string
	// PublicKeys holds every active key from authentication_sshkey, in
	// OpenSSH authorized_keys format. A user may register several, so a
	// signature counts as valid if any one of them verifies it.
	PublicKeys []string
	// OTPSecretKey is a base32 TOTP secret, empty if the user has no MFA
	// device enrolled.
	OTPSecretKey string
}

// NewCoreDB opens a connection to Core's database. dsn is a lib/pq DSN, e.g.
// "postgres://user:pass@host:5432/jumpserver?sslmode=disable".
func NewCoreDB(dsn, secretKey string) (*CoreDB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open core db: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping core db: %w", err)
	}
	return &CoreDB{db: db, key: deriveKey(secretKey)}, nil
}

func (c *CoreDB) Close() error {
	return c.db.Close()
}

func (c *CoreDB) decrypt(value string) (string, error) {
	return decryptField(value, c.key)
}

// activeSSHKeys reads and decrypts every active SSH key in the install, with
// its owning user. This is how a caller is identified by their key instead of
// a typed username -- the whole point being that the user never types
// anything.
//
// It is a full scan by necessity, not by choice. public_key is stored per-row
// as AES-GCM with a random nonce (see decryptField), so identical plaintexts
// never produce identical ciphertext; there is no indexed or WHERE-based
// lookup available, and every active key has to be decrypted and hashed to
// find a match. At production scale that is 22 keys in single-digit
// milliseconds -- cheap enough per call, but far too expensive to pay on
// every HTTP request, which is why userDirectory caches the result rather
// than calling this directly from the request path (see keycache.go).
//
// Since one scan sees every active key with its owner, whole userRecords can
// be assembled from it and there is no second lookup by username. Keys
// deliberately do not come from users_user.public_key: that column is legacy.
// Migration authentication/0003_sshkey moved keys into their own table so a
// user can hold more than one, and anyone who registered a key through the
// current profile page has it only there. JumpServer's own
// User.check_public_key() agrees -- it iterates ssh_keys.filter(is_active=True)
// and never reads self.public_key (docs/PLAN.md §5.19).
func (c *CoreDB) activeSSHKeys() ([]sshKeyRow, error) {
	rows, err := c.db.Query(
		`SELECT u.id, u.username, u.otp_secret_key, k.public_key
		 FROM authentication_sshkey k
		 JOIN users_user u ON u.id = k.user_id
		 WHERE k.is_active = true`,
	)
	if err != nil {
		return nil, fmt.Errorf("scan ssh keys: %w", err)
	}
	defer rows.Close()

	var candidates []sshKeyRow
	for rows.Next() {
		var (
			row                           sshKeyRow
			encOTPSecretKey, encPublicKey sql.NullString
		)
		if err := rows.Scan(&row.userID, &row.username, &encOTPSecretKey, &encPublicKey); err != nil {
			return nil, fmt.Errorf("scan ssh keys: %w", err)
		}
		if encPublicKey.String == "" {
			continue
		}
		if row.publicKey, err = c.decrypt(encPublicKey.String); err != nil {
			continue // one undecryptable row shouldn't fail every lookup
		}
		if encOTPSecretKey.String != "" {
			// Skipped, not fatal -- the same rule the public_key branch above
			// follows, and for a sharper reason. This scan walks EVERY active
			// key in the install, so returning here let one user's
			// undecryptable otp_secret_key (a rotated SECRET_KEY, a legacy or
			// hand-edited value) fail the lookup for EVERYBODY: every caller
			// got a 401 naming that one unrelated user, pointing the
			// investigation at the wrong person entirely.
			//
			// The field is only ever read by handleMFA, so losing it costs
			// this one user their MFA step and nothing else -- and it fails
			// closed: an empty secret makes handleMFA refuse the code rather
			// than accept it.
			secret, decryptErr := c.decrypt(encOTPSecretKey.String)
			if decryptErr != nil {
				debugf("decrypt otp_secret_key for %s: %v (that user cannot pass MFA until it is fixed)",
					row.username, decryptErr)
			} else {
				row.otpSecretKey = secret
			}
		}
		candidates = append(candidates, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan ssh keys: %w", err)
	}
	return candidates, nil
}

// collectUser folds every row belonging to userID into one record. A user may
// have registered several keys and a signature counts as valid if any one of
// them verifies it, so they all come along -- not just the one that matched
// the fingerprint.
func collectUser(rows []sshKeyRow, userID string) *userRecord {
	user := &userRecord{ID: userID}
	for _, row := range rows {
		if row.userID != userID {
			continue
		}
		user.Username = row.username
		user.OTPSecretKey = row.otpSecretKey
		user.PublicKeys = append(user.PublicKeys, row.publicKey)
	}
	return user
}

// sshKeyRow is one decrypted authentication_sshkey row, joined with its
// owning user. Kept separate from userRecord because a single scan sees one
// row per key, several of which may belong to the same user -- so the user
// columns repeat across rows.
type sshKeyRow struct {
	userID, username, otpSecretKey, publicKey string
}
