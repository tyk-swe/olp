package codeallowance

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
)

// Fixture fields are pinned from the official client openai/codex c0100ad,
// codex-rs/codex-backend-openapi-models RateLimitStatusPayload,
// RateLimitStatusDetails, RateLimitWindowSnapshot and CreditStatusDetails,
// fetched 2026-11-05 from raw.githubusercontent.com.

var usageNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

const validUsage = `{
  "plan_type": "plus",
  "rate_limit": {
    "allowed": true,
    "limit_reached": false,
    "primary_window": {"used_percent": 25, "limit_window_seconds": 18000, "reset_after_seconds": 3600, "reset_at": 1794038400},
    "secondary_window": {"used_percent": 40, "limit_window_seconds": 604800, "reset_after_seconds": 7200, "reset_at": 1794643200}
  },
  "credits": {"has_credits": true, "unlimited": false, "balance": "12.50"},
  "additional_rate_limits": null
}`

func window(t *testing.T, a codemode.Allowance, limit, name string) codemode.AllowanceWindow {
	t.Helper()
	for _, w := range a.Windows {
		if w.LimitID == limit && w.Window == name {
			return w
		}
	}
	t.Fatalf("missing window %s/%s in %+v", limit, name, a.Windows)
	return codemode.AllowanceWindow{}
}

func TestParseUsageMapsPinnedWindowsAndCredits(t *testing.T) {
	allowance, ok := ParseUsage([]byte(validUsage), usageNow)
	if !ok {
		t.Fatal("pinned payload must parse")
	}
	primary := window(t, allowance, "codex", "primary")
	if primary.Unit != codemode.AllowanceUnitPercent || primary.Evidence != codemode.AllowanceEvidenceActive ||
		primary.UsedPercent != 25 || primary.RemainingPercent != 75 ||
		primary.WindowMinutes == nil || *primary.WindowMinutes != 300 ||
		primary.ResetsAt == nil || !primary.ResetsAt.Equal(time.Unix(1794038400, 0).UTC()) ||
		!primary.ObservedAt.Equal(usageNow) {
		t.Fatalf("primary window = %+v", primary)
	}
	secondary := window(t, allowance, "codex", "secondary")
	if secondary.RemainingPercent != 60 || secondary.WindowMinutes == nil || *secondary.WindowMinutes != 10080 {
		t.Fatalf("secondary window = %+v", secondary)
	}
	if allowance.RemainingPercent != nil || allowance.ResetsAt != nil {
		t.Fatalf("unqualified active evidence must not mirror to the top level, got %+v", allowance)
	}
	if allowance.Credits == nil || !allowance.Credits.HasCredits || allowance.Credits.Unlimited ||
		allowance.Credits.Balance == nil || *allowance.Credits.Balance != "12.50" || !allowance.Credits.ObservedAt.Equal(usageNow) {
		t.Fatalf("credits must stay distinct from included allowance, got %+v", allowance.Credits)
	}
	if len(allowance.Windows) != 2 {
		t.Fatalf("credits must not become a window, got %+v", allowance.Windows)
	}
	if err := allowance.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestParseUsageRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"not json":              `{`,
		"trailing data":         validUsage + `{}`,
		"missing plan_type":     `{"rate_limit": null, "credits": null}`,
		"plan_type not string":  `{"plan_type": 3, "rate_limit": null}`,
		"plan_type empty":       `{"plan_type": "", "rate_limit": null}`,
		"rate_limit not object": `{"plan_type": "plus", "rate_limit": "yes"}`,
		"allowed not bool":      `{"plan_type": "plus", "rate_limit": {"allowed": "true", "limit_reached": false, "primary_window": null, "secondary_window": null}}`,
		"limit_reached missing": `{"plan_type": "plus", "rate_limit": {"allowed": true, "primary_window": null, "secondary_window": null}}`,
		"used_percent string":   `{"plan_type": "plus", "rate_limit": {"allowed": true, "limit_reached": false, "primary_window": {"used_percent": "25", "limit_window_seconds": 18000, "reset_after_seconds": 1, "reset_at": 1794038400}, "secondary_window": null}}`,
		"used_percent float":    `{"plan_type": "plus", "rate_limit": {"allowed": true, "limit_reached": false, "primary_window": {"used_percent": 25.5, "limit_window_seconds": 18000, "reset_after_seconds": 1, "reset_at": 1794038400}, "secondary_window": null}}`,
		"used_percent missing":  `{"plan_type": "plus", "rate_limit": {"allowed": true, "limit_reached": false, "primary_window": {"limit_window_seconds": 18000, "reset_after_seconds": 1, "reset_at": 1794038400}, "secondary_window": null}}`,
		"used_percent negative": `{"plan_type": "plus", "rate_limit": {"allowed": true, "limit_reached": false, "primary_window": {"used_percent": -1, "limit_window_seconds": 18000, "reset_after_seconds": 1, "reset_at": 1794038400}, "secondary_window": null}}`,
		"reset_at zero":         `{"plan_type": "plus", "rate_limit": {"allowed": true, "limit_reached": false, "primary_window": {"used_percent": 1, "limit_window_seconds": 18000, "reset_after_seconds": 1, "reset_at": 0}, "secondary_window": null}}`,
		"reset_at string":       `{"plan_type": "plus", "rate_limit": {"allowed": true, "limit_reached": false, "primary_window": {"used_percent": 1, "limit_window_seconds": 18000, "reset_after_seconds": 1, "reset_at": "1794038400"}, "secondary_window": null}}`,
		"seconds string":        `{"plan_type": "plus", "rate_limit": {"allowed": true, "limit_reached": false, "primary_window": {"used_percent": 1, "limit_window_seconds": "18000", "reset_after_seconds": 1, "reset_at": 1794038400}, "secondary_window": null}}`,
		"credits balance num":   `{"plan_type": "plus", "rate_limit": null, "credits": {"has_credits": true, "unlimited": false, "balance": 12.5}}`,
		"credits has string":    `{"plan_type": "plus", "rate_limit": null, "credits": {"has_credits": "true", "unlimited": false, "balance": null}}`,
		"credits bad balance":   `{"plan_type": "plus", "rate_limit": null, "credits": {"has_credits": true, "unlimited": false, "balance": "lots"}}`,
		"additional not array":  `{"plan_type": "plus", "rate_limit": null, "additional_rate_limits": {}}`,
		"everything null":       `{"plan_type": "plus", "rate_limit": null, "credits": null, "additional_rate_limits": null}`,
		"windows null only":     `{"plan_type": "plus", "rate_limit": {"allowed": true, "limit_reached": false, "primary_window": null, "secondary_window": null}, "credits": null}`,
	}
	for name, body := range cases {
		if allowance, ok := ParseUsage([]byte(body), usageNow); ok {
			t.Errorf("%s must write nothing, got %+v", name, allowance)
		}
	}
}

