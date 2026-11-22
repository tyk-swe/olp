package telemetry

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type closingExporter struct {
	err error
}

func (closingExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }
func (e closingExporter) Shutdown(context.Context) error                           { return e.err }

type delayedShutdownContext struct {
	context.Context
	workerDone <-chan struct{}
	firstCheck bool
}

func (ctx *delayedShutdownContext) Done() <-chan struct{} {
	if ctx.firstCheck {
		select {
		case <-ctx.workerDone:
		case <-ctx.Context.Done():
		}
	}
	ctx.firstCheck = true
	return ctx.Context.Done()
}

func TestTraceShutdownReturnsExporterResultAfterWorkerExit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), exportTimeout)
	defer cancel()
	exporterError := errors.New("collector shutdown failed")
	for _, want := range []error{nil, exporterError} {
		for range 16 {
			processor := newBoundedSpanProcessor(closingExporter{err: want}, nil)
			delayed := &delayedShutdownContext{Context: ctx, workerDone: processor.done}
			if got := processor.Shutdown(delayed); !errors.Is(got, want) {
				t.Fatalf("trace shutdown returned %v, want %v", got, want)
			}
		}
	}
}

func TestBusinessMetricsExposeLintCleanFamilies(t *testing.T) {
	tenants, err := ParseMetricsTenantLabels("end_user,project,key")
	if err != nil {
		t.Fatal(err)
	}
	metrics := NewBusinessMetrics(1000, tenants)
	labels := BusinessLabels{Route: "chat", ProviderKind: "openai_compatible", Project: "p", Key: "k", EndUser: "u"}
	input, output := int64(4), int64(6)
	metrics.Tokens(labels, &input, &output)
	metrics.Cost(labels, "USD", 0.25)
	metrics.FirstToken(labels, 0.2)
	metrics.OutputRate(labels, 6, 0.5)
	metrics.Guardrail(labels, "input", "block", "matched", 12)
	metrics.Cache("chat", "miss", 128, true)

	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(metrics)
	problems, err := testutil.GatherAndLint(registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("lint problems: %v", problems)
	}
	gathered, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	help := make(map[string]string, len(gathered))
	for _, family := range gathered {
		help[family.GetName()] = family.GetHelp()
	}
	for _, family := range []string{
		"olp_tokens_total", "olp_cost_total", "olp_time_to_first_token_seconds", "olp_output_tokens_per_second",
		"olp_guardrail_decisions_total", "olp_guardrail_duration_seconds",
		"olp_cache_requests_total", "olp_cache_bytes_stored_total", "olp_cache_errors_total",
	} {
		text, present := help[family]
		if !present {
			t.Errorf("%s was not exposed", family)
			continue
		}
		if text == "" {
			t.Errorf("%s has no help text", family)
		}
	}
}
