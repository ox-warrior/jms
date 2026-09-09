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

func fixedLookup(answers ...string) lookupIPFunc {
	return func(context.Context, string, string) ([]net.IP, error) {
		ips := make([]net.IP, 0, len(answers))
		for _, a := range answers {
			ips = append(ips, net.ParseIP(a))
		}
		return ips, nil
	}
}

// The address that gets dialled must be the one that was checked -- pinning it
// here is the whole DNS-rebinding defence, since dialling the NAME would
// resolve a second time and that lookup belongs to whoever controls the record.
func TestResolveTunnelTargetPinsTheCheckedAddress(t *testing.T) {
	policy := mustPolicy(t, TunnelRule{CIDR: "10.20.0.0/16", Ports: []int{443}})

	ip, err := resolveTunnelTarget(context.Background(), fixedLookup("10.20.1.5"), policy, "app.internal", 443)
	if err != nil {
		t.Fatalf("permitted name refused: %v", err)
	}
	if !ip.Equal(net.ParseIP("10.20.1.5")) {
		t.Fatalf("resolved to %s, want the checked address 10.20.1.5", ip)
	}

	// A literal address never reaches the resolver at all.
	_, err = resolveTunnelTarget(context.Background(), func(context.Context, string, string) ([]net.IP, error) {
		return nil, errors.New("resolver must not be called for a literal IP")
	}, policy, "10.20.1.5", 443)
	if err != nil {
		t.Fatalf("literal IP: %v", err)
	}
}

// The regression: this used to demand that EVERY resolved address pass, which
// refused essentially every real hostname. A dual-stack name answers with both
// A and AAAA records, so an allow list covering only IPv4 rejected the name
// outright -- and the message blamed an IPv6 address the operator had never
// knowingly excluded ("www.google.com -> 2404:6800:...:443 refused: not in
// tunnel.allow"). Every page in a browser pointed at the tunnel failed.
//
// Demanding all of them bought nothing: the rebinding hole is closed by
// pinning the address that gets dialled, which the test above covers.
func TestResolveTunnelTargetPicksAPermittedAddressFromADualStackName(t *testing.T) {
	policy := mustPolicy(t, TunnelRule{CIDR: "0.0.0.0/0", Ports: []int{443}})

	// AAAA first, exactly the order a resolver commonly returns.
	ip, err := resolveTunnelTarget(context.Background(),
		fixedLookup("2404:6800:4003:c1a::65", "142.251.151.119"), policy, "www.google.com", 443)
	if err != nil {
		t.Fatalf("dual-stack name refused with an IPv4 allow list: %v", err)
	}
	if !ip.Equal(net.ParseIP("142.251.151.119")) {
		t.Fatalf("dialling %s, want the permitted IPv4 address 142.251.151.119", ip)
	}
}

// Skipping over a refused address must not become a way to reach one. A name
// that resolves ONLY to denied addresses is still refused, and the error says
// which -- otherwise the loopback and metadata denials could be sidestepped by
// publishing a name with a second, permitted record.
func TestResolveTunnelTargetRefusesWhenNoAddressPasses(t *testing.T) {
	policy := mustPolicy(t, TunnelRule{CIDR: "0.0.0.0/0", Ports: []int{443}})

	_, err := resolveTunnelTarget(context.Background(),
		fixedLookup("127.0.0.1", "169.254.169.254"), policy, "evil.internal", 443)
	if err == nil {
		t.Fatal("a name resolving only to loopback and metadata was allowed")
	}
	if !strings.Contains(err.Error(), "metadata") && !strings.Contains(err.Error(), "loopback") {
		t.Errorf("error %q does not say which address was refused", err)
	}

	// And the port still has to match, whichever address answers.
	_, err = resolveTunnelTarget(context.Background(),
		fixedLookup("142.251.151.119"), policy, "www.google.com", 22)
	if err == nil {
		t.Fatal("port 22 was allowed by a rule listing only 443")
	}
}
