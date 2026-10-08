package access

import "testing"

func TestRouteLimitsValidateIndependently(t *testing.T) {
	invalid := int64(0)
	valid := int64(3)
	for _, policy := range []RouteLimits{{"bad route": {}}, {"route": {RequestsPerMinute: &invalid}}} {
		if policy.Validate() == nil {
			t.Fatal("accepted invalid limits")
		}
	}
	policy := RouteLimits{"route": {RequestsPerMinute: &valid}, "future": {}}
	if err := policy.Validate(); err != nil || !policy.Limited() || policy.CostBudgeted() {
		t.Fatalf("invalid rate policy: %v", err)
	}
}
