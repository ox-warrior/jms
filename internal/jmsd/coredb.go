package jmsd

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/lib/pq"
)

// CoreDB is a read-only connection to JumpServer Core's PostgreSQL database.
// jmsd never writes to it. All it reads is who is active: a Google sign-in
// names a person by email, and users_user is where that email meets a
// JumpServer user id.
type CoreDB struct {
	db *sql.DB
}

// userRecord is one active users_user row.
type userRecord struct {
	ID       string
	Username string
	// Email is users_user.email, which a Google sign-in is matched against.
	Email string
	// PublicKeys is filled in per request by Server.identify with the one key
	// that may sign for this caller (a bound key or a service key). Core's own
	// authentication_sshkey table is not read.
	PublicKeys []string
	// Service marks a caller identified by a service key (config
	// service_keys) rather than a Google-bound key; set by Server.identify.
	Service bool
	// OnBehalfOf is who a service key said it is acting for, already
	// verified as signed (Server.authorize). Recorded, never authorised on.
	OnBehalfOf string
}

// NewCoreDB opens a connection to Core's database. dsn is a lib/pq DSN, e.g.
// "postgres://user:pass@host:5432/jumpserver?sslmode=disable".
func NewCoreDB(dsn string) (*CoreDB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open core db: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping core db: %w", err)
	}
	return &CoreDB{db: db}, nil
}

func (c *CoreDB) Close() error {
	return c.db.Close()
}

// activeUsers reads every active user. Filtering on is_active here is what
// makes disabling someone in JumpServer cut off their bound keys: identify
// can no longer find the user the binding points at.
func (c *CoreDB) activeUsers() ([]userRecord, error) {
	// Bounded: userDirectory holds its lock across this, so a hung Core
	// database would otherwise stall every request jmsd is serving.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := c.db.QueryContext(ctx, `SELECT id, username, email FROM users_user WHERE is_active = true`)
	if err != nil {
		return nil, fmt.Errorf("scan users: %w", err)
	}
	defer rows.Close()
	var users []userRecord
	for rows.Next() {
		var (
			u     userRecord
			email sql.NullString
		)
		if err := rows.Scan(&u.ID, &u.Username, &email); err != nil {
			return nil, fmt.Errorf("scan users: %w", err)
		}
		u.Email = email.String
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan users: %w", err)
	}
	return users, nil
}
