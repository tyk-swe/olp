package observability

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/usage"
)

// State carries the dependencies the readiness and metrics collectors probe.
// Everything is a small function so collectors stay honest when a plane was
// never configured: an unconfigured probe reports absence, not failure.
type State struct {
	Pool   storeQuerier
	PingDB func(context.Context) error
	// Mode classifies which responsibilities this process must evidence.
	ServesGateway bool
	// Runtime probes the pinned authority and generation.
	Runtime func() RuntimeProbe
	// Limiter probes the Valkey limiter. Configured is false when the
	// installation has no shared state backend at all.
	Limiter       func(ctx context.Context) (configured, healthy bool)
	LimiterCounts func() (failOpen, dailyRejections, monthlyRejections int64)
	// Circuits counts currently open upstream circuits.
	Circuits func() int64
	// Emitter snapshots the local request metadata buffer.
	Emitter func() *usage.Snapshot
	// Spool reports media spool capacity and usage; nil means no spool.
	Spool func() (capacity, used *int64)
	// MediaGaps counts reconciliation steps that could not be checkpointed.
	MediaGaps func() int64
	// LossCounters returns the durably reported metadata-loss totals.
	LossCounters func() (events, dropped, abandoned int64)
	// HardLimits reports whether any pinned key carries hard limits; only then
	// does a limiter outage degrade gateway readiness.
	HardLimits func() bool
}

// storeQuerier is the subset of the pool the collectors use.
type storeQuerier interface {
	access.Queryer
	media.Querier
}

// RuntimeProbe is the pinned-runtime picture readiness needs.
type RuntimeProbe struct {
	Generation     *int64
	AuthorityAge   *time.Duration
	AuthorityStale bool
	// AllTransports is false when a pinned provider has no transport.
	AllTransports bool
}

