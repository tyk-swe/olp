package authority

import "net/netip"

const MaxKeyCIDRs = 64

// AllowsClientIP evaluates the address resolved by the gateway's trusted-proxy
// policy. Empty ranges impose no network restriction. Invalid persisted policy
// or an unparseable client address fails closed for restricted keys.
func (a Authority) AllowsClientIP(clientIP string) bool {
	if len(a.Policy.AllowedCIDRs) == 0 {
		return true
	}
	addr, err := netip.ParseAddr(clientIP)
	if err != nil || addr.Zone() != "" || len(a.Policy.AllowedCIDRs) > MaxKeyCIDRs {
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
