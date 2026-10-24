package resources

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/usage"
)

func (m *Management) registerRetainedManagement(mux *http.ServeMux) {
	m.Access.Route(mux, "POST /api/v1/provider-resources/{id}/reconcile", m.reconcileRetained)
	m.Access.Route(mux, "POST /api/v1/provider-resources/{id}/usage-evidence", m.retainedEvidence)
}
func (m *Management) retainedManagementResource(r *http.Request, tx pgx.Tx, p access.Principal) (*Resource, error) {
	id, err := parseLocal(r.PathValue("id"))
	if err != nil {
		return nil, access.Fail(404, "not_found", "No resource exists in this scope.")
	}
	resource, err := scan(tx.QueryRow(r.Context(), `SELECT `+columns+` FROM olp.provider_resources WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, access.Fail(404, "not_found", "No resource exists in this scope.")
	}
	if err != nil {
		return nil, err
	}
	if resource.ID != r.PathValue("id") {
		return nil, access.Fail(404, "not_found", "No resource exists in this scope.")
	}
	var project *string
	err = tx.QueryRow(r.Context(), `SELECT COALESCE(k.project_id,p.project_id,rt.project_id)::text FROM olp.provider_resources r JOIN olp.api_keys k ON k.id=r.api_key_id JOIN olp.providers p ON p.id=r.provider_id LEFT JOIN olp.routes rt ON rt.slug=r.route_slug WHERE r.id=$1`, id).Scan(&project)
	if err != nil {
		return nil, err
	}
	if err = p.Project(project, access.Change); err != nil {
		return nil, err
	}
	return resource, nil
}
func (m *Management) reconcileRetained(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input struct {
		UpstreamID string `json:"upstream_id"`
	}
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	tx, err := m.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := m.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Authorize(access.Configure); err != nil {
		return access.Reply{}, err
	}
	resource, err := m.retainedManagementResource(r, tx, p)
	if err != nil {
		return access.Reply{}, err
	}
	store := New(m.Pool)
	if err = store.RequestRetainedReconciliationTx(r.Context(), tx, resource.UUID, input.UpstreamID); errors.Is(err, ErrNotFound) {
		return access.Reply{}, access.Fail(404, "settlement_not_found", "This resource has no retained settlement owner.")
	} else if err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "provider_resource.reconcile", "provider_resource", resource.UUID.String(), "success"); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, access.OK(map[string]any{"id": resource.ID, "state": "reconciliation_requested"}))
}

type retainedEvidenceInput struct {
	InvocationCount    *int64           `json:"invocation_count,omitempty"`
	IdempotencyKey     string           `json:"idempotency_key"`
	EvidenceDigest     string           `json:"evidence_digest"`
	Start              time.Time        `json:"start"`
	End                time.Time        `json:"end"`
	Model              string           `json:"model,omitempty"`
	TrainingTokens     *int64           `json:"training_tokens,omitempty"`
	StorageByteSeconds *string          `json:"storage_byte_seconds,omitempty"`
	CacheTokenSeconds  *string          `json:"cache_token_seconds,omitempty"`
	ContainerSessions  map[string]int64 `json:"container_sessions,omitempty"`
	InputTokens        *int64           `json:"input_tokens,omitempty"`
	OutputTokens       *int64           `json:"output_tokens,omitempty"`
	CachedInputTokens  *int64           `json:"cached_input_tokens,omitempty"`
	Terminal           bool             `json:"terminal"`
}

func (m *Management) retainedEvidence(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input retainedEvidenceInput
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	tx, err := m.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := m.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Authorize(access.Settings); err != nil {
		return access.Reply{}, err
	}
	resource, err := m.retainedManagementResource(r, tx, p)
	if err != nil {
		return access.Reply{}, err
	}
	var raw []byte
	var operation string
	if err = tx.QueryRow(r.Context(), `SELECT templates,operation FROM olp.retained_settlements WHERE resource_id=$1 FOR UPDATE`, resource.UUID).Scan(&raw, &operation); errors.Is(err, pgx.ErrNoRows) {
		return access.Reply{}, access.Fail(404, "settlement_not_found", "This resource has no retained settlement owner.")
	} else if err != nil {
		return access.Reply{}, err
	}
	var templates map[string]*usage.Event
	if err = json.Unmarshal(raw, &templates); err != nil {
		return access.Reply{}, err
	}
	model := input.Model
	if model == "" && len(templates) == 1 {
		for m := range templates {
			model = m
		}
	}
	if templates[model] == nil {
		return access.Reply{}, access.Invalid("model", "Choose a model pinned by this resource's original acceptance.")
	}
	u := usage.AttemptUsage{InvocationCount: input.InvocationCount, TrainingTokens: input.TrainingTokens, StorageByteSeconds: input.StorageByteSeconds, CacheTokenSeconds: input.CacheTokenSeconds, ContainerSessions: input.ContainerSessions, InputTokens: input.InputTokens, OutputTokens: input.OutputTokens, CachedInputTokens: input.CachedInputTokens}
	var lines []RetainedEvidenceLine
	add := func(component, tier string, v usage.AttemptUsage) {
		lines = append(lines, RetainedEvidenceLine{Component: component, Model: model, MemoryTier: tier, Start: input.Start, End: input.End, Usage: v})
	}
	switch operation {
	case "fine_tuning":
		add("training", "", u)
	case "vector_store":
		add("storage", "", u)
	case "context_cache":
		add("cache_storage", "", u)
	case "eval":
		add("generation", "", u)
	case "container":
		for tier, count := range input.ContainerSessions {
			v := u
			v.ContainerSessions = map[string]int64{tier: count}
			add("container", tier, v)
		}
	}
	if len(lines) == 0 {
		return access.Reply{}, access.Invalid("quantities", "Applicable quantities are required; omitted quantities are unknown.")
	}
	store := New(m.Pool)
	verified, terminatedAt, err := store.RetainedEvidenceTerminationTx(r.Context(), tx, resource.UUID)
	if err != nil {
		return access.Reply{}, err
	}
	observed := time.Now().UTC()
	if verified && terminatedAt != nil {
		observed = *terminatedAt
	}
	// A client terminal flag cannot supply the remote-termination proof.
	observation := RetainedObservation{Lines: lines, ObservedAt: observed, RemoteTerminated: verified, CoverageComplete: input.Terminal}
	_, err = store.ApplyRetainedEvidenceTx(r.Context(), tx, resource.UUID, input.IdempotencyKey, input.EvidenceDigest, "operator_verified", p.Kind+":"+p.ID, observation, nil)
	if errors.Is(err, ErrEvidenceConflict) {
		return access.Reply{}, access.Fail(409, "evidence_conflict", "Evidence overlaps or conflicts with already accounted coverage.")
	}
	if errors.Is(err, ErrContract) {
		return access.Reply{}, access.Invalid("evidence", "Supply a nonnegative exact quantity, SHA-256 digest, idempotency key, and UTC interval.")
	}
	if err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "provider_resource.usage_evidence", "provider_resource", resource.UUID.String(), "success"); err != nil {
		return access.Reply{}, err
	}
	var status string
	err = tx.QueryRow(r.Context(), `SELECT state FROM olp.retained_settlements WHERE resource_id=$1`, resource.UUID).Scan(&status)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, access.OK(map[string]any{"id": resource.ID, "settlement_status": status, "provenance": "operator_verified"}))
}
