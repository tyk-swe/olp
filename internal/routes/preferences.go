package routes

import (
	"encoding/json"

	"github.com/tyk-swe/olp/internal/access"
)

// Preferences is the request-level routing contract shared by simulations and
// playground execution. Only weighted selection and the fallback switch are
// available until routing policies arrive in M5.
type Preferences struct {
	Strategy                 *string         `json:"strategy"`
	AllowFallbacks           *bool           `json:"allow_fallbacks"`
	Order                    []string        `json:"order"`
	PreferredMaxLatencyMS    *int64          `json:"preferred_max_latency_ms"`
	PreferredMinThroughput   *float64        `json:"preferred_min_throughput"`
	DenyDataCollection       *bool           `json:"deny_data_collection"`
	Ignore                   []string        `json:"ignore"`
	MaxPrice                 json.RawMessage `json:"max_price"`
	Only                     []string        `json:"only"`
	Quantizations            []string        `json:"quantizations"`
	Regions                  []string        `json:"regions"`
	RequireParameters        *bool           `json:"require_parameters"`
	RequireZeroDataRetention *bool           `json:"require_zero_data_retention"`
}

// Validate rejects preferences that execution cannot honor.
func (p *Preferences) Validate(field string) error {
	if p == nil {
		return nil
	}
	if p.Strategy != nil && *p.Strategy != "weighted" {
		return access.Invalid(field+".strategy", "only the weighted routing strategy is available")
	}
	constrained := len(p.Order) > 0 || len(p.Ignore) > 0 || len(p.Only) > 0 ||
		len(p.Quantizations) > 0 || len(p.Regions) > 0 ||
		p.PreferredMaxLatencyMS != nil || p.PreferredMinThroughput != nil ||
		(len(p.MaxPrice) > 0 && string(p.MaxPrice) != "null") ||
		(p.DenyDataCollection != nil && *p.DenyDataCollection) ||
		(p.RequireParameters != nil && *p.RequireParameters) ||
		(p.RequireZeroDataRetention != nil && *p.RequireZeroDataRetention)
	if constrained {
		return access.Invalid(field, "routing constraints and ordering preferences are not available yet")
	}
	return nil
}

// Budget applies the fallback switch to the route's attempt budget.
func (p *Preferences) Budget(published int) int {
	if p != nil && p.AllowFallbacks != nil && !*p.AllowFallbacks {
		return 1
	}
	return published
}
