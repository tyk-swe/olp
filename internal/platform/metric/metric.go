package metric

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
)

type Family struct {
	desc *prometheus.Desc
	kind prometheus.ValueType
}

func Gauge(name, help string, labels ...string) Family {
	return Family{desc: prometheus.NewDesc(name, help, labels, nil), kind: prometheus.GaugeValue}
}

func Counter(name, help string, labels ...string) Family {
	return Family{desc: prometheus.NewDesc(name, help, labels, nil), kind: prometheus.CounterValue}
}

func (f Family) Sample(value float64, labels ...string) prometheus.Metric {
	m, err := prometheus.NewConstMetric(f.desc, f.kind, value, labels...)
	if err != nil {
		return prometheus.NewInvalidMetric(f.desc, err)
	}
	return m
}

func Flag(v bool) float64 {
	if v {
		return 1
	}
	return 0
}

// Collector serves series whose label values are only known at scrape time,
// so it is an unchecked collector and describes nothing up front.
type Collector func() []prometheus.Metric

func (Collector) Describe(chan<- *prometheus.Desc) {}

func (c Collector) Collect(ch chan<- prometheus.Metric) {
	for _, m := range c() {
		ch <- m
	}
}

type Querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func Rows(ctx context.Context, q Querier, sql string, scan func(pgx.CollectableRow) ([]prometheus.Metric, error)) []prometheus.Metric {
	rows, err := q.Query(ctx, sql)
	if err != nil {
		return nil
	}
	series, err := pgx.CollectRows(rows, scan)
	if err != nil {
		return nil
	}
	return slices.Concat(series...)
}
