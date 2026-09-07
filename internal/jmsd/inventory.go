package jmsd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// InventoryDB is a read-only connection to the cloud-metadata table that
// cloud-sync (the sibling jumpserver-tool repo) writes. jmsd never writes to
// it -- same posture as CoreDB.
//
// It is optional. Deployments without inventory_db_dsn keep working; the
// describe endpoint just reports that nothing is configured, rather than jmsd
// refusing to start over a feature nobody asked for.
type InventoryDB struct {
	db *sql.DB
	// load is loadAll in production, and now is time.Now. Both are fields so
	// the cache's expiry and its serve-stale-on-failure rule are testable
	// without a MySQL and without sleeping -- the same injection
	// userDirectory uses for the key index.
	load func() (map[string]*HostInfo, error)
	now  func() time.Time
	// async runs a refresh off the request path. `go f()` in production;
	// tests substitute an inline call so the refresh is deterministic.
	async func(func())
	ttl   time.Duration

	// mu guards the snapshot AND is held across the refresh, which makes the
	// refresh single-flight -- the same shape userDirectory uses, and for the
	// same reason: a burst on an expired cache should produce one query with
	// the rest waiting on it, not one query per keypress.
	mu       sync.Mutex
	hosts    map[string]*HostInfo
	loadedAt time.Time
	// refreshing keeps a burst of requests arriving on an expired snapshot
	// from starting a refresh each.
	refreshing bool
	// failing logs the "cannot refresh" line once per outage rather than on
	// every request behind a database that stays down.
	failing bool
}

// inventoryTTL is how old a snapshot may get before a read schedules a
// refresh behind itself.
//
// It bounds staleness only; it never bounds anybody's latency. A request past
// the TTL is answered from the copy already in memory and the reload happens
// on another goroutine, so no user ever waits on this table -- see snapshot.
//
// Five minutes is well inside cloud-sync's 30-minute write cycle, which is
// what actually caps how fresh this data can be.
const inventoryTTL = 5 * time.Minute

// inventoryTable is not configurable on purpose: the column list below is
// written against this exact schema (cloud-sync migrations/001), so pointing
// jmsd at a differently-shaped table would fail at scan time rather than do
// something useful.
const inventoryTable = "cloud_instance_info"

// SecurityGroupRef and DiskRef mirror the JSON stored in the corresponding
// columns. The tags match cloud-sync's model.SecurityGroupRef/DiskRef, which
// is what produced the stored bytes -- they are the wire format, not a
// convention this side is free to change.
type SecurityGroupRef struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type DiskRef struct {
	Device   string `json:"device,omitempty"`
	SizeGB   int    `json:"size_gb,omitempty"`
	Type     string `json:"type,omitempty"`
	IOPS     int    `json:"iops,omitempty"`
	VolumeID string `json:"volume_id,omitempty"`
}

// HostInfo is one cloud_instance_info row.
//
// The pointer fields are nil when the cloud does not report the value, never
// zero: 0 MB of memory, "pay-as-you-go" and the zero time each read as a fact
// the row does not actually carry. IsPrepaid is three-state -- Aliyun
// subscription, Aliyun pay-as-you-go, and AWS, where the concept does not
// exist.
type HostInfo struct {
	Provider   string `json:"provider"`
	Account    string `json:"account"`
	ResourceID string `json:"resource_id"`

	AccountAlias string `json:"account_alias"`
	Region       string `json:"region"`
	ZoneID       string `json:"zone_id"`

	Name      string `json:"name"`
	OwnerTeam string `json:"owner_team"`

	InstanceType string `json:"instance_type"`
	VCPU         *int   `json:"vcpu"`
	MemoryMB     *int   `json:"memory_mb"`

	ImageID string `json:"image_id"`
	OSName  string `json:"os_name"`

	PrivateIP string `json:"private_ip"`
	PublicIP  string `json:"public_ip"`
	VpcID     string `json:"vpc_id"`
	SubnetID  string `json:"subnet_id"`

	IsPrepaid *bool `json:"is_prepaid"`
	// AutoRenew narrows ExpiredAt: with it on the expiry is a date that
	// moves, with it off it is a deadline. nil wherever the question does not
	// apply -- AWS, pay-as-you-go, or a renewal attribute cloud-sync could
	// not read.
	AutoRenew *bool      `json:"auto_renew"`
	ExpiredAt *time.Time `json:"expired_at"`
	CreatedAt *time.Time `json:"created_at"`
	StartedAt *time.Time `json:"started_at"`

	SecurityGroups []SecurityGroupRef `json:"security_groups"`
	Disks          []DiskRef          `json:"disks"`
	Tags           map[string]string  `json:"tags"`

	// SyncedAt is when cloud-sync last refreshed this row. Shown because
	// every other field is a snapshot, and a snapshot with no timestamp
	// invites reading a stale value as current.
	SyncedAt time.Time `json:"synced_at"`
}

