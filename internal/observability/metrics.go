package observability

import (
	"context"
	"math"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tyk-swe/olp/internal/media/jobs"
	"github.com/tyk-swe/olp/internal/metric"
	"github.com/tyk-swe/olp/internal/observability/workerhealth"
	"github.com/tyk-swe/olp/internal/usage"
)

var (
	providerMetricsComplete = metric.Gauge("olp_provider_metrics_complete", "Whether provider collection completed without truncation or query failure.")
	providerMetricsEmitted  = metric.Gauge("olp_provider_metrics_emitted", "Number of providers in this snapshot.")
	runtimeGeneration       = metric.Gauge("olp_runtime_generation", "Current immutable runtime generation.")

	metadataDropped           = metric.Counter("olp_request_metadata_events_dropped_total", "Metadata events dropped from the bounded buffer.")
	metadataAbandoned         = metric.Counter("olp_request_metadata_events_abandoned_total", "Accepted metadata events abandoned during shutdown or worker failure.")
	metadataPending           = metric.Gauge("olp_request_metadata_events_pending", "Accepted metadata events not yet written to the stream.")
	metadataRetrying          = metric.Gauge("olp_request_metadata_stream_retrying", "Whether the local writer is retrying Valkey.")
	metadataSinkAvailable     = metric.Gauge("olp_request_metadata_persistence_available", "Whether a request metadata sink is active.")
	consumerPending           = metric.Gauge("olp_request_metadata_consumer_pending_events", "Delivered request metadata events awaiting consumer acknowledgement.")
	consumerLag               = metric.Gauge("olp_request_metadata_consumer_lag_events", "Request metadata stream events not yet delivered to the persistence consumer.")
	consumerHeartbeatAge      = metric.Gauge("olp_request_metadata_consumer_heartbeat_age_seconds", "Age of the last durable worker checkpoint.")
	consumerHealthy           = metric.Gauge("olp_request_metadata_consumer_healthy", "Whether the durable consumer is current and fully drained.")
	consumerStale             = metric.Gauge("olp_request_metadata_consumer_stale", "Whether the durable consumer missed its heartbeat threshold.")
	consumerOldestPendingAge  = metric.Gauge("olp_request_metadata_consumer_oldest_pending_age_seconds", "Age of the oldest delivered metadata entry awaiting acknowledgement.")
	gatewayOpenEpochs         = metric.Gauge("olp_request_metadata_gateway_open_epochs", "Gateway process epochs still emitting checkpoints.")
	gatewayUnresolvedEpochs   = metric.Gauge("olp_request_metadata_gateway_unresolved_epochs", "Unclean gateway epochs awaiting operator acknowledgement.")
	historicalUncertainGaps   = metric.Gauge("olp_request_metadata_historical_uncertain_gaps", "Retained exactness gaps across raw and hourly evidence.")
	unresolvedEventLowerBound = metric.Gauge("olp_request_metadata_gateway_unresolved_event_lower_bound", "Last durable in-flight event lower bound across unresolved epochs.")
	metadataLossReported      = metric.Counter("olp_request_metadata_loss_reported_total", "Local buffer loss durably reported by the gateway checkpoint.", "kind")
	metadataReclaimed         = metric.Counter("olp_request_metadata_events_reclaimed_total", "Metadata entries transferred from stale consumer ownership.")
	metadataRecovered         = metric.Counter("olp_request_metadata_events_recovered_total", "Pending metadata entries durably resolved by a recovery pass.")
	metadataDuplicates        = metric.Counter("olp_request_metadata_persistence_duplicates_total", "Duplicate metadata persistence outcomes accepted idempotently.")
	metadataProcessed         = metric.Counter("olp_request_metadata_events_processed_total", "Stream metadata entries durably resolved by the replicated consumer group.")

	limiterAvailable = metric.Gauge("olp_distributed_limiter_available", "Whether a Valkey limiter connection is installed.")
	limitsFailOpen   = metric.Counter("olp_limits_fail_open_total", "Rate- or concurrency-limited requests admitted without a lease under the fail-open outage policy.")
	budgetRejections = metric.Counter("olp_key_budget_rejections_total", "API-key requests rejected because a cost budget was exhausted or could not hold the request's estimated cost.", "window")
	openCircuits     = metric.Gauge("olp_open_target_circuits", "Number of target circuits currently open or half-open.")
	shadowRequests   = metric.Counter("olp_shadow_requests_total", "Shadow attempts mirrored, or dropped because the shadow pool was full.", "outcome")

	mediaPending       = metric.Gauge("olp_media_reconciliation_pending", "Metadata-only media jobs awaiting reconciliation.")
	mediaStale         = metric.Gauge("olp_media_reconciliation_stale", "Media reconciliation jobs past their grace period.")
	mediaFailed        = metric.Gauge("olp_media_reconciliation_failed", "Media jobs whose latest autonomous reconciliation attempt failed.")
	mediaGaps          = metric.Counter("olp_media_reconciliation_gaps_total", "Upstream media side effects that could not be durably recorded.")
	mediaSpoolCapacity = metric.Gauge("olp_media_spool_capacity_bytes", "Configured capacity of the private local media spool.")
	mediaSpoolUsed     = metric.Gauge("olp_media_spool_used_bytes", "Bytes currently reserved in the private local media spool.")

	asyncObservability    = metric.Gauge("olp_async_worker_observability_available", "Whether all PostgreSQL-backed asynchronous worker summaries were available.")
	asyncPlaneCurrent     = metric.Gauge("olp_async_plane_current", "Whether every replicated worker responsibility has a current successful checkpoint.")
	asyncPlaneDrained     = metric.Gauge("olp_async_plane_drained", "Whether request-metadata backlogs are drained.")
	asyncPlaneHealthy     = metric.Gauge("olp_async_plane_healthy", "Whether the asynchronous plane is both current and drained.")
	asyncPlaneProgress    = metric.Gauge("olp_async_plane_last_progress_timestamp_seconds", "Unix time of the latest durable worker progress.")
	workerTaskHealthy     = metric.Gauge("olp_worker_task_healthy", "Whether a fixed worker responsibility has a current successful checkpoint.", "task")
	workerTaskHeartbeat   = metric.Gauge("olp_worker_task_heartbeat_age_seconds", "Age of the latest checkpoint for a fixed worker responsibility.", "task")
	workerTaskLastSuccess = metric.Gauge("olp_worker_task_last_success_age_seconds", "Age of the latest successful checkpoint for a fixed worker responsibility.", "task")
	workerTaskRuns        = metric.Counter("olp_worker_task_runs_total", "Durable checkpoint outcomes across all worker replicas.", "task", "outcome")

	operationsAvailable   = metric.Gauge("olp_operational_metrics_available", "Whether the PostgreSQL operational rollup was available.")
	requests5m            = metric.Gauge("olp_requests_5m", "Metadata requests observed during the trailing five minutes.")
	requestSuccessRatio5m = metric.Gauge("olp_request_success_ratio_5m", "Successful request ratio during the trailing five minutes.")
	requestLatency        = metric.Gauge("olp_request_latency_seconds", "Request latency quantiles during the trailing five minutes.", "quantile")
	upstreamCancellations = metric.Gauge("olp_upstream_cancellations_5m", "Cancelled upstream attempts during the trailing five minutes.")

	providerHealth       = metric.Gauge("olp_provider_health", "Provider health classification over the trailing fifteen minutes.", "provider_id", "provider_name", "provider_kind", "status")
	providerAttempts     = metric.Gauge("olp_provider_attempts_15m", "Number of sampled provider attempts in the trailing fifteen minutes.", "provider_id", "provider_kind")
	providerSuccessRatio = metric.Gauge("olp_provider_success_ratio_15m", "Provider attempt success ratio over the trailing fifteen minutes.", "provider_id", "provider_kind")
	providerLatency      = metric.Gauge("olp_provider_latency_seconds_15m", "Provider average attempt latency over the trailing fifteen minutes.", "provider_id", "provider_kind")
)

