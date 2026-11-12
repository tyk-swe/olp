package notifications

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/management/contract"
)

func storedConfiguration(event contract.NotificationEvent, configuration string) (map[string]any, error) {
	keyKind, day := contract.BudgetSubjectKindApiKey, contract.BudgetWindowKindDay
	keyID, percent := "7f0b6d0e-4b2a-4c55-9d43-0c3c8d6c1a10", 75
	input := ruleInput{Name: "rule", Event: event, DestinationID: "0d2f5f4e-2b8a-4a51-8f3c-5c7a3e9b1d20"}
	if configuration != "" {
		input.Configuration = json.RawMessage(configuration)
	}
	switch event {
	case contract.NotificationEventBudgetThreshold:
		input.SubjectKind, input.SubjectID, input.WindowKind, input.ThresholdPercent = &keyKind, &keyID, &day, &percent
	case contract.NotificationEventKeyExpiring:
		input.SubjectKind, input.SubjectID = &keyKind, &keyID
	}
	target, err := parseRule(input, nil)
	if err != nil {
		return nil, err
	}
	var stored map[string]any
	return stored, json.Unmarshal(target.configuration, &stored)
}

func wantObject(t *testing.T, raw string) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal([]byte(raw), &object); err != nil {
		t.Fatal(err)
	}
	return object
}

func TestRuleConfigurationDefaults(t *testing.T) {
	t.Parallel()
	for event, want := range map[contract.NotificationEvent]string{
		contract.NotificationEventBudgetThreshold:           `{}`,
		contract.NotificationEventProviderGrantLapsed:       `{}`,
		contract.NotificationEventKeyExpiring:               `{"lead_time_seconds":86400}`,
		contract.NotificationEventReportSpend:               `{"period":"daily"}`,
		contract.NotificationEventBudgetExhausted:           `{"cooldown_seconds":900}`,
		contract.NotificationEventProviderErrorRate:         `{"window_seconds":300,"minimum_samples":20,"threshold":"0.1","recovery_threshold":"0.05","cooldown_seconds":900}`,
		contract.NotificationEventProviderCredentialFailing: `{"window_seconds":300,"minimum_samples":3,"threshold":"3","recovery_threshold":"0","cooldown_seconds":900}`,
		contract.NotificationEventRouteLatency:              `{"window_seconds":300,"minimum_samples":20,"threshold":"1000","recovery_threshold":"800","cooldown_seconds":900,"metric":"latency"}`,
		contract.NotificationEventModelRetirement:           `{"lead_days":30,"cooldown_seconds":900}`,
		contract.NotificationEventProviderCircuitOpen:       `{"cooldown_seconds":900}`,
		contract.NotificationEventProviderCircuitClosed:     `{"cooldown_seconds":900}`,
		contract.NotificationEventRuntimeInstallFailed:      `{"cooldown_seconds":900}`,
		contract.NotificationEventWorkerStale:               `{"cooldown_seconds":900}`,
	} {
		for _, configuration := range []string{"", "null", "{}", want} {
			t.Run(string(event)+"/"+configuration, func(t *testing.T) {
				t.Parallel()
				got, err := storedConfiguration(event, configuration)
				if err != nil {
					t.Fatal(err)
				}
				if want := wantObject(t, want); !reflect.DeepEqual(got, want) {
					t.Errorf("stored configuration = %v, want %v", got, want)
				}
			})
		}
	}
}

