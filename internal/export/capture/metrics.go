package capture

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/tyk-swe/olp/internal/metric"
)

var (
	queuedTotal          = metric.Counter("olp_capture_queued_total", "Payload captures accepted into the in-memory queue.")
	deliveredTotal       = metric.Counter("olp_capture_delivered_total", "Payload captures delivered to their sink.")
	droppedTotal         = metric.Counter("olp_capture_dropped_total", "Payload captures dropped by bounds or staleness.")
	failedTotal          = metric.Counter("olp_capture_failed_total", "Payload capture deliveries that failed.")
	redactionFailedTotal = metric.Counter("olp_capture_redaction_failed_total", "Captures refused because redaction could not apply.")
)

func (m *Manager) Metrics() []prometheus.Metric {
	return []prometheus.Metric{
		queuedTotal.Sample(float64(m.queued.Load())),
		deliveredTotal.Sample(float64(m.delivered.Load())),
		droppedTotal.Sample(float64(m.dropped.Load())),
		failedTotal.Sample(float64(m.failed.Load())),
		redactionFailedTotal.Sample(float64(m.redactionFailed.Load())),
	}
}
