package notifications

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/observability/workerhealth"
	"github.com/tyk-swe/olp/internal/usage"
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
			return latencySignal(rule, usage.RouteLatencyObservation{Route: "route", Samples: samples, Milliseconds: milliseconds}), nil
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
	signal := latencySignal(rule, usage.RouteLatencyObservation{Route: "route", Samples: 20})
	if signal.evidence["project_id"] != &project {
		t.Fatal("global route measurement lost its project-scoped rule boundary")
	}
}
