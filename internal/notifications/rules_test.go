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

func TestRuleConfigurationDefaults(t *testing.T) {
	for _, test := range []struct {
		event string
		raw   string
		want  RuleConfiguration
	}{
		{
			event: "provider.credential.failing",
			want:  RuleConfiguration{WindowSeconds: 300, MinimumSamples: 3, Threshold: "3", RecoveryThreshold: "0", CooldownSeconds: 900},
		},
		{
			event: "provider.credential.failing",
			raw:   `{"threshold":"5"}`,
			want:  RuleConfiguration{WindowSeconds: 300, MinimumSamples: 3, Threshold: "5", RecoveryThreshold: "4", CooldownSeconds: 900},
		},
		{
			event: "provider.error_rate",
			want:  RuleConfiguration{WindowSeconds: 300, MinimumSamples: 20, Threshold: "0.1", RecoveryThreshold: "0.05", CooldownSeconds: 900},
		},
		{
			event: "provider.error_rate",
			raw:   `{"threshold":"0.5"}`,
			want:  RuleConfiguration{WindowSeconds: 300, MinimumSamples: 20, Threshold: "0.5", RecoveryThreshold: "0.4", CooldownSeconds: 900},
		},
		{
			event: "route.latency",
			want:  RuleConfiguration{WindowSeconds: 300, MinimumSamples: 20, Threshold: "1000", RecoveryThreshold: "800", CooldownSeconds: 900, Metric: "latency"},
		},
		{
			event: "route.latency",
			raw:   `{"metric":"ttft","threshold":"250","recovery_threshold":"100","window_seconds":600,"minimum_samples":5,"cooldown_seconds":60}`,
			want:  RuleConfiguration{WindowSeconds: 600, MinimumSamples: 5, Threshold: "250", RecoveryThreshold: "100", CooldownSeconds: 60, Metric: "ttft"},
		},
		{event: "key.expiring", want: RuleConfiguration{LeadTimeSeconds: 86400}},
		{event: "model.retirement", want: RuleConfiguration{LeadDays: 30, CooldownSeconds: 900}},
		{event: "report.spend", want: RuleConfiguration{Period: "daily"}},
		{event: "report.spend", raw: `{"period":"weekly"}`, want: RuleConfiguration{Period: "weekly"}},
		{event: "worker.stale", raw: `{"cooldown_seconds":60}`, want: RuleConfiguration{CooldownSeconds: 60}},
		{event: "budget.exhausted", want: RuleConfiguration{CooldownSeconds: 900}},
		{event: "provider.circuit.open", want: RuleConfiguration{CooldownSeconds: 900}},
		{event: "provider.circuit.closed", want: RuleConfiguration{CooldownSeconds: 900}},
		{event: "runtime.install_failed", want: RuleConfiguration{CooldownSeconds: 900}},
		{event: "worker.stale", want: RuleConfiguration{CooldownSeconds: 900}},
		{event: "budget.threshold", want: RuleConfiguration{}},
		{event: "provider.grant.lapsed", raw: `{}`, want: RuleConfiguration{}},
	} {
		t.Run(test.event+"/"+test.raw, func(t *testing.T) {
			got, err := ParseRuleConfiguration(test.event, json.RawMessage(test.raw))
			if err != nil {
				t.Fatalf("ParseRuleConfiguration() error = %v", err)
			}
			if got != test.want {
				t.Errorf("ParseRuleConfiguration() = %#v, want %#v", got, test.want)
			}
		})
	}
}
