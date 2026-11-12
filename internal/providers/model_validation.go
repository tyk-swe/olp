package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/providers/connection"
)

func modelJSON(m storedModel) map[string]any {
	capabilities := make([]map[string]any, 0, len(m.Capabilities))
	for _, c := range m.Capabilities {
		capabilities = append(capabilities, map[string]any{"operation": c.Operation, "surface": c.Surface, "mode": c.Mode, "source": c.Source, "certified_at": c.CertifiedAt, "tool_calls": c.ToolCalls})
	}
	return map[string]any{"id": m.ID, "upstream_model": m.UpstreamModel, "display_name": m.DisplayName, "enabled": m.Enabled, "capabilities": capabilities, "discovered_at": m.DiscoveredAt}
}

func ValidModelName(field, value string) error {
	if len(value) == 0 || len(value) > 200 {
		return access.Invalid(field, "Use 1–200 characters.")
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return access.Invalid(field, "Control characters are not allowed.")
		}
	}
	return nil
}

// prepare reads what an upstream call needs without holding the installation
// lock: the provider, its etag precondition, and the enabled probe credential.
// The caller's project scope is decided first, so a provider outside it answers
// 404 before its precondition or credential is consulted.
func (s *Server) prepare(r *http.Request, p access.Principal, id string) (*record, probeTarget, []byte, error) {
	tx, err := s.Access.Pool.Begin(r.Context())
	if err != nil {
		return nil, probeTarget{}, nil, err
	}
	defer tx.Rollback(r.Context())
	current, err := load(r.Context(), tx, id, false)
	if err != nil {
		return nil, probeTarget{}, nil, err
	}
	if err = p.Project(current.ProjectID, access.Change); err != nil {
		return nil, probeTarget{}, nil, err
	}
	if err = access.Match(r, current.ETag); err != nil {
		return nil, probeTarget{}, nil, err
	}
	credential, _, err := s.credentialFor(r.Context(), tx, current)
	if err != nil {
		return nil, probeTarget{}, nil, err
	}
	rows, err := tx.Query(r.Context(), "SELECT upstream_model FROM olp.provider_models WHERE provider_id=$1 ORDER BY enabled DESC,upstream_model LIMIT 2000", id)
	if err != nil {
		return nil, probeTarget{}, nil, err
	}
	models, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, probeTarget{}, nil, err
	}
	return current, current.target(models...), credential, nil
}

func (s *Server) probe(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	current, target, credential, err := s.prepare(r, p, id)
	if err != nil {
		return access.Reply{}, err
	}
	at := time.Now().UTC()
	models, err := s.listModelFacts(r.Context(), target, credential)
	var discovered any
	succeeded, detail := err == nil, ""
	if succeeded {
		discovered = len(models)
		detail = fmt.Sprintf("Reached the upstream; %d models listed.", len(models))
	} else {
		detail = classify(err).Detail
	}
	if err = s.recordProbe(r, "provider.probe", id, at, succeeded, detail); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(map[string]any{"provider_id": id, "succeeded": succeeded, "checked_at": at, "probe_type": "models", "detail": detail, "discovered_models": discovered}, current.ETag), nil
}

// recordProbe stores a probe outcome on the provider and audits it in one
// transaction. The upstream call ran without the installation lock, so the
// caller's authority and project reach are decided again under it before
// anything is written.
func (s *Server) recordProbe(r *http.Request, action, id string, at time.Time, succeeded bool, detail string) error {
	tx, p, err := s.Access.BeginMutation(r)
	if err != nil {
		return err
	}
	defer tx.Rollback(r.Context())
	current, err := load(r.Context(), tx, id, true)
	if err != nil {
		return err
	}
	if err := p.Project(current.ProjectID, access.Change); err != nil {
		return err
	}
	if err := s.storeProbe(r.Context(), tx, id, at, succeeded, detail); err != nil {
		return err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), action, "provider", id, auditOutcome(succeeded)); err != nil {
		return err
	}
	return tx.Commit(r.Context())
}

// auditOutcome maps a probe result onto the audit outcome vocabulary.
func auditOutcome(succeeded bool) string {
	if succeeded {
		return "success"
	}
	return "failure"
}

func probeStatus(succeeded bool) string {
	if succeeded {
		return "succeeded"
	}
	return "failed"
}

type discoverRequest struct {
	Models []struct {
		UpstreamModel string `json:"upstream_model"`
		DisplayName   string `json:"display_name"`
	} `json:"models"`
}

