package codexwire_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codexwire"
)

func TestAllowanceHeadersStampPercentPassiveWindows(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	headers := http.Header{}
	headers.Set("X-Codex-Primary-Used-Percent", "25")
	headers.Set("X-Codex-Primary-Window-Minutes", "300")
	headers.Set("X-Codex-Primary-Reset-At", "1794000000")
	headers.Set("X-Codex-Secondary-Used-Percent", "10")
	allowance := codexwire.Allowance(headers, now)
	if allowance == nil || len(allowance.Windows) != 2 {
		t.Fatalf("allowance = %+v", allowance)
	}
	for _, window := range allowance.Windows {
		if window.Unit != "percent" || window.Evidence != "passive" {
			t.Fatalf("window not stamped percent/passive: %+v", window)
		}
		if window.UsedValue != nil || window.RemainingValue != nil {
			t.Fatalf("percent window gained vendor values: %+v", window)
		}
	}
	primary := allowance.Windows[0]
	if primary.Window != "primary" || primary.UsedPercent != 25 || primary.RemainingPercent != 75 || primary.ResetsAt == nil || !primary.ResetsAt.Equal(time.Unix(1794000000, 0).UTC()) {
		t.Fatalf("primary window changed: %+v", primary)
	}
}

func TestCodexProbesIgnoreWidenedWindowNames(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	headers := http.Header{}
	headers.Set("X-Codex-Primary-Used-Percent", "25")
	headers.Set("X-Codex-Five_hour-Used-Percent", "50")
	headers.Set("X-Codex-Weekly-Used-Percent", "60")
	headers.Set("X-Codex-Monthly-Used-Percent", "70")
	allowance := codexwire.Allowance(headers, now)
	if allowance == nil || len(allowance.Windows) != 1 || allowance.Windows[0].Window != "primary" {
		t.Fatalf("header allowance = %+v", allowance)
	}
	body := []byte(`{"type":"codex.rate_limits","limit_name":"codex","rate_limits":{"primary":{"used_percent":40},"five_hour":{"used_percent":50},"weekly":{"used_percent":60},"monthly":{"used_percent":70}}}`)
	observation := codexwire.Observe(body, false)
	if observation.Allowance == nil || len(observation.Allowance.Windows) != 1 || observation.Allowance.Windows[0].Window != "primary" {
		t.Fatalf("event allowance = %+v", observation.Allowance)
	}
	widenedOnly := []byte(`{"type":"codex.rate_limits","limit_name":"codex","rate_limits":{"five_hour":{"used_percent":50},"weekly":{"used_percent":60},"monthly":{"used_percent":70}}}`)
	if got := codexwire.Observe(widenedOnly, false).Allowance; got != nil {
		t.Fatalf("widened-only event allowance = %+v", got)
	}
}

func TestRateLimitEventStampsPercentPassiveWindows(t *testing.T) {
	body := []byte(`{"type":"codex.rate_limits","limit_name":"codex","rate_limits":{"primary":{"used_percent":40,"window_minutes":300,"reset_at":1794000000}}}`)
	observation := codexwire.Observe(body, false)
	if observation.Allowance == nil || len(observation.Allowance.Windows) != 1 {
		t.Fatalf("allowance = %+v", observation.Allowance)
	}
	window := observation.Allowance.Windows[0]
	if window.Unit != "percent" || window.Evidence != "passive" || window.RemainingPercent != 60 {
		t.Fatalf("window = %+v", window)
	}
}

// Fixture shape per https://opencode.ai/v2/docs/console/api/inference/ (fetched 2026-11-05): Responses streams carry usage.cost and usage.opencode on response.completed; allowance is Go only, in dollars of the plan monthly limit.
func TestResponsesCompletedReportsOpenCodeAllowanceCharge(t *testing.T) {
	body := `{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":1200,"output_tokens":300,"total_tokens":1500,"cost":0.00412,"opencode":{"billing_source":"go","list_cost":0.00412,"allowance":0.0021,"tokens":{"input":1200,"output":300}}}}}`
	observation := codexwire.Observe([]byte(body), false)
	if !observation.Terminal || !observation.Successful {
		t.Fatalf("observation = %+v", observation)
	}
	var charge map[string]any
	if observation.AllowanceCharge == nil || json.Unmarshal(observation.AllowanceCharge, &charge) != nil {
		t.Fatalf("charge = %s, want the reported vendor object", observation.AllowanceCharge)
	}
	if charge["billing_source"] != "go" || charge["allowance"] != 0.0021 || charge["cost"] != 0.00412 {
		t.Fatalf("charge = %v, want the vendor object as reported plus usage.cost", charge)
	}
}

func TestResponsesUnaryReportsOpenCodeAllowanceCharge(t *testing.T) {
	body := `{"id":"resp_2","status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5,"opencode":{"billing_source":"go","allowance":0.0021}}}`
	observation := codexwire.Observe([]byte(body), true)
	var charge map[string]any
	if observation.AllowanceCharge == nil || json.Unmarshal(observation.AllowanceCharge, &charge) != nil || charge["allowance"] != 0.0021 {
		t.Fatalf("charge = %s, want the reported vendor object", observation.AllowanceCharge)
	}
	if _, present := charge["cost"]; present {
		t.Fatalf("charge = %v, want no invented cost member", charge)
	}
}

func TestResponsesWithoutOpenCodeObjectKeepsChargeUnknown(t *testing.T) {
	body := `{"type":"response.completed","response":{"id":"resp_3","status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`
	observation := codexwire.Observe([]byte(body), false)
	if observation.AllowanceCharge != nil {
		t.Fatalf("charge = %s, want nil (unknown, never zero)", observation.AllowanceCharge)
	}
}
