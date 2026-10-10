package telemetry

import (
	"fmt"
	"math"
	"strings"
	"sync"
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

func TestBusinessMetricsHighCardinalityCapKeepsExistingSeries(t *testing.T) {
	m := NewBusinessMetrics(40, nil)
	input := int64(1)
	var wg sync.WaitGroup
	for i := range 1000 {
		wg.Go(func() {
			m.Tokens(BusinessLabels{Route: fmt.Sprintf("route-%d", i), ProviderKind: "openai"}, &input, nil)
		})
	}
	wg.Wait()
	if m.used != 40 || len(m.series) != 40 || m.overflow != 960 {
		t.Fatalf("cap violated: used=%d tuples=%d overflow=%d", m.used, len(m.series), m.overflow)
	}
	for _, s := range m.series {
		before := s.value
		m.add(s.name, s.labels, 5, nil)
		if s.value != before+5 {
			t.Fatal("existing tuple did not continue counting")
		}
	}
}

func TestBusinessMetricsTenantLabelsAndEscaping(t *testing.T) {
	for _, raw := range []string{"user", "key,key", "project,"} {
		if _, err := ParseMetricsTenantLabels(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	labels, err := ParseMetricsTenantLabels("key,project")
	if err != nil || strings.Join(labels, ",") != "project,key" {
		t.Fatalf("labels=%v error=%v", labels, err)
	}
	m := NewBusinessMetrics(10, nil)
	input := int64(1)
	m.Tokens(BusinessLabels{Route: "a\"b\\c\n", Key: "secret-id", EndUser: "secret-digest"}, &input, nil)
	var b strings.Builder
	m.WritePrometheus(&b)
	if !strings.Contains(b.String(), `route="a\"b\\c\n"`) || strings.Contains(b.String(), "secret-") {
		t.Fatal("invalid escaping or tenant labels enabled by default")
	}
}
