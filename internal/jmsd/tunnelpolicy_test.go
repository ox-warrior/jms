package jmsd

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

func mustPolicy(t *testing.T, rules ...TunnelRule) *tunnelPolicy {
	t.Helper()
	p, err := compileTunnelPolicy(rules)
	if err != nil {
		t.Fatalf("compileTunnelPolicy: %v", err)
	}
	return p
}

// The allow list is operator input, and "0.0.0.0/0" gets typed. When it does,
// the addresses that turn the tunnel into an attack on jmsd itself must still
// be refused: link-local is the cloud metadata service (instance credentials
// for jmsd's own host) and loopback is where jmsd's Redis lives, which anyone
// who can write it uses to skip any user's MFA.
func TestTunnelPolicyHardDeniesBeatAllowAll(t *testing.T) {
	policy := mustPolicy(t,
		TunnelRule{CIDR: "0.0.0.0/0", Ports: []int{80, 443, 6379}},
		TunnelRule{CIDR: "::/0", Ports: []int{80, 443, 6379}},
	)
	for _, addr := range []string{
		"169.254.169.254", // AWS/GCP/Azure metadata
		"127.0.0.1",       // jmsd's own localhost: Redis, control plane
		"127.0.0.53",
		"0.0.0.0",
		"::1",
		"fe80::1",
		"224.0.0.1",
	} {
		if err := policy.allow(net.ParseIP(addr), 80); err == nil {
			t.Errorf("allow(%s:80) = nil, want refusal", addr)
		}
	}
}

// An IPv4-mapped IPv6 form of a denied address must be denied too, or the
// deny list is one notation away from being bypassed.
func TestTunnelPolicyDeniesMappedLoopback(t *testing.T) {
	policy := mustPolicy(t, TunnelRule{CIDR: "0.0.0.0/0", Ports: []int{80}})
	if err := policy.allow(net.ParseIP("::ffff:127.0.0.1"), 80); err == nil {
		t.Fatal("allow(::ffff:127.0.0.1) = nil, wanted the loopback refusal")
	}
}

func TestTunnelPolicyPortsAndRanges(t *testing.T) {
	policy := mustPolicy(t,
		TunnelRule{CIDR: "10.20.0.0/16", Ports: []int{443, 5432}},
		// Overlaps the rule above with a different port list: this is how
		// "the subnet, plus redis on one host in it" is expressed, and it must
		// not matter which rule is listed first.
		TunnelRule{CIDR: "10.20.7.9/32", Ports: []int{6379}},
	)
	for _, tc := range []struct {
		addr string
		port int
		want bool
	}{
		{"10.20.1.5", 443, true},
		{"10.20.1.5", 5432, true},
		{"10.20.1.5", 6379, false}, // redis only on the /32 below
		{"10.20.7.9", 6379, true},
		{"10.20.7.9", 443, true}, // still inside the /16
		{"10.21.0.1", 443, false},
		{"10.20.1.5", 22, false},
	} {
		err := policy.allow(net.ParseIP(tc.addr), tc.port)
		if (err == nil) != tc.want {
			t.Errorf("allow(%s:%d) error = %v, want allowed = %t", tc.addr, tc.port, err, tc.want)
		}
	}
}

// A rule that cannot be read as an intentional grant is refused at startup
// rather than at the first connection: an empty port list is an editing
// accident far more often than a deliberate wildcard, and reading it as "all
// ports" would silently widen the tunnel.
func TestCompileTunnelPolicyRejectsUnusableRules(t *testing.T) {
	for _, tc := range []struct {
		name string
		rule TunnelRule
	}{
		{"bad cidr", TunnelRule{CIDR: "10.20.0.0/33", Ports: []int{443}}},
		{"bare ip", TunnelRule{CIDR: "10.20.0.1", Ports: []int{443}}},
		{"no ports", TunnelRule{CIDR: "10.20.0.0/16"}},
		{"port zero", TunnelRule{CIDR: "10.20.0.0/16", Ports: []int{0}}},
		{"port too high", TunnelRule{CIDR: "10.20.0.0/16", Ports: []int{70000}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := compileTunnelPolicy([]TunnelRule{tc.rule}); err == nil {
				t.Fatalf("compileTunnelPolicy(%+v) = nil error, want refusal", tc.rule)
			}
		})
	}
}

// The check must apply to what the name resolves to, and the caller must dial
// the address that was checked. Checking a name and then dialling it resolves
// twice, and the second lookup belongs to whoever controls the record.
func TestResolveTunnelTargetChecksEveryResolvedAddress(t *testing.T) {
	policy := mustPolicy(t, TunnelRule{CIDR: "10.20.0.0/16", Ports: []int{443}})
	lookup := func(answers ...string) lookupIPFunc {
		return func(context.Context, string, string) ([]net.IP, error) {
			ips := make([]net.IP, 0, len(answers))
			for _, a := range answers {
				ips = append(ips, net.ParseIP(a))
			}
			return ips, nil
		}
	}

	ip, err := resolveTunnelTarget(context.Background(), lookup("10.20.1.5"), policy, "app.internal", 443)
	if err != nil {
		t.Fatalf("permitted name refused: %v", err)
	}
	if !ip.Equal(net.ParseIP("10.20.1.5")) {
		t.Fatalf("resolved to %s, want the checked address 10.20.1.5", ip)
	}

	// One permitted and one forbidden answer: refused outright, because there
	// is no telling which record a later lookup would return.
	_, err = resolveTunnelTarget(context.Background(), lookup("10.20.1.5", "127.0.0.1"), policy, "rebind.internal", 443)
	if err == nil {
		t.Fatal("a name resolving to both 10.20.1.5 and 127.0.0.1 was allowed")
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Errorf("error %q does not say which address was refused", err)
	}

	// A literal address never reaches the resolver at all.
	_, err = resolveTunnelTarget(context.Background(), func(context.Context, string, string) ([]net.IP, error) {
		return nil, errors.New("resolver must not be called for a literal IP")
	}, policy, "10.20.1.5", 443)
	if err != nil {
		t.Fatalf("literal IP: %v", err)
	}
}