// Health is the management readiness payload. Field names and semantics match
// the OpenAPI contract the console renders.
type Health struct {
	Status                                     string     `json:"status"`
	AsynchronousPlane                          string     `json:"asynchronous_plane"`
	AsynchronousPlaneCurrent                   bool       `json:"asynchronous_plane_current"`
	AsynchronousPlaneDrained                   bool       `json:"asynchronous_plane_drained"`
	AsynchronousPlaneLastProgressAt            *time.Time `json:"asynchronous_plane_last_progress_at,omitempty"`
	WorkerTasksStale                           int64      `json:"worker_tasks_stale"`
	WorkerTasksUnknown                         int64      `json:"worker_tasks_unknown"`
	Generation                                 *int64     `json:"generation,omitempty"`
	Database                                   string     `json:"database"`
	Limits                                     string     `json:"limits"`
	RequestMetadataComplete                    bool       `json:"request_metadata_complete"`
	RequestMetadataConsumer                    string     `json:"request_metadata_consumer"`
	RequestMetadataConsumerPendingEvents       int64      `json:"request_metadata_consumer_pending_events"`
	RequestMetadataConsumerLagEvents           int64      `json:"request_metadata_consumer_lag_events"`
	RequestMetadataConsumerOldestPendingAt     *time.Time `json:"request_metadata_consumer_oldest_pending_at,omitempty"`
	RequestMetadataConsumerOldestPendingAgeSec *int64     `json:"request_metadata_consumer_oldest_pending_age_seconds,omitempty"`
	RequestMetadataConsumerCheckedAt           *time.Time `json:"request_metadata_consumer_checked_at,omitempty"`
	RequestMetadataConsumerHeartbeatAgeSeconds *int64     `json:"request_metadata_consumer_heartbeat_age_seconds,omitempty"`
	RequestMetadataReclaimedEventsTotal        *int64     `json:"request_metadata_reclaimed_events_total,omitempty"`
	RequestMetadataRecoveredEventsTotal        *int64     `json:"request_metadata_recovered_events_total,omitempty"`
	RequestMetadataDuplicatePersistenceTotal   *int64     `json:"request_metadata_duplicate_persistence_total,omitempty"`
	RequestMetadataGatewayOpenEpochs           int64      `json:"request_metadata_gateway_open_epochs"`
	RequestMetadataGatewayUnresolvedEpochs     int64      `json:"request_metadata_gateway_unresolved_epochs"`
	RequestMetadataHistoricalUncertainGaps     int64      `json:"request_metadata_historical_uncertain_gaps"`
	RequestMetadataGatewayUnresolvedLowerBound int64      `json:"request_metadata_gateway_unresolved_event_lower_bound"`
	RuntimeOutbox                              string     `json:"runtime_outbox"`
	RuntimeOutboxPendingRows                   int64      `json:"runtime_outbox_pending_rows"`
	RuntimeOutboxOldestPendingAt               *time.Time `json:"runtime_outbox_oldest_pending_at,omitempty"`
	RuntimeOutboxOldestPendingAgeSeconds       *int64     `json:"runtime_outbox_oldest_pending_age_seconds,omitempty"`
	RuntimeOutboxOwnerActive                   bool       `json:"runtime_outbox_owner_active"`
	RuntimeOutboxClaimedRows                   int64      `json:"runtime_outbox_claimed_rows"`
	RuntimeOutboxOwnerAbandoned                bool       `json:"runtime_outbox_owner_abandoned"`
	RuntimeOutboxHeartbeatAgeSeconds           *int64     `json:"runtime_outbox_heartbeat_age_seconds,omitempty"`
	RuntimeOutboxPublicationAttemptsTotal      *int64     `json:"runtime_outbox_publication_attempts_total,omitempty"`
	RuntimeOutboxPublicationRetriesTotal       *int64     `json:"runtime_outbox_publication_retries_total,omitempty"`
	RuntimeOutboxRepeatedPublicationAttempts   *int64     `json:"runtime_outbox_repeated_publication_attempts_total,omitempty"`
	RuntimeOutboxAbandonedOwnershipTotal       *int64     `json:"runtime_outbox_abandoned_ownership_total,omitempty"`
	RuntimeOutboxFailedTakeoversTotal          *int64     `json:"runtime_outbox_failed_takeovers_total,omitempty"`
	MediaReconciliation                        string     `json:"media_reconciliation"`
	MediaReconciliationPending                 int64      `json:"media_reconciliation_pending"`
	MediaReconciliationStale                   int64      `json:"media_reconciliation_stale"`
	MediaReconciliationFailed                  int64      `json:"media_reconciliation_failed"`
	MediaReconciliationGapsTotal               int64      `json:"media_reconciliation_gaps_total"`
	MediaSpoolUsedBytes                        *int64     `json:"media_spool_used_bytes,omitempty"`
	MediaSpoolCapacityBytes                    *int64     `json:"media_spool_capacity_bytes,omitempty"`
}

// LiveHealth is the liveness response: every dependency unchecked.
func LiveHealth() Health {
	return Health{
		Status:                  "ok",
		AsynchronousPlane:       "not_checked",
		Database:                "not_checked",
		Limits:                  "not_checked",
		RequestMetadataComplete: true,
		RequestMetadataConsumer: "not_checked",
		RuntimeOutbox:           "not_checked",
		MediaReconciliation:     "not_checked",
	}
}

// CollectReadiness probes every dependency and builds the readiness payload.
// Database failure degrades honestly: a gateway with a pinned runtime may keep
// serving from the last known good snapshot, which is reported as such.
func CollectReadiness(ctx context.Context, s *State) (*Health, error) {
	now := time.Now()
	var probe RuntimeProbe
	if s.Runtime != nil {
		probe = s.Runtime()
	}
	generation := probe.Generation
	store, err := probeStore(ctx, s, now, generation != nil)
	if err != nil {
		return nil, err
	}
	if s.ServesGateway {
		if generation == nil {
			return nil, errors.New("runtime_generation_unavailable")
		}
		if probe.AuthorityStale {
			return nil, errors.New("api_key_authority_stale")
		}
		if !probe.AllTransports {
			return nil, errors.New("provider_transport_unavailable")
		}
	}
	limiterConfigured, limitsHealthy := false, false
	if s.Limiter != nil {
		limiterConfigured, limitsHealthy = s.Limiter(ctx)
	}
	hardLimits := s.HardLimits != nil && s.HardLimits()
	// Valkey loss degrades only requests whose keys declare hard limits; the
	// request path fails those keys closed while unlimited keys remain safe.
	degradedLimits := s.ServesGateway && hardLimits && !limitsHealthy
	mediaGaps := int64(0)
	if s.MediaGaps != nil {
		mediaGaps = s.MediaGaps()
	}
	degradedMedia := (store.media != nil && (store.media.Stale > 0 || store.media.Failed > 0)) || mediaGaps > 0
	localMetadataComplete := true
	if s.ServesGateway && s.Emitter != nil {
		if snapshot := s.Emitter(); snapshot != nil {
			localMetadataComplete = snapshot.Complete()
		}
	}
	expectsConsumer := limiterConfigured
	metadataComplete := localMetadataComplete &&
		(!expectsConsumer || store.consumer.Complete()) &&
		store.epochs.UnresolvedEpochs == 0
	current, drained := asynchronousPlaneFlags(store.tasks, expectedTasks(limiterConfigured), store.consumer)
	response := readinessResponse(s, now, generation, store, expectedTasks(limiterConfigured),
		current, drained, metadataComplete, degradedLimits, degradedMedia, limitsHealthy, limiterConfigured, mediaGaps)
	return response, nil
}

