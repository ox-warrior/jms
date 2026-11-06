package jmsd

import (
	"context"
	"fmt"
	"net"
)

// lookupIPFunc is net.Resolver.LookupIP's signature, taken as a parameter so
// the two rules resolveTunnelTarget enforces can be tested against fixed
// answers instead of whatever DNS the test machine happens to have.
type lookupIPFunc func(ctx context.Context, network, host string) ([]net.IP, error)

// tunnelPolicy is the compiled form of TunnelConfig.Allow: the one place that
// answers "may this caller's stream reach this address". Kept free of network
// and database access so the invariants below are unit-testable without a
// listener, a Core, or a DNS server.
type tunnelPolicy struct {
	rules []tunnelRule
}

type tunnelRule struct {
	net   *net.IPNet
	ports map[int]bool
}

// compileTunnelPolicy parses the YAML rules once at startup. A rule with no
// ports is rejected rather than read as "all ports": the config is meant to
// be reviewable, and an empty list is far more likely to be an editing
// accident than a deliberate wildcard.
func compileTunnelPolicy(rules []TunnelRule) (*tunnelPolicy, error) {
	out := &tunnelPolicy{}
	for i, r := range rules {
		_, ipNet, err := net.ParseCIDR(r.CIDR)
		if err != nil {
			return nil, fmt.Errorf("rule %d: parse cidr %q: %w", i, r.CIDR, err)
		}
		if len(r.Ports) == 0 {
			return nil, fmt.Errorf("rule %d (%s): ports must not be empty", i, r.CIDR)
		}
		ports := make(map[int]bool, len(r.Ports))
		for _, p := range r.Ports {
			if p < 1 || p > 65535 {
				return nil, fmt.Errorf("rule %d (%s): port %d out of range", i, r.CIDR, p)
			}
			ports[p] = true
		}
		out.rules = append(out.rules, tunnelRule{net: ipNet, ports: ports})
	}
	return out, nil
}

// hardDenyReason names why an address may never be tunnelled to, whatever the
// allow list says. These are not "sensitive networks" in general -- they are
// the addresses that mean something different at jmsd's end of the tunnel than
// they do at the client's, and each one is a way to turn a tunnel into an
// attack on jmsd itself:
//
//   - link-local carries 169.254.169.254, the cloud metadata service. A stream
//     to it returns the instance credentials of the host jmsd runs on.
//   - loopback is jmsd's own localhost, where its Redis lives -- and anything
//     that can write that Redis can create a jmsd:mfa:<user> key and thereby
//     skip any user's MFA gate (see RedisConfig). The control-plane port and,
//     in many deployments, Core's own database sit there too.
//   - unspecified and multicast do not name a single peer at all; what they
//     reach depends on jmsd's routing table rather than on this policy.
//
// It exists as a separate check ahead of the allow list because the allow list
// is operator input, and "cidr: 0.0.0.0/0" typed once in a hurry must not be
// able to open these.
func hardDenyReason(ip net.IP) string {
	switch {
	case ip.IsUnspecified():
		return "unspecified address"
	case ip.IsLoopback():
		return "loopback (jmsd's own host)"
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return "link-local (cloud metadata)"
	case ip.IsMulticast():
		return "multicast"
	}
	return ""
}

// allow reports whether ip:port may be dialled. The error is what gets logged
// and, in redacted form, sent back as the channel rejection -- so it names the
// reason rather than just failing.
func (p *tunnelPolicy) allow(ip net.IP, port int) error {
	if reason := hardDenyReason(ip); reason != "" {
		return fmt.Errorf("%s:%d refused: %s", ip, port, reason)
	}
	// ponytail: no rules = any destination (hard denies above still apply).
	// Write allow rules to narrow it once a tunnel needs a boundary.
	if len(p.rules) == 0 {
		return nil
	}
	for _, r := range p.rules {
		if r.net.Contains(ip) {
			if r.ports[port] {
				return nil
			}
			// Keep looking: overlapping CIDRs with different port lists are a
			// legitimate way to express "this subnet plus one extra port on
			// one host in it", and returning here would let rule order decide.
			continue
		}
	}
	return fmt.Errorf("%s:%d refused: not in tunnel.allow", ip, port)
}

// resolveTunnelTarget turns the host a client asked for into the single IP that
// will be dialled, and it is the caller's job to dial exactly that IP.
//
// Dialling the checked IP -- never the name again -- is the whole rebinding
// defence. Checking a name and then dialling it resolves twice, and the second
// lookup belongs to whoever controls the record: the classic hole where
// 10.20.0.5 passes the check and 127.0.0.1 gets connected. Because the address
// is pinned here, that hole is closed regardless of what else the name
// resolves to.
//
// So the policy picks the first address that PASSES, rather than demanding
// that every address does. Requiring all of them looks stricter and buys
// nothing on top of pinning -- while breaking almost every real name: a
// dual-stack host answers with both A and AAAA records, so an allow list
// covering only IPv4 refused www.example.com outright even though its A record
// was permitted. The refusal then read "not in tunnel.allow" against an IPv6
// address the operator never knowingly excluded.
//
// The error names the last address that failed, since with nothing permitted
// that is the most specific thing there is to report.
func resolveTunnelTarget(ctx context.Context, lookup lookupIPFunc, policy *tunnelPolicy, host string, port int) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if err := policy.allow(ip, port); err != nil {
			return nil, err
		}
		return ip, nil
	}
	addrs, err := lookup(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}
	var lastErr error
	for _, ip := range addrs {
		if err := policy.allow(ip, port); err != nil {
			lastErr = err
			continue
		}
		return ip, nil
	}
	return nil, fmt.Errorf("%s -> %w", host, lastErr)
}
