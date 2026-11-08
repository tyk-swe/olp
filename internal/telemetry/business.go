package telemetry

import (
	"errors"
	"math"
	"slices"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	businessLatencyBuckets    = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30}
	businessThroughputBuckets = []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000}
	tenantLabelNames          = []string{"project", "key", "end_user"}
)

type BusinessLabels struct {
	Route        string
	ProviderKind string
	Project      string
	Key          string
	EndUser      string
}

func (l BusinessLabels) tenant(name string) string {
	switch name {
	case "project":
		return l.Project
	case "key":
		return l.Key
	default:
		return l.EndUser
	}
}

type businessFamilies struct {
	tokens, cost, firstToken, outputRate         *prometheus.Desc
	guardrailDecisions, guardrailDuration        *prometheus.Desc
	cacheRequests, cacheBytesStored, cacheErrors *prometheus.Desc
	seriesActive, seriesOverflow                 *prometheus.Desc
}

func newBusinessFamilies(tenants []string) businessFamilies {
	desc := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(name, help, append(append([]string{"route"}, labels...), tenants...), nil)
	}
	return businessFamilies{
		tokens:             desc("olp_tokens_total", "Observed provider attempt tokens; unknown usage is omitted.", "provider_kind", "direction"),
		cost:               desc("olp_cost_total", "Approximate process-local cost from pinned attempt prices, not the durable ledger.", "currency"),
		firstToken:         desc("olp_time_to_first_token_seconds", "Time to first meaningful streaming output token."),
		outputRate:         desc("olp_output_tokens_per_second", "Observed streaming output tokens per second after first meaningful output."),
		guardrailDecisions: desc("olp_guardrail_decisions_total", "Guardrail decisions by phase, action and outcome.", "phase", "action", "outcome"),
		guardrailDuration:  desc("olp_guardrail_duration_seconds", "Guardrail evaluation duration by phase, action and outcome.", "phase", "action", "outcome"),
		cacheRequests:      desc("olp_cache_requests_total", "Response cache lookups by result.", "result"),
		cacheBytesStored:   desc("olp_cache_bytes_stored_total", "Response bytes written to the response cache."),
		cacheErrors:        desc("olp_cache_errors_total", "Response cache operations that failed."),
		seriesActive:       prometheus.NewDesc("olp_metrics_series_active", "Business metric sample series admitted under the cap.", nil, nil),
		seriesOverflow:     prometheus.NewDesc("olp_metrics_series_overflow_total", "Observations refused because admitting new series would exceed the cap.", nil, nil),
	}
}

type businessSeries struct {
	desc    *prometheus.Desc
	labels  []string
	value   float64
	count   uint64
	buckets []float64
	counts  []uint64
}

type BusinessMetrics struct {
	families businessFamilies
	tenants  []string
	cap      int
	mu       sync.Mutex
	used     int
	series   map[string]*businessSeries
	overflow uint64
}

func ParseMetricsTenantLabels(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	labels := strings.Split(raw, ",")
	requested := make([]string, 0, len(labels))
	for _, label := range labels {
		name := strings.TrimSpace(label)
		if !slices.Contains(tenantLabelNames, name) {
			return nil, errors.New("accepts project, key and end_user")
		}
		if slices.Contains(requested, name) {
			return nil, errors.New("must not repeat labels")
		}
		requested = append(requested, name)
	}
	return slices.DeleteFunc(slices.Clone(tenantLabelNames), func(name string) bool { return !slices.Contains(requested, name) }), nil
}

func NewBusinessMetrics(seriesCap int, tenants []string) *BusinessMetrics {
	return &BusinessMetrics{families: newBusinessFamilies(tenants), tenants: slices.Clone(tenants), cap: seriesCap, series: map[string]*businessSeries{}}
}

func (m *BusinessMetrics) labelValues(l BusinessLabels, extra ...string) []string {
	values := append([]string{l.Route}, extra...)
	for _, tenant := range m.tenants {
		values = append(values, l.tenant(tenant))
	}
	return values
}

