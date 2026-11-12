package notifications

import (
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

func defaults(t *testing.T, event contract.NotificationEvent) ruleConfiguration {
	t.Helper()
	configuration, err := events[event].configuration.parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	return configuration
}

func TestSignalMeasurementsKeepUnknownSeparateFromRecovery(t *testing.T) {
	project, p95, ratio := "01990000-0000-7000-8000-000000000004", "290", "0.25"
	latency := func(samples int64, milliseconds *string) func(*testing.T) (notificationSignal, error) {
		return func(t *testing.T) (notificationSignal, error) {
			rule := signalRule{event: contract.NotificationEventRouteLatency, configuration: defaults(t, contract.NotificationEventRouteLatency), project: &project}
			return latencySignal(rule, reports.RouteLatencyObservation{Route: "route", Samples: samples, Milliseconds: milliseconds}), nil
		}
	}
	scan := func(read func(pgx.Row) (notificationSignal, error), row signalTestRow) func(*testing.T) (notificationSignal, error) {
		return func(*testing.T) (notificationSignal, error) { return read(row) }
	}
	configured := func(event contract.NotificationEvent, read func(pgx.Row, ruleConfiguration) (notificationSignal, error), row signalTestRow) func(*testing.T) (notificationSignal, error) {
		return func(t *testing.T) (notificationSignal, error) { return read(row, defaults(t, event)) }
	}
	for _, test := range []struct {
		name, subject string
		signal        func(*testing.T) (notificationSignal, error)
		known, active bool
		measurement   *string
	}{
		{"latency enough samples", "route", latency(20, &p95), true, false, &p95},
		{"latency no usage", "route", latency(0, nil), false, false, nil},
		{"latency insufficient samples", "route", latency(19, &p95), false, false, &p95},
		{"provider enough samples", "provider", configured(contract.NotificationEventProviderErrorRate, scanProviderError, signalTestRow{"provider", "Provider", int64(20), &ratio}), true, false, &ratio},
		{"provider no attempts", "provider", configured(contract.NotificationEventProviderErrorRate, scanProviderError, signalTestRow{"provider", "Provider", int64(0), nil}), false, false, nil},
		{"credential enough samples", "credential", configured(contract.NotificationEventProviderCredentialFailing, scanCredentialFailure, signalTestRow{"credential", "provider", int64(3), int64(3)}), true, false, new("3")},
		{"credential insufficient samples", "credential", configured(contract.NotificationEventProviderCredentialFailing, scanCredentialFailure, signalTestRow{"credential", "provider", int64(2), int64(2)}), false, false, new("2")},
		{"worker at bound", "export_delivery", scan(scanWorkerStale, signalTestRow{"export_delivery", "", "30"}), true, false, nil},
		{"worker stale", "export_delivery", scan(scanWorkerStale, signalTestRow{"export_delivery", "", "30.000001"}), true, true, nil},
		{"fast worker at bound", string(workerhealth.TaskGrantRefresh), scan(scanWorkerStale, signalTestRow{string(workerhealth.TaskGrantRefresh), "", "20"}), true, false, nil},
		{"fast worker stale", string(workerhealth.TaskGrantRefresh), scan(scanWorkerStale, signalTestRow{string(workerhealth.TaskGrantRefresh), "", "20.000001"}), true, true, nil},
		{"slow worker at bound", string(workerhealth.TaskBillingImport), scan(scanWorkerStale, signalTestRow{string(workerhealth.TaskBillingImport), "", "180"}), true, false, nil},
		{"slow worker stale", string(workerhealth.TaskBillingImport), scan(scanWorkerStale, signalTestRow{string(workerhealth.TaskBillingImport), "", "180.000001"}), true, true, nil},
		{"unknown worker", "future_task", scan(scanWorkerStale, signalTestRow{"future_task", "", "1000"}), false, true, nil},
		{"runtime failed", "gateway", scan(scanRuntimeInstall, signalTestRow{"gateway", int64(4), int64(3), true}), true, true, nil},
		{"runtime installed", "gateway", scan(scanRuntimeInstall, signalTestRow{"gateway", int64(4), int64(4), true}), true, false, nil},
		{"budget exact limit", "project:id:day:7", scan(scanBudgetExhausted, signalTestRow{"project", "id", &project, "Project", "day", int64(7), "20", "20", true}), true, true, nil},
		{"budget under limit", "project:id:day:7", scan(scanBudgetExhausted, signalTestRow{"project", "id", &project, "Project", "day", int64(7), "19.999999999999", "20", true}), true, false, nil},
		{"weekly unknown", "project:id:week:8", scan(scanBudgetExhausted, signalTestRow{"project", "id", &project, "Project", "week", int64(8), "0", "20", false}), false, false, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			signal, err := test.signal(t)
			if err != nil || signal.subject != test.subject || signal.known != test.known || signal.active != test.active {
				t.Fatalf("signal %+v, error %v", signal, err)
			}
			if test.measurement != nil && (signal.measurement == nil || *signal.measurement != *test.measurement) {
				t.Fatalf("measurement %v, want %s", signal.measurement, *test.measurement)
			}
		})
	}
}

