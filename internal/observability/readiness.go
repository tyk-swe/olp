package observability

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/media/jobs"
	"github.com/tyk-swe/olp/internal/observability/workerhealth"
	"github.com/tyk-swe/olp/internal/usage"
)

// Sources are what the readiness and metrics collectors probe. A nil source
// means that plane is not configured in this process, which is reported as
// absence rather than failure.
type Sources struct {
	Region        string
	Store         Store
	ServesGateway bool
	Runtime       RuntimeSource
	Limiter       Pinger
	Gateway       GatewayCounters
	Emitter       MetadataBuffer
	Spool         Spool
	Media         MediaJobs
	MediaGaps     *atomic.Uint64
	Loss          LossCounters
	Export        func(context.Context) []prometheus.Metric
}

type Store interface {
	access.Queryer
	Pinger
}

type MediaJobs interface {
	Summary(ctx context.Context, now time.Time) (jobs.Summary, error)
}

type Pinger interface {
	Ping(context.Context) error
}

type RuntimeSource interface {
	Probe() RuntimeProbe
	HasHardLimits() bool
}

type GatewayCounters interface {
	OpenCircuits() int64
	ShadowCounts() (mirrored, dropped int64)
	LimiterCounts() (failOpen, daily, weekly, monthly int64)
}

type MetadataBuffer interface {
	Snapshot() usage.Snapshot
}

type Spool interface {
	CapacityBytes() int64
	UsedBytes() int64
}

type LossCounters interface {
	Totals() (events, dropped, abandoned int64)
}

// RuntimeProbe is the pinned-runtime picture readiness needs.
type RuntimeProbe struct {
	Generation     *int64
	AuthorityStale bool
	// AllTransports is false when a pinned provider has no transport.
	AllTransports bool
}

func (s *Sources) runtimeProbe() RuntimeProbe {
	if s.Runtime == nil {
		return RuntimeProbe{}
	}
	return s.Runtime.Probe()
}

func (s *Sources) limiter(ctx context.Context) (configured, healthy bool) {
	if s.Limiter == nil {
		return false, false
	}
	return true, s.Limiter.Ping(ctx) == nil
}

func (s *Sources) mediaGaps() int64 {
	if s.MediaGaps == nil {
		return 0
	}
	return int64(s.MediaGaps.Load())
}

func (s *Sources) localMetadataComplete() bool {
	return !s.ServesGateway || s.Emitter == nil || s.Emitter.Snapshot().Complete()
}

type Status string

const (
	StatusOK             Status = "ok"
	StatusDegraded       Status = "degraded"
	StatusHealthy        Status = "healthy"
	StatusBacklogged     Status = "backlogged"
	StatusStale          Status = "stale"
	StatusUnknown        Status = "unknown"
	StatusUnavailable    Status = "unavailable"
	StatusUnavailableLKG Status = "unavailable_lkg"
	StatusNotConfigured  Status = "not_configured"
	StatusNotChecked     Status = "not_checked"
	StatusDisabled       Status = "disabled"
)

type Health struct {
	Status     Status `json:"status"`
	Generation *int64 `json:"generation,omitempty"`
	Database   Status `json:"database"`
	Limits     Status `json:"limits"`
	AsynchronousPlaneHealth
	RequestMetadataHealth
	MediaHealth
}

type AsynchronousPlaneHealth struct {
	AsynchronousPlane               Status     `json:"asynchronous_plane"`
	AsynchronousPlaneCurrent        bool       `json:"asynchronous_plane_current"`
	AsynchronousPlaneDrained        bool       `json:"asynchronous_plane_drained"`
	AsynchronousPlaneLastProgressAt *time.Time `json:"asynchronous_plane_last_progress_at,omitempty"`
	WorkerTasksStale                int64      `json:"worker_tasks_stale"`
	WorkerTasksUnknown              int64      `json:"worker_tasks_unknown"`
}

type RequestMetadataHealth struct {
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
}

type MediaHealth struct {
	MediaReconciliation          Status `json:"media_reconciliation"`
	MediaReconciliationPending   int64  `json:"media_reconciliation_pending"`
	MediaReconciliationStale     int64  `json:"media_reconciliation_stale"`
	MediaReconciliationFailed    int64  `json:"media_reconciliation_failed"`
	MediaReconciliationGapsTotal int64  `json:"media_reconciliation_gaps_total"`
	MediaSpoolUsedBytes          *int64 `json:"media_spool_used_bytes,omitempty"`
	MediaSpoolCapacityBytes      *int64 `json:"media_spool_capacity_bytes,omitempty"`
}