func (m *BusinessMetrics) add(desc *prometheus.Desc, labels []string, value float64, buckets []float64) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := desc.String() + "\xff" + strings.Join(labels, "\xff")
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
		series = &businessSeries{desc: desc, labels: labels, buckets: buckets, counts: make([]uint64, len(buckets))}
		m.series[key] = series
		m.used += weight
	}
	series.value += value
	if buckets == nil {
		return
	}
	series.count++
	for i, bound := range series.buckets {
		if value <= bound {
			series.counts[i]++
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
			m.add(m.families.tokens, m.labelValues(l, l.ProviderKind, item.direction), float64(*item.value), nil)
		}
	}
}

func (m *BusinessMetrics) Cost(l BusinessLabels, currency string, amount float64) {
	if m != nil && l.Route != "" && currency != "" {
		m.add(m.families.cost, m.labelValues(l, currency), amount, nil)
	}
}

func (m *BusinessMetrics) FirstToken(l BusinessLabels, seconds float64) {
	if m != nil && l.Route != "" {
		m.add(m.families.firstToken, m.labelValues(l), seconds, businessLatencyBuckets)
	}
}

func (m *BusinessMetrics) OutputRate(l BusinessLabels, tokens int64, seconds float64) {
	if m != nil && l.Route != "" && tokens >= 0 && seconds > 0 {
		m.add(m.families.outputRate, m.labelValues(l), float64(tokens)/seconds, businessThroughputBuckets)
	}
}

// Guardrail records bounded execution labels; it never accepts matched values.
func (m *BusinessMetrics) Guardrail(l BusinessLabels, phase, action, outcome string, latencyMS int64) {
	if m == nil {
		return
	}
	labels := m.labelValues(l, phase, action, outcome)
	m.add(m.families.guardrailDecisions, labels, 1, nil)
	m.add(m.families.guardrailDuration, labels, float64(latencyMS)/1000, businessLatencyBuckets)
}

// Cache records bounded, content-free cache outcome metrics.
func (m *BusinessMetrics) Cache(route, result string, storedBytes int, failed bool) {
	if m == nil || route == "" {
		return
	}
	l := BusinessLabels{Route: route}
	m.add(m.families.cacheRequests, m.labelValues(l, result), 1, nil)
	if storedBytes > 0 {
		m.add(m.families.cacheBytesStored, m.labelValues(l), float64(storedBytes), nil)
	}
	if failed {
		m.add(m.families.cacheErrors, m.labelValues(l), 1, nil)
	}
}

func (m *BusinessMetrics) Describe(ch chan<- *prometheus.Desc) {
	f := m.families
	for _, desc := range []*prometheus.Desc{f.tokens, f.cost, f.firstToken, f.outputRate, f.guardrailDecisions, f.guardrailDuration, f.cacheRequests, f.cacheBytesStored, f.cacheErrors, f.seriesActive, f.seriesOverflow} {
		ch <- desc
	}
}

func (m *BusinessMetrics) Collect(ch chan<- prometheus.Metric) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.series {
		ch <- s.metric()
	}
	ch <- prometheus.MustNewConstMetric(m.families.seriesActive, prometheus.GaugeValue, float64(m.used))
	ch <- prometheus.MustNewConstMetric(m.families.seriesOverflow, prometheus.CounterValue, float64(m.overflow))
}

func (s *businessSeries) metric() prometheus.Metric {
	m, err := s.build()
	if err != nil {
		return prometheus.NewInvalidMetric(s.desc, err)
	}
	return m
}

func (s *businessSeries) build() (prometheus.Metric, error) {
	if s.buckets == nil {
		return prometheus.NewConstMetric(s.desc, prometheus.CounterValue, s.value, s.labels...)
	}
	buckets := make(map[float64]uint64, len(s.buckets))
	for i, bound := range s.buckets {
		buckets[bound] = s.counts[i]
	}
	return prometheus.NewConstHistogram(s.desc, s.count, s.value, buckets, s.labels...)
}
