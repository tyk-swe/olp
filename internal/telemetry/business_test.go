package telemetry

import (
	"math"
	"strings"
	"testing"
)

func TestBusinessMetricsCountsKnownUsageAndBoundsActualSeries(t *testing.T) {
	m := NewBusinessMetrics(30, []string{"project", "key", "end_user"})
	l := BusinessLabels{Route: "chat", ProviderKind: "openai", Project: "project", Key: "key", EndUser: "digest"}
	input, output := int64(100), int64(20)
	m.Tokens(l, &input, &output)
	m.Cost(l, "USD", 0.25)
	m.FirstToken(l, 0.5)
	m.OutputRate(l, 20, 2)
	m.Tokens(l, nil, nil)
	m.Tokens(BusinessLabels{Route: "chat", ProviderKind: "other"}, &input, &output)
	m.Cost(l, "USD", math.NaN())
	m.Cost(l, "USD", -1)
	m.OutputRate(l, 20, 0)
	var b strings.Builder
	m.WritePrometheus(&b)
	body := b.String()
	for _, want := range []string{
		`olp_tokens_total{route="chat",provider_kind="openai",direction="input",project="project",key="key",end_user="digest"} 100`,
		`olp_cost_total{route="chat",currency="USD",project="project",key="key",end_user="digest"} 0.25`,
		`olp_time_to_first_token_seconds_bucket{route="chat",project="project",key="key",end_user="digest",le="0.5"} 1`,
		`olp_output_tokens_per_second_sum{route="chat",project="project",key="key",end_user="digest"} 10`,
		"olp_metrics_series_active 30\n",
		"olp_metrics_series_overflow_total 0\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in %s", want, body)
		}
	}
}
