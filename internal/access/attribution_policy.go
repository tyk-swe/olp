package access

import (
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/attribution"
)

func ValidateAttributionPolicy(p *attribution.Policy) error {
	if p == nil {
		return nil
	}
	if err := p.Validate(); err != nil {
		return httpkit.Invalid("attribution_policy", "Use at most four distinct machine-token keys, unique requirements, and valid pinned values.")
	}
	return nil
}
