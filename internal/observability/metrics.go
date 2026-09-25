package observability

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/usage"
)

// successRatio is the successful fraction of total, treating "nothing
// observed" as fully healthy so an idle window does not read as an outage.
func successRatio(success, total int64) float64 {
	if total == 0 {
		return 1.0
	}
	return float64(success) / float64(total)
}

// CollectMetrics renders the process's Prometheus exposition body. Every
// series is metadata: counts, ages, and configured capacities only.
func CollectMetrics(ctx context.Context, s *State) (string, error) {
	var body strings.Builder
	body.Grow(8192)
	var snapshot *usage.Snapshot
	if s.Emitter != nil {
		snapshot = s.Emitter()
	}
	limiterConfigured, limiterHealthy := false, false
	if s.Limiter != nil {
		limiterConfigured, limiterHealthy = s.Limiter(ctx)
	}
	now := time.Now()
	mediaResult, mediaErr := media.ReconciliationSummary(ctx, s.Pool, now)
	mediaSummary := &mediaResult
	consumer, consumerErr := usage.ReadConsumerStatus(ctx, s.Pool, now)
	// Epoch health defaults to zero when the read fails, matching the Rust
	// collector: an absent epoch table must not suppress the whole exposition.
	epochs, _ := ReadEpochHealth(ctx, s.Pool)
	operations, operationsErr := ReadOperationsSummary(ctx, s.Pool, 5)
	providers, providersComplete, providersErr := ReadProviderHealthMetrics(ctx, s.Pool)
	tasks, tasksErr := ReadWorkerTaskHealth(ctx, s.Pool)
	counters, countersErr := ReadWorkerRecoveryCounters(ctx, s.Pool)

	body.WriteString("# HELP olp_provider_metrics_complete Whether provider collection completed without truncation or query failure.\n" +
		"# TYPE olp_provider_metrics_complete gauge\n" +
		"# HELP olp_provider_metrics_emitted Number of providers in this snapshot.\n" +
		"# TYPE olp_provider_metrics_emitted gauge\n")
	fmt.Fprintf(&body, "olp_provider_metrics_complete %d\nolp_provider_metrics_emitted %d\n",
		boolInt(providersErr == nil && providersComplete), len(providers))

	// Runtime generation and metadata pipeline gauges.
	var generation int64
	if s.Runtime != nil {
		if g := s.Runtime().Generation; g != nil {
			generation = *g
		}
	}
	var dropped, abandoned, pending int64
	var retrying, sinkAvailable int
	if snapshot != nil {
		dropped, abandoned, pending = snapshot.Dropped, snapshot.Abandoned, snapshot.Pending()
		retrying = boolInt(snapshot.Retrying)
		sinkAvailable = boolInt(!snapshot.Closed)
	}
	body.WriteString("# HELP olp_runtime_generation Current immutable runtime generation.\n" +
		"# TYPE olp_runtime_generation gauge\n" +
		"# HELP olp_request_metadata_events_dropped_total Metadata events dropped from the bounded buffer.\n" +
		"# TYPE olp_request_metadata_events_dropped_total counter\n" +
		"# HELP olp_request_metadata_events_abandoned_total Accepted metadata events abandoned during shutdown or worker failure.\n" +
		"# TYPE olp_request_metadata_events_abandoned_total counter\n" +
		"# HELP olp_request_metadata_events_pending Accepted metadata events not yet written to the stream.\n" +
		"# TYPE olp_request_metadata_events_pending gauge\n" +
		"# HELP olp_request_metadata_stream_retrying Whether the local writer is retrying Valkey.\n" +
		"# TYPE olp_request_metadata_stream_retrying gauge\n" +
		"# HELP olp_request_metadata_persistence_available Whether a request metadata sink is active.\n" +
		"# TYPE olp_request_metadata_persistence_available gauge\n" +
		"# HELP olp_request_metadata_consumer_pending_events Delivered request metadata events awaiting consumer acknowledgement.\n" +
		"# TYPE olp_request_metadata_consumer_pending_events gauge\n" +
		"# HELP olp_request_metadata_consumer_lag_events Request metadata stream events not yet delivered to the persistence consumer.\n" +
		"# TYPE olp_request_metadata_consumer_lag_events gauge\n" +
		"# HELP olp_request_metadata_consumer_heartbeat_age_seconds Age of the last durable worker checkpoint.\n" +
		"# TYPE olp_request_metadata_consumer_heartbeat_age_seconds gauge\n" +
		"# HELP olp_request_metadata_consumer_healthy Whether the durable consumer is current and fully drained.\n" +
		"# TYPE olp_request_metadata_consumer_healthy gauge\n" +
		"# HELP olp_request_metadata_consumer_stale Whether the durable consumer missed its heartbeat threshold.\n" +
		"# TYPE olp_request_metadata_consumer_stale gauge\n" +
		"# HELP olp_request_metadata_gateway_open_epochs Gateway process epochs still emitting checkpoints.\n" +
		"# TYPE olp_request_metadata_gateway_open_epochs gauge\n" +
		"# HELP olp_request_metadata_gateway_unresolved_epochs Unclean gateway epochs awaiting operator acknowledgement.\n" +
		"# TYPE olp_request_metadata_gateway_unresolved_epochs gauge\n" +
		"# HELP olp_request_metadata_historical_uncertain_gaps Retained exactness gaps across raw and hourly evidence.\n" +
		"# TYPE olp_request_metadata_historical_uncertain_gaps gauge\n" +
		"# HELP olp_request_metadata_gateway_unresolved_event_lower_bound Last durable in-flight event lower bound across unresolved epochs.\n" +
		"# TYPE olp_request_metadata_gateway_unresolved_event_lower_bound gauge\n")
	var heartbeatAge int64 = int64Max
	healthy, stale := 0, 0
	if consumerErr != nil {
		consumer = usage.ConsumerStatus{State: usage.ConsumerUnknown}
	}
	if consumerErr == nil {
		if consumer.HeartbeatAgeSeconds != nil {
			heartbeatAge = *consumer.HeartbeatAgeSeconds
		}
		healthy = boolInt(consumer.Complete())
		stale = boolInt(consumer.State == usage.ConsumerStale)
	}
	fmt.Fprintf(&body, "olp_runtime_generation %d\n"+
		"olp_request_metadata_events_dropped_total %d\n"+
		"olp_request_metadata_events_abandoned_total %d\n"+
		"olp_request_metadata_events_pending %d\n"+
		"olp_request_metadata_stream_retrying %d\n"+
		"olp_request_metadata_persistence_available %d\n"+
		"olp_request_metadata_consumer_pending_events %d\n"+
		"olp_request_metadata_consumer_lag_events %d\n"+
		"olp_request_metadata_consumer_heartbeat_age_seconds %d\n"+
		"olp_request_metadata_consumer_healthy %d\n"+
		"olp_request_metadata_consumer_stale %d\n"+
		"olp_request_metadata_gateway_open_epochs %d\n"+
		"olp_request_metadata_gateway_unresolved_epochs %d\n"+
		"olp_request_metadata_historical_uncertain_gaps %d\n"+
		"olp_request_metadata_gateway_unresolved_event_lower_bound %d\n",
		generation, dropped, abandoned, pending, retrying, sinkAvailable,
		consumer.PendingEvents, consumer.LagEvents, heartbeatAge, healthy, stale,
		epochs.OpenEpochs, epochs.UnresolvedEpochs, epochs.HistoricalUncertainGaps,
		epochs.UnresolvedEventLowerBound)

	// Limiter, circuits, and media reconciliation.
	var failOpen, dailyRejections, monthlyRejections int64
	if s.LimiterCounts != nil {
		failOpen, dailyRejections, monthlyRejections = s.LimiterCounts()
	}
	var circuits int64
	if s.Circuits != nil {
		circuits = s.Circuits()
	}
	var mediaGaps int64
	if s.MediaGaps != nil {
		mediaGaps = s.MediaGaps()
	}
	var mediaPending, mediaStale, mediaFailed int64
	if mediaErr == nil && mediaSummary != nil {
		mediaPending, mediaStale, mediaFailed = mediaSummary.Pending, mediaSummary.Stale, mediaSummary.Failed
	}
	body.WriteString("# HELP olp_distributed_limiter_available Whether a Valkey limiter connection is installed.\n" +
		"# TYPE olp_distributed_limiter_available gauge\n" +
		"# HELP olp_limits_fail_open_total Rate- or concurrency-limited requests admitted without a lease under the fail-open outage policy.\n" +
		"# TYPE olp_limits_fail_open_total counter\n" +
		"# HELP olp_key_budget_rejections_total API-key requests rejected because a cost budget was exhausted.\n" +
		"# TYPE olp_key_budget_rejections_total counter\n" +
		"# HELP olp_open_target_circuits Number of target circuits currently open or half-open.\n" +
		"# TYPE olp_open_target_circuits gauge\n" +
		"# HELP olp_media_reconciliation_pending Metadata-only media jobs awaiting reconciliation.\n" +
		"# TYPE olp_media_reconciliation_pending gauge\n" +
		"# HELP olp_media_reconciliation_stale Media reconciliation jobs past their grace period.\n" +
		"# TYPE olp_media_reconciliation_stale gauge\n" +
		"# HELP olp_media_reconciliation_failed Media jobs whose latest autonomous reconciliation attempt failed.\n" +
		"# TYPE olp_media_reconciliation_failed gauge\n" +
		"# HELP olp_media_reconciliation_gaps_total Upstream media side effects that could not be durably recorded.\n" +
		"# TYPE olp_media_reconciliation_gaps_total counter\n")
	fmt.Fprintf(&body, "olp_distributed_limiter_available %d\n"+
		"olp_limits_fail_open_total %d\n"+
		"olp_key_budget_rejections_total{window=\"daily\"} %d\n"+
		"olp_key_budget_rejections_total{window=\"monthly\"} %d\n"+
		"olp_open_target_circuits %d\n"+
		"olp_media_reconciliation_pending %d\n"+
		"olp_media_reconciliation_stale %d\n"+
		"olp_media_reconciliation_failed %d\n"+
		"olp_media_reconciliation_gaps_total %d\n",
		boolInt(limiterConfigured && limiterHealthy), failOpen, dailyRejections, monthlyRejections,
		circuits, mediaPending, mediaStale, mediaFailed, mediaGaps)

	// Durably reported request-metadata loss totals. All three series are
	// always emitted, including at zero: an absent line means the exporter
	// itself is broken rather than that nothing was lost.
	var lossEvents, lossDropped, lossAbandoned int64
	if s.LossCounters != nil {
		lossEvents, lossDropped, lossAbandoned = s.LossCounters()
	}
	body.WriteString("# HELP olp_request_metadata_loss_reported_total Local buffer loss durably reported by the gateway checkpoint.\n" +
		"# TYPE olp_request_metadata_loss_reported_total counter\n")
	fmt.Fprintf(&body, "olp_request_metadata_loss_reported_total{kind=\"events\"} %d\n"+
		"olp_request_metadata_loss_reported_total{kind=\"dropped\"} %d\n"+
		"olp_request_metadata_loss_reported_total{kind=\"abandoned\"} %d\n",
		lossEvents, lossDropped, lossAbandoned)

	// Media spool gauges. A deployment without a spool reports nothing: a
	// missing series says the spool is not configured at all.
	if s.Spool != nil {
		capacity, used := s.Spool()
		if capacity != nil {
			body.WriteString("# HELP olp_media_spool_capacity_bytes Configured capacity of the private local media spool.\n" +
				"# TYPE olp_media_spool_capacity_bytes gauge\n")
			fmt.Fprintf(&body, "olp_media_spool_capacity_bytes %d\n", *capacity)
		}
		if used != nil {
			body.WriteString("# HELP olp_media_spool_used_bytes Bytes currently reserved in the private local media spool.\n" +
				"# TYPE olp_media_spool_used_bytes gauge\n")
			fmt.Fprintf(&body, "olp_media_spool_used_bytes %d\n", *used)
		}
	}

	// Asynchronous worker plane.
	available := consumerErr == nil && tasksErr == nil && countersErr == nil
	body.WriteString("# HELP olp_async_worker_observability_available Whether all PostgreSQL-backed asynchronous worker summaries were available.\n" +
		"# TYPE olp_async_worker_observability_available gauge\n")
	fmt.Fprintf(&body, "olp_async_worker_observability_available %d\n", boolInt(available))
	if tasks != nil {
		expected := expectedTasks(limiterConfigured)
		current, drained := asynchronousPlaneFlags(tasks, expected, consumer)
		var lastProgress int64
		if at := tasks.LastProgressFor(WorkerTasks); at != nil {
			lastProgress = at.Unix()
		}
		body.WriteString("# HELP olp_async_plane_current Whether every replicated worker responsibility has a current successful checkpoint.\n" +
			"# TYPE olp_async_plane_current gauge\n" +
			"# HELP olp_async_plane_drained Whether request-metadata backlogs are drained.\n" +
			"# TYPE olp_async_plane_drained gauge\n" +
			"# HELP olp_async_plane_healthy Whether the asynchronous plane is both current and drained.\n" +
			"# TYPE olp_async_plane_healthy gauge\n" +
			"# HELP olp_async_plane_last_progress_timestamp_seconds Unix time of the latest durable worker progress.\n" +
			"# TYPE olp_async_plane_last_progress_timestamp_seconds gauge\n" +
			"# HELP olp_request_metadata_consumer_oldest_pending_age_seconds Age of the oldest delivered metadata entry awaiting acknowledgement.\n" +
			"# TYPE olp_request_metadata_consumer_oldest_pending_age_seconds gauge\n")
		var oldestPendingAge int64
		if consumer.OldestPendingAt != nil {
			if age := int64(now.Sub(*consumer.OldestPendingAt) / time.Second); age > 0 {
				oldestPendingAge = age
			}
		}
		fmt.Fprintf(&body, "olp_async_plane_current %d\nolp_async_plane_drained %d\nolp_async_plane_healthy %d\n"+
			"olp_async_plane_last_progress_timestamp_seconds %d\n"+
			"olp_request_metadata_consumer_oldest_pending_age_seconds %d\n",
			boolInt(current), boolInt(drained), boolInt(current && drained), lastProgress, oldestPendingAge)
		body.WriteString("# HELP olp_worker_task_healthy Whether a fixed worker responsibility has a current successful checkpoint.\n" +
			"# TYPE olp_worker_task_healthy gauge\n" +
			"# HELP olp_worker_task_heartbeat_age_seconds Age of the latest checkpoint for a fixed worker responsibility.\n" +
			"# TYPE olp_worker_task_heartbeat_age_seconds gauge\n" +
			"# HELP olp_worker_task_last_success_age_seconds Age of the latest successful checkpoint for a fixed worker responsibility.\n" +
			"# TYPE olp_worker_task_last_success_age_seconds gauge\n" +
			"# HELP olp_worker_task_runs_total Durable checkpoint outcomes across all worker replicas.\n" +
			"# TYPE olp_worker_task_runs_total counter\n")
		for _, task := range tasks.Tasks {
			var heartbeat, lastSuccess int64 = int64Max, int64Max
			if task.HeartbeatAgeSeconds != nil {
				heartbeat = *task.HeartbeatAgeSeconds
			}
			if task.LastSuccessAgeSeconds != nil {
				lastSuccess = *task.LastSuccessAgeSeconds
			}
			fmt.Fprintf(&body, "olp_worker_task_healthy{task=%q} %d\n"+
				"olp_worker_task_heartbeat_age_seconds{task=%q} %d\n"+
				"olp_worker_task_last_success_age_seconds{task=%q} %d\n"+
				"olp_worker_task_runs_total{task=%q,outcome=\"success\"} %d\n"+
				"olp_worker_task_runs_total{task=%q,outcome=\"failure\"} %d\n"+
				"olp_worker_task_runs_total{task=%q,outcome=\"skipped\"} %d\n",
				task.Task, boolInt(task.State == TaskStateHealthy),
				task.Task, heartbeat, task.Task, lastSuccess,
				task.Task, task.SuccessesTotal, task.Task, task.FailuresTotal,
				task.Task, task.SkippedTotal)
		}
		body.WriteString("# HELP olp_request_metadata_events_reclaimed_total Metadata entries transferred from stale consumer ownership.\n" +
			"# TYPE olp_request_metadata_events_reclaimed_total counter\n" +
			"# HELP olp_request_metadata_events_recovered_total Pending metadata entries durably resolved by a recovery pass.\n" +
			"# TYPE olp_request_metadata_events_recovered_total counter\n" +
			"# HELP olp_request_metadata_persistence_duplicates_total Duplicate metadata persistence outcomes accepted idempotently.\n" +
			"# TYPE olp_request_metadata_persistence_duplicates_total counter\n" +
			"# HELP olp_request_metadata_events_processed_total Stream metadata entries durably resolved by the replicated consumer group.\n" +
			"# TYPE olp_request_metadata_events_processed_total counter\n")
		fmt.Fprintf(&body, "olp_request_metadata_events_reclaimed_total %d\n"+
			"olp_request_metadata_events_recovered_total %d\n"+
			"olp_request_metadata_persistence_duplicates_total %d\n"+
			"olp_request_metadata_events_processed_total %d\n",
			counters.RequestMetadataReclaimed, counters.RequestMetadataRecovered,
			counters.RequestMetadataDuplicates, counters.RequestMetadataProcessed)
	}

	// Operations rollup.
	body.WriteString("# HELP olp_operational_metrics_available Whether the PostgreSQL operational rollup was available.\n" +
		"# TYPE olp_operational_metrics_available gauge\n")
	fmt.Fprintf(&body, "olp_operational_metrics_available %d\n", boolInt(operationsErr == nil))
	if operationsErr == nil {
		body.WriteString("# HELP olp_requests_5m Metadata requests observed during the trailing five minutes.\n" +
			"# TYPE olp_requests_5m gauge\n" +
			"# HELP olp_request_success_ratio_5m Successful request ratio during the trailing five minutes.\n" +
			"# TYPE olp_request_success_ratio_5m gauge\n" +
			"# HELP olp_request_latency_seconds Request latency quantiles during the trailing five minutes.\n" +
			"# TYPE olp_request_latency_seconds gauge\n" +
			"# HELP olp_upstream_cancellations_5m Cancelled upstream attempts during the trailing five minutes.\n" +
			"# TYPE olp_upstream_cancellations_5m gauge\n")
		var p95, p99 float64
		if operations.P95LatencyMs != nil {
			p95 = *operations.P95LatencyMs / 1000
		}
		if operations.P99LatencyMs != nil {
			p99 = *operations.P99LatencyMs / 1000
		}
		fmt.Fprintf(&body, "olp_requests_5m %d\n"+
			"olp_request_success_ratio_5m %.6f\n"+
			"olp_request_latency_seconds{quantile=\"0.95\"} %.6f\n"+
			"olp_request_latency_seconds{quantile=\"0.99\"} %.6f\n"+
			"olp_upstream_cancellations_5m %d\n",
			operations.RequestCount,
			successRatio(operations.SuccessCount, operations.RequestCount),
			p95, p99, operations.CancelledAttempts)
	}

	// Provider health series.
	if len(providers) > 0 {
		body.WriteString("# HELP olp_provider_attempts_15m Number of sampled provider attempts in the trailing fifteen minutes.\n" +
			"# TYPE olp_provider_attempts_15m gauge\n" +
			"# HELP olp_provider_health Provider health classification over the trailing fifteen minutes.\n" +
			"# TYPE olp_provider_health gauge\n" +
			"# HELP olp_provider_success_ratio_15m Provider attempt success ratio over the trailing fifteen minutes.\n" +
			"# TYPE olp_provider_success_ratio_15m gauge\n" +
			"# HELP olp_provider_latency_seconds_15m Provider average attempt latency over the trailing fifteen minutes.\n" +
			"# TYPE olp_provider_latency_seconds_15m gauge\n")
		for _, provider := range providers {
			// Values are escaped once, for the exposition format; %q would
			// escape them again in Go syntax, which that format does not define.
			id := prometheusLabel(provider.ProviderID)
			name := prometheusLabel(provider.ProviderName)
			kind := prometheusLabel(provider.ProviderKind)
			fmt.Fprintf(&body, "olp_provider_health{provider_id=\"%s\",provider_name=\"%s\",provider_kind=\"%s\",status=\"%s\"} 1\n",
				id, name, kind, prometheusLabel(provider.Status))
			fmt.Fprintf(&body, "olp_provider_attempts_15m{provider_id=\"%s\",provider_kind=\"%s\"} %d\n",
				id, kind, provider.AttemptCount)
			if provider.AttemptCount == 0 {
				continue
			}
			var averageLatency float64
			if provider.AverageLatencyMs != nil {
				averageLatency = *provider.AverageLatencyMs / 1000
			}
			fmt.Fprintf(&body, "olp_provider_success_ratio_15m{provider_id=\"%s\",provider_kind=\"%s\"} %.6f\n"+
				"olp_provider_latency_seconds_15m{provider_id=\"%s\",provider_kind=\"%s\"} %.6f\n",
				id, kind, successRatio(provider.SuccessCount, provider.AttemptCount),
				id, kind, averageLatency)
		}
	}

	return body.String(), nil
}

// prometheusLabel escapes a label value per the exposition format.
func prometheusLabel(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\n", "\\n")
	return strings.ReplaceAll(value, "\"", "\\\"")
}

const int64Max = math.MaxInt64

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
