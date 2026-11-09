package focus

import (
	"context"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/tyk-swe/olp/internal/metric"
)

const pendingSQL = `SELECT sink_id::text, count(*) FROM olp.focus_datasets fd
 LEFT JOIN olp.focus_dirty_days dd ON dd.day=fd.charge_date
 WHERE fd.last_error_code IS NOT NULL OR fd.delivered_at IS NULL
  OR COALESCE(dd.version,0) IS DISTINCT FROM fd.dirty_version
 GROUP BY sink_id`

var (
	pendingDatasets   = metric.Gauge("olp_focus_pending_datasets", "FOCUS datasets awaiting delivery or restatement per sink.", "sink")
	datasetDeliveries = metric.Counter("olp_focus_dataset_deliveries_total", "FOCUS dataset deliveries by sink and outcome.", "sink", "outcome")
)

func (w *Worker) Metrics(ctx context.Context) []prometheus.Metric {
	samples := metric.Rows(ctx, w.Pool, pendingSQL, func(row pgx.CollectableRow) ([]prometheus.Metric, error) {
		var sink string
		var n int64
		err := row.Scan(&sink, &n)
		return []prometheus.Metric{pendingDatasets.Sample(float64(n), sink)}, err
	})
	w.deliveries.Range(func(k, v any) bool {
		key := k.(deliveryKey)
		samples = append(samples, datasetDeliveries.Sample(float64(v.(*atomic.Int64).Load()), key.sink, string(key.outcome)))
		return true
	})
	return samples
}
