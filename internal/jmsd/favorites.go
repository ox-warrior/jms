package jmsd

import (
	"database/sql"
	"fmt"

	"github.com/google/uuid"
)

// RecentLimit caps how many distinct assets the recent list reports.
const RecentLimit = 50

// Favorites lists the asset IDs a user has starred, read straight from
// Core's assets_favoriteasset table.
//
// This is deliberately not the REST API: /api/v1/assets/favorite-assets/ is
// scoped to whoever holds the access key -- that is, jmsd's own service
// account -- and ignores a user_id/user filter (verified against v4.10.16),
// so it cannot answer "what has THIS user starred". The table can.
//
// Reading Core's own table rather than keeping a private list means stars set
// in the JumpServer web console show up here and vice versa.
func (c *CoreDB) Favorites(userID string) ([]string, error) {
	rows, err := c.db.Query(
		`SELECT asset_id::text FROM assets_favoriteasset WHERE user_id = $1::uuid`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list favorites: %w", err)
	}
	defer rows.Close()

	assetIDs := []string{}
	for rows.Next() {
		var assetID string
		if err := rows.Scan(&assetID); err != nil {
			return nil, fmt.Errorf("list favorites: %w", err)
		}
		assetIDs = append(assetIDs, assetID)
	}
	return assetIDs, rows.Err()
}

// AddFavorite stars an asset for a user. It is idempotent: the table carries
// a UNIQUE (user_id, asset_id) constraint, so a duplicate star is a no-op
// rather than an error.
//
// The columns are filled the way Django would: id and the timestamps have no
// database defaults, and comment is NOT NULL, so all four are supplied here.
// A bad asset or user ID is rejected by the table's own foreign keys, which
// is why nothing validates them first.
func (c *CoreDB) AddFavorite(userID, assetID string) error {
	_, err := c.db.Exec(
		`INSERT INTO assets_favoriteasset (id, user_id, asset_id, comment, date_created, date_updated)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, '', now(), now())
		 ON CONFLICT (user_id, asset_id) DO NOTHING`,
		uuid.NewString(), userID, assetID,
	)
	if err != nil {
		return fmt.Errorf("add favorite: %w", err)
	}
	return nil
}

// RemoveFavorite unstars an asset. Removing one that was never starred is not
// an error -- the caller's intent (it should not be starred) is satisfied.
func (c *CoreDB) RemoveFavorite(userID, assetID string) error {
	_, err := c.db.Exec(
		`DELETE FROM assets_favoriteasset WHERE user_id = $1::uuid AND asset_id = $2::uuid`,
		userID, assetID,
	)
	if err != nil {
		return fmt.Errorf("remove favorite: %w", err)
	}
	return nil
}

// RecentAssets lists the assets a user connected to, most recent first, from
// Core's own session history.
//
// Deriving this from terminal_session rather than tracking it client-side
// means it counts every route into a machine -- the web console and a plain
// Koko login as well as this client -- and follows the user to whatever
// machine they run jms on.
//
// Only successful sessions count: "recent" should mean machines you got into,
// not ones you failed to reach. Rows are collapsed per asset so revisiting a
// machine promotes it instead of filling the list with one host.
//
// Note terminal_session.user_id/asset_id are varchar here, not uuid (Core
// denormalises them), hence no casts.
func (c *CoreDB) RecentAssets(userID string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = RecentLimit
	}
	rows, err := c.db.Query(
		`SELECT asset_id FROM terminal_session
		 WHERE user_id = $1 AND is_success = true
		 GROUP BY asset_id
		 ORDER BY max(date_start) DESC
		 LIMIT $2`,
		userID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list recent assets: %w", err)
	}
	defer rows.Close()

	assetIDs := []string{}
	for rows.Next() {
		var assetID sql.NullString
		if err := rows.Scan(&assetID); err != nil {
			return nil, fmt.Errorf("list recent assets: %w", err)
		}
		if assetID.String != "" {
			assetIDs = append(assetIDs, assetID.String)
		}
	}
	return assetIDs, rows.Err()
}
