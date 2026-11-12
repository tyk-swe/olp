package notifications

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/observability/workerhealth"
	"github.com/tyk-swe/olp/internal/usage/reports"
)

type signalTestRow []any

func (r signalTestRow) Scan(destinations ...any) error {
	if len(r) != len(destinations) {
		return fmt.Errorf("got %d destinations for %d columns", len(destinations), len(r))
	}
	for i, destination := range destinations {
		target := reflect.ValueOf(destination).Elem()
		if r[i] == nil {
			target.SetZero()
			continue
		}
		value := reflect.ValueOf(r[i])
		if !value.Type().AssignableTo(target.Type()) {
			return fmt.Errorf("column %d type %s cannot scan into %s", i, value.Type(), target.Type())
		}
		target.Set(value)
	}
	return nil
}

func resolved[C any](t *testing.T, resolve func(json.RawMessage) (C, error)) C {
	t.Helper()
	configuration, err := resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	return configuration
}

func describe(r reading) string {
	switch r := r.(type) {
	case observed:
		return fmt.Sprintf("observed active=%t", r.active)
	case measured:
		return "measured " + r.value.String()
	default:
		return "unknown"
	}
}

func TestSignalMeasurementsKeepUnknownSeparateFromRecovery(t *testing.T) {
	project, p95, ratio := "01990000-0000-7000-8000-000000000004", "290", "0.25"
	latency := func(samples int64, milliseconds *string) func(*testing.T) (notificationSignal, error) {
		return func(t *testing.T) (notificationSignal, error) {
			rule := signalRule{event: contract.NotificationEventRouteLatency, project: &project}
			return latencySignal(rule, resolved(t, resolveLatency), reports.RouteLatencyObservation{Route: "route", Samples: samples, Milliseconds: milliseconds})
		}
	}
	scan := func(read func(pgx.Row) (notificationSignal, error), row signalTestRow) func(*testing.T) (notificationSignal, error) {
		return func(*testing.T) (notificationSignal, error) { return read(row) }
	}
	configured := func(defaults measuredDefaults, read func(pgx.Row, measuredConfig) (notificationSignal, error), row signalTestRow) func(*testing.T) (notificationSignal, error) {
		return func(t *testing.T) (notificationSignal, error) { return read(row, resolved(t, defaults.resolve)) }
	}
	for _, test := range []struct {
		name, subject string
		signal        func(*testing.T) (notificationSignal, error)
		reading       string
	}{
		{"latency enough samples", "route", latency(20, &p95), "measured 290"},
		{"latency no usage", "route", latency(0, nil), "unknown"},
		{"latency insufficient samples", "route", latency(19, &p95), "unknown"},
		{"provider enough samples", "provider", configured(errorRateDefaults, scanProviderError, signalTestRow{"provider", "Provider", int64(20), &ratio}), "measured 0.25"},
		{"provider no attempts", "provider", configured(errorRateDefaults, scanProviderError, signalTestRow{"provider", "Provider", int64(0), nil}), "unknown"},
		{"credential enough samples", "credential", configured(credentialDefaults, scanCredentialFailure, signalTestRow{"credential", "provider", int64(3), int64(3)}), "measured 3"},
		{"credential insufficient samples", "credential", configured(credentialDefaults, scanCredentialFailure, signalTestRow{"credential", "provider", int64(2), int64(2)}), "unknown"},
		{"worker at bound", "export_delivery", scan(scanWorkerStale, signalTestRow{"export_delivery", "", "30"}), "observed active=false"},
		{"worker stale", "export_delivery", scan(scanWorkerStale, signalTestRow{"export_delivery", "", "30.000001"}), "observed active=true"},
		{"fast worker at bound", string(workerhealth.TaskGrantRefresh), scan(scanWorkerStale, signalTestRow{string(workerhealth.TaskGrantRefresh), "", "20"}), "observed active=false"},
		{"fast worker stale", string(workerhealth.TaskGrantRefresh), scan(scanWorkerStale, signalTestRow{string(workerhealth.TaskGrantRefresh), "", "20.000001"}), "observed active=true"},
		{"slow worker at bound", string(workerhealth.TaskBillingImport), scan(scanWorkerStale, signalTestRow{string(workerhealth.TaskBillingImport), "", "180"}), "observed active=false"},
		{"slow worker stale", string(workerhealth.TaskBillingImport), scan(scanWorkerStale, signalTestRow{string(workerhealth.TaskBillingImport), "", "180.000001"}), "observed active=true"},
		{"unknown worker", "future_task", scan(scanWorkerStale, signalTestRow{"future_task", "", "1000"}), "unknown"},
		{"runtime failed", "gateway", scan(scanRuntimeInstall, signalTestRow{"gateway", int64(4), int64(3), true}), "observed active=true"},
		{"runtime installed", "gateway", scan(scanRuntimeInstall, signalTestRow{"gateway", int64(4), int64(4), true}), "observed active=false"},
		{"budget exact limit", "project:id:day:7", scan(scanBudgetExhausted, signalTestRow{"project", "id", &project, "Project", "day", int64(7), "20", "20", true}), "observed active=true"},
		{"budget under limit", "project:id:day:7", scan(scanBudgetExhausted, signalTestRow{"project", "id", &project, "Project", "day", int64(7), "19.999999999999", "20", true}), "observed active=false"},
		{"weekly unknown", "project:id:week:8", scan(scanBudgetExhausted, signalTestRow{"project", "id", &project, "Project", "week", int64(8), "0", "20", false}), "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			signal, err := test.signal(t)
			if err != nil || signal.subject != test.subject || describe(signal.reading) != test.reading {
				t.Fatalf("signal %s reading %s, error %v, want %s %s", signal.subject, describe(signal.reading), err, test.subject, test.reading)
			}
		})
	}
}