func TestProjectScopedLatencyKeepsRuleProject(t *testing.T) {
	project := "01990000-0000-7000-8000-000000000004"
	rule := signalRule{event: contract.NotificationEventRouteLatency, configuration: defaults(t, contract.NotificationEventRouteLatency), project: &project}
	signal := latencySignal(rule, reports.RouteLatencyObservation{Route: "route", Samples: 20})
	if signal.evidence["project_id"] != &project {
		t.Fatal("global route measurement lost its project-scoped rule boundary")
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
	for _, test := range []struct {
		event contract.NotificationEvent
		steps []signalStep
	}{
		{contract.NotificationEventProviderErrorRate, []signalStep{
			{at: 0, value: "0.09"},
			{at: time.Minute, value: "0.1", active: true, trigger: true, incident: 1},
			{at: 2 * time.Minute, value: "0.06", active: true, incident: 1},
			{at: 3 * time.Minute, value: "0.4", active: true, incident: 1},
			{at: 16 * time.Minute, value: "0.4", active: true, trigger: true, incident: 1},
			{at: 17 * time.Minute, value: "0.05", recovered: true, incident: 1},
			{at: 18 * time.Minute, value: "0.07", incident: 1},
			{at: 40 * time.Minute, value: "0.2", active: true, trigger: true, incident: 2},
		}},
		{contract.NotificationEventProviderCredentialFailing, []signalStep{
			{at: 0, value: "2"},
			{at: time.Minute, value: "3", active: true, trigger: true, incident: 1},
			{at: 2 * time.Minute, value: "1", active: true, incident: 1},
			{at: 3 * time.Minute, value: "0", recovered: true, incident: 1},
		}},
		{contract.NotificationEventRouteLatency, []signalStep{
			{at: 0, value: "999.9"},
			{at: time.Minute, value: "1000", active: true, trigger: true, incident: 1},
			{at: 2 * time.Minute, value: "800.1", active: true, incident: 1},
			{at: 3 * time.Minute, value: "800", recovered: true, incident: 1},
			{at: 4 * time.Minute, value: "900", incident: 1},
		}},
	} {
		t.Run(string(test.event), func(t *testing.T) {
			t.Parallel()
			configuration := defaults(t, test.event)
			start := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
			active, incident, lastTriggerAt := false, int64(0), (*time.Time)(nil)
			for _, step := range test.steps {
				desired, err := configuration.breached(step.value, active)
				if err != nil {
					t.Fatal(err)
				}
				next, err := transitionSignal(active, incident, lastTriggerAt, desired, start.Add(step.at), time.Duration(configuration.CooldownSeconds)*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				if next.active != step.active || next.trigger != step.trigger || next.recover != step.recovered || next.incident != step.incident {
					t.Fatalf("at %s value %s: got active %t trigger %t recover %t incident %d, want %+v",
						step.at, step.value, next.active, next.trigger, next.recover, next.incident, step)
				}
				active, incident, lastTriggerAt = next.active, next.incident, next.lastTriggerAt
			}
		})
	}
}

func TestSignalMeasurementRejectsInvalidValues(t *testing.T) {
	t.Parallel()
	configuration := defaults(t, contract.NotificationEventRouteLatency)
	for _, value := range []string{"", "-1", "fast", "1e400"} {
		for _, active := range []bool{false, true} {
			if _, err := configuration.breached(value, active); err == nil {
				t.Errorf("breached(%q, %t) accepted an invalid measurement", value, active)
			}
		}
	}
}
