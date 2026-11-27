package manage

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ptr"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/usage"
)

type lockedResource struct {
	id               uuid.UUID
	localID          string
	project          *string
	operation        string
	templatesRaw     []byte
	remoteTerminated bool
	terminalAt       *time.Time
}

func lockResourceTx(ctx context.Context, tx pgx.Tx, rawID string, p authz.Principal) (lockedResource, error) {
	expectedKind, id, err := resources.ParseLocalID(rawID)
	if err != nil {
		return lockedResource{}, httpkit.Fail(http.StatusNotFound, "not_found", "No resource exists in this scope.")
	}
	var kind string
	var projectID *uuid.UUID
	err = tx.QueryRow(ctx, `SELECT r.kind, COALESCE(k.project_id, p.project_id, rt.project_id)
		FROM olp.provider_resources r
		JOIN olp.api_keys k ON k.id = r.api_key_id
		JOIN olp.providers p ON p.id = r.provider_id
		LEFT JOIN olp.routes rt ON rt.slug = r.route_slug
		WHERE r.id = $1`, id).Scan(&kind, &projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return lockedResource{}, httpkit.Fail(http.StatusNotFound, "not_found", "No resource exists in this scope.")
	}
	if err != nil {
		return lockedResource{}, err
	}
	if kind != expectedKind {
		return lockedResource{}, httpkit.Fail(http.StatusNotFound, "not_found", "No resource exists in this scope.")
	}
	var project *string
	if projectID != nil {
		project = new(projectID.String())
	}
	if err := p.Project(project, authz.Change); err != nil {
		return lockedResource{}, err
	}
	locked := lockedResource{id: id, localID: resources.LocalID(kind, id), project: project}
	err = tx.QueryRow(ctx, `SELECT templates, operation, remote_terminated, terminal_at
		FROM olp.retained_settlements
		WHERE resource_id = $1
		FOR UPDATE`, id).Scan(&locked.templatesRaw, &locked.operation, &locked.remoteTerminated, &locked.terminalAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return lockedResource{}, httpkit.Fail(http.StatusNotFound, "settlement_not_found", "This resource has no retained settlement owner.")
	}
	if err != nil {
		return lockedResource{}, err
	}
	return locked, nil
}

func (a *API) reconcile(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var input contract.RetainedReconcileRequest
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	ctx := r.Context()
	rawID := r.PathValue("id")
	return access.Transact(a.server, r, func(tx pgx.Tx, p authz.Principal) (lockedResource, error) {
		if err := p.Authorize(authz.Configure); err != nil {
			return lockedResource{}, err
		}
		return lockResourceTx(ctx, tx, rawID, p)
	}, func(tx pgx.Tx, _ authz.Principal, resource lockedResource) (httpkit.Reply, access.Event, error) {
		err := a.settlements.RequestReconciliationTx(ctx, tx, resource.id, ptr.Value(input.UpstreamId))
		if errors.Is(err, resources.ErrNotFound) {
			return httpkit.Reply{}, access.Event{}, httpkit.Fail(http.StatusNotFound, "settlement_not_found", "This resource has no retained settlement owner.")
		}
		if errors.Is(err, resources.ErrSettlementClosed) {
			return httpkit.Reply{}, access.Event{}, httpkit.Fail(http.StatusConflict, "settlement_closed", "This resource's retained settlement is already closed.")
		}
		if errors.Is(err, resources.ErrContract) {
			return httpkit.Reply{}, access.Event{}, httpkit.Invalid("upstream_id", "Reconciliation hint must be at most 512 bytes.")
		}
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.OK(contract.RetainedActionResponse{
				Id:    resource.localID,
				State: new("reconciliation_requested"),
			}),
			access.Event{Action: "provider_resource.reconcile", Resource: "provider_resource", ID: resource.id.String(), Project: resource.project}, nil
	})
}

func (a *API) usageEvidence(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var input contract.RetainedUsageEvidenceRequest
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	ctx := r.Context()
	rawID := r.PathValue("id")
	return access.Transact(a.server, r, func(tx pgx.Tx, p authz.Principal) (lockedResource, error) {
		if err := p.Authorize(authz.Settings); err != nil {
			return lockedResource{}, err
		}
		return lockResourceTx(ctx, tx, rawID, p)
	}, func(tx pgx.Tx, p authz.Principal, resource lockedResource) (httpkit.Reply, access.Event, error) {
		status, err := a.recordRetainedEvidence(ctx, tx, p, resource, input)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.OK(contract.RetainedActionResponse{
				Id:               resource.localID,
				SettlementStatus: new(status),
				Provenance:       new("operator_verified"),
			}),
			access.Event{Action: "provider_resource.usage_evidence", Resource: "provider_resource", ID: resource.id.String(), Project: resource.project}, nil
	})
}

