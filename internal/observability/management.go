package observability

import (
	"context"
	"encoding/base64"
	"net/http"
	"strconv"
	"time"

	"github.com/oapi-codegen/nullable"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/management/contract"
)

// Management serves the operator-facing health surface: the cached readiness
// snapshot plus the paged provider-health listing. Every response is
// metadata-only.
type Management struct {
	Access *access.Server
	Cache  *Cache
	Pool   access.Queryer
	// Fleet reads the circuit and probe state gateways share; nil when no
	// shared state is configured.
	Fleet FleetHealth
}

// FleetHealth reads the provider health the gateway fleet shares.
type FleetHealth interface {
	Circuits(ctx context.Context, now time.Time) (map[string]time.Time, error)
	Probes(ctx context.Context) (map[string]limits.ProbeResult, error)
}

// Register mounts the health routes on the management surface.
func (m *Management) Register(mux *http.ServeMux) {
	m.Access.Route(mux, "GET /api/v1/health/ready", m.ready)
	m.Access.Route(mux, "GET /api/v1/provider-health", m.providerHealth)
}

func (m *Management) ready(_ *http.Request, _ access.Principal) (access.Reply, error) {
	health, err := readyHealth(m.Cache.readinessSnapshot(), time.Now())
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(healthResponse(health)), nil
}

func (m *Management) providerHealth(r *http.Request, p access.Principal) (access.Reply, error) {
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
	page, err := ReadProviderHealth(r.Context(), m.Pool, ProviderHealthQuery{
		WindowMinutes: window,
		Before:        &pagination.Before,
		Limit:         pagination.Limit,
		AllProjects:   p.AllProjects,
		Projects:      p.ProjectIDs(),
	})
	if err != nil {
		return access.Reply{}, err
	}
	shared, circuits, probes := m.fleet(r.Context())
	items := make([]contract.ProviderHealthItem, len(page.Items))
	for i, record := range page.Items {
		items[i] = providerHealthItem(&record)
		if until, open := circuits[record.ProviderID.String()]; open {
			items[i].CircuitOpenUntil = nullable.NewNullableWithValue(until.UTC())
			if record.Status != StatusDisabled {
				items[i].Status = string(StatusUnavailable)
			}
		}
		if probe, ok := probes[record.ProviderID.String()]; ok {
			items[i].ActiveProbe = nullable.NewNullableWithValue(probeResult(probe))
		}
	}
	response := contract.ProviderHealthResponse{
		Items:         items,
		WindowMinutes: int32(window),
		SharedState:   shared,
	}
	if page.NextCursor != nil {
		response.NextCursor = nullable.NewNullableWithValue(base64.RawURLEncoding.EncodeToString([]byte(*page.NextCursor)))
	}
	return access.OK(response), nil
}

// fleet reads the shared circuits and probe results. An unreadable store
// leaves every item without them rather than failing the listing.
func (m *Management) fleet(ctx context.Context) (contract.ProviderHealthResponseSharedState, map[string]time.Time, map[string]limits.ProbeResult) {
	if m.Fleet == nil {
		return contract.ProviderHealthResponseSharedStateUnconfigured, nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, fleetReadTimeout)
	defer cancel()
	circuits, err := m.Fleet.Circuits(ctx, time.Now())
	if err != nil {
		return contract.ProviderHealthResponseSharedStateUnavailable, nil, nil
	}
	probes, err := m.Fleet.Probes(ctx)
	if err != nil {
		return contract.ProviderHealthResponseSharedStateUnavailable, nil, nil
	}
	return contract.ProviderHealthResponseSharedStateCurrent, circuits, probes
}

// fleetReadTimeout bounds the shared-state reads of one listing.
const fleetReadTimeout = time.Second

func probeResult(probe limits.ProbeResult) contract.ProviderProbeResult {
	result := contract.ProviderProbeResult{
		Status:     contract.ProviderProbeResultStatus(probe.Status),
		Model:      probe.Model,
		ObservedAt: probe.ObservedAt.UTC(),
		LatencyMs:  probe.LatencyMS,
	}
	if probe.Class != "" {
		result.Class = nullable.NewNullableWithValue(probe.Class)
	} else {
		result.Class = nullable.NewNullNullable[string]()
	}
	return result
}