// NewInventoryDB opens the cloud-metadata database. dsn is a go-sql-driver
// DSN and must carry parseTime=true&loc=UTC: the DATETIME columns hold UTC
// with no zone of their own, so without loc the driver would relabel them as
// local time and every timestamp shown would be silently wrong.
func NewInventoryDB(dsn string) (*InventoryDB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open inventory db: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping inventory db: %w", err)
	}
	inv := &InventoryDB{
		db:    db,
		now:   time.Now,
		async: func(f func()) { go f() },
		ttl:   inventoryTTL,
	}
	inv.load = inv.loadAll
	return inv, nil
}

func (i *InventoryDB) Close() error { return i.db.Close() }

// hostKey is the primary key, joined. It is the same triple the asset comment
// carries, so a lookup is a single map hit with no parsing left to do.
func hostKey(provider, account, resourceID string) string {
	return provider + ":" + account + ":" + resourceID
}

// Warm loads the table once, synchronously. Called at startup so the first
// user request finds the snapshot already in memory rather than paying for
// the query -- the table lives in another region and the load is measured in
// seconds, which is a fine cost for a daemon starting up and a bad one for
// somebody waiting on a screen.
//
// A failure is reported, not fatal: the daemon runs without cloud metadata
// and the next read schedules another attempt.
func (i *InventoryDB) Warm() error {
	hosts, err := i.load()
	if err != nil {
		return err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.hosts, i.loadedAt, i.failing = hosts, i.now(), false
	return nil
}

// snapshot returns whatever is in memory, immediately. It does not block, and
// it cannot fail.
//
// This runs inside /v1/assets, which is the whole workspace, so it must not
// be able to make anyone wait: an expired snapshot is served as-is and the
// reload is scheduled behind it. The alternative -- reload first, answer
// second -- put a cross-region query in front of the first person to open jms
// after every expiry, which is nearly every launch, since nobody opens jms
// twice within a TTL.
//
// A nil result (a cold start whose load failed) means no cloud metadata this
// time, not an error: this data authorises nothing and the asset list must
// not depend on it.
func (i *InventoryDB) snapshot() map[string]*HostInfo {
	i.mu.Lock()
	hosts := i.hosts
	// The claim on the refresh is made under the lock; the refresh itself is
	// started outside it. refresh takes the same lock to install its result,
	// so scheduling it while still holding it deadlocks the moment the
	// scheduler runs the work inline instead of on a goroutine.
	stale := hosts == nil || i.now().Sub(i.loadedAt) >= i.ttl
	claimed := stale && !i.refreshing
	if claimed {
		i.refreshing = true
	}
	i.mu.Unlock()

	if claimed {
		i.async(i.refresh)
	}
	return hosts
}

// refresh reloads the table off the request path. A failure keeps the
// previous snapshot -- unlike userDirectory, which refuses to serve a stale
// key index, this data authorises nothing, so losing the panel to a database
// blip is the worse trade. Every row carries its own SyncedAt, so the panel
// still says how old it is.
func (i *InventoryDB) refresh() {
	started := i.now()
	hosts, err := i.load()

	i.mu.Lock()
	defer i.mu.Unlock()
	i.refreshing = false

	if err != nil {
		if !i.failing {
			i.failing = true
			log.Printf("jmsd: inventory refresh failed, serving what is in memory: %v", err)
		}
		return
	}
	i.hosts, i.loadedAt, i.failing = hosts, i.now(), false
	debugf("inventory: loaded %d rows from %s in %s",
		len(hosts), inventoryTable, i.now().Sub(started).Round(time.Millisecond))
}

// loadAll reads the whole table. There is no WHERE and no pagination on
// purpose -- see inventoryTTL.
func (i *InventoryDB) loadAll() (map[string]*HostInfo, error) {
	rows, err := i.db.Query(
		`SELECT provider, account, resource_id, account_alias, region, zone_id,
		        name, owner_team, instance_type, vcpu, memory_mb,
		        image_id, os_name, private_ip, public_ip, vpc_id, subnet_id,
		        is_prepaid, auto_renew, expired_at, created_at, started_at,
		        security_groups, disks, tags, synced_at
		   FROM ` + inventoryTable)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", inventoryTable, err)
	}
	defer rows.Close()

	hosts := make(map[string]*HostInfo)
	for rows.Next() {
		var (
			h                               HostInfo
			groupsJSON, disksJSON, tagsJSON []byte
		)
		if err := rows.Scan(
			&h.Provider, &h.Account, &h.ResourceID, &h.AccountAlias, &h.Region, &h.ZoneID,
			&h.Name, &h.OwnerTeam, &h.InstanceType, &h.VCPU, &h.MemoryMB,
			&h.ImageID, &h.OSName, &h.PrivateIP, &h.PublicIP, &h.VpcID, &h.SubnetID,
			&h.IsPrepaid, &h.AutoRenew, &h.ExpiredAt, &h.CreatedAt, &h.StartedAt,
			&groupsJSON, &disksJSON, &tagsJSON, &h.SyncedAt,
		); err != nil {
			return nil, fmt.Errorf("read %s: %w", inventoryTable, err)
		}
		// A malformed JSON column costs that one column, not the whole
		// snapshot -- the scalar fields are what an operator is usually
		// after, and one bad row must not blank the table for everybody.
		unmarshalOrDrop(groupsJSON, &h.SecurityGroups, "security_groups", h.ResourceID)
		unmarshalOrDrop(disksJSON, &h.Disks, "disks", h.ResourceID)
		unmarshalOrDrop(tagsJSON, &h.Tags, "tags", h.ResourceID)
		hosts[hostKey(h.Provider, h.Account, h.ResourceID)] = &h
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", inventoryTable, err)
	}
	return hosts, nil
}