// successRatio treats "nothing observed" as fully healthy so an idle window
// does not read as an outage.
func successRatio(success, total int64) float64 {
	if total == 0 {
		return 1.0
	}
	return float64(success) / float64(total)
}

func ageSecondsOrInf(age *int64) float64 {
	if age == nil {
		return math.Inf(1)
	}
	return float64(*age)
}

func CollectMetrics(ctx context.Context, s *Sources) []prometheus.Metric {
	now := time.Now()
	providers, providersComplete, providersErr := ReadProviderHealthMetrics(ctx, s.Store)
	limiterConfigured, limiterHealthy := s.limiter(ctx)
	consumer, consumerErr := usage.ReadConsumerStatus(ctx, s.Store, now)
	if consumerErr != nil {
		consumer = usage.ConsumerStatus{State: usage.ConsumerUnknown}
	}
	tasks, tasksErr := ReadRegionalWorkerTaskHealth(ctx, s.Store, s.Region)
	counters, countersErr := ReadWorkerRecoveryCounters(ctx, s.Store)
	operations, operationsErr := ReadOperationsSummary(ctx, s.Store)
	metrics := [][]prometheus.Metric{
		{
			providerMetricsComplete.Sample(metric.Flag(providersErr == nil && providersComplete)),
			providerMetricsEmitted.Sample(float64(len(providers))),
			runtimeGeneration.Sample(float64(currentGeneration(s.runtimeProbe()))),
			limiterAvailable.Sample(metric.Flag(limiterConfigured && limiterHealthy)),
			asyncObservability.Sample(metric.Flag(consumerErr == nil && tasksErr == nil && countersErr == nil)),
			operationsAvailable.Sample(metric.Flag(operationsErr == nil)),
		},
		metadataMetrics(ctx, s, consumer, consumerErr == nil),
		gatewayMetrics(s.Gateway),
		mediaMetrics(ctx, s, now),
		asyncPlaneMetrics(tasks, consumer, limiterConfigured, now),
		providerMetrics(providers),
	}
	if countersErr == nil {
		metrics = append(metrics, recoveryMetrics(counters))
	}
	if operationsErr == nil {
		metrics = append(metrics, operationsMetrics(operations))
	}
	if s.Export != nil {
		metrics = append(metrics, s.Export(ctx))
	}
	return concat(metrics)
}