// storeProbe bundles the store-side probes.
type storeProbe struct {
	database string
	media    *media.Summary
	consumer usage.ConsumerStatus
	epochs   EpochHealth
	tasks    *WorkerTaskHealth
	counters *WorkerRecoveryCounters
}

func probeStore(ctx context.Context, s *State, now time.Time, hasGeneration bool) (*storeProbe, error) {
	probe := &storeProbe{
		database: "unavailable_lkg",
		consumer: usage.ConsumerStatus{State: usage.ConsumerUnknown},
		tasks:    &WorkerTaskHealth{},
	}
	if err := s.PingDB(ctx); err != nil {
		// A gateway holding a pinned runtime can keep serving from the last
		// known good snapshot; anything else is simply unready.
		if s.ServesGateway && hasGeneration {
			for _, task := range WorkerTasks {
				probe.tasks.Tasks = append(probe.tasks.Tasks, WorkerTaskStatus{Task: task.Name, State: TaskStateUnknown})
			}
			return probe, nil
		}
		return nil, errors.New("database_unavailable")
	}
	probe.database = "ok"
	summary, err := media.ReconciliationSummary(ctx, s.Pool, now)
	if err != nil {
		return nil, fmt.Errorf("database_unavailable: media reconciliation summary: %w", err)
	}
	probe.media = &summary
	consumer, err := usage.ReadConsumerStatus(ctx, s.Pool, now)
	if err != nil {
		return nil, fmt.Errorf("database_unavailable: metadata consumer: %w", err)
	}
	probe.consumer = consumer
	epochs, err := ReadEpochHealth(ctx, s.Pool)
	if err != nil {
		return nil, fmt.Errorf("database_unavailable: gateway epochs: %w", err)
	}
	probe.epochs = epochs
	tasks, err := ReadWorkerTaskHealth(ctx, s.Pool)
	if err != nil {
		return nil, fmt.Errorf("database_unavailable: worker tasks: %w", err)
	}
	probe.tasks = tasks
	counters, err := ReadWorkerRecoveryCounters(ctx, s.Pool)
	if err != nil {
		return nil, fmt.Errorf("database_unavailable: worker counters: %w", err)
	}
	probe.counters = &counters
	return probe, nil
}

// expectedTasks lists the worker responsibilities this process must see live.
func expectedTasks(limiterConfigured bool) []WorkerTask {
	if limiterConfigured {
		return WorkerTasks
	}
	out := make([]WorkerTask, 0, len(WorkerTasks)-len(ValkeyWorkerTasks))
	for _, task := range WorkerTasks {
		valkey := false
		for _, name := range ValkeyWorkerTasks {
			if task.Name == name {
				valkey = true
			}
		}
		if !valkey {
			out = append(out, task)
		}
	}
	return out
}

// asynchronousPlaneFlags classifies the replicated worker plane's freshness
// and drainage. There is no runtime outbox on this backend, so the plane is
// the metadata consumer plus the fixed tasks.
func asynchronousPlaneFlags(tasks *WorkerTaskHealth, expected []WorkerTask, consumer usage.ConsumerStatus) (current, drained bool) {
	expectsConsumer := false
	for _, task := range expected {
		if task.Name == string(usage.TaskRequestMetadataConsumer) {
			expectsConsumer = true
		}
	}
	current = tasks.CurrentFor(expected) &&
		(!expectsConsumer || consumer.State == usage.ConsumerHealthy || consumer.State == usage.ConsumerBacklogged)
	drained = !expectsConsumer || (consumer.PendingEvents == 0 && consumer.LagEvents == 0)
	return current, drained
}

