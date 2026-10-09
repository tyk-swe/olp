package access

import (
	"encoding/json"
	"testing"
)

func TestRegionalLimitsInheritDefaultsAndLeaveGlobalCostsUnchanged(t *testing.T) {
	defaultRPM, westRPM, tokens, concurrency := int64(10), int64(4), int64(100), int64(3)
	daily := "1.00"
	policy := KeyPolicy{RequestsPerMinute: &defaultRPM, TokensPerMinute: &tokens, MaxConcurrency: &concurrency, DailyCostLimit: &daily, RegionalLimits: RegionalLimits{"west": {RequestsPerMinute: &westRPM}}}
	west := policy.InRegion("west")
	if *west.RequestsPerMinute != 4 || *west.TokensPerMinute != 100 || *west.MaxConcurrency != 3 || *west.DailyCostLimit != daily {
		t.Fatalf("west policy = %+v", west)
	}
	for _, region := range []string{"east", ""} {
		if *policy.InRegion(region).RequestsPerMinute != 10 {
			t.Fatal("default limit was divided")
		}
	}
	if *policy.RequestsPerMinute != 10 {
		t.Fatal("regional normalization mutated default policy")
	}
}

func TestRegionalLimitOverridesRejectInvalidNamesCountsAndCostDimensions(t *testing.T) {
	for _, raw := range []string{`{"west":{"daily_cost_limit":"1"}}`, `{"west":{"unexpected":1}}`} {
		var limits RegionalLimits
		if err := json.Unmarshal([]byte(raw), &limits); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, raw := range []string{`{"West":{}}`, `{"west":{"requests_per_minute":0}}`, `{"west":{"tokens_per_minute":9007199254740992}}`} {
		var limits RegionalLimits
		if err := json.Unmarshal([]byte(raw), &limits); err != nil {
			t.Fatal(err)
		}
		if err := limits.Validate(); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