func concat(groups [][]prometheus.Metric) []prometheus.Metric {
	total := 0
	for _, group := range groups {
		total += len(group)
	}
	all := make([]prometheus.Metric, 0, total)
	for _, group := range groups {
		all = append(all, group...)
	}
	return all
}

func currentGeneration(probe RuntimeProbe) int64 {
	if probe.Generation == nil {
		return 0
	}
	return *probe.Generation
}

func metadataMetrics(ctx context.Context, s *Sources, consumer usage.ConsumerStatus, consumerRead bool) []prometheus.Metric {
	snapshot := usage.Snapshot{Closed: true}
	if s.Emitter != nil {
		snapshot = s.Emitter.Snapshot()
	}
	// An absent epoch table reads as zero rather than suppressing the exposition.
	epochs, _ := ReadEpochHealth(ctx, s.Store)
	heartbeatAge := math.Inf(1)
	if consumerRead {
		heartbeatAge = ageSecondsOrInf(consumer.HeartbeatAgeSeconds)
	}
	var events, dropped, abandoned int64
	if s.Loss != nil {
		events, dropped, abandoned = s.Loss.Totals()
	}
	return []prometheus.Metric{
		metadataDropped.Sample(float64(snapshot.Dropped)),
		metadataAbandoned.Sample(float64(snapshot.Abandoned)),
		metadataPending.Sample(float64(snapshot.Pending())),
		metadataRetrying.Sample(metric.Flag(snapshot.Retrying)),
		metadataSinkAvailable.Sample(metric.Flag(!snapshot.Closed)),
		consumerPending.Sample(float64(consumer.PendingEvents)),
		consumerLag.Sample(float64(consumer.LagEvents)),
		consumerHeartbeatAge.Sample(heartbeatAge),
		consumerHealthy.Sample(metric.Flag(consumerRead && consumer.Complete())),
		consumerStale.Sample(metric.Flag(consumerRead && consumer.State == usage.ConsumerStale)),
		gatewayOpenEpochs.Sample(float64(epochs.OpenEpochs)),
		gatewayUnresolvedEpochs.Sample(float64(epochs.UnresolvedEpochs)),
		historicalUncertainGaps.Sample(float64(epochs.HistoricalUncertainGaps)),
		unresolvedEventLowerBound.Sample(float64(epochs.UnresolvedEventLowerBound)),
		// All three loss series are emitted at zero too: an absent line means
		// the exporter is broken rather than that nothing was lost.
		metadataLossReported.Sample(float64(events), "events"),
		metadataLossReported.Sample(float64(dropped), "dropped"),
		metadataLossReported.Sample(float64(abandoned), "abandoned"),
	}
}

func gatewayMetrics(gateway GatewayCounters) []prometheus.Metric {
	var failOpen, daily, weekly, monthly, circuits, mirrored, shadowDropped int64
	if gateway != nil {
		failOpen, daily, weekly, monthly = gateway.LimiterCounts()
		circuits = gateway.OpenCircuits()
		mirrored, shadowDropped = gateway.ShadowCounts()
	}
	return []prometheus.Metric{
		limitsFailOpen.Sample(float64(failOpen)),
		budgetRejections.Sample(float64(daily), "daily"),
		budgetRejections.Sample(float64(weekly), "weekly"),
		budgetRejections.Sample(float64(monthly), "monthly"),
		openCircuits.Sample(float64(circuits)),
		shadowRequests.Sample(float64(mirrored), "mirrored"),
		shadowRequests.Sample(float64(shadowDropped), "dropped"),
	}
}

