package notifications

import (
	"encoding/json"
	"testing"
)

func TestThresholdConfigurationAndExactHysteresis(t *testing.T) {
	c, err := ParseRuleConfiguration("provider.error_rate", json.RawMessage(`{"threshold":"0.2"}`))
	if err != nil || c.RecoveryThreshold != "0.16" || c.WindowSeconds != 300 || c.MinimumSamples != 20 || c.CooldownSeconds != 900 {
		t.Fatalf("configuration=%+v error=%v", c, err)
	}
	for _, test := range []struct {
		value  string
		active bool
		want   bool
	}{{"0.199999999999", false, false}, {"0.2", false, true}, {"0.17", true, true}, {"0.16", true, false}, {"0.15", true, false}} {
		if got, err := c.ThresholdState(test.value, test.active); err != nil || got != test.want {
			t.Fatalf("measurement=%s active=%t got=%t error=%v", test.value, test.active, got, err)
		}
	}
}

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

func TestAllNotificationEventsHaveAValidDefaultConfiguration(t *testing.T) {
	for _, event := range Events {
		if _, err := ParseRuleConfiguration(event, nil); err != nil {
			t.Errorf("%s: %v", event, err)
		}
	}
}
