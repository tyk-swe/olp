package access

import (
	"github.com/tyk-swe/olp/internal/attribution"
)

func ValidateAttributionPolicy(p *attribution.Policy) error {
	if p == nil {
		return nil
	}
	if err := p.Validate(); err != nil {
		return Invalid("attribution_policy", "Use at most four distinct machine-token keys, unique requirements, and valid pinned values.")
	}
	return nil
}
