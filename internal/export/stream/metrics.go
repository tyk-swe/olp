package stream

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/tyk-swe/olp/internal/metric"
)

var (
	lag              = metric.Gauge("olp_export_lag_seconds", "Age of the oldest undelivered export record per sink stream.", "sink", "stream")
	deliveries       = metric.Counter("olp_export_deliveries_total", "Export records delivered or failed per sink.", "sink", "outcome")
	gaps             = metric.Counter("olp_export_gap_total", "Export records retention expired before a sink delivered them.", "sink")
	pending          = metric.Gauge("olp_export_pending", "Export records currently awaiting delivery.")
	deliveryAttempts = metric.Counter("olp_export_delivery_attempts_total", "Export delivery attempts claimed by this process.")
	deliveryFailed   = metric.Counter("olp_export_delivery_failed_total", "Export delivery attempts that failed in this process.")
)

func (w *Worker) Metrics(ctx context.Context) []prometheus.Metric {
	return slices.Concat(
		metric.Rows(ctx, w.Pool, lagMetricsSQL, func(row pgx.CollectableRow) ([]prometheus.Metric, error) {
			var sink, stream string
			var seconds float64
			err := row.Scan(&sink, &stream, &seconds)
			return []prometheus.Metric{lag.Sample(seconds, sink, stream)}, err
		}),
		metric.Rows(ctx, w.Pool, deliveryMetricsSQL, func(row pgx.CollectableRow) ([]prometheus.Metric, error) {
			var sink string
			var delivered, failed, gap int64
			err := row.Scan(&sink, &delivered, &failed, &gap)
			return []prometheus.Metric{
				deliveries.Sample(float64(delivered), sink, "delivered"),
				deliveries.Sample(float64(failed), sink, "failed"),
				gaps.Sample(float64(gap), sink),
			}, err
		}),
		metric.Rows(ctx, w.Pool, pendingMetricsSQL, func(row pgx.CollectableRow) ([]prometheus.Metric, error) {
			var n int64
			err := row.Scan(&n)
			return []prometheus.Metric{pending.Sample(float64(n))}, err
		}),
		[]prometheus.Metric{
			deliveryAttempts.Sample(float64(w.attempts.Load())),
			deliveryFailed.Sample(float64(w.failed.Load())),
		},
	)
}
