package codexwire

import (
	"testing"
)

func TestAllowanceEventsPreserveMeteredIdentityAndIndependentResetTimes(t *testing.T) {
	o := Observe([]byte(`{"type":"codex.rate_limits","metered_limit_name":"CODEX-OTHER","limit_name":"ignored","rate_limits":{"primary":{"used_percent":20,"window_minutes":300,"reset_at":1900000000},"secondary":{"used_percent":100,"window_minutes":10080,"reset_at":1900500000}},"credits":{"has_credits":false,"unlimited":true,"balance":"secret-canary"}}`), false)
	if o.Terminal || o.Successful || o.Allowance == nil || len(o.Allowance.Windows) != 2 || o.Allowance.RemainingPercent != nil {
		t.Fatalf("observation: %+v", o)
	}
	for _, w := range o.Allowance.Windows {
		if w.LimitID != "codex_other" {
			t.Fatalf("limit identity: %+v", w)
		}
	}
	if o.Allowance.Windows[0].ResetsAt.Equal(*o.Allowance.Windows[1].ResetsAt) || o.Allowance.Credits.Balance != nil {
		t.Fatal("reset identities or credit privacy lost")
	}
	for _, body := range []string{
		`{"type":"codex.rate_limits","credits":{"has_credits":false,"unlimited":false}}`,
		`{"type":"codex.rate_limits","limit_name":"codex-extra","rate_limits":{"secondary":{"used_percent":100}}}`,
		`{"type":"codex.rate_limits","limit_name":"gpt-5.2-codex-sonic","rate_limits":{"primary":{"used_percent":100}}}`,
		`{"type":"error","status":429,"headers":{"x-codex-secondary-used-percent":"100","x-codex-other-primary-used-percent":"45"}}`,
	} {
		o := Observe([]byte(body), false)
		if o.Allowance == nil || o.Allowance.Validate() != nil {
			t.Fatalf("observation lost: %s", body)
		}
	}
}
