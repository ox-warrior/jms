package jmsd

import (
	"errors"
	"testing"
	"time"
)

// No test here opens a database. The comment parser is pure, and the cache
// takes its loader and its clock as fields for exactly this reason.

func TestParseCommentTag_StripsTheRedisSuffix(t *testing.T) {
	// cloud-sync creates a SECOND asset for a box running self-hosted Redis,
	// with ":redis" appended to the source id. Both assets are one machine and
	// the inventory has the one row, keyed without the suffix -- so without
	// this the describe panel is empty on exactly the assets people reach for
	// it on.
	_, _, resourceID, ok := parseCommentTag("[cloud-sync:aws:mega-base:i-0abc123:redis]")
	if !ok || resourceID != "i-0abc123" {
		t.Fatalf("got (%q, %v), want (\"i-0abc123\", true)", resourceID, ok)
	}
}

func TestParseCommentTag_KeepsColonsInsideTheResourceID(t *testing.T) {
	// AWS resource ids for databases are ARNs, which are mostly colons. A
	// plain 3-way split would truncate this at "arn".
	const arn = "arn:aws:rds:us-east-1:1234:db:prod"
	provider, account, resourceID, ok := parseCommentTag("[cloud-sync:aws:mega-base:" + arn + "]")
	if !ok || provider != "aws" || account != "mega-base" || resourceID != arn {
		t.Fatalf("got (%q, %q, %q, %v)", provider, account, resourceID, ok)
	}
}

func TestParseCommentTag_RejectsAnythingElse(t *testing.T) {
	// A hand-written comment must not resolve to a lookup key. The empty-field
	// cases matter most: "" for provider or account would key into a row that
	// does not exist, which is harmless, but an empty resource id would make
	// the panel claim a machine's identity triple is blank.
	for _, comment := range []string{
		"",
		"a note somebody typed",
		"[cloud-sync:aws:mega-base]",
		"[cloud-sync:aws::i-1]",
		"[cloud-sync::mega-base:i-1]",
		"[cloud-sync:aws:mega-base:]",
		"[cloud-sync:aws:mega-base:i-1",
	} {
		if _, _, _, ok := parseCommentTag(comment); ok {
			t.Errorf("parsed %q as a cloud-sync tag", comment)
		}
	}
}

// clock is a hand-wound time source, so TTL expiry is tested by advancing it
// rather than by sleeping.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

// countingCache returns a cache whose loader records how many times it ran and
// can be made to fail.
func countingCache(t *testing.T) (*InventoryDB, *clock, *int, *error) {
	t.Helper()
	c := &clock{t: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)}
	calls := 0
	var loadErr error
	// async runs inline so a scheduled refresh has finished by the time the
	// call that scheduled it returns -- the goroutine is what makes this
	// untestable, not the caching.
	inv := &InventoryDB{now: c.now, ttl: time.Minute, async: func(f func()) { f() }}
	inv.load = func() (map[string]*HostInfo, error) {
		calls++
		if loadErr != nil {
			return nil, loadErr
		}
		return map[string]*HostInfo{
			hostKey("aws", "mega-base", "i-1"): {ResourceID: "i-1", InstanceType: "m5.large"},
		}, nil
	}
	return inv, c, &calls, &loadErr
}

// assets is the listing hostsFor is asked about: one cloud-sync host, one
// asset somebody registered by hand.
func assets() []permAsset {
	return []permAsset{
		{ID: "asset-1", Comment: "[cloud-sync:aws:mega-base:i-1]"},
		{ID: "asset-2", Comment: "a note somebody typed"},
	}
}

func TestInventoryCache_ServesManyListingsFromOneQuery(t *testing.T) {
	inv, _, calls, _ := countingCache(t)
	require(t, inv.Warm())

	// The point of the snapshot: /v1/assets costs a map lookup per asset, not
	// a query. Without it every workspace load re-reads the whole table.
	for range 5 {
		if host := inv.lookup(inv.briefs(assets())["asset-1"].Key); host == nil {
			t.Fatal("lost the host")
		}
	}
	if *calls != 1 {
		t.Fatalf("loaded the table %d times, want 1", *calls)
	}
}

func TestInventoryCache_AnExpiredReadIsAnsweredFromMemory(t *testing.T) {
	// The reason this is not a plain "reload, then answer" cache. That shape
	// put a cross-region query in front of whoever opened jms first after
	// every expiry -- which is nearly every launch, since nobody opens jms
	// twice inside a TTL. Past the TTL the caller still gets an answer now;
	// the reload happens behind it.
	inv, c, calls, _ := countingCache(t)
	require(t, inv.Warm())
	c.add(time.Hour)

	host := inv.lookup(inv.briefs(assets())["asset-1"].Key)
	if host == nil || host.InstanceType != "m5.large" {
		t.Fatalf("an expired read was not answered from memory: %+v", host)
	}
	if *calls != 2 {
		t.Fatalf("the refresh behind it did not run (loads=%d)", *calls)
	}
}

