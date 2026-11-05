package codexwire_test

import (
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