func mediaMetrics(ctx context.Context, s *Sources, now time.Time) []prometheus.Metric {
	summary, err := s.Media.Summary(ctx, now)
	if err != nil {
		summary = jobs.Summary{}
	}
	metrics := []prometheus.Metric{
		mediaPending.Sample(float64(summary.Pending)),
		mediaStale.Sample(float64(summary.Stale)),
		mediaFailed.Sample(float64(summary.Failed)),
		mediaGaps.Sample(float64(s.mediaGaps())),
	}
	// Without a spool the series are absent, which says no spool is configured.
	if s.Spool == nil {
		return metrics
	}
	metrics = append(metrics,
		mediaSpoolCapacity.Sample(float64(s.Spool.CapacityBytes())),
		mediaSpoolUsed.Sample(float64(s.Spool.UsedBytes())),
	)
	return metrics
}

func asyncPlaneMetrics(tasks *WorkerTaskHealth, consumer usage.ConsumerStatus, limiterConfigured bool, now time.Time) []prometheus.Metric {
	if tasks == nil {
		return nil
	}
	plane := asynchronousPlane(tasks, expectedTasks(limiterConfigured), consumer)
	current, drained := plane.AsynchronousPlaneCurrent, plane.AsynchronousPlaneDrained
	var lastProgress int64
	if at := tasks.LastProgressFor(workerhealth.Tasks[:]); at != nil {
		lastProgress = at.Unix()
	}
	var oldestPendingAge float64
	if consumer.OldestPendingAt != nil {
		oldestPendingAge = max(float64(now.Sub(*consumer.OldestPendingAt)/time.Second), 0)
	}
	metrics := []prometheus.Metric{
		asyncPlaneCurrent.Sample(metric.Flag(current)),
		asyncPlaneDrained.Sample(metric.Flag(drained)),
		asyncPlaneHealthy.Sample(metric.Flag(current && drained)),
		asyncPlaneProgress.Sample(float64(lastProgress)),
		consumerOldestPendingAge.Sample(oldestPendingAge),
	}
	for _, task := range tasks.Tasks {
		name := string(task.Task)
		metrics = append(metrics,
			workerTaskHealthy.Sample(metric.Flag(task.State == TaskHealthy), name),
			workerTaskHeartbeat.Sample(ageSecondsOrInf(task.HeartbeatAgeSeconds), name),
			workerTaskLastSuccess.Sample(ageSecondsOrInf(task.LastSuccessAgeSeconds), name),
			workerTaskRuns.Sample(float64(task.SuccessesTotal), name, "success"),
			workerTaskRuns.Sample(float64(task.FailuresTotal), name, "failure"),
			workerTaskRuns.Sample(float64(task.SkippedTotal), name, "skipped"),
		)
	}
	return metrics
}

// recoveryMetrics is only called after a successful read: publishing zeros on
// failure would read as a counter reset.
func recoveryMetrics(counters WorkerRecoveryCounters) []prometheus.Metric {
	return []prometheus.Metric{
		metadataReclaimed.Sample(float64(counters.RequestMetadataReclaimed)),
		metadataRecovered.Sample(float64(counters.RequestMetadataRecovered)),
		metadataDuplicates.Sample(float64(counters.RequestMetadataDuplicates)),
		metadataProcessed.Sample(float64(counters.RequestMetadataProcessed)),
	}
}

func operationsMetrics(operations OperationsSummary) []prometheus.Metric {
	var p95, p99 float64
	if operations.P95LatencyMs != nil {
		p95 = *operations.P95LatencyMs / 1000
	}
	if operations.P99LatencyMs != nil {
		p99 = *operations.P99LatencyMs / 1000
	}
	return []prometheus.Metric{
		requests5m.Sample(float64(operations.RequestCount)),
		requestSuccessRatio5m.Sample(successRatio(operations.SuccessCount, operations.RequestCount)),
		requestLatency.Sample(p95, "0.95"),
		requestLatency.Sample(p99, "0.99"),
		upstreamCancellations.Sample(float64(operations.CancelledAttempts)),
	}
}

func providerMetrics(providers []ProviderHealthRecord) []prometheus.Metric {
	metrics := make([]prometheus.Metric, 0, 4*len(providers))
	for _, p := range providers {
		id := p.ProviderID.String()
		metrics = append(metrics,
			providerHealth.Sample(1, id, p.ProviderName, p.ProviderKind, string(p.Status)),
			providerAttempts.Sample(float64(p.AttemptCount), id, p.ProviderKind),
		)
		if p.AttemptCount == 0 {
			continue
		}
		var latency float64
		if p.AverageLatencyMs != nil {
			latency = *p.AverageLatencyMs / 1000
		}
		metrics = append(metrics,
			providerSuccessRatio.Sample(successRatio(p.SuccessCount, p.AttemptCount), id, p.ProviderKind),
			providerLatency.Sample(latency, id, p.ProviderKind),
		)
	}
	return metrics
}
