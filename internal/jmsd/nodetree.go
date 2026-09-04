package jmsd

import (
	"strings"

	"github.com/jumpserver-dev/sdk-go/model"
)

// NodeTree is the nested shape jmsd hands to the jms client for the scope
// tree panel (see docs/PLAN.md §7.2, GET /v1/nodes).
// JumpServer's colon-separated hierarchy path ("1:3:0") is deliberately not
// forwarded: it is what BuildNodeTree derives the nesting from, and once the
// nesting exists the client has no use for it.
type NodeTree struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Children []NodeTree `json:"children,omitempty"`
}

// BuildNodeTree reconstructs the node hierarchy from the flat list returned
// by GET /api/v1/perms/users/{id}/nodes/. That endpoint's serializer
// (NodePermedSerializer, apps/perms/serializers/user_permission.py) only
// emits id/name/key/value/full_value/org_id/assets_amount -- there is no
// parent field -- so the hierarchy must be derived from Key, JumpServer's
// colon-separated path (e.g. "1:3:0" is a child of "1:3"). A node whose
// parent key isn't present in the input -- because it's the org root, or
// because the user's grant starts partway down the tree -- becomes a
// top-level entry.
func BuildNodeTree(nodes []model.Node) []NodeTree {
	ordered := make([]model.Node, len(nodes))
	copy(ordered, nodes)
	model.SortNodesByKey(ordered)

	byKey := make(map[string]*model.Node, len(ordered))
	for i := range ordered {
		byKey[ordered[i].Key] = &ordered[i]
	}

	children := make(map[string][]*model.Node)
	var roots []*model.Node
	for i := range ordered {
		n := &ordered[i]
		if parentKey := parentKeyOf(n.Key); parentKey != "" {
			if _, ok := byKey[parentKey]; ok {
				children[parentKey] = append(children[parentKey], n)
				continue
			}
		}
		roots = append(roots, n)
	}
	return buildLevel(roots, children)
}

func buildLevel(nodes []*model.Node, children map[string][]*model.Node) []NodeTree {
	if len(nodes) == 0 {
		return nil
	}
	result := make([]NodeTree, 0, len(nodes))
	for _, n := range nodes {
		result = append(result, NodeTree{
			ID:       n.ID,
			Name:     n.Name,
			Children: buildLevel(children[n.Key], children),
		})
	}
	return result
}

func parentKeyOf(key string) string {
	idx := strings.LastIndex(key, ":")
	if idx < 0 {
		return ""
	}
	return key[:idx]
}