func TestInventoryCache_ConcurrentExpiredReadsShareOneRefresh(t *testing.T) {
	inv, c, calls, _ := countingCache(t)
	require(t, inv.Warm())
	c.add(time.Hour)

	// The clock does not move between these, so all three see an expired
	// snapshot. Without the refreshing guard each would start its own query.
	for range 3 {
		inv.briefs(assets())
	}
	if *calls != 2 {
		t.Fatalf("three expired reads caused %d loads, want 2 (warm + one refresh)", *calls)
	}
}

func TestInventoryCache_KeepsServingWhenARefreshFails(t *testing.T) {
	inv, c, _, loadErr := countingCache(t)
	require(t, inv.Warm())

	// Unlike userDirectory, a stale snapshot here is served rather than
	// refused: this data authorises nothing, and losing the panel to a
	// database blip is the worse trade. Every row carries its own SyncedAt,
	// so the panel still says how old it is.
	*loadErr = errors.New("mysql is down")
	c.add(time.Hour)

	host := inv.lookup(inv.briefs(assets())["asset-1"].Key)
	if host == nil || host.InstanceType != "m5.large" {
		t.Fatalf("stale snapshot lost: %+v", host)
	}
}

func TestInventoryCache_ADeadDatabaseCostsMetadataNotTheAssetList(t *testing.T) {
	// hostsFor runs inside /v1/assets, which is the whole workspace. With
	// nothing ever loaded it must come back empty rather than fail or block
	// -- trading the asset list for a display feature would take the tool
	// down over a database nothing else depends on.
	inv, _, _, loadErr := countingCache(t)
	*loadErr = errors.New("mysql is down")

	if err := inv.Warm(); err == nil {
		t.Fatal("Warm must report a failed first load")
	}
	if got := inv.briefs(assets()); len(got) != 0 {
		t.Fatalf("want no keys, got %v", got)
	}
}

func TestInventoryCache_NoInventoryConfigured(t *testing.T) {
	// inventory_db_dsn unset leaves the store nil, and handleAssets calls
	// straight through it.
	var inv *InventoryDB
	if got := inv.briefs(assets()); len(got) != 0 {
		t.Fatalf("want no keys, got %v", got)
	}
}

func TestInventoryCache_AssetsWithoutARowAreAbsent(t *testing.T) {
	inv, _, _, _ := countingCache(t)
	require(t, inv.Warm())
	briefs := inv.briefs(assets())

	// asset-2's comment is not a cloud-sync tag, so it must not appear at
	// all. A missing brief is what lets the client answer "d" with "no cloud
	// metadata" instead of making a request to be told so.
	if _, ok := briefs["asset-2"]; ok {
		t.Error("a hand-registered asset got a brief")
	}
	if _, ok := briefs["asset-missing"]; ok {
		t.Error("an unknown asset got a brief")
	}
	if inv.lookup("aws:mega-base:i-does-not-exist") != nil {
		t.Error("lookup invented a row")
	}
}

func require(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestInventoryBrief_CarriesTheInstanceTypeForTheListingColumn(t *testing.T) {
	// The resource table shows this under ":ec2", so it has to arrive with the
	// listing rather than behind the describe call -- a column that needs a
	// request per row is not a column.
	inv, _, _, _ := countingCache(t)
	require(t, inv.Warm())

	brief := inv.briefs(assets())["asset-1"]
	if brief.Key == "" {
		t.Fatal("brief has no key")
	}
	if brief.InstanceType != "m5.large" {
		t.Fatalf("instance type = %q, want m5.large", brief.InstanceType)
	}
}

func TestInventoryBrief_ReadsTheModuleTagAndOmitsItWhenAbsent(t *testing.T) {
	// Application:module is on 72% of production machines, so absent is the
	// ordinary case. It has to come back empty rather than as some
	// placeholder the client would then have to recognise.
	c := &clock{t: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)}
	inv := &InventoryDB{now: c.now, ttl: time.Minute, async: func(f func()) { f() }}
	inv.load = func() (map[string]*HostInfo, error) {
		return map[string]*HostInfo{
			hostKey("aws", "mega-base", "i-1"): {
				ResourceID: "i-1", OwnerTeam: "Platform",
				Tags: map[string]string{appModuleTag: "Kafka", "env": "prod"},
			},
			hostKey("aws", "mega-base", "i-2"): {
				ResourceID: "i-2", OwnerTeam: "Platform",
				Tags: map[string]string{"env": "prod"},
			},
		}, nil
	}
	require(t, inv.Warm())

	briefs := inv.briefs([]permAsset{
		{ID: "tagged", Comment: "[cloud-sync:aws:mega-base:i-1]"},
		{ID: "untagged", Comment: "[cloud-sync:aws:mega-base:i-2]"},
	})
	if got := briefs["tagged"]; got.AppModule != "Kafka" || got.OwnerTeam != "Platform" {
		t.Errorf("tagged brief = %+v", got)
	}
	if got := briefs["untagged"]; got.AppModule != "" || got.OwnerTeam != "Platform" {
		t.Errorf("untagged brief = %+v, want an empty module and the team", got)
	}
}
