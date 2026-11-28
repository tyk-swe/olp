package access

import (
	"github.com/tyk-swe/olp/internal/domain/attribution"
	"github.com/tyk-swe/olp/internal/domain/problem"
)

func ValidateAttributionPolicy(p *attribution.Policy) error {
	if p == nil {
		return nil
	}
	if err := p.Validate(); err != nil {
		return problem.Invalid("attribution_policy", "Use at most four distinct machine-token keys, unique requirements, and valid pinned values.")
	}
	return nil
}