func (a *API) recordRetainedEvidence(ctx context.Context, tx pgx.Tx, p authz.Principal, resource lockedResource, input contract.RetainedUsageEvidenceRequest) (string, error) {
	model, err := resolveEvidenceModel(resource.templatesRaw, ptr.Value(input.Model))
	if err != nil {
		return "", err
	}
	lines := retainedEvidenceLines(resource.operation, model, input)
	if len(lines) == 0 {
		return "", httpkit.Invalid("quantities", "Applicable quantities are required; omitted quantities are unknown.")
	}
	observed := time.Now().UTC()
	if resource.remoteTerminated && resource.terminalAt != nil {
		observed = *resource.terminalAt
	}
	observation := resources.RetainedObservation{
		Lines:            lines,
		ObservedAt:       observed,
		RemoteTerminated: resource.remoteTerminated,
		CoverageComplete: input.Terminal,
	}
	_, err = a.settlements.ApplyEvidenceTx(ctx, tx, resources.EvidenceSubmission{
		ResourceID:  resource.id,
		Key:         input.IdempotencyKey,
		Digest:      input.EvidenceDigest,
		Provenance:  "operator_verified",
		Actor:       p.Kind + ":" + p.ID,
		Observation: observation,
	})
	if errors.Is(err, resources.ErrEvidenceConflict) {
		return "", httpkit.Fail(http.StatusConflict, "evidence_conflict", "Evidence overlaps or conflicts with already accounted coverage.")
	}
	if errors.Is(err, resources.ErrContract) {
		return "", httpkit.Invalid("evidence", "Supply a nonnegative exact quantity, SHA-256 digest, idempotency key, and UTC interval.")
	}
	if err != nil {
		return "", err
	}
	var status string
	err = tx.QueryRow(ctx, `SELECT state FROM olp.retained_settlements WHERE resource_id = $1`, resource.id).Scan(&status)
	return status, err
}

func resolveEvidenceModel(templatesRaw []byte, requested string) (string, error) {
	var templates map[string]*usage.Event
	if err := json.Unmarshal(templatesRaw, &templates); err != nil {
		return "", err
	}
	model := requested
	if model == "" && len(templates) == 1 {
		for k := range templates {
			model = k
		}
	}
	if templates[model] == nil {
		return "", httpkit.Invalid("model", "Choose a model pinned by this resource's original acceptance.")
	}
	return model, nil
}

func retainedEvidenceLines(operation, model string, input contract.RetainedUsageEvidenceRequest) []resources.RetainedEvidenceLine {
	sessions := nullableMap(input.ContainerSessions)
	baseUsage := usage.AttemptUsage{
		UsageQuantities: usage.UsageQuantities{
			InvocationCount:    input.InvocationCount,
			TrainingTokens:     nullablePtr(input.TrainingTokens),
			StorageByteSeconds: nullablePtr(input.StorageByteSeconds),
			CacheTokenSeconds:  nullablePtr(input.CacheTokenSeconds),
			ContainerSessions:  sessions,
			InputTokens:        input.InputTokens,
			OutputTokens:       input.OutputTokens,
			CachedInputTokens:  input.CachedInputTokens,
		},
	}
	if component, ok := operationComponent(operation); ok {
		return []resources.RetainedEvidenceLine{{
			Component: component,
			Model:     model,
			Start:     input.Start,
			End:       input.End,
			Usage:     baseUsage,
		}}
	}
	if operation != "container" {
		return nil
	}
	tiers := slices.Sorted(maps.Keys(sessions))
	lines := make([]resources.RetainedEvidenceLine, 0, len(tiers))
	for _, tier := range tiers {
		tierUsage := baseUsage
		tierUsage.ContainerSessions = map[string]int64{tier: sessions[tier]}
		lines = append(lines, resources.RetainedEvidenceLine{
			Component:  "container",
			Model:      model,
			MemoryTier: tier,
			Start:      input.Start,
			End:        input.End,
			Usage:      tierUsage,
		})
	}
	return lines
}

func operationComponent(operation string) (string, bool) {
	switch operation {
	case "fine_tuning":
		return "training", true
	case "vector_store":
		return "storage", true
	case "context_cache":
		return "cache_storage", true
	case "eval":
		return "generation", true
	default:
		return "", false
	}
}

func nullablePtr[T any](v nullable.Nullable[T]) *T {
	if !v.IsSpecified() || v.IsNull() {
		return nil
	}
	value := v.MustGet()
	return &value
}

func nullableMap[K comparable, V any](v nullable.Nullable[map[K]V]) map[K]V {
	if !v.IsSpecified() || v.IsNull() {
		return nil
	}
	return v.MustGet()
}
