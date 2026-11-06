package cli

import (
	"errors"
	"testing"

	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
)

func TestMatchAssetExactAndCaseSensitive(t *testing.T) {
	assets := []jmsd.AssetRow{
		{ID: "1", Name: "ng0", Category: "host"},
		{ID: "2", Name: "NG0", Category: "host"},
		{ID: "3", Name: "ng0", Category: "database", Type: "mysql"},
	}
	got, _, err := matchAsset(assets, jmsd.ResourceEC2, "ng0")
	if err != nil {
		t.Fatalf("matchAsset() error = %v, want nil", err)
	}
	if got.ID != "1" {
		t.Errorf("matchAsset() = %+v, want the exact-case ec2 match (id=1)", got)
	}
}

func TestMatchAssetNotFound(t *testing.T) {
	assets := []jmsd.AssetRow{{ID: "1", Name: "ng0", Category: "host"}}
	_, _, err := matchAsset(assets, jmsd.ResourceEC2, "ng1")
	if !errors.Is(err, ErrAssetNotFound) {
		t.Errorf("matchAsset() error = %v, want ErrAssetNotFound", err)
	}
}

func TestMatchAssetAmbiguousAcrossOrgsOrNodes(t *testing.T) {
	// JumpServer allows the same name to recur across nodes/orgs -- this must
	// not silently pick one.
	assets := []jmsd.AssetRow{
		{ID: "1", Name: "ng0", Category: "host"},
		{ID: "2", Name: "ng0", Category: "host"},
	}
	_, matches, err := matchAsset(assets, jmsd.ResourceEC2, "ng0")
	if !errors.Is(err, ErrAssetAmbiguous) {
		t.Errorf("matchAsset() error = %v, want ErrAssetAmbiguous", err)
	}
	if len(matches) != 2 {
		t.Errorf("matchAsset() matches = %v, want both candidates for the caller to list", matches)
	}
}

func TestMatchAssetKindFiltersFirst(t *testing.T) {
	// Same name, different kind -- ":ec2 ng0" must not match a database
	// asset that happens to share the name.
	assets := []jmsd.AssetRow{
		{ID: "1", Name: "ng0", Category: "database", Type: "mysql"},
	}
	_, _, err := matchAsset(assets, jmsd.ResourceEC2, "ng0")
	if !errors.Is(err, ErrAssetNotFound) {
		t.Errorf("matchAsset() error = %v, want ErrAssetNotFound -- kind must be checked, not just name", err)
	}
}

func TestMatchAssetAllAdmitsEveryKind(t *testing.T) {
	assets := []jmsd.AssetRow{{ID: "1", Name: "ng0", Category: "cloud", Type: "k8s"}}
	got, _, err := matchAsset(assets, jmsd.ResourceAll, "ng0")
	if err != nil || got.ID != "1" {
		t.Errorf("matchAsset(ResourceAll, ...) = (%+v, %v), want the k8s asset matched", got, err)
	}
}

func TestMatchAccountByUsername(t *testing.T) {
	options := []jmsd.AccountOption{
		{Alias: "a1", Name: "Root Account", Username: "root"},
		{Alias: "a2", Name: "Ops", Username: "ops"},
	}
	got, err := matchAccountByUsername(options, "ops")
	if err != nil || got.Alias != "a2" {
		t.Errorf("matchAccountByUsername(ops) = (%+v, %v), want the ops account", got, err)
	}
}

func TestMatchAccountByUsernameNotFound(t *testing.T) {
	options := []jmsd.AccountOption{{Alias: "a1", Username: "root"}}
	_, err := matchAccountByUsername(options, "admin")
	if !errors.Is(err, ErrAccountNotFound) {
		t.Errorf("matchAccountByUsername() error = %v, want ErrAccountNotFound", err)
	}
}

func TestMatchAccountByUsernameAmbiguous(t *testing.T) {
	// A manually-added account and an auto-discovered one can share a
	// Username on the same asset -- must not be picked between.
	options := []jmsd.AccountOption{
		{Alias: "a1", Name: "manual", Username: "root"},
		{Alias: "a2", Name: "discovered", Username: "root"},
	}
	_, err := matchAccountByUsername(options, "root")
	if !errors.Is(err, ErrAccountAmbiguous) {
		t.Errorf("matchAccountByUsername() error = %v, want ErrAccountAmbiguous", err)
	}
}

func TestMatchAssetByIP(t *testing.T) {
	assets := []jmsd.AssetRow{
		{ID: "1", Name: "web-10.0.0.13", Address: "10.0.0.13", Category: "host", Type: "linux"},
		{ID: "2", Name: "other", Address: "10.0.0.14", Category: "host", Type: "linux"},
	}
	got, _, err := matchAsset(assets, jmsd.ResourceEC2, "10.0.0.13")
	if err != nil || got.ID != "1" {
		t.Fatalf("matchAsset by IP = %+v, %v; want asset 1", got, err)
	}
	if _, _, err := matchAsset(assets, jmsd.ResourceEC2, "10.0.0"); err == nil {
		t.Fatal("a partial IP matched")
	}
}