func asynchronousPlaneState(current, drained bool, tasks *WorkerTaskHealth, expected []WorkerTask, consumer usage.ConsumerStatus) string {
	if current && drained {
		return "healthy"
	}
	if current {
		return "backlogged"
	}
	if tasks.UnknownFor(expected) > 0 || consumer.State == usage.ConsumerUnknown {
		return "unknown"
	}
	return "stale"
}

func readinessResponse(s *State, now time.Time, generation *int64, probe *storeProbe, expected []WorkerTask,
	current, drained, metadataComplete, degradedLimits, degradedMedia, limitsHealthy, limiterConfigured bool, mediaGaps int64) *Health {
	consumer := probe.consumer
	epochs := probe.epochs
	counters := probe.counters
	summary := probe.media
	status := "ok"
	if degradedLimits || degradedMedia || !metadataComplete || !(current && drained) {
		status = "degraded"
	}
	limits := "not_configured"
	if limiterConfigured {
		if limitsHealthy {
			limits = "ok"
		} else {
			limits = "unavailable"
		}
	}
	h := &Health{
		Status:                                     status,
		AsynchronousPlane:                          asynchronousPlaneState(current, drained, probe.tasks, expected, consumer),
		AsynchronousPlaneCurrent:                   current,
		AsynchronousPlaneDrained:                   drained,
		AsynchronousPlaneLastProgressAt:            probe.tasks.LastProgressFor(expected),
		WorkerTasksStale:                           probe.tasks.StaleFor(expected),
		WorkerTasksUnknown:                         probe.tasks.UnknownFor(expected),
		Generation:                                 generation,
		Database:                                   probe.database,
		Limits:                                     limits,
		RequestMetadataComplete:                    metadataComplete,
		RequestMetadataConsumer:                    consumer.State,
		RequestMetadataConsumerPendingEvents:       consumer.PendingEvents,
		RequestMetadataConsumerLagEvents:           consumer.LagEvents,
		RequestMetadataConsumerOldestPendingAt:     consumer.OldestPendingAt,
		RequestMetadataConsumerCheckedAt:           consumer.CheckedAt,
		RequestMetadataConsumerHeartbeatAgeSeconds: consumer.HeartbeatAgeSeconds,
		RequestMetadataGatewayOpenEpochs:           epochs.OpenEpochs,
		RequestMetadataGatewayUnresolvedEpochs:     epochs.UnresolvedEpochs,
		RequestMetadataHistoricalUncertainGaps:     epochs.HistoricalUncertainGaps,
		RequestMetadataGatewayUnresolvedLowerBound: epochs.UnresolvedEventLowerBound,
		// This backend publishes runtime revisions in one transaction; there
		// is no outbox to report.
		RuntimeOutbox:                "not_configured",
		MediaReconciliationGapsTotal: mediaGaps,
	}
	if summary != nil {
		h.MediaReconciliation = "ok"
		h.MediaReconciliationPending = summary.Pending
		h.MediaReconciliationStale = summary.Stale
		h.MediaReconciliationFailed = summary.Failed
	} else {
		h.MediaReconciliation = "unknown"
	}
	if consumer.OldestPendingAt != nil {
		age := max(int64(now.Sub(*consumer.OldestPendingAt)/time.Second), 0)
		h.RequestMetadataConsumerOldestPendingAgeSec = &age
	}
	if counters != nil {
		h.RequestMetadataReclaimedEventsTotal = &counters.RequestMetadataReclaimed
		h.RequestMetadataRecoveredEventsTotal = &counters.RequestMetadataRecovered
		h.RequestMetadataDuplicatePersistenceTotal = &counters.RequestMetadataDuplicates
	}
	if s.Spool != nil {
		h.MediaSpoolCapacityBytes, h.MediaSpoolUsedBytes = s.Spool()
	}
	return h
}
