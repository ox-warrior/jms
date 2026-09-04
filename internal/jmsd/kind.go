package jmsd

import "strings"

// ResourceKind is the coarse resource grouping jms recognizes, both for the
// TUI's ":" command and the "-c TYPE [NAME]" shortcut. It lives here rather
// than in internal/tui because AssetRow -- the thing being classified -- is
// defined in this package, and the CLI's headless connect path needs the same
// mapping without depending on the TUI.
type ResourceKind string

const (
	ResourceAll   ResourceKind = "all"   // no category/type filter -- every resource
	ResourceEC2   ResourceKind = "ec2"   // category "host" -- AWS ec2 and Aliyun ecs both land there
	ResourceRDS   ResourceKind = "rds"   // category "database", any type except "redis"
	ResourceRedis ResourceKind = "redis" // category "database", type "redis"
	ResourceK8s   ResourceKind = "k8s"   // category "cloud", type "k8s"
)

// KindOf classifies one resource's JumpServer category/type into the kind it
// belongs to, or "" when it fits none of them. The mapping and its cloud-sync
// citations are in docs/PLAN.md.
//
// This is the single place the mapping lives: the TUI's ":" filter, its SOURCE
// TYPE column, and connectflow's k8s check all go through it, so none of them
// can disagree about what a given asset is. Matching is case-insensitive
// because nothing guarantees Core spells a category the same way on every
// endpoint, and a k8s cluster that came back as "Cloud" must not quietly
// become an ssh host.
func KindOf(category, typ string) ResourceKind {
	category, typ = strings.ToLower(category), strings.ToLower(typ)
	switch {
	case category == "host":
		return ResourceEC2
	case category == "database" && typ == "redis":
		return ResourceRedis
	case category == "database":
		return ResourceRDS
	case category == "cloud" && typ == "k8s":
		return ResourceK8s
	}
	return ""
}

// MatchesKind reports whether a resource's category/type belongs to kind.
// ResourceAll admits everything, including resources KindOf cannot place --
// "all" means "every permitted asset", not "every asset this client has a name
// for".
func MatchesKind(kind ResourceKind, category, typ string) bool {
	if kind == ResourceAll {
		return true
	}
	// The "" guard matters: an unclassifiable resource must not match an empty
	// kind, which is not a view the user can ever be looking at.
	got := KindOf(category, typ)
	return got != "" && got == kind
}

// resourceKindTokens is every accepted spelling, and the one list both
// ParseResourceKind and ResourceKindTokens read. alias marks a spelling that is
// accepted but not advertised.
var resourceKindTokens = []struct {
	token string
	kind  ResourceKind
	alias bool
}{
	{token: "all", kind: ResourceAll},
	{token: "ec2", kind: ResourceEC2},
	{token: "rds", kind: ResourceRDS},
	{token: "redis", kind: ResourceRedis},
	{token: "k8s", kind: ResourceK8s},
	{token: "kubernetes", kind: ResourceK8s, alias: true},
}

// ParseResourceKind parses a user-typed token (case-insensitive) into a
// ResourceKind, for the TUI's ":" command and the CLI's "-c" shortcut.
func ParseResourceKind(token string) (ResourceKind, bool) {
	token = strings.ToLower(strings.TrimSpace(token))
	for _, entry := range resourceKindTokens {
		if entry.token == token {
			return entry.kind, true
		}
	}
	return "", false
}

// ResourceKindTokens lists the type tokens worth printing in an error message,
// e.g. `-c: unknown resource type %q (want one of: %s)`.
func ResourceKindTokens() []string {
	tokens := make([]string, 0, len(resourceKindTokens))
	for _, entry := range resourceKindTokens {
		if !entry.alias {
			tokens = append(tokens, entry.token)
		}
	}
	return tokens
}
