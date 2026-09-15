package providers

import (
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/limits"
)

func TestQuotaValidationMatchesEnforcement(t *testing.T) {
	for _, tc := range []struct {
		name  string
		quota Limits
		valid bool
	}{
		{"unset", Limits{}, true},
		{"maximum tokens", Limits{TokensPerMinute: ptr(limits.MaxCounter)}, true},
		{"zero requests", Limits{RequestsPerMinute: ptr(int64(0))}, false},
		{"zero concurrency", Limits{MaxConcurrency: ptr(int64(0))}, false},
		{"zero tokens", Limits{TokensPerMinute: ptr(int64(0))}, false},
		{"unsafe tokens", Limits{TokensPerMinute: ptr(limits.MaxCounter + 1)}, false},
		{"oversized requests", Limits{RequestsPerMinute: ptr(int64(1 << 31))}, false},
		{"oversized concurrency", Limits{MaxConcurrency: ptr(int64(1 << 31))}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Configuration{Kind: KindOpenAICompatible, AuthMode: AuthNone,
				Endpoint: ptr("https://example.com/v1"), Options: Options{Limits: &tc.quota}}
			cfg.normalize()
			if err := cfg.validate(&egress.Policy{}); (err == nil) != tc.valid {
				t.Fatalf("connection quota: %v", err)
			}
			slot := slotInput{Name: "slot", RequestsPerMinute: tc.quota.RequestsPerMinute,
				TokensPerMinute: tc.quota.TokensPerMinute, MaxConcurrency: tc.quota.MaxConcurrency}
			if err := validSlot(&slot, ""); (err == nil) != tc.valid {
				t.Fatalf("slot quota: %v", err)
			}
		})
	}
}