// LiveHealth is the liveness response: every dependency unchecked.
func LiveHealth() Health {
	return Health{
		Status:                  StatusOK,
		Database:                StatusNotChecked,
		Limits:                  StatusNotChecked,
		AsynchronousPlaneHealth: AsynchronousPlaneHealth{AsynchronousPlane: StatusNotChecked},
		RequestMetadataHealth:   RequestMetadataHealth{RequestMetadataComplete: true, RequestMetadataConsumer: string(StatusNotChecked)},
		MediaHealth:             MediaHealth{MediaReconciliation: StatusNotChecked},
	}
}

// CollectReadiness probes every dependency and builds the readiness payload.
// Database failure degrades honestly: a gateway with a pinned runtime may keep
// serving from the last known good snapshot, which is reported as such.
func CollectReadiness(ctx context.Context, s *Sources) (*Health, error) {
	now := time.Now()
	runtime := s.runtimeProbe()
	store, err := probeStore(ctx, s, now, runtime.Generation != nil)
	if err != nil {
		return nil, err
	}
	if err := gatewayReady(s.ServesGateway, runtime); err != nil {
		return nil, err
	}
	limiterConfigured, limiterHealthy := s.limiter(ctx)
	limits := limitsStatus(limiterConfigured, limiterHealthy)
	gaps := s.mediaGaps()
	plane := asynchronousPlane(store.tasks, expectedTasks(limiterConfigured), store.consumer)
	metadata := requestMetadata(store, now, s.localMetadataComplete(), limiterConfigured)
	// Valkey loss degrades only requests whose keys declare hard limits; the
	// request path fails those keys closed while unlimited keys remain safe.
	degradedLimits := s.ServesGateway && s.Runtime != nil && s.Runtime.HasHardLimits() && !limiterHealthy
	degradedMedia := mediaDegraded(store.media, gaps)
	status := StatusOK
	if degradedLimits || degradedMedia || !metadata.RequestMetadataComplete || plane.AsynchronousPlane != StatusHealthy {
		status = StatusDegraded
	}
	return &Health{
		Status:                  status,
		Generation:              runtime.Generation,
		Database:                store.database,
		Limits:                  limits,
		AsynchronousPlaneHealth: plane,
		RequestMetadataHealth:   metadata,
		MediaHealth:             mediaHealth(store.media, gaps, degradedMedia, s.Spool),
	}, nil
}

func gatewayReady(servesGateway bool, runtime RuntimeProbe) error {
	switch {
	case !servesGateway:
		return nil
	case runtime.Generation == nil:
		return errors.New("runtime_generation_unavailable")
	case runtime.AuthorityStale:
		return errors.New("api_key_authority_stale")
	case !runtime.AllTransports:
		return errors.New("provider_transport_unavailable")
	default:
		return nil
	}
}

func limitsStatus(configured, healthy bool) Status {
	switch {
	case !configured:
		return StatusNotConfigured
	case healthy:
		return StatusOK
	default:
		return StatusUnavailable
	}
}

func mediaDegraded(summary *jobs.Summary, gaps int64) bool {
	return (summary != nil && (summary.Stale > 0 || summary.Failed > 0)) || gaps > 0
}

type storeProbe struct {
	database Status
	media    *jobs.Summary
	consumer usage.ConsumerStatus
	epochs   EpochHealth
	tasks    *WorkerTaskHealth
	counters *WorkerRecoveryCounters
}

