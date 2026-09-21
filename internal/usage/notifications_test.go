package usage

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestThresholdMetExactDecimal(t *testing.T) {
	str := func(s string) *string { return &s }
	for _, tc := range []struct {
		name      string
		accrued   string
		limit     *string
		threshold int
		want      bool
	}{
		{"below threshold", "9.99", str("10"), 100, false},
		{"at threshold", "10.00", str("10"), 100, true},
		{"above threshold", "10.01", str("10"), 100, true},
		{"below percent", "7.49", str("10"), 75, false},
		{"at percent", "7.50", str("10"), 75, true},
		{"fractional limit", "0.004", str("0.005"), 80, true},
		{"tiny fractional limit below", "0.0039", str("0.005"), 80, false},
		{"nil limit", "99", nil, 50, false},
		{"malformed limit", "99", str("abc"), 50, false},
		{"empty limit", "99", str(""), 50, false},
		{"zero limit never fires", "99", str("0"), 1, false},
		{"negative limit never fires", "99", str("-5"), 1, false},
		{"malformed accrued", "oops", str("10"), 50, false},
		{"zero accrued below any positive limit", "0", str("0.001"), 1, false},
		{"hundred percent exact", "123.456", str("123.456"), 100, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := thresholdMet(tc.accrued, tc.limit, tc.threshold); got != tc.want {
				t.Fatalf("thresholdMet(%q,%v,%d) = %v, want %v", tc.accrued, tc.limit, tc.threshold, got, tc.want)
			}
		})
	}
}

func TestRetryDueBackoff(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	minute := time.Minute
	at := func(d time.Duration) *time.Time {
		v := now.Add(-d)
		return &v
	}
	for _, tc := range []struct {
		name     string
		last     *time.Time
		attempts int
		want     bool
	}{
		{"fresh claim sends now", nil, 0, true},
		{"first retry under one minute waits", at(30 * time.Second), 1, false},
		{"first retry after one minute sends", at(minute), 1, true},
		{"second retry under two minutes waits", at(minute), 2, false},
		{"second retry after two minutes sends", at(2 * minute), 2, true},
		{"third retry under four minutes waits", at(3 * minute), 3, false},
		{"third retry after four minutes sends", at(4 * minute), 3, true},
		{"fourth retry waits eight minutes", at(7 * minute), 4, false},
		{"fourth retry after eight minutes sends", at(8 * minute), 4, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryDue(tc.last, tc.attempts, now); got != tc.want {
				t.Fatalf("retryDue(%v,%d) = %v, want %v", tc.last, tc.attempts, got, tc.want)
			}
		})
	}
}

func TestAlertBodyIsMetadataOnly(t *testing.T) {
	subject := "0195f3c2-0000-7000-8000-0000000000aa"
	limit := "10.000000000000"
	d := delivery{
		id: "0195f3c2-0000-7000-8000-0000000000bb", ruleID: "0195f3c2-0000-7000-8000-0000000000cc",
		windowID: 4421, threshold: 80, ruleName: "warn", subjectKind: "api_key",
		subjectID: &subject, windowKind: "day", accrued: "8.000000000000", limit: &limit,
	}
	body, err := alertBody(d, "USD")
	if err != nil {
		t.Fatalf("alertBody: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if decoded["event"] != "budget.threshold" {
		t.Fatalf("event = %v", decoded["event"])
	}
	if decoded["accrued"] != "8.000000000000" || decoded["limit"] != "10.000000000000" || decoded["currency"] != "USD" {
		t.Fatalf("spend fields = %v", decoded)
	}
	if decoded["window_id"].(float64) != 4421 || decoded["threshold_percent"].(float64) != 80 {
		t.Fatalf("window/threshold = %v", decoded)
	}
	for _, forbidden := range []string{"prompt", "messages", "content", "attribution", "secret", "url"} {
		if _, ok := decoded[forbidden]; ok {
			t.Fatalf("payload leaked %s: %v", forbidden, decoded)
		}
	}
}

func TestDeliveryErrorCodeIsSafe(t *testing.T) {
	timeout := &url.Error{Op: http.MethodPost, URL: "https://hooks.example.com", Err: context.DeadlineExceeded}
	if code := deliveryErrorCode(timeout); code != "timeout" {
		t.Fatalf("timeout code = %s", code)
	}
	if code := deliveryErrorCode(errors.New("dial tcp 10.0.0.1: connection refused")); code != "network" {
		t.Fatalf("network code = %s", code)
	}
	if code := deliveryErrorCode(context.DeadlineExceeded); code != "timeout" {
		t.Fatalf("deadline code = %s", code)
	}
	for _, code := range []string{"timeout", "network", "http_4xx", "http_5xx", "invalid_destination"} {
		if code == "" || len(code) > 32 {
			t.Fatalf("unsafe persisted code %q", code)
		}
	}
}

func TestValidateAttributionBounds(t *testing.T) {
	valid := map[string]string{"cost_center": "eng-01", "feature.flag": "rollout_v2"}
	if err := ValidateAttribution(valid); err != nil {
		t.Fatalf("valid attribution rejected: %v", err)
	}
	if err := ValidateAttribution(map[string]string{}); err != nil {
		t.Fatalf("empty attribution rejected: %v", err)
	}
	for _, tc := range []struct {
		name  string
		value map[string]string
	}{
		{"too many keys", map[string]string{"a": "1", "b": "2", "c": "3", "d": "4", "e": "5"}},
		{"key starts with digit", map[string]string{"1key": "v"}},
		{"key too long", map[string]string{"abcdefghijklmnopqrstuvwxyzabcdefg": "v"}},
		{"key with space", map[string]string{"bad key": "v"}},
		{"key empty", map[string]string{"": "v"}},
		{"value free text", map[string]string{"k": "this is a sentence with spaces"}},
		{"value empty", map[string]string{"k": ""}},
		{"value too long", map[string]string{"k": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
		{"value leading punctuation", map[string]string{"k": ".hidden"}},
		{"value with newline", map[string]string{"k": "line\nbreak"}},
		{"value with json", map[string]string{"k": `{"nested":true}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateAttribution(tc.value); err == nil {
				t.Fatalf("invalid attribution accepted: %v", tc.value)
			}
		})
	}
}

func TestAttributionJSONCanonical(t *testing.T) {
	if got := string(AttributionJSON(nil)); got != "{}" {
		t.Fatalf("nil attribution = %s", got)
	}
	got := string(AttributionJSON(map[string]string{"b": "2", "a": "1"}))
	if got != `{"a":"1","b":"2"}` {
		t.Fatalf("attribution JSON = %s", got)
	}
}