func (s *Server) discover(r *http.Request, p access.Principal) (access.Reply, error) {
	a := s.Access
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input discoverRequest
	if err = access.DecodeUnique(r, &input, 1<<20); err != nil {
		return access.Reply{}, err
	}
	if len(input.Models) > 2000 {
		return access.Reply{}, access.Invalid("models", "Declare at most 2000 models per request.")
	}
	var models []discoveredModel
	seen := map[string]bool{}
	for _, m := range input.Models {
		if err = ValidModelName("models.upstream_model", m.UpstreamModel); err != nil {
			return access.Reply{}, err
		}
		if m.DisplayName == "" {
			m.DisplayName = m.UpstreamModel
		}
		if err = ValidModelName("models.display_name", m.DisplayName); err != nil {
			return access.Reply{}, err
		}
		if !seen[m.UpstreamModel] {
			seen[m.UpstreamModel] = true
			models = append(models, discoveredModel{m.UpstreamModel, m.DisplayName, nil})
		}
	}
	current, target, credential, err := s.prepare(r, p, id)
	if err != nil {
		return access.Reply{}, err
	}
	upstream := len(models) == 0
	at := time.Now().UTC()
	if upstream {
		listed, err := s.listModelFacts(r.Context(), target, credential)
		if err != nil {
			pe := classify(err)
			if err := s.recordProbe(r, "provider.discover", id, at, false, pe.Detail); err != nil {
				return access.Reply{}, err
			}
			return access.Reply{}, access.Fail(422, "discovery_failed", pe.Detail)
		}
		for _, name := range listed {
			models = append(models, discoveredModel{name.Name, name.Display, name.Metadata})
		}
	}
	tx, p, err := a.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	locked, err := load(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(locked.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	if locked.ETag != current.ETag {
		return access.Reply{}, access.Fail(412, "etag_mismatch", "The connection changed during discovery; reload and retry.")
	}
	if err := s.storeDiscoveredModels(r.Context(), tx, locked, models, upstream, at); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "provider.discover", "provider", id, "success"); err != nil {
		return access.Reply{}, err
	}
	result, err := s.detailReply(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	return result, tx.Commit(r.Context())
}

func (s *Server) models(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	if _, err = visibleProvider(r.Context(), s.Access.Pool, p, id); err != nil {
		return access.Reply{}, err
	}
	d, err := s.detail(r.Context(), s.Access.Pool, id)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT id::text,upstream_model,display_name,enabled,capabilities,discovered_at FROM olp.provider_models WHERE provider_id=$1 AND id<$2 ORDER BY id DESC LIMIT $3", id, page.Before, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var m storedModel
		var capabilities []byte
		if err = rows.Scan(&m.ID, &m.UpstreamModel, &m.DisplayName, &m.Enabled, &capabilities, &m.DiscoveredAt); err != nil {
			return access.Reply{}, err
		}
		if err = json.Unmarshal(capabilities, &m.Capabilities); err != nil {
			return access.Reply{}, err
		}
		items = append(items, modelJSON(m))
	}
	if err = rows.Err(); err != nil {
		return access.Reply{}, err
	}
	reply := access.ListReply(items, page)
	reply.Body.(map[string]any)["provider"] = d
	reply.ETag = d.ETag
	return reply, nil
}

type setModelRequest struct {
	Enabled      bool               `json:"enabled"`
	Capabilities *[]CapabilityInput `json:"capabilities"`
}

func loadModel(ctx context.Context, q access.Queryer, providerID, modelID string, lock bool) (*storedModel, error) {
	query := "SELECT id::text,upstream_model,display_name,enabled,capabilities,discovered_at FROM olp.provider_models WHERE provider_id=$1 AND id=$2"
	if lock {
		query += " FOR UPDATE"
	}
	var m storedModel
	var capabilities []byte
	if err := q.QueryRow(ctx, query, providerID, modelID).Scan(&m.ID, &m.UpstreamModel, &m.DisplayName, &m.Enabled, &capabilities, &m.DiscoveredAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(capabilities, &m.Capabilities); err != nil {
		return nil, err
	}
	if m.Capabilities == nil {
		m.Capabilities = []storedCapability{}
	}
	return &m, nil
}

func ValidCapabilities(inputs []CapabilityInput) ([]CapabilityInput, error) {
	if len(inputs) > 64 {
		return nil, access.Invalid("capabilities", "Declare at most 64 capabilities per model.")
	}
	var out []CapabilityInput
	seen := map[CapabilityInput]bool{}
	for _, c := range inputs {
		supported := operationregistry.Catalog().Supports(c.Operation, c.Surface, c.Mode)
		for _, option := range capabilityVocabulary {
			supported = supported || option == c
		}
		if !supported {
			return nil, access.Fail(422, "capability_unavailable", fmt.Sprintf("The %s/%s/%s capability is not available in this release.", c.Operation, c.Surface, c.Mode))
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *Server) setModel(r *http.Request, _ access.Principal) (access.Reply, error) {
	a := s.Access
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	modelID, err := access.IDParam(r, "model_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input setModelRequest
	if err = access.DecodeUnique(r, &input, 1<<20); err != nil {
		return access.Reply{}, err
	}
	tx, p, err := a.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	current, err := load(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(current.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, current.ETag); err != nil {
		return access.Reply{}, err
	}
	if err := s.setDraftModel(r.Context(), tx, current, modelID, input); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "provider.model.update", "provider_model", modelID, "success"); err != nil {
		return access.Reply{}, err
	}
	result, err := s.detailReply(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	return result, tx.Commit(r.Context())
}

func (s *Server) certify(r *http.Request, p access.Principal) (access.Reply, error) {
	a := s.Access
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	modelID, err := access.IDParam(r, "model_id")
	if err != nil {
		return access.Reply{}, err
	}
	current, target, credential, err := s.prepare(r, p, id)
	if err != nil {
		return access.Reply{}, err
	}
	m, err := loadModel(r.Context(), a.Pool, id, modelID, false)
	if err != nil {
		return access.Reply{}, err
	}
	if len(m.Capabilities) == 0 {
		return access.Reply{}, access.Fail(422, "no_capabilities", "Declare at least one capability before certifying.")
	}
	slots, err := loadSlots(r.Context(), a.Pool, id)
	if err != nil {
		return access.Reply{}, err
	}
	selected := selectProbeSlot(slots, current.Connection.Document())
	if selected == nil {
		return access.Reply{}, access.Fail(422, "credential_required", "Enable a credential slot before certifying.")
	}
	defaultSlot := *selected

	at := time.Now().UTC()
	results := make([]map[string]any, 0, len(m.Capabilities))
	certified := 0
	// The route deadline gives each capability a probe budget; the last one
	// stays reserved so the results are always recorded.
	probeCtx, cancel := probeBudget(r.Context())
	defer cancel()
	for i := range m.Capabilities {
		c := &m.Capabilities[i]
		err := s.certifyTuple(probeCtx, target, credential, m.UpstreamModel, CapabilityInput{c.Operation, c.Surface, c.Mode}, probeBodyLimit)
		item := map[string]any{"operation": c.Operation, "surface": c.Surface, "mode": c.Mode, "succeeded": err == nil, "detail": "Certified.", "error_code": nil}
		if err == nil {
			certified++
			c.Source, c.CertifiedAt = "certified", &at
			c.CredentialFingerprint = defaultSlot.credentialFingerprint(current.Connection.Document())
			c.ToolCalls = c.Operation == "generation" && (c.Surface == "openai" || c.Surface == "anthropic" || c.Surface == "gemini") && (c.Mode == ModeUnary || c.Mode == ModeStreaming) && s.certifyToolCalls(probeCtx, target, credential, m.UpstreamModel, c.Mode == ModeStreaming) == nil
			item["tool_calls"] = c.ToolCalls
		} else {
			pe := classify(err)
			item["detail"], item["error_code"] = pe.Detail, pe.Code
			c.Source, c.CertifiedAt = "declared", nil
			c.CredentialFingerprint = ""
			c.ToolCalls = false
		}
		results = append(results, item)
	}
	status := "certified"
	switch {
	case certified == 0:
		status = "failed"
	case certified < len(m.Capabilities):
		status = "partial"
	}
	tx, p, err := a.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	locked, err := load(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(locked.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	if locked.ETag != current.ETag {
		return access.Reply{}, access.Fail(412, "etag_mismatch", "The connection changed during certification; reload and retry.")
	}
	etag, err := s.storeCertification(r.Context(), tx, current, m, defaultSlot, certified, at)
	if err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "provider.model.certify", "provider_model", modelID, auditOutcome(certified > 0)); err != nil {
		return access.Reply{}, err
	}
	result := access.Detail(map[string]any{"provider_id": id, "model_id": modelID, "status": status, "checked_at": at, "certified_count": certified, "attempted_count": len(m.Capabilities), "results": results}, etag)
	reply, err := result, tx.Commit(r.Context())
	if err == nil && certified == len(m.Capabilities) {
		s.clearValidatedCooldowns(r.Context(), id, defaultSlot)
	}
	return reply, err
}

func (s *Catalogue) setDraftModel(ctx context.Context, tx pgx.Tx, current *record, modelID string, input setModelRequest) error {
	m, err := loadModel(ctx, tx, current.ID, modelID, true)
	if err != nil {
		return err
	}
	capabilities := m.Capabilities
	if input.Capabilities != nil {
		requested, err := ValidCapabilities(*input.Capabilities)
		if err != nil {
			return err
		}
		existing := map[CapabilityInput]storedCapability{}
		for _, c := range m.Capabilities {
			existing[CapabilityInput{c.Operation, c.Surface, c.Mode}] = c
		}
		capabilities = make([]storedCapability, 0, len(requested))
		for _, c := range requested {
			if !configurationCertifiable(current.Connection.Transport(), c) {
				return access.Fail(422, "capability_unavailable", "This connector cannot certify the requested tuple.")
			}
			if kept, ok := existing[c]; ok {
				capabilities = append(capabilities, kept)
				continue
			}
			capabilities = append(capabilities, storedCapability{Operation: c.Operation, Surface: c.Surface, Mode: c.Mode, Source: "declared"})
		}
	}
	encoded, _ := json.Marshal(capabilities)
	if _, err = tx.Exec(ctx, "UPDATE olp.provider_models SET enabled=$3,capabilities=$4 WHERE provider_id=$1 AND id=$2", current.ID, modelID, input.Enabled, encoded); err != nil {
		return err
	}
	if _, err = touch(ctx, tx, current.ID); err != nil {
		return err
	}
	return nil
}

func (s *Catalogue) storeDiscoveredModels(ctx context.Context, tx pgx.Tx, current *record, models []discoveredModel, upstream bool, at time.Time) error {
	document := current.Connection.Document()
	metadata := maps.Clone(document.Options.Models)
	for _, m := range models {
		facts, merged, err := connection.MergeDiscoveredFacts(metadata, m.Name, m.Metadata)
		if err != nil {
			return err
		}
		if merged {
			metadata[m.Name] = facts
		}
		if _, err = tx.Exec(ctx, "INSERT INTO olp.provider_models(id,provider_id,upstream_model,display_name,enabled,capabilities,discovered_at) VALUES($1,$2,$3,$4,false,'[]',$5) ON CONFLICT(provider_id,upstream_model) DO UPDATE SET display_name=excluded.display_name,discovered_at=excluded.discovered_at", access.NewID(), current.ID, m.Name, m.Display, at); err != nil {
			return err
		}
	}
	document.Options.Models = metadata
	config, err := json.Marshal(document)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE olp.providers SET configuration=$2 WHERE id=$1", current.ID, config); err != nil {
		return err
	}
	if upstream {
		if _, err = tx.Exec(ctx, "UPDATE olp.providers SET last_probe_at=$2,last_probe_status='succeeded',last_probe_detail=$3 WHERE id=$1", current.ID, at, fmt.Sprintf("Discovered %d models.", len(models))); err != nil {
			return err
		}
	}
	if _, err = touch(ctx, tx, current.ID); err != nil {
		return err
	}
	return nil
}

func (s *Catalogue) storeCertification(ctx context.Context, tx pgx.Tx, current *record, model *storedModel, defaultSlot slotRow, certified int, at time.Time) (string, error) {
	var err error
	encoded, _ := json.Marshal(model.Capabilities)
	if _, err = tx.Exec(ctx, "UPDATE olp.provider_models SET capabilities=$3 WHERE provider_id=$1 AND id=$2", current.ID, model.ID, encoded); err != nil {
		return "", err
	}
	models, err := loadModels(ctx, tx, current.ID, true)
	if err != nil {
		return "", err
	}
	if validatedAt := defaultSlot.certificationTime(current.Connection.Document(), models); validatedAt != nil {
		fingerprint := defaultSlot.validationFingerprint(current.Connection.Document(), models)
		if _, err = tx.Exec(ctx, "UPDATE olp.provider_slots SET validated_at=$2,validated_fingerprint=$3 WHERE id=$1", defaultSlot.ID, validatedAt, fingerprint); err != nil {
			return "", err
		}
	}
	detail := fmt.Sprintf("Certified %d of %d capabilities for %s.", certified, len(model.Capabilities), model.UpstreamModel)
	if _, err = tx.Exec(ctx, "UPDATE olp.providers SET last_probe_at=$2,last_probe_status=$3,last_probe_detail=$4 WHERE id=$1", current.ID, at, probeStatus(certified > 0), detail); err != nil {
		return "", err
	}
	etag, err := touch(ctx, tx, current.ID)
	if err != nil {
		return "", err
	}
	return etag, nil
}

func (s *Catalogue) storeProbe(ctx context.Context, tx pgx.Tx, id string, at time.Time, succeeded bool, detail string) error {
	_, err := tx.Exec(ctx, "UPDATE olp.providers SET last_probe_at=$2,last_probe_status=$3,last_probe_detail=$4 WHERE id=$1", id, at, probeStatus(succeeded), detail)
	return err
}