func TestParseUsageCreditsOnlyAndNullableRateLimit(t *testing.T) {
	allowance, ok := ParseUsage([]byte(`{"plan_type": "free", "rate_limit": null, "credits": {"has_credits": false, "unlimited": true, "balance": null}}`), usageNow)
	if !ok || len(allowance.Windows) != 0 || allowance.Credits == nil || !allowance.Credits.Unlimited || allowance.Credits.Balance != nil {
		t.Fatalf("credits-only payload = %+v, ok %v", allowance, ok)
	}
}

func TestParseUsageWindowMinutesOnlyWhenDivisible(t *testing.T) {
	body := `{"plan_type": "pro", "rate_limit": {"allowed": true, "limit_reached": false, "primary_window": {"used_percent": 130, "limit_window_seconds": 17999, "reset_after_seconds": 5, "reset_at": 1794038400}, "secondary_window": null}, "credits": null}`
	allowance, ok := ParseUsage([]byte(body), usageNow)
	if !ok {
		t.Fatal("payload with a non-divisible window must parse")
	}
	primary := window(t, allowance, "codex", "primary")
	if primary.WindowMinutes != nil || primary.RemainingPercent != 0 {
		t.Fatalf("non-divisible seconds keep minutes unknown and over-use floors at zero, got %+v", primary)
	}
}

func TestParseUsageAdditionalLimitsSkippedByName(t *testing.T) {
	body := `{
	  "plan_type": "team",
	  "rate_limit": {"allowed": true, "limit_reached": false, "primary_window": {"used_percent": 10, "limit_window_seconds": 18000, "reset_after_seconds": 1, "reset_at": 1794038400}, "secondary_window": null},
	  "credits": null,
	  "additional_rate_limits": [
	    {"limit_name": "Spark-Extra", "metered_feature": "spark", "rate_limit": {"allowed": true, "limit_reached": false, "primary_window": {"used_percent": 50, "limit_window_seconds": 3600, "reset_after_seconds": 1, "reset_at": 1794038400}, "secondary_window": null}},
	    {"limit_name": "not a limit!", "metered_feature": "x", "rate_limit": {"allowed": true, "limit_reached": false, "primary_window": {"used_percent": 99, "limit_window_seconds": 3600, "reset_after_seconds": 1, "reset_at": 1794038400}, "secondary_window": null}}
	  ]
	}`
	allowance, ok := ParseUsage([]byte(body), usageNow)
	if !ok {
		t.Fatal("an unusable additional limit name must not sink the payload")
	}
	extra := window(t, allowance, "spark_extra", "primary")
	if extra.RemainingPercent != 50 || extra.Evidence != codemode.AllowanceEvidenceActive {
		t.Fatalf("additional window = %+v", extra)
	}
	if len(allowance.Windows) != 2 {
		t.Fatalf("the invalid additional limit must be skipped, got %+v", allowance.Windows)
	}
}

func TestUsageURLIsTheApprovedChatGPTWhamEndpoint(t *testing.T) {
	// openai/codex c0100ad: chatgpt_base_url.rs DEFAULT_CHATGPT_BASE_URL is
	// https://chatgpt.com/backend-api/ and client.rs rate_limit_status_url
	// appends /wham/usage on the ChatGptApi path style, fetched 2026-11-05.
	if UsageURL != "https://chatgpt.com/backend-api/wham/usage" {
		t.Fatalf("usage URL = %q", UsageURL)
	}
}