func TestProjectScopedLatencyKeepsRuleProject(t *testing.T) {
	project := "01990000-0000-7000-8000-000000000004"
	rule := signalRule{event: contract.NotificationEventRouteLatency, project: &project}
	signal, err := latencySignal(rule, resolved(t, resolveLatency), reports.RouteLatencyObservation{Route: "route", Samples: 20})
	if err != nil || signal.evidence["project_id"] != &project {
		t.Fatalf("global route measurement lost its project-scoped rule boundary, error %v", err)
	}
}

type signalStep struct {
	at                         time.Duration
	value                      string
	active, trigger, recovered bool
	incident                   int64
}

func TestMeasuredSignalHysteresisAndCooldown(t *testing.T) {
	t.Parallel()
	latency := func(raw json.RawMessage) (measuredConfig, error) {
		configuration, err := resolveLatency(raw)
		return configuration.measuredConfig, err
	}
	for _, test := range []struct {
		event   contract.NotificationEvent
		resolve func(json.RawMessage) (measuredConfig, error)
		steps   []signalStep
	}{
		{contract.NotificationEventProviderErrorRate, errorRateDefaults.resolve, []signalStep{
			{at: 0, value: "0.09"},
			{at: time.Minute, value: "0.1", active: true, trigger: true, incident: 1},
			{at: 2 * time.Minute, value: "0.06", active: true, incident: 1},
			{at: 3 * time.Minute, value: "0.4", active: true, incident: 1},
			{at: 16 * time.Minute, value: "0.4", active: true, trigger: true, incident: 1},
			{at: 17 * time.Minute, value: "0.05", recovered: true, incident: 1},
			{at: 18 * time.Minute, value: "0.07", incident: 1},
			{at: 40 * time.Minute, value: "0.2", active: true, trigger: true, incident: 2},
		}},
		{contract.NotificationEventProviderCredentialFailing, credentialDefaults.resolve, []signalStep{
			{at: 0, value: "2"},
			{at: time.Minute, value: "3", active: true, trigger: true, incident: 1},
			{at: 2 * time.Minute, value: "1", active: true, incident: 1},
			{at: 3 * time.Minute, value: "0", recovered: true, incident: 1},
		}},
		{contract.NotificationEventRouteLatency, latency, []signalStep{
			{at: 0, value: "999.9"},
			{at: time.Minute, value: "1000", active: true, trigger: true, incident: 1},
			{at: 2 * time.Minute, value: "800.1", active: true, incident: 1},
			{at: 3 * time.Minute, value: "800", recovered: true, incident: 1},
			{at: 4 * time.Minute, value: "900", incident: 1},
		}},
	} {
		t.Run(string(test.event), func(t *testing.T) {
			t.Parallel()
			configuration := resolved(t, test.resolve)
			cooldown := time.Duration(configuration.CooldownSeconds) * time.Second
			start := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
			state := signalState{}
			for _, step := range test.steps {
				r, err := configuration.measure(int64(configuration.MinimumSamples), &step.value)
				if err != nil {
					t.Fatal(err)
				}
				next, err := transitionSignal(state, r, start.Add(step.at), cooldown)
				if err != nil {
					t.Fatal(err)
				}
				if next.state.active != step.active || (next.change == triggered) != step.trigger || (next.change == recovered) != step.recovered || next.state.incident != step.incident {
					t.Fatalf("at %s value %s: got %+v, want %+v", step.at, step.value, next, step)
				}
				state = next.state
			}
		})
	}
}

func TestSignalMeasurementRejectsInvalidValues(t *testing.T) {
	t.Parallel()
	configuration := resolved(t, resolveLatency)
	for _, value := range []string{"", "-1", "fast", "1e400"} {
		if _, err := configuration.measure(int64(configuration.MinimumSamples), &value); err == nil {
			t.Errorf("measurement %q was accepted", value)
		}
	}
}
