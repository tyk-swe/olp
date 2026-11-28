package apikeys

import (
	"net/netip"

	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/keyauthority"
)

func validateKeyCIDRs(cidrs []string) error {
	if len(cidrs) > keyauthority.MaxKeyCIDRs {
		return problem.Invalid("allowed_cidrs", "Use at most 64 CIDR ranges.")
	}
	seen := make(map[netip.Prefix]bool, len(cidrs))
	for _, value := range cidrs {
		prefix, err := netip.ParsePrefix(value)
		if err != nil || prefix.Addr().Is4In6() {
			return problem.Invalid("allowed_cidrs", "Use IPv4 or IPv6 CIDR ranges; express IPv4-mapped addresses as IPv4.")
		}
		prefix = prefix.Masked()
		if seen[prefix] {
			return problem.Invalid("allowed_cidrs", "Use unique CIDR ranges.")
		}
		seen[prefix] = true
	}
	return nil
}
