package telemetry

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

var BusinessLatencyBuckets = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30}
var BusinessThroughputBuckets = []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000}

type BusinessLabels struct {
	Route        string
	ProviderKind string
	Project      string
	Key          string
	EndUser      string
}

type businessSeries struct {
	name   string
	labels string
	value  float64
	count  uint64
	bounds []float64
	counts []uint64
}

type BusinessMetrics struct {
	mu       sync.Mutex
	cap      int
	used     int
	tenants  []string
	series   map[string]*businessSeries
	overflow uint64
}

func ParseMetricsTenantLabels(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	seen := map[string]bool{}
	for _, label := range strings.Split(raw, ",") {
		label = strings.TrimSpace(label)
		if label != "project" && label != "key" && label != "end_user" {
			return nil, fmt.Errorf("OLP_METRICS_TENANT_LABELS accepts project, key and end_user")
		}
		if seen[label] {
			return nil, fmt.Errorf("OLP_METRICS_TENANT_LABELS must not repeat labels")
		}
		seen[label] = true
	}
	labels := make([]string, 0, len(seen))
	for _, label := range []string{"project", "key", "end_user"} {
		if seen[label] {
			labels = append(labels, label)
		}
	}
	return labels, nil
}

func NewBusinessMetrics(seriesCap int, tenants []string) *BusinessMetrics {
	if seriesCap < 1 {
		panic("telemetry: invalid business metrics series cap")
	}
	return &BusinessMetrics{cap: seriesCap, tenants: append([]string(nil), tenants...), series: map[string]*businessSeries{}}
}

func metricQuoted(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return "\"" + s + "\""
}

func (m *BusinessMetrics) labels(l BusinessLabels, extra ...string) string {
	labels := []string{"route=" + metricQuoted(l.Route)}
	labels = append(labels, extra...)
	for _, tenant := range m.tenants {
		value := ""
		switch tenant {
		case "project":
			value = l.Project
		case "key":
			value = l.Key
		case "end_user":
			value = l.EndUser
		}
		labels = append(labels, tenant+"="+metricQuoted(value))
	}
	return strings.Join(labels, ",")
}

func (m *BusinessMetrics) add(name, labels string, value float64, buckets []float64) {
	if m == nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := name + "{" + labels + "}"
	series := m.series[key]
	if series == nil {
		weight := 1
		if buckets != nil {
			weight = len(buckets) + 3
		}
		if m.used+weight > m.cap {
			m.overflow++
			return
		}
		series = &businessSeries{name: name, labels: labels, bounds: append([]float64(nil), buckets...), counts: make([]uint64, len(buckets))}
		m.series[key] = series
		m.used += weight
	}
	series.value += value
	if buckets != nil {
		series.count++
		for i, bound := range series.bounds {
			if value <= bound {
				series.counts[i]++
			}
		}
	}
}

func (m *BusinessMetrics) Tokens(l BusinessLabels, input, output *int64) {
	if m == nil || l.Route == "" {
		return
	}
	for _, item := range []struct {
		direction string
		value     *int64
	}{{"input", input}, {"output", output}} {
		if item.value != nil && *item.value >= 0 {
			m.add("olp_tokens_total", m.labels(l, "provider_kind="+metricQuoted(l.ProviderKind), "direction="+metricQuoted(item.direction)), float64(*item.value), nil)
		}
	}
}

func (m *BusinessMetrics) Cost(l BusinessLabels, currency string, amount float64) {
	if m != nil && l.Route != "" && currency != "" {
		m.add("olp_cost_total", m.labels(l, "currency="+metricQuoted(currency)), amount, nil)
	}
}

func (m *BusinessMetrics) FirstToken(l BusinessLabels, seconds float64) {
	if m != nil && l.Route != "" {
		m.add("olp_time_to_first_token_seconds", m.labels(l), seconds, BusinessLatencyBuckets)
	}
}

func (m *BusinessMetrics) OutputRate(l BusinessLabels, tokens int64, seconds float64) {
	if m != nil && l.Route != "" && tokens >= 0 && seconds > 0 {
		m.add("olp_output_tokens_per_second", m.labels(l), float64(tokens)/seconds, BusinessThroughputBuckets)
	}
}

func (m *BusinessMetrics) WritePrometheus(body *strings.Builder) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	body.WriteString("# HELP olp_tokens_total Observed provider attempt tokens; unknown usage is omitted.\n# TYPE olp_tokens_total counter\n")
	body.WriteString("# HELP olp_cost_total Approximate process-local cost from pinned attempt prices, not the durable ledger.\n# TYPE olp_cost_total counter\n")
	body.WriteString("# HELP olp_time_to_first_token_seconds Time to first meaningful streaming output token.\n# TYPE olp_time_to_first_token_seconds histogram\n")
	body.WriteString("# HELP olp_output_tokens_per_second Observed streaming output tokens per second after first meaningful output.\n# TYPE olp_output_tokens_per_second histogram\n")
	keys := make([]string, 0, len(m.series))
	for key := range m.series {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		s := m.series[key]
		if s.bounds == nil {
			fmt.Fprintf(body, "%s{%s} %s\n", s.name, s.labels, strconv.FormatFloat(s.value, 'g', -1, 64))
			continue
		}
		for i, bound := range s.bounds {
			fmt.Fprintf(body, "%s_bucket{%s,le=\"%s\"} %d\n", s.name, s.labels, strconv.FormatFloat(bound, 'g', -1, 64), s.counts[i])
		}
		fmt.Fprintf(body, "%s_bucket{%s,le=\"+Inf\"} %d\n%s_sum{%s} %s\n%s_count{%s} %d\n",
			s.name, s.labels, s.count, s.name, s.labels, strconv.FormatFloat(s.value, 'g', -1, 64), s.name, s.labels, s.count)
	}
	fmt.Fprintf(body, "# HELP olp_metrics_series_active Business metric sample series admitted under the cap.\n# TYPE olp_metrics_series_active gauge\nolp_metrics_series_active %d\n", m.used)
	fmt.Fprintf(body, "# HELP olp_metrics_series_overflow_total Observations refused because admitting new series would exceed the cap.\n# TYPE olp_metrics_series_overflow_total counter\nolp_metrics_series_overflow_total %d\n", m.overflow)
}
