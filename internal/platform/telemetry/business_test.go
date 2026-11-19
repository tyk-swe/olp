package telemetry

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

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
