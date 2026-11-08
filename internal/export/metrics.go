package export

import (
	"context"
	"slices"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/metric"
)

var (
	exportLag              = metric.Gauge("olp_export_lag_seconds", "Age of the oldest undelivered export record per sink stream.", "sink", "stream")
	exportDeliveries       = metric.Counter("olp_export_deliveries_total", "Export records delivered or failed per sink.", "sink", "outcome")
	exportGap              = metric.Counter("olp_export_gap_total", "Export records retention expired before a sink delivered them.", "sink")
	exportPending          = metric.Gauge("olp_export_pending", "Export records currently awaiting delivery.")
	focusPendingDatasets   = metric.Gauge("olp_focus_pending_datasets", "FOCUS datasets awaiting delivery or restatement per sink.", "sink")
	focusDatasetDeliveries = metric.Counter("olp_focus_dataset_deliveries_total", "FOCUS dataset deliveries by sink and outcome.", "sink", "outcome")
	deliveryAttempts       = metric.Counter("olp_export_delivery_attempts_total", "Export delivery attempts claimed by this process.")
	deliveryFailed         = metric.Counter("olp_export_delivery_failed_total", "Export delivery attempts that failed in this process.")
	captureQueued          = metric.Counter("olp_capture_queued_total", "Payload captures accepted into the in-memory queue.")
	captureDelivered       = metric.Counter("olp_capture_delivered_total", "Payload captures delivered to their sink.")
	captureDropped         = metric.Counter("olp_capture_dropped_total", "Payload captures dropped by bounds or staleness.")
	captureFailed          = metric.Counter("olp_capture_failed_total", "Payload capture deliveries that failed.")
	captureRedactionFailed = metric.Counter("olp_capture_redaction_failed_total", "Captures refused because redaction could not apply.")
)

const focusPendingSQL = `SELECT sink_id::text, count(*) FROM olp.focus_datasets fd
	LEFT JOIN olp.focus_dirty_days dd ON dd.day=fd.charge_date
	WHERE fd.last_error_code IS NOT NULL OR fd.delivered_at IS NULL
	   OR COALESCE(dd.version,0) IS DISTINCT FROM fd.dirty_version
	GROUP BY sink_id`

// Metrics skips a series whose query fails so one broken read never hides the
// rest of the scrape.
func Metrics(ctx context.Context, q access.Queryer, worker *Worker, focus *FocusWorker, capture *Manager) []prometheus.Metric {
	return slices.Concat(
		lagMetrics(ctx, q),
		deliveryMetrics(ctx, q),
		pendingMetrics(ctx, q),
		focusMetrics(ctx, q, focus),
		workerMetrics(worker),
		captureMetrics(capture),
	)
}

func lagMetrics(ctx context.Context, q access.Queryer) []prometheus.Metric {
	rows, err := q.Query(ctx, exportMetricsSQL)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var metrics []prometheus.Metric
	for rows.Next() {
		var sink, stream string
		var seconds float64
		var delivered, failed int64
		if rows.Scan(&sink, &stream, &seconds, &delivered, &failed) == nil {
			metrics = append(metrics, exportLag.Sample(seconds, sink, stream))
		}
	}
	return metrics
}

func deliveryMetrics(ctx context.Context, q access.Queryer) []prometheus.Metric {
	rows, err := q.Query(ctx, exportDeliveryMetricsSQL)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var metrics []prometheus.Metric
	for rows.Next() {
		var sink string
		var delivered, failed, gap int64
		if rows.Scan(&sink, &delivered, &failed, &gap) == nil {
			metrics = append(metrics,
				exportDeliveries.Sample(float64(delivered), sink, "delivered"),
				exportDeliveries.Sample(float64(failed), sink, "failed"),
				exportGap.Sample(float64(gap), sink),
			)
		}
	}
	return metrics
}

func pendingMetrics(ctx context.Context, q access.Queryer) []prometheus.Metric {
	var pending int64
	if q.QueryRow(ctx, exportPendingMetricsSQL).Scan(&pending) != nil {
		return nil
	}
	return []prometheus.Metric{exportPending.Sample(float64(pending))}
}

func focusMetrics(ctx context.Context, q access.Queryer, focus *FocusWorker) []prometheus.Metric {
	if focus == nil {
		return nil
	}
	var metrics []prometheus.Metric
	if rows, err := q.Query(ctx, focusPendingSQL); err == nil {
		for rows.Next() {
			var sink string
			var n int64
			if rows.Scan(&sink, &n) == nil {
				metrics = append(metrics, focusPendingDatasets.Sample(float64(n), sink))
			}
		}
		rows.Close()
	}
	for key, n := range focus.FocusMetrics() {
		metrics = append(metrics, focusDatasetDeliveries.Sample(float64(n), key[0], key[1]))
	}
	return metrics
}

func workerMetrics(worker *Worker) []prometheus.Metric {
	if worker == nil {
		return nil
	}
	attempts, _, failed := worker.Metrics()
	return []prometheus.Metric{
		deliveryAttempts.Sample(float64(attempts)),
		deliveryFailed.Sample(float64(failed)),
	}
}

func captureMetrics(capture *Manager) []prometheus.Metric {
	if capture == nil {
		return nil
	}
	queued, delivered, dropped, failed, redactionFailed := capture.Metrics()
	return []prometheus.Metric{
		captureQueued.Sample(float64(queued)),
		captureDelivered.Sample(float64(delivered)),
		captureDropped.Sample(float64(dropped)),
		captureFailed.Sample(float64(failed)),
		captureRedactionFailed.Sample(float64(redactionFailed)),
	}
}