func unmarshalOrDrop(raw []byte, dst any, column, resourceID string) {
	if len(raw) == 0 {
		return
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		debugf("inventory: %s.%s for %s is not valid JSON: %v", inventoryTable, column, resourceID, err)
	}
}

// commentTagPrefix and commentTagParts mirror cloud-sync's
// model.CommentTag(): "[cloud-sync:{provider}:{account}:{source_id}]". This
// comment is the only thing joining a JumpServer asset to its inventory row.
const commentTagPrefix = "[cloud-sync:"

// redisSourceSuffix is appended by cloud-sync to the source id of the SECOND
// asset it creates for a box running self-hosted Redis (aws/provider.go:227,
// aliyun/provider.go:186). Two assets, one machine -- and the inventory has
// the one machine, keyed without the suffix.
const redisSourceSuffix = ":redis"

// parseCommentTag pulls the identity triple out of an asset comment.
//
// The source id is everything after the second colon, not the third field of
// a plain split: AWS resource ids for other kinds are ARNs, which are full of
// colons.
func parseCommentTag(comment string) (provider, account, resourceID string, ok bool) {
	comment = strings.TrimSpace(comment)
	if !strings.HasPrefix(comment, commentTagPrefix) || !strings.HasSuffix(comment, "]") {
		return "", "", "", false
	}
	parts := strings.SplitN(comment[len(commentTagPrefix):len(comment)-1], ":", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], strings.TrimSuffix(parts[2], redisSourceSuffix), true
}