func probeStore(ctx context.Context, s *Sources, now time.Time, hasGeneration bool) (*storeProbe, error) {
	if err := s.Store.Ping(ctx); err != nil {
		// A gateway holding a pinned runtime can keep serving from the last
		// known good snapshot; anything else is simply unready.
		if s.ServesGateway && hasGeneration {
			return &storeProbe{
				database: StatusUnavailableLKG,
				consumer: usage.ConsumerStatus{State: usage.ConsumerUnknown},
				tasks:    unknownWorkerTasks(),
			}, nil
		}
		return nil, errors.New("database_unavailable")
	}
	summary, err := s.Media.Summary(ctx, now)
	if err != nil {
		return nil, fmt.Errorf("database_unavailable: media reconciliation summary: %w", err)
	}
	consumer, err := usage.ReadConsumerStatus(ctx, s.Store, now)
	if err != nil {
		return nil, fmt.Errorf("database_unavailable: metadata consumer: %w", err)
	}
	epochs, err := ReadEpochHealth(ctx, s.Store)
	if err != nil {
		return nil, fmt.Errorf("database_unavailable: gateway epochs: %w", err)
	}
	tasks, err := ReadRegionalWorkerTaskHealth(ctx, s.Store, s.Region)
	if err != nil {
		return nil, fmt.Errorf("database_unavailable: worker tasks: %w", err)
	}
	counters, err := ReadWorkerRecoveryCounters(ctx, s.Store)
	if err != nil {
		return nil, fmt.Errorf("database_unavailable: worker counters: %w", err)
	}
	return &storeProbe{database: StatusOK, media: &summary, consumer: consumer, epochs: epochs, tasks: tasks, counters: &counters}, nil
}

// expectedTasks lists the worker responsibilities this process must see live.
func expectedTasks(limiterConfigured bool) []workerhealth.Task {
	if limiterConfigured {
		return workerhealth.Tasks[:]
	}
	return slices.Collect(func(yield func(workerhealth.Task) bool) {
		for _, task := range workerhealth.Tasks {
			if !task.Regional() && !yield(task) {
				return
			}
		}
	})
}

func asynchronousPlane(tasks *WorkerTaskHealth, expected []workerhealth.Task, consumer usage.ConsumerStatus) AsynchronousPlaneHealth {
	expectsConsumer := slices.Contains(expected, workerhealth.TaskRequestMetadataConsumer)
	current := tasks.Count(expected, TaskHealthy) == int64(len(expected)) &&
		(!expectsConsumer || consumer.State == usage.ConsumerHealthy || consumer.State == usage.ConsumerBacklogged)
	drained := !expectsConsumer || (consumer.PendingEvents == 0 && consumer.LagEvents == 0)
	unknown := tasks.Count(expected, TaskUnknown)
	state := StatusStale
	switch {
	case current && drained:
		state = StatusHealthy
	case current:
		state = StatusBacklogged
	case unknown > 0 || consumer.State == usage.ConsumerUnknown:
		state = StatusUnknown
	}
	return AsynchronousPlaneHealth{
		AsynchronousPlane:               state,
		AsynchronousPlaneCurrent:        current,
		AsynchronousPlaneDrained:        drained,
		AsynchronousPlaneLastProgressAt: tasks.LastProgressFor(expected),
		WorkerTasksStale:                tasks.Count(expected, TaskStale),
		WorkerTasksUnknown:              unknown,
	}
}

func requestMetadata(store *storeProbe, now time.Time, localComplete, expectsConsumer bool) RequestMetadataHealth {
	consumer, epochs := store.consumer, store.epochs
	health := RequestMetadataHealth{
		RequestMetadataComplete: localComplete &&
			(!expectsConsumer || consumer.Complete()) &&
			epochs.UnresolvedEpochs == 0,
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
	}
	if consumer.OldestPendingAt != nil {
		age := max(int64(now.Sub(*consumer.OldestPendingAt)/time.Second), 0)
		health.RequestMetadataConsumerOldestPendingAgeSec = &age
	}
	if counters := store.counters; counters != nil {
		health.RequestMetadataReclaimedEventsTotal = &counters.RequestMetadataReclaimed
		health.RequestMetadataRecoveredEventsTotal = &counters.RequestMetadataRecovered
		health.RequestMetadataDuplicatePersistenceTotal = &counters.RequestMetadataDuplicates
	}
	return health
}

func mediaHealth(summary *jobs.Summary, gaps int64, degraded bool, spool Spool) MediaHealth {
	health := MediaHealth{MediaReconciliation: StatusUnknown, MediaReconciliationGapsTotal: gaps}
	if summary != nil {
		health.MediaReconciliation = StatusOK
		if degraded {
			health.MediaReconciliation = StatusDegraded
		}
		health.MediaReconciliationPending = summary.Pending
		health.MediaReconciliationStale = summary.Stale
		health.MediaReconciliationFailed = summary.Failed
	}
	if spool != nil {
		capacity, used := spool.CapacityBytes(), spool.UsedBytes()
		health.MediaSpoolCapacityBytes, health.MediaSpoolUsedBytes = &capacity, &used
	}
	return health
}
