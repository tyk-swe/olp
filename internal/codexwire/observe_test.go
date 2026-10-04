package codexwire

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
)

func TestAllowanceObservesDistinctWindowsAndCredits(t *testing.T) {
	now := time.Now().UTC()
	h := http.Header{}
	for name, value := range map[string]string{
		"x-codex-primary-used-percent": "20", "x-codex-primary-window-minutes": "300", "x-codex-primary-reset-at": "1900000000",
		"x-codex-secondary-used-percent": "100", "x-codex-secondary-window-minutes": "10080", "x-codex-secondary-reset-at": "1900500000",
		"x-codex-other-secondary-used-percent": "101", "x-codex-other-secondary-reset-at": "1900600000",
		"x-codex-credits-has-credits": "true", "x-codex-credits-unlimited": "0", "x-codex-credits-balance": "12.50",
		"authorization": "secret-canary", "x-codex-promo-message": "payload-canary",
	} {
		h.Set(name, value)
	}
	a := Allowance(h, now)
	if a == nil || len(a.Windows) != 3 || *a.RemainingPercent != 80 || a.Validate() != nil {
		t.Fatalf("allowance: %+v", a)
	}
	if w := a.Windows[1]; w.LimitID != "codex" || w.Window != "secondary" || w.RemainingPercent != 0 || *w.WindowMinutes != 10080 || w.ResetsAt.Unix() != 1900500000 {
		t.Fatalf("secondary: %+v", w)
	}
	if w := a.Windows[2]; w.LimitID != "codex_other" || w.RemainingPercent != 0 || w.UsedPercent != 101 {
		t.Fatalf("metered overage: %+v", w)
	}
	if a.Credits == nil || !a.Credits.HasCredits || a.Credits.Unlimited || *a.Credits.Balance != "12.50" {
		t.Fatalf("credits: %+v", a.Credits)
	}
	encoded, _ := json.Marshal(a)
	if strings.Contains(string(encoded), "canary") || h.Get("Authorization") != "secret-canary" {
		t.Fatal("raw metadata was retained or input changed")
	}
}

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

func TestAllowanceRejectsAmbiguousAndInvalidMetadata(t *testing.T) {
	now := time.Now().UTC()
	for _, h := range []http.Header{
		{"X-Codex-Secondary-Used-Percent": {"10", "100"}},
		{"X-Codex-Secondary-Used-Percent": {"NaN"}},
		{"X-Codex-Secondary-Used-Percent": {"-2"}},
		{"X-Codex-Secondary-Used-Percent": {"9007199254740992"}},
		{"X-Codex-Credits-Has-Credits": {"t"}, "X-Codex-Credits-Unlimited": {"false"}},
	} {
		if a := Allowance(h, now); a != nil {
			t.Fatalf("invalid allowance accepted: %+v", a)
		}
	}
	a := Allowance(http.Header{"X-Codex-Secondary-Used-Percent": {"100"}, "X-Codex-Secondary-Reset-At": {"253402300800"}}, now)
	if a == nil || a.Windows[0].ResetsAt != nil {
		t.Fatal("invalid reset must not invent recovery")
	}
	invalid := codemode.Allowance{ObservedAt: now, Windows: []codemode.AllowanceWindow{{LimitID: "secret\ncanary", Window: "primary", ObservedAt: now}}}
	if invalid.Validate() == nil {
		t.Fatal("unbounded limit identifier accepted")
	}
	if o := Observe([]byte(`{"type":"error","headers":{"x-codex-secondary-used-percent":"100","X-Codex-Secondary-Used-Percent":"10"}}`), false); o.Allowance != nil {
		t.Fatal("ambiguous embedded header values were trusted")
	}
}

func TestAllowanceIgnoresEmptyHeaderWindowsButKeepsReportedZeroUsage(t *testing.T) {
	now := time.Now().UTC()
	headers := http.Header{"X-Codex-Primary-Used-Percent": {"0"}, "X-Codex-Primary-Window-Minutes": {"0"}}
	if a := Allowance(headers, now); a != nil {
		t.Fatal("empty HTTP window could clear an observed exhaustion")
	}
	headers.Set("X-Codex-Primary-Window-Minutes", "300")
	if a := Allowance(headers, now); a == nil || len(a.Windows) != 1 || a.Windows[0].RemainingPercent != 100 {
		t.Fatal("reported zero usage with a window duration was lost")
	}
	o := Observe([]byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":0}}}`), false)
	if o.Allowance == nil || len(o.Allowance.Windows) != 1 || o.Allowance.Windows[0].RemainingPercent != 100 {
		t.Fatal("explicit WebSocket zero usage was lost")
	}
}

func TestOutcomeAndSuccessRemainSeparateFromUsage(t *testing.T) {
	for _, tc := range []struct {
		body              string
		unary, successful bool
		kind              string
		status            int
	}{
		{`{"type":"response.completed","response":{"id":"one"}}`, false, true, "completed", 0},
		{`{"object":"response","status":"completed"}`, true, true, "completed", 0},
		{`{"object":"response.compaction","output":[]}`, true, true, "completed", 0},
		{`{"object":"response.compaction","status":"failed"}`, true, false, "failed", 0},
		{`{"object":"response.compaction","error":{"message":"secret-canary"}}`, true, false, "failed", 0},
		{`{"type":"response.incomplete","response":{}}`, false, false, "incomplete", 0},
		{`{"type":"response.failed","response":{"error":{"message":"secret-canary"}}}`, false, false, "failed", 0},
		{`{"type":"error","status":401,"error":{"message":"secret-canary"}}`, false, false, "failed", 401},
		{`{"type":"error","status_code":429}`, false, false, "failed", 429},
		{`{"type":"error","status":500}`, false, false, "failed", 500},
	} {
		o := Observe([]byte(tc.body), tc.unary)
		if !o.Terminal || o.Successful != tc.successful || o.Usage.Total != nil || o.Outcome == nil || o.Outcome.Kind != tc.kind || o.Status != tc.status || o.Outcome.Validate() != nil {
			t.Fatalf("%s: %+v", tc.body, o)
		}
	}
	for _, body := range []string{`{"type":"response.created"}`, `{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":0}}}`, `{}`} {
		if o := Observe([]byte(body), false); o.Successful || o.Outcome != nil {
			t.Fatalf("non-generation reported success: %+v", o)
		}
	}
	if o := Observe([]byte(`{"object":"response.compaction","status":"in_progress"}`), true); o.Successful || o.Terminal || o.Outcome != nil {
		t.Fatalf("unfinished compaction reported an outcome: %+v", o)
	}
}
