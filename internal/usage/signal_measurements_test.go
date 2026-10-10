package usage

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/notifications"
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

func TestSignalMeasurementsKeepUnknownSeparateFromRecovery(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	project, p95, ratio := "01990000-0000-7000-8000-000000000004", "290", "0.25"
	for _, test := range []struct {
		name, event, subject string
		row                  signalTestRow
		known, active        bool
		measurement          *string
	}{
		{"latency enough samples", "route.latency", "route", signalTestRow{"route", nil, int64(20), &p95}, true, false, &p95},
		{"latency no usage", "route.latency", "route", signalTestRow{"route", nil, int64(0), nil}, false, false, nil},
		{"latency insufficient samples", "route.latency", "route", signalTestRow{"route", nil, int64(19), &p95}, false, false, &p95},
		{"provider enough samples", "provider.error_rate", "provider", signalTestRow{"provider", "Provider", int64(20), &ratio}, true, false, &ratio},
		{"provider no attempts", "provider.error_rate", "provider", signalTestRow{"provider", "Provider", int64(0), nil}, false, false, nil},
		{"credential enough samples", "provider.credential.failing", "credential", signalTestRow{"credential", "provider", int64(3), int64(3)}, true, false, nil},
		{"credential insufficient samples", "provider.credential.failing", "credential", signalTestRow{"credential", "provider", int64(2), int64(2)}, false, false, nil},
		{"worker at bound", "worker.stale", "export_delivery", signalTestRow{"export_delivery", "", "30"}, true, false, nil},
		{"worker stale", "worker.stale", "export_delivery", signalTestRow{"export_delivery", "", "30.000001"}, true, true, nil},
		{"unknown worker", "worker.stale", "future_task", signalTestRow{"future_task", "", "1000"}, false, true, nil},
		{"runtime failed", "runtime.install_failed", "gateway", signalTestRow{"gateway", int64(4), int64(3), true}, true, true, nil},
		{"runtime installed", "runtime.install_failed", "gateway", signalTestRow{"gateway", int64(4), int64(4), true}, true, false, nil},
		{"budget exact limit", "budget.exhausted", "project:id:day:7", signalTestRow{"project", "id", &project, "Project", "day", int64(7), "20", "20", true}, true, true, nil},
		{"budget under limit", "budget.exhausted", "project:id:day:7", signalTestRow{"project", "id", &project, "Project", "day", int64(7), "19.999999999999", "20", true}, true, false, nil},
		{"weekly unknown", "budget.exhausted", "project:id:week:8", signalTestRow{"project", "id", &project, "Project", "week", int64(8), "0", "20", false}, false, false, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			configuration, err := notifications.ParseRuleConfiguration(test.event, nil)
			if err != nil {
				t.Fatal(err)
			}
			signal, err := scanRuleSignal(test.row, signalRule{event: test.event, configuration: configuration, project: &project}, now, nil, nil)
			if err != nil || signal.subject != test.subject || signal.known != test.known || signal.active != test.active {
				t.Fatalf("signal %+v, error %v", signal, err)
			}
			if test.measurement != nil && (signal.measurement == nil || *signal.measurement != *test.measurement) {
				t.Fatalf("measurement %v, want %s", signal.measurement, *test.measurement)
			}
			if test.event == "provider.credential.failing" && (signal.measurement == nil || *signal.measurement != fmt.Sprint(test.row[2])) {
				t.Fatalf("credential failures were not retained: %+v", signal)
			}
			if test.event == "route.latency" && signal.evidence["project_id"] != &project {
				t.Fatal("global route measurement lost its project-scoped rule boundary")
			}
		})
	}
}

func TestSignalMeasurementsRejectInvalidStoredBudgetAndWorkerEvidence(t *testing.T) {
	for _, test := range []struct {
		event string
		row   signalTestRow
	}{
		{"worker.stale", signalTestRow{"export_delivery", "", "-1"}},
		{"worker.stale", signalTestRow{"export_delivery", "", "not-a-number"}},
		{"budget.exhausted", signalTestRow{"project", "id", nil, "Project", "day", int64(7), "-1", "20", true}},
		{"budget.exhausted", signalTestRow{"project", "id", nil, "Project", "day", int64(7), "1", "0", true}},
	} {
		if _, err := scanRuleSignal(test.row, signalRule{event: test.event}, time.Now(), nil, nil); err == nil {
			t.Fatalf("accepted invalid %s evidence %v", test.event, test.row)
		}
	}
}
