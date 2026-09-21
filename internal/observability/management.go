package observability

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
)

// Management serves the operator-facing health surface: the cached readiness
// snapshot plus the paged provider-health listing. Every response is
// metadata-only.
type Management struct {
	Access *access.Server
	Cache  *Cache
	Pool   access.Queryer
}

// Register mounts the health routes on the management surface.
func (m *Management) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v3/health/ready", m.Access.Handle(m.ready))
	mux.HandleFunc("GET /api/v3/provider-health", m.Access.Handle(m.providerHealth))
}

func (m *Management) ready(r *http.Request) (access.Reply, error) {
	if _, err := m.Access.Principal(r, m.Access.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
	snapshot := m.Cache.Readiness()
	if !readinessIsCurrent(snapshot, time.Now()) {
		return access.Reply{}, access.Fail(503, "observability_snapshot_stale", "The readiness snapshot is stale.")
	}
	if snapshot.Result == nil {
		return access.Reply{}, access.Fail(503, "observability_snapshot_unavailable", "No readiness snapshot has been collected.")
	}
	return access.OK(healthResponse(snapshot.Result)), nil
}

func (m *Management) providerHealth(r *http.Request) (access.Reply, error) {
	p, err := m.Access.Principal(r, m.Access.Pool, "read")
	if err != nil {
		return access.Reply{}, err
	}
	query := r.URL.Query()
	window := 15
	if raw := query.Get("window_minutes"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 1440 {
			return access.Reply{}, access.Invalid("window_minutes", "Use a whole number of minutes between 1 and 1440.")
		}
		window = parsed
	}
	pagination, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	page, err := ReadProviderHealth(r.Context(), m.Pool, window, &pagination.Before, pagination.Limit,
		p.AllProjects, p.ProjectIDs())
	if err != nil {
		return access.Reply{}, err
	}
	items := make([]contract.ProviderHealthItem, len(page.Items))
	for i, record := range page.Items {
		items[i] = providerHealthItem(&record)
	}
	response := contract.ProviderHealthResponse{
		Items:         items,
		WindowMinutes: int32(window),
	}
	if page.NextCursor != nil {
		response.NextCursor = nullable.NewNullableWithValue(base64.RawURLEncoding.EncodeToString([]byte(*page.NextCursor)))
	}
	return access.OK(response), nil
}

// providerHealthItem renders one record in the management wire shape.
func providerHealthItem(record *ProviderHealthRecord) contract.ProviderHealthItem {
	item := contract.ProviderHealthItem{
		ProviderId:          uuid.MustParse(record.ProviderID),
		ProviderName:        record.ProviderName,
		ProviderKind:        contract.ProviderKind(record.ProviderKind),
		ProviderState:       record.ProviderState,
		Status:              record.Status,
		AttemptCount:        record.AttemptCount,
		SuccessCount:        record.SuccessCount,
		RateLimitCount:      record.RateLimitCount,
		ServerErrorCount:    record.ServerErrorCount,
		TransportErrorCount: record.TransportErrorCount,
	}
	if record.AverageLatencyMs != nil {
		item.AverageLatencyMs = nullable.NewNullableWithValue(*record.AverageLatencyMs)
	}
	if record.LastAttemptAt != nil {
		item.LastAttemptAt = nullable.NewNullableWithValue(record.LastAttemptAt.UTC())
	}
	if record.LastProbeAt != nil {
		item.LastProbeAt = nullable.NewNullableWithValue(record.LastProbeAt.UTC())
	}
	if record.LastProbeDetail != nil {
		item.LastProbeDetail = nullable.NewNullableWithValue(*record.LastProbeDetail)
	}
	if record.LastProbeStatus != nil {
		item.LastProbeStatus = nullable.NewNullableWithValue(*record.LastProbeStatus)
	}
	return item
}

// healthResponse renders the readiness payload in the management wire shape.
func healthResponse(h *Health) contract.HealthResponse {
	response := contract.HealthResponse{
		Status:                                 h.Status,
		AsynchronousPlane:                      h.AsynchronousPlane,
		AsynchronousPlaneCurrent:               h.AsynchronousPlaneCurrent,
		AsynchronousPlaneDrained:               h.AsynchronousPlaneDrained,
		Database:                               h.Database,
		Limits:                                 h.Limits,
		MediaReconciliation:                    h.MediaReconciliation,
		MediaReconciliationFailed:              h.MediaReconciliationFailed,
		MediaReconciliationGapsTotal:           h.MediaReconciliationGapsTotal,
		MediaReconciliationPending:             h.MediaReconciliationPending,
		MediaReconciliationStale:               h.MediaReconciliationStale,
		RequestMetadataComplete:                h.RequestMetadataComplete,
		RequestMetadataConsumer:                h.RequestMetadataConsumer,
		RequestMetadataConsumerLagEvents:       h.RequestMetadataConsumerLagEvents,
		RequestMetadataConsumerPendingEvents:   h.RequestMetadataConsumerPendingEvents,
		RequestMetadataGatewayOpenEpochs:       h.RequestMetadataGatewayOpenEpochs,
		RequestMetadataGatewayUnresolvedEpochs: h.RequestMetadataGatewayUnresolvedEpochs,
		RequestMetadataHistoricalUncertainGaps: h.RequestMetadataHistoricalUncertainGaps,
		RuntimeOutbox:                          h.RuntimeOutbox,
		RuntimeOutboxClaimedRows:               h.RuntimeOutboxClaimedRows,
		RuntimeOutboxOwnerAbandoned:            h.RuntimeOutboxOwnerAbandoned,
		RuntimeOutboxOwnerActive:               h.RuntimeOutboxOwnerActive,
		RuntimeOutboxPendingRows:               h.RuntimeOutboxPendingRows,
		WorkerTasksStale:                       h.WorkerTasksStale,
		WorkerTasksUnknown:                     h.WorkerTasksUnknown,
	}
	optionalTime(&response.AsynchronousPlaneLastProgressAt, h.AsynchronousPlaneLastProgressAt)
	optionalInt(&response.Generation, h.Generation)
	optionalInt(&response.MediaSpoolCapacityBytes, h.MediaSpoolCapacityBytes)
	optionalInt(&response.MediaSpoolUsedBytes, h.MediaSpoolUsedBytes)
	optionalTime(&response.RequestMetadataConsumerCheckedAt, h.RequestMetadataConsumerCheckedAt)
	optionalInt(&response.RequestMetadataConsumerHeartbeatAgeSeconds, h.RequestMetadataConsumerHeartbeatAgeSeconds)
	optionalInt(&response.RequestMetadataConsumerOldestPendingAgeSeconds, h.RequestMetadataConsumerOldestPendingAgeSec)
	optionalTime(&response.RequestMetadataConsumerOldestPendingAt, h.RequestMetadataConsumerOldestPendingAt)
	optionalInt(&response.RequestMetadataDuplicatePersistenceTotal, h.RequestMetadataDuplicatePersistenceTotal)
	optionalInt(&response.RequestMetadataReclaimedEventsTotal, h.RequestMetadataReclaimedEventsTotal)
	optionalInt(&response.RequestMetadataRecoveredEventsTotal, h.RequestMetadataRecoveredEventsTotal)
	optionalInt(&response.RuntimeOutboxAbandonedOwnershipTotal, h.RuntimeOutboxAbandonedOwnershipTotal)
	optionalInt(&response.RuntimeOutboxFailedTakeoversTotal, h.RuntimeOutboxFailedTakeoversTotal)
	optionalInt(&response.RuntimeOutboxHeartbeatAgeSeconds, h.RuntimeOutboxHeartbeatAgeSeconds)
	optionalInt(&response.RuntimeOutboxOldestPendingAgeSeconds, h.RuntimeOutboxOldestPendingAgeSeconds)
	optionalTime(&response.RuntimeOutboxOldestPendingAt, h.RuntimeOutboxOldestPendingAt)
	optionalInt(&response.RuntimeOutboxPublicationAttemptsTotal, h.RuntimeOutboxPublicationAttemptsTotal)
	optionalInt(&response.RuntimeOutboxPublicationRetriesTotal, h.RuntimeOutboxPublicationRetriesTotal)
	optionalInt(&response.RuntimeOutboxRepeatedPublicationAttemptsTotal, h.RuntimeOutboxRepeatedPublicationAttempts)
	return response
}

func optionalTime(dst *nullable.Nullable[time.Time], src *time.Time) {
	if src != nil {
		*dst = nullable.NewNullableWithValue(src.UTC())
	}
}

func optionalInt(dst *nullable.Nullable[int64], src *int64) {
	if src != nil {
		*dst = nullable.NewNullableWithValue(*src)
	}
}