// providerHealthItem renders one record in the management wire shape.
func providerHealthItem(record *ProviderHealthRecord) contract.ProviderHealthItem {
	item := contract.ProviderHealthItem{
		ProviderId:          record.ProviderID,
		ProviderName:        record.ProviderName,
		ProviderKind:        contract.ProviderKind(record.ProviderKind),
		ProviderState:       record.ProviderState,
		Status:              string(record.Status),
		AttemptCount:        record.AttemptCount,
		SuccessCount:        record.SuccessCount,
		RateLimitCount:      record.RateLimitCount,
		ServerErrorCount:    record.ServerErrorCount,
		TransportErrorCount: record.TransportErrorCount,
		CircuitOpenUntil:    nullable.NewNullNullable[time.Time](),
		ActiveProbe:         nullable.NewNullNullable[contract.ProviderProbeResult](),
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

func healthResponse(h *Health) contract.HealthResponse {
	return contract.HealthResponse{
		Status:                                 string(h.Status),
		Generation:                             nullableOf(h.Generation),
		Database:                               string(h.Database),
		Limits:                                 string(h.Limits),
		AsynchronousPlane:                      string(h.AsynchronousPlane),
		AsynchronousPlaneCurrent:               h.AsynchronousPlaneCurrent,
		AsynchronousPlaneDrained:               h.AsynchronousPlaneDrained,
		AsynchronousPlaneLastProgressAt:        nullableTime(h.AsynchronousPlaneLastProgressAt),
		WorkerTasksStale:                       h.WorkerTasksStale,
		WorkerTasksUnknown:                     h.WorkerTasksUnknown,
		RequestMetadataComplete:                h.RequestMetadataComplete,
		RequestMetadataConsumer:                h.RequestMetadataConsumer,
		RequestMetadataConsumerPendingEvents:   h.RequestMetadataConsumerPendingEvents,
		RequestMetadataConsumerLagEvents:       h.RequestMetadataConsumerLagEvents,
		RequestMetadataConsumerOldestPendingAt: nullableTime(h.RequestMetadataConsumerOldestPendingAt),
		RequestMetadataConsumerOldestPendingAgeSeconds: nullableOf(h.RequestMetadataConsumerOldestPendingAgeSec),
		RequestMetadataConsumerCheckedAt:               nullableTime(h.RequestMetadataConsumerCheckedAt),
		RequestMetadataConsumerHeartbeatAgeSeconds:     nullableOf(h.RequestMetadataConsumerHeartbeatAgeSeconds),
		RequestMetadataReclaimedEventsTotal:            nullableOf(h.RequestMetadataReclaimedEventsTotal),
		RequestMetadataRecoveredEventsTotal:            nullableOf(h.RequestMetadataRecoveredEventsTotal),
		RequestMetadataDuplicatePersistenceTotal:       nullableOf(h.RequestMetadataDuplicatePersistenceTotal),
		RequestMetadataGatewayOpenEpochs:               h.RequestMetadataGatewayOpenEpochs,
		RequestMetadataGatewayUnresolvedEpochs:         h.RequestMetadataGatewayUnresolvedEpochs,
		RequestMetadataHistoricalUncertainGaps:         h.RequestMetadataHistoricalUncertainGaps,
		MediaReconciliation:                            string(h.MediaReconciliation),
		MediaReconciliationPending:                     h.MediaReconciliationPending,
		MediaReconciliationStale:                       h.MediaReconciliationStale,
		MediaReconciliationFailed:                      h.MediaReconciliationFailed,
		MediaReconciliationGapsTotal:                   h.MediaReconciliationGapsTotal,
		MediaSpoolUsedBytes:                            nullableOf(h.MediaSpoolUsedBytes),
		MediaSpoolCapacityBytes:                        nullableOf(h.MediaSpoolCapacityBytes),
	}
}

func nullableOf[T any](value *T) nullable.Nullable[T] {
	if value == nil {
		return nil
	}
	return nullable.NewNullableWithValue(*value)
}

func nullableTime(value *time.Time) nullable.Nullable[time.Time] {
	if value == nil {
		return nil
	}
	return nullable.NewNullableWithValue(value.UTC())
}
