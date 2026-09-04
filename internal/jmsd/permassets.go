package jmsd

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/jumpserver-dev/sdk-go/model"
	"github.com/jumpserver-dev/sdk-go/service"
)

// permAssetPageSize matches what sdk-go's own getPaginationAssets uses. Core
// caps a page at MAX_PAGE_SIZE=1000 (apps/jumpserver/conf.py:702); staying at
// 100 keeps each response small and mirrors the pagination the rest of the
// codebase already exercises against this endpoint.
const permAssetPageSize = 100

// permAssetMaxPages bounds the crawl below. Core paginates by echoing a `next`
// URL back, and the loop's only exit is that field going empty -- so a Core
// that returns a `next` pointing at the page just fetched (a proxy rewriting
// it, a bug, a hand-rolled endpoint) spins forever with the client's request
// hung on it. 200 pages is 20,000 assets, well past any real permission grant
// and still a definite end.
const permAssetMaxPages = 200

// permAsset mirrors Core's AssetPermedSerializer output. It exists because
// sdk-go's model.PermAsset drops the `nodes` field, and model.PaginationResponse
// hard-codes []PermAsset -- so the typed helpers (GetAllUserPermsAssets /
// GetUserNodeAssets) structurally cannot carry node membership through.
//
// `nodes` is in the serializer's only_fields (v4.10.16
// apps/perms/serializers/user_permission.py:36) and both the all-assets and
// node-scoped endpoints share that serializer (apps/perms/api/user_permission/
// assets.py:47), so it is present on either path.
//
// Note the field lists DIRECT membership only -- it does not include ancestor
// nodes. Core's own node-scoped query is recursive by contrast
// (apps/perms/utils/user_perm.py:145), so any per-node tally built from this
// has to aggregate down the subtree itself.
type permAsset struct {
	ID       string           `json:"id"`
	Name     string           `json:"name"`
	Address  string           `json:"address"`
	Category model.LabelField `json:"category"`
	Type     model.LabelField `json:"type"`
	Nodes    []permAssetNode  `json:"nodes"`
}

// permAssetNode is the trimmed node reference embedded in each asset. Only the
// ID is read: node names are NOT unique in practice (production has nine
// distinct nodes named "Platform"), so identity must come from the ID.
type permAssetNode struct {
	ID string `json:"id"`
}

type permAssetPage struct {
	NextURL string      `json:"next"`
	Results []permAsset `json:"results"`
}

// userPermAssets lists every asset the user may reach. There is no per-node
// variant on purpose: the client tallies node counts across the whole tree
// from this one listing (see AssetRow.NodeIDs), which a node-scoped fetch
// would both make impossible and cost an extra round-trip for.
//
// This deliberately bypasses sdk-go's typed wrappers (see permAsset) but goes
// through JMService.Call, the SDK's own passthrough to its signed HTTP client
// -- the same escape hatch cmd/jmsd-admin already uses for endpoints the SDK
// doesn't model.
func (s *Server) userPermAssets(userID string) ([]permAsset, error) {
	reqURL := fmt.Sprintf("/api/v1/perms/users/%s/assets/", userID)
	params := []map[string]string{{
		"limit":  strconv.Itoa(permAssetPageSize),
		"offset": "0",
	}}

	var all []permAsset
	for pageNumber := 1; ; pageNumber++ {
		var page permAssetPage
		pageDone := timed("core api: perms assets page %d user=%s", pageNumber, userID)
		err := func() error {
			defer pageDone()
			_, err := s.core.Call(http.MethodGet, reqURL, nil, &page, params...)
			return err
		}()
		if err != nil {
			return nil, err
		}
		all = append(all, page.Results...)
		if page.NextURL == "" {
			return all, nil
		}
		if pageNumber >= permAssetMaxPages {
			return nil, fmt.Errorf("perms assets for %s still paginating after %d pages -- refusing to keep following `next`",
				userID, permAssetMaxPages)
		}
		// `next` comes back absolute; the signed client expects a path. Its
		// paging params are already baked into that URL, so none are passed
		// again -- as an empty slice rather than a nil map, because the SDK
		// counts a nil map as one argument and glues a stray "&" onto the URL
		// (httplib.Client.parseQueryUrl).
		reqURL = service.TrimHost(page.NextURL)
		params = nil
	}
}
