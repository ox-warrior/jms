package jmsd

import "testing"

func TestKindOf(t *testing.T) {
	cases := []struct {
		category, typ string
		want          ResourceKind
	}{
		{"host", "", ResourceEC2},
		{"host", "ecs", ResourceEC2},
		{"database", "redis", ResourceRedis},
		{"database", "mysql", ResourceRDS},
		{"database", "", ResourceRDS},
		{"cloud", "k8s", ResourceK8s},
		{"cloud", "vm", ""},
		{"web", "", ""},
		{"", "", ""},
		// Case-insensitive: connectflow.IsK8sAsset defers to this, and a
		// cluster that came back as "Cloud" must not fall through to being
		// treated as an ssh host.
		{"CLOUD", "K8S", ResourceK8s},
		{"Host", "", ResourceEC2},
		{"Database", "Redis", ResourceRedis},
	}
	for _, c := range cases {
		if got := KindOf(c.category, c.typ); got != c.want {
			t.Errorf("KindOf(%q, %q) = %q, want %q", c.category, c.typ, got, c.want)
		}
	}
}

func TestMatchesKind(t *testing.T) {
	cases := []struct {
		name     string
		kind     ResourceKind
		category string
		typ      string
		want     bool
	}{
		// "all" means every permitted asset, not every classified one -- an
		// unplaceable resource still belongs to it.
		{"all admits an unclassifiable resource", ResourceAll, "web", "", true},
		{"all matches host", ResourceAll, "host", "", true},
		{"all matches redis", ResourceAll, "database", "redis", true},
		{"all matches k8s", ResourceAll, "cloud", "k8s", true},

		{"ec2 matches host category", ResourceEC2, "host", "", true},
		{"ec2 does not match database", ResourceEC2, "database", "mysql", false},
		{"ec2 does not match an unclassifiable resource", ResourceEC2, "web", "", false},

		{"rds matches mysql database", ResourceRDS, "database", "mysql", true},
		{"rds matches postgresql database", ResourceRDS, "database", "postgresql", true},
		// The critical boundary: a redis asset must never show up under rds,
		// or vice versa -- both are category "database", split only by type.
		{"rds excludes redis database", ResourceRDS, "database", "redis", false},
		{"redis matches redis database", ResourceRedis, "database", "redis", true},
		{"redis excludes mysql database", ResourceRedis, "database", "mysql", false},

		{"k8s matches cloud+k8s", ResourceK8s, "cloud", "k8s", true},
		{"k8s excludes cloud+public", ResourceK8s, "cloud", "public", false},
		{"k8s excludes host category", ResourceK8s, "host", "", false},

		{"unknown kind matches nothing", ResourceKind("bogus"), "host", "", false},
		// KindOf returns "" for anything it cannot place. An empty kind is not
		// a view the user can be looking at, so it must not become the bucket
		// every unrecognised asset falls into.
		{"empty kind matches nothing", ResourceKind(""), "web", "website", false},
	}
	for _, c := range cases {
		if got := MatchesKind(c.kind, c.category, c.typ); got != c.want {
			t.Errorf("MatchesKind(%q, %q, %q) = %v, want %v (%s)",
				c.kind, c.category, c.typ, got, c.want, c.name)
		}
	}
}

func TestParseResourceKind(t *testing.T) {
	cases := []struct {
		token string
		want  ResourceKind
		ok    bool
	}{
		{"ec2", ResourceEC2, true},
		{"EC2", ResourceEC2, true},
		{" k8s ", ResourceK8s, true},
		{"kubernetes", ResourceK8s, true},
		{"KUBERNETES", ResourceK8s, true},
		{"all", ResourceAll, true},
		{"bogus", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := ParseResourceKind(c.token)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseResourceKind(%q) = (%q, %t), want (%q, %t)", c.token, got, ok, c.want, c.ok)
		}
	}
}

func TestResourceKindTokensCoversEveryConstant(t *testing.T) {
	tokens := ResourceKindTokens()
	for _, kind := range []ResourceKind{ResourceAll, ResourceEC2, ResourceRDS, ResourceRedis, ResourceK8s} {
		found := false
		for _, token := range tokens {
			if parsed, ok := ParseResourceKind(token); ok && parsed == kind {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("ResourceKindTokens() has no token parsing to %q", kind)
		}
	}
	// "kubernetes" is a second spelling, not a second name for the same
	// thing -- it must not show up in the list error messages print.
	for _, token := range tokens {
		if token == "kubernetes" {
			t.Error("ResourceKindTokens() must not advertise the \"kubernetes\" alias")
		}
	}
}
