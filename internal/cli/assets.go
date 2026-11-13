package cli

import (
	"fmt"
	"time"

	"github.com/leiyupeng/jumpserver-cli/internal/assetcache"
	"github.com/leiyupeng/jumpserver-cli/internal/connectflow"
	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
	"github.com/leiyupeng/jumpserver-cli/internal/jmslog"
)

// loadAssets is the TUI's loadWorkspace without the display: the local cache
// while it is fresh, otherwise nodes and assets from jmsd. cached reports
// which, so a caller can distrust a cached miss.
//
// Nodes are fetched even though -c never shows them, because the cache
// file is shared with the TUI and holds the two as a pair: the tree's
// per-node counts are computed from the asset list, and saving assets alone
// would hand the next TUI launch a tree with nothing in it.
func loadAssets(gate *connectflow.Gate, source, username string, ttl time.Duration, refresh bool) (assets []jmsd.AssetRow, cached bool, err error) {
	if !refresh {
		if entry := assetcache.Load(source, username, ttl); entry != nil {
			return entry.Assets, true, nil
		}
	}
	jc, ctx := gate.JC, gate.Ctx
	nodes, err := connectflow.WithMFA(gate, func() ([]jmsd.NodeTree, error) { return jc.Nodes(ctx) })
	if err != nil {
		return nil, false, fmt.Errorf("load nodes: %w", err)
	}
	assets, err = connectflow.WithMFA(gate, func() ([]jmsd.AssetRow, error) { return jc.Assets(ctx) })
	if err != nil {
		return nil, false, fmt.Errorf("load assets: %w", err)
	}
	if err := assetcache.Save(source, username, nodes, assets); err != nil {
		jmslog.Printf("asset cache: write failed: %v", err)
	}
	return assets, false, nil
}