// cloudBrief is the part of a machine's metadata that rides along with every
// asset in the listing, as opposed to the part fetched on demand.
//
// Adding a field here is how a new column appears in the client's resource
// table, and the bar for doing so is deliberately high: these bytes are paid
// for every asset on every load, including the vast majority nobody will ever
// press "d" on. Short, single-value, and worth seeing without opening
// anything -- everything else belongs behind Key.
type cloudBrief struct {
	// Key addresses the full row (see handleDescribe). Empty is impossible
	// here: a brief only exists for an asset that has one.
	Key string
	// InstanceType is the machine's shape, and only hosts have one -- this
	// table holds no databases or caches.
	InstanceType string
	// OwnerTeam is the owning team, already lifted out of the tags into its
	// own column by cloud-sync.
	OwnerTeam string
	// AppModule is what the machine runs (Redis, Kafka, ...), read from the
	// tag named by appModuleTag.
	AppModule string
}

// appModuleTag is the tag cloud resources carry to say what runs on them. It
// is a fixed string rather than config because it is one org's tagging
// convention and there is exactly one org here; the day a second one needs a
// different key, this becomes a config field the way the owner tag already is
// (cloud-sync's grouping.rules[].tag_key).
const appModuleTag = "Application:module"

// briefs maps each asset id to its listing-visible metadata, for the assets
// that actually have a row. Everything else is absent, so an asset with no
// brief means "pressing d here will find nothing" and the client can say so
// without a round-trip.
//
// It never fails and never blocks. This runs inside /v1/assets, which is the
// entire workspace -- neither losing the asset list nor delaying it is worth
// a display feature.
func (i *InventoryDB) briefs(assets []permAsset) map[string]cloudBrief {
	if i == nil {
		return nil
	}
	hosts := i.snapshot()
	if len(hosts) == 0 {
		return nil
	}
	out := make(map[string]cloudBrief, len(assets))
	for _, a := range assets {
		provider, account, resourceID, ok := parseCommentTag(a.Comment)
		if !ok {
			continue // not a cloud-sync asset; the common case for hand-registered ones
		}
		key := hostKey(provider, account, resourceID)
		if host := hosts[key]; host != nil {
			out[a.ID] = cloudBrief{
				Key:          key,
				InstanceType: host.InstanceType,
				OwnerTeam:    host.OwnerTeam,
				AppModule:    host.Tags[appModuleTag],
			}
		}
	}
	return out
}

// lookup returns one row by its key, or nil when there is none.
func (i *InventoryDB) lookup(key string) *HostInfo {
	if i == nil {
		return nil
	}
	return i.snapshot()[key]
}

// handleDescribe answers GET /v1/assets/describe?key=<cloud_key> from the
// in-memory snapshot: one map lookup, no database and no Core call.
//
// The key is not re-authorised against the caller's permissions, and that is
// a deliberate trade rather than an oversight. Doing so would mean asking
// Core about the asset on every press -- the request this endpoint exists to
// keep cheap. What still holds: requireSignature means the caller is a known
// JumpServer user, and keys are only ever handed out attached to assets that
// user is permitted, so reaching another machine's row requires obtaining its
// key some other way. The contents are specs, tags and network ids, not
// credentials.
func (s *Server) handleDescribe(w http.ResponseWriter, r *http.Request, user *userRecord) {
	key := r.URL.Query().Get("key")
	if key == "" {
		fail(w, r, user.Username, http.StatusBadRequest, "key is required")
		return
	}
	host := s.inventory.lookup(key)
	if host == nil {
		// The client only sends keys the listing gave it, so a miss means the
		// row went away between the two calls -- worth a line, unlike the
		// ordinary "this asset has no key at all", which never gets here.
		note(r, user.Username, fmt.Sprintf("no inventory row for key %q", key))
		fail(w, r, user.Username, http.StatusNotFound, "no cloud metadata for "+key)
		return
	}
	writeJSON(w, host)
}
