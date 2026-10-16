package notifications

import (
	"encoding/json"
	"testing"
)

func TestRuleConfigurationRejectsInvalidOrIrrelevantFields(t *testing.T) {
	for _, test := range []struct{ event, raw string }{
		{"provider.error_rate", `{"threshold":"1.01"}`},
		{"route.latency", `{"threshold":"0"}`},
		{"route.latency", `{"threshold":"1e-1000000"}`},
		{"route.latency", `{"threshold":"100","recovery_threshold":"100"}`},
		{"provider.credential.failing", `{"threshold":"2.5"}`},
		{"provider.error_rate", `{"minimum_samples":0}`},
		{"provider.error_rate", `{"window_seconds":59}`},
		{"worker.stale", `{"cooldown_seconds":0}`},
		{"report.spend", `{"period":"hourly"}`},
		{"key.expiring", `{"lead_time_seconds":2678401}`},
		{"provider.circuit.open", `{"threshold":"1"}`},
		{"route.latency", `{"metric":"bytes"}`},
		{"unknown", `{}`},
		{"budget.exhausted", `null`},
	} {
		if _, err := ParseRuleConfiguration(test.event, json.RawMessage(test.raw)); err == nil {
			t.Fatalf("accepted %s %s", test.event, test.raw)
		}
	}
}
