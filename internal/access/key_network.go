package access

import "net/netip"

const maxKeyCIDRs = 64

func validateKeyCIDRs(cidrs []string) error {
	if len(cidrs) > maxKeyCIDRs {
		return Invalid("allowed_cidrs", "Use at most 64 CIDR ranges.")
	}
	seen := make(map[netip.Prefix]bool, len(cidrs))
	for _, value := range cidrs {
		prefix, err := netip.ParsePrefix(value)
		if err != nil || prefix.Addr().Is4In6() {
			return Invalid("allowed_cidrs", "Use IPv4 or IPv6 CIDR ranges; express IPv4-mapped addresses as IPv4.")
		}
		prefix = prefix.Masked()
		if seen[prefix] {
			return Invalid("allowed_cidrs", "Use unique CIDR ranges.")
		}
		seen[prefix] = true
	}
	return nil
}

// AllowsClientIP evaluates the address resolved by the gateway's trusted-proxy
// policy. Empty ranges impose no network restriction. Invalid persisted policy
// or an unparseable client address fails closed for restricted keys.
func (a Authority) AllowsClientIP(clientIP string) bool {
	if len(a.Policy.AllowedCIDRs) == 0 {
		return true
	}
	addr, err := netip.ParseAddr(clientIP)
	if err != nil || addr.Zone() != "" || len(a.Policy.AllowedCIDRs) > maxKeyCIDRs {
		return false
	}
	allowed := false
	for _, value := range a.Policy.AllowedCIDRs {
		prefix, err := netip.ParsePrefix(value)
		if err != nil || prefix.Addr().Is4In6() {
			return false
		}
		allowed = allowed || prefix.Contains(addr.Unmap())
	}
	return allowed
}