func TestRuleConfigurationOverrides(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		event         contract.NotificationEvent
		configuration string
		want          string
	}{
		{"lead time lower bound", contract.NotificationEventKeyExpiring, `{"lead_time_seconds":60}`, `{"lead_time_seconds":60}`},
		{"lead time upper bound", contract.NotificationEventKeyExpiring, `{"lead_time_seconds":2678400}`, `{"lead_time_seconds":2678400}`},
		{"spend period", contract.NotificationEventReportSpend, `{"period":"monthly"}`, `{"period":"monthly"}`},
		{"cooldown upper bound", contract.NotificationEventBudgetExhausted, `{"cooldown_seconds":86400}`, `{"cooldown_seconds":86400}`},
		{"retirement bounds", contract.NotificationEventModelRetirement, `{"lead_days":366,"cooldown_seconds":60}`, `{"lead_days":366,"cooldown_seconds":60}`},
		{"recovery derived from threshold", contract.NotificationEventProviderErrorRate, `{"threshold":"0.5"}`,
			`{"window_seconds":300,"minimum_samples":20,"threshold":"0.5","recovery_threshold":"0.4","cooldown_seconds":900}`},
		{"error rate of one with zero recovery", contract.NotificationEventProviderErrorRate, `{"threshold":"1","recovery_threshold":"0"}`,
			`{"window_seconds":300,"minimum_samples":20,"threshold":"1","recovery_threshold":"0","cooldown_seconds":900}`},
		{"recovery only below default threshold", contract.NotificationEventProviderErrorRate, `{"recovery_threshold":"0.01"}`,
			`{"window_seconds":300,"minimum_samples":20,"threshold":"0.1","recovery_threshold":"0.01","cooldown_seconds":900}`},
		{"non-canonical thresholds", contract.NotificationEventProviderErrorRate, `{"threshold":"0.10","recovery_threshold":"0.050"}`,
			`{"window_seconds":300,"minimum_samples":20,"threshold":"0.1","recovery_threshold":"0.05","cooldown_seconds":900}`},
		{"integral credential spelling", contract.NotificationEventProviderCredentialFailing, `{"threshold":"3.0"}`,
			`{"window_seconds":300,"minimum_samples":3,"threshold":"3","recovery_threshold":"2.4","cooldown_seconds":900}`},
		{"credential failures", contract.NotificationEventProviderCredentialFailing, `{"threshold":"5","minimum_samples":10000}`,
			`{"window_seconds":300,"minimum_samples":10000,"threshold":"5","recovery_threshold":"4","cooldown_seconds":900}`},
		{"latency every field", contract.NotificationEventRouteLatency,
			`{"metric":"ttft","threshold":"2000","window_seconds":60,"minimum_samples":1,"cooldown_seconds":60}`,
			`{"window_seconds":60,"minimum_samples":1,"threshold":"2000","recovery_threshold":"1600","cooldown_seconds":60,"metric":"ttft"}`},
		{"latency upper bound", contract.NotificationEventRouteLatency, `{"threshold":"1000000000","window_seconds":86400,"minimum_samples":10000}`,
			`{"window_seconds":86400,"minimum_samples":10000,"threshold":"1000000000","recovery_threshold":"800000000","cooldown_seconds":900,"metric":"latency"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := storedConfiguration(test.event, test.configuration)
			if err != nil {
				t.Fatal(err)
			}
			if want := wantObject(t, test.want); !reflect.DeepEqual(got, want) {
				t.Errorf("stored configuration = %v, want %v", got, want)
			}
		})
	}
}

func TestRuleConfigurationRejects(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		event         contract.NotificationEvent
		configuration string
	}{
		{"array", contract.NotificationEventWorkerStale, `[]`},
		{"string", contract.NotificationEventReportSpend, `"daily"`},
		{"trailing document", contract.NotificationEventWorkerStale, `{"cooldown_seconds":900} {}`},
		{"field on fieldless event", contract.NotificationEventBudgetThreshold, `{"cooldown_seconds":900}`},
		{"field of another event", contract.NotificationEventKeyExpiring, `{"lead_days":3}`},
		{"threshold on cooldown event", contract.NotificationEventWorkerStale, `{"threshold":"1"}`},
		{"wrong case lead time", contract.NotificationEventKeyExpiring, `{"Lead_Time_Seconds":3600}`},
		{"wrong case period", contract.NotificationEventReportSpend, `{"PERIOD":"daily"}`},
		{"wrong case threshold", contract.NotificationEventRouteLatency, `{"Threshold":"10"}`},
		{"null lead time", contract.NotificationEventKeyExpiring, `{"lead_time_seconds":null}`},
		{"null period", contract.NotificationEventReportSpend, `{"period":null}`},
		{"null metric", contract.NotificationEventRouteLatency, `{"metric":null}`},
		{"null threshold", contract.NotificationEventProviderErrorRate, `{"threshold":null}`},
		{"null recovery", contract.NotificationEventProviderErrorRate, `{"recovery_threshold":null}`},
		{"null cooldown", contract.NotificationEventWorkerStale, `{"cooldown_seconds":null}`},
		{"null lead days", contract.NotificationEventModelRetirement, `{"lead_days":null}`},
		{"window too short", contract.NotificationEventRouteLatency, `{"window_seconds":59}`},
		{"window too long", contract.NotificationEventRouteLatency, `{"window_seconds":86401}`},
		{"no samples", contract.NotificationEventProviderErrorRate, `{"minimum_samples":0}`},
		{"too many samples", contract.NotificationEventProviderErrorRate, `{"minimum_samples":10001}`},
		{"cooldown too short", contract.NotificationEventBudgetExhausted, `{"cooldown_seconds":59}`},
		{"cooldown too long", contract.NotificationEventBudgetExhausted, `{"cooldown_seconds":86401}`},
		{"fractional cooldown", contract.NotificationEventBudgetExhausted, `{"cooldown_seconds":900.5}`},
		{"string cooldown", contract.NotificationEventBudgetExhausted, `{"cooldown_seconds":"900"}`},
		{"lead time too short", contract.NotificationEventKeyExpiring, `{"lead_time_seconds":59}`},
		{"lead time too long", contract.NotificationEventKeyExpiring, `{"lead_time_seconds":2678401}`},
		{"no lead days", contract.NotificationEventModelRetirement, `{"lead_days":0}`},
		{"too many lead days", contract.NotificationEventModelRetirement, `{"lead_days":367}`},
		{"unknown period", contract.NotificationEventReportSpend, `{"period":"yearly"}`},
		{"wrong case period value", contract.NotificationEventReportSpend, `{"period":"Daily"}`},
		{"unknown metric", contract.NotificationEventRouteLatency, `{"metric":"tpot"}`},
		{"numeric threshold", contract.NotificationEventRouteLatency, `{"threshold":1500}`},
		{"zero threshold", contract.NotificationEventRouteLatency, `{"threshold":"0"}`},
		{"negative threshold", contract.NotificationEventRouteLatency, `{"threshold":"-5"}`},
		{"non-decimal threshold", contract.NotificationEventRouteLatency, `{"threshold":"fast"}`},
		{"threshold above bound", contract.NotificationEventRouteLatency, `{"threshold":"1000000001"}`},
		{"padded threshold", contract.NotificationEventRouteLatency, `{"threshold":"0000000000000000000000000000001000"}`},
		{"padded recovery", contract.NotificationEventRouteLatency, `{"recovery_threshold":"0000000000000000000000000000000800"}`},
		{"threshold too precise", contract.NotificationEventProviderErrorRate, `{"threshold":"0.0000000000001"}`},
		{"error rate above one", contract.NotificationEventProviderErrorRate, `{"threshold":"1.5"}`},
		{"recovery equal to threshold", contract.NotificationEventProviderErrorRate, `{"threshold":"0.5","recovery_threshold":"0.5"}`},
		{"recovery above threshold", contract.NotificationEventProviderErrorRate, `{"threshold":"0.5","recovery_threshold":"0.6"}`},
		{"recovery equal to default threshold", contract.NotificationEventRouteLatency, `{"recovery_threshold":"1000"}`},
		{"negative recovery", contract.NotificationEventRouteLatency, `{"recovery_threshold":"-1"}`},
		{"fractional credential failures", contract.NotificationEventProviderCredentialFailing, `{"threshold":"3.5"}`},
		{"no credential failures", contract.NotificationEventProviderCredentialFailing, `{"threshold":"0"}`},
		{"too many credential failures", contract.NotificationEventProviderCredentialFailing, `{"threshold":"10001"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got, err := storedConfiguration(test.event, test.configuration); err == nil {
				t.Fatalf("stored configuration %v, want rejection", got)
			}
		})
	}
}
