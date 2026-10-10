package export

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
)

func WriteMetrics(ctx context.Context, q access.Queryer, worker *Worker, capture *Manager, body *strings.Builder) {
	rows, err := q.Query(ctx, exportMetricsSQL)
	if err == nil {
		defer rows.Close()
		body.WriteString("# HELP olp_export_lag_seconds Age of the oldest undelivered export record per sink stream.\n# TYPE olp_export_lag_seconds gauge\n")
		for rows.Next() {
			var sink, stream string
			var seconds float64
			var delivered, failed int64
			if err := rows.Scan(&sink, &stream, &seconds, &delivered, &failed); err == nil {
				fmt.Fprintf(body, "olp_export_lag_seconds{sink=%s,stream=%s} %s\n", strconv.Quote(sink), strconv.Quote(stream), strconv.FormatFloat(seconds, 'g', -1, 64))
			}
		}
	}
	rows2, err := q.Query(ctx, exportDeliveryMetricsSQL)
	if err == nil {
		defer rows2.Close()
		var sinks []struct {
			id                        string
			delivered, failed, gapSum int64
		}
		for rows2.Next() {
			var item struct {
				id                        string
				delivered, failed, gapSum int64
			}
			if err := rows2.Scan(&item.id, &item.delivered, &item.failed, &item.gapSum); err == nil {
				sinks = append(sinks, item)
			}
		}
		body.WriteString("# HELP olp_export_deliveries_total Export records delivered or failed per sink.\n# TYPE olp_export_deliveries_total counter\n")
		for _, item := range sinks {
			fmt.Fprintf(body, "olp_export_deliveries_total{sink=%s,outcome=%s} %d\n", strconv.Quote(item.id), `"delivered"`, item.delivered)
			fmt.Fprintf(body, "olp_export_deliveries_total{sink=%s,outcome=%s} %d\n", strconv.Quote(item.id), `"failed"`, item.failed)
		}
		body.WriteString("# HELP olp_export_gap_total Export records retention expired before a sink delivered them.\n# TYPE olp_export_gap_total counter\n")
		for _, item := range sinks {
			fmt.Fprintf(body, "olp_export_gap_total{sink=%s} %d\n", strconv.Quote(item.id), item.gapSum)
		}
	}
	var pending int64
	if err := q.QueryRow(ctx, exportPendingMetricsSQL).Scan(&pending); err == nil {
		fmt.Fprintf(body, "# HELP olp_export_pending Export records currently awaiting delivery.\n# TYPE olp_export_pending gauge\nolp_export_pending %d\n", pending)
	}
	if worker != nil {
		attempts, _, failed := worker.Metrics()
		fmt.Fprintf(body, "# HELP olp_export_delivery_attempts_total Export delivery attempts claimed by this process.\n# TYPE olp_export_delivery_attempts_total counter\nolp_export_delivery_attempts_total %d\n", attempts)
		fmt.Fprintf(body, "# HELP olp_export_delivery_failed_total Export delivery attempts that failed in this process.\n# TYPE olp_export_delivery_failed_total counter\nolp_export_delivery_failed_total %d\n", failed)
	}
	if capture != nil {
		queued, delivered, dropped, failed, redactionFailed := capture.Metrics()
		fmt.Fprintf(body, "# HELP olp_capture_queued_total Payload captures accepted into the in-memory queue.\n# TYPE olp_capture_queued_total counter\nolp_capture_queued_total %d\n", queued)
		fmt.Fprintf(body, "# HELP olp_capture_delivered_total Payload captures delivered to their sink.\n# TYPE olp_capture_delivered_total counter\nolp_capture_delivered_total %d\n", delivered)
		fmt.Fprintf(body, "# HELP olp_capture_dropped_total Payload captures dropped by bounds or staleness.\n# TYPE olp_capture_dropped_total counter\nolp_capture_dropped_total %d\n", dropped)
		fmt.Fprintf(body, "# HELP olp_capture_failed_total Payload capture deliveries that failed.\n# TYPE olp_capture_failed_total counter\nolp_capture_failed_total %d\n", failed)
		fmt.Fprintf(body, "# HELP olp_capture_redaction_failed_total Captures refused because redaction could not apply.\n# TYPE olp_capture_redaction_failed_total counter\nolp_capture_redaction_failed_total %d\n", redactionFailed)
	}
}
