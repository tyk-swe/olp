package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/providers/probe"
	"github.com/tyk-swe/olp/internal/routing"
)

type capabilityReply struct {
	connection.Capability
	Source      string     `json:"source"`
	CertifiedAt *time.Time `json:"certified_at"`
	ToolCalls   bool       `json:"tool_calls"`
}

type modelReply struct {
	ID            string            `json:"id"`
	UpstreamModel string            `json:"upstream_model"`
	DisplayName   string            `json:"display_name"`
	Enabled       bool              `json:"enabled"`
	Capabilities  []capabilityReply `json:"capabilities"`
	DiscoveredAt  *time.Time        `json:"discovered_at"`
}

func (m storedModel) reply() modelReply {
	capabilities := make([]capabilityReply, len(m.Capabilities))
	for i, c := range m.Capabilities {
		capabilities[i] = capabilityReply{Capability: c.Capability, Source: c.Source, CertifiedAt: c.CertifiedAt, ToolCalls: c.ToolCalls}
	}
	return modelReply{ID: m.ID, UpstreamModel: m.UpstreamModel, DisplayName: m.DisplayName, Enabled: m.Enabled, Capabilities: capabilities, DiscoveredAt: m.DiscoveredAt}
}

func publishedModelReply(m routing.RevisionModel) modelReply {
	capabilities := make([]capabilityReply, len(m.Capabilities))
	for i, c := range m.Capabilities {
		capabilities[i] = capabilityReply{Capability: connection.Capability{Operation: c.Operation, Surface: c.Surface, Mode: c.Mode}, Source: c.Source, CertifiedAt: c.CertifiedAt, ToolCalls: c.ToolCalls}
	}
	return modelReply{ID: m.ID, UpstreamModel: m.UpstreamModel, DisplayName: m.DisplayName, Enabled: true, Capabilities: capabilities}
}

func modelID(m modelReply) string { return m.ID }

// prepare reads what an upstream call needs without holding the installation
// lock: the provider, its etag precondition, and the enabled probe credential.
// The caller's project scope is decided first, so a provider outside it answers
// 404 before its precondition or credential is consulted.
func (s *Server) prepare(r *http.Request, p access.Principal, id string) (*record, probe.Target, error) {
	tx, err := s.access.Pool.Begin(r.Context())
	if err != nil {
		return nil, probe.Target{}, err
	}
	defer tx.Rollback(r.Context())
	current, err := readProvider(r.Context(), tx, id)
	if err != nil {
		return nil, probe.Target{}, err
	}
	if err = p.Project(current.ProjectID, access.Change); err != nil {
		return nil, probe.Target{}, err
	}
	if err = access.Match(r, current.ETag); err != nil {
		return nil, probe.Target{}, err
	}
	credential, _, err := s.credentialFor(r.Context(), tx, current)
	if err != nil {
		return nil, probe.Target{}, err
	}
	rows, err := tx.Query(r.Context(), "SELECT upstream_model FROM olp.provider_models WHERE provider_id=$1 ORDER BY enabled DESC,upstream_model LIMIT 2000", id)
	if err != nil {
		return nil, probe.Target{}, err
	}
	models, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, probe.Target{}, err
	}
	return current, current.target(credential, models...), nil
}

func (s *Server) probe(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	current, target, err := s.prepare(r, p, id)
	if err != nil {
		return access.Reply{}, err
	}
	at := time.Now().UTC()
	models, err := s.prober.ListModelFacts(r.Context(), target)
	var discovered any
	succeeded, detail := err == nil, ""
	if succeeded {
		discovered = len(models)
		detail = fmt.Sprintf("Reached the upstream; %d models listed.", len(models))
	} else {
		detail = probe.Classify(err).Detail
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
	_, err := access.Transact(s.access, r, changeable(r.Context(), id), func(tx pgx.Tx, p access.Principal, _ *record) (access.Reply, error) {
		if err := storeProbe(r.Context(), tx, id, at, succeeded, detail); err != nil {
			return access.Reply{}, err
		}
		return access.Reply{}, access.Audit(r.Context(), tx, r, p.Actor(), action, "provider", id, auditOutcome(succeeded))
	})
	return err
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
	var models []probe.DiscoveredModel
	seen := map[string]bool{}
	for _, m := range input.Models {
		if err = connection.ValidModelName("models.upstream_model", m.UpstreamModel); err != nil {
			return access.Reply{}, err
		}
		if m.DisplayName == "" {
			m.DisplayName = m.UpstreamModel
		}
		if err = connection.ValidModelName("models.display_name", m.DisplayName); err != nil {
			return access.Reply{}, err
		}
		if !seen[m.UpstreamModel] {
			seen[m.UpstreamModel] = true
			models = append(models, probe.DiscoveredModel{Name: m.UpstreamModel, Display: m.DisplayName})
		}
	}
	current, target, err := s.prepare(r, p, id)
	if err != nil {
		return access.Reply{}, err
	}
	upstream := len(models) == 0
	at := time.Now().UTC()
	if upstream {
		listed, err := s.prober.ListModelFacts(r.Context(), target)
		if err != nil {
			pe := probe.Classify(err)
			if err := s.recordProbe(r, "provider.discover", id, at, false, pe.Detail); err != nil {
				return access.Reply{}, err
			}
			return access.Reply{}, access.Fail(422, "discovery_failed", pe.Detail)
		}
		models = append(models, listed...)
	}
	return access.Transact(s.access, r, unchangedSince(r.Context(), current, "discovery"), func(tx pgx.Tx, p access.Principal, locked *record) (access.Reply, error) {
		if err := s.storeDiscoveredModels(r.Context(), tx, locked, models, upstream, at); err != nil {
			return access.Reply{}, err
		}
		if err := access.Audit(r.Context(), tx, r, p.Actor(), "provider.discover", "provider", id, "success"); err != nil {
			return access.Reply{}, err
		}
		return s.detailReply(r.Context(), tx, id)
	})
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
	if _, err = visibleProvider(r.Context(), s.access.Pool, p, id); err != nil {
		return access.Reply{}, err
	}
	d, err := s.detail(r.Context(), s.access.Pool, id)
	if err != nil {
		return access.Reply{}, err
	}
	models, err := queryModels(r.Context(), s.access.Pool, "SELECT "+modelColumns+" FROM olp.provider_models WHERE provider_id=$1 AND id<$2 ORDER BY id DESC LIMIT $3", id, page.Before, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	items := make([]modelReply, len(models))
	for i, m := range models {
		items[i] = m.reply()
	}
	return access.Detail(struct {
		access.ListBody[modelReply]
		Provider *detail `json:"provider"`
	}{access.ListPage(items, page, modelID), d}, d.ETag), nil
}

type setModelRequest struct {
	Enabled      bool                     `json:"enabled"`
	Capabilities *[]connection.Capability `json:"capabilities"`
}

func readModel(ctx context.Context, q access.Queryer, providerID, modelID string) (storedModel, error) {
	return scanModel(q.QueryRow(ctx, "SELECT "+modelColumns+" FROM olp.provider_models WHERE provider_id=$1 AND id=$2", providerID, modelID))
}

func lockModel(ctx context.Context, tx pgx.Tx, providerID, modelID string) (storedModel, error) {
	return scanModel(tx.QueryRow(ctx, "SELECT "+modelColumns+" FROM olp.provider_models WHERE provider_id=$1 AND id=$2 FOR UPDATE", providerID, modelID))
}

func (s *Server) setModel(r *http.Request, _ access.Principal) (access.Reply, error) {
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
	return access.Transact(s.access, r, changeable(r.Context(), id), func(tx pgx.Tx, p access.Principal, current *record) (access.Reply, error) {
		if err := access.Match(r, current.ETag); err != nil {
			return access.Reply{}, err
		}
		if err := s.setDraftModel(r.Context(), tx, current, modelID, input); err != nil {
			return access.Reply{}, err
		}
		if err := access.Audit(r.Context(), tx, r, p.Actor(), "provider.model.update", "provider_model", modelID, "success"); err != nil {
			return access.Reply{}, err
		}
		return s.detailReply(r.Context(), tx, id)
	})
}

func (s *Server) certify(r *http.Request, p access.Principal) (access.Reply, error) {
	a := s.access
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	modelID, err := access.IDParam(r, "model_id")
	if err != nil {
		return access.Reply{}, err
	}
	current, target, err := s.prepare(r, p, id)
	if err != nil {
		return access.Reply{}, err
	}
	m, err := readModel(r.Context(), a.Pool, id, modelID)
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
	// The route deadline gives each capability a probe budget; the last one
	// stays reserved so the results are always recorded.
	probeCtx, cancel := probe.Budget(r.Context())
	defer cancel()
	fingerprint := defaultSlot.credentialFingerprint(current.Connection.Document())
	capabilities := make([]storedCapability, len(m.Capabilities))
	results := make([]certifyResult, len(m.Capabilities))
	for i, c := range m.Capabilities {
		capabilities[i], results[i] = s.certifyCapability(probeCtx, target, m.UpstreamModel, c.Capability, fingerprint, at)
	}
	certified := countCertified(results)
	status := "certified"
	switch {
	case certified == 0:
		status = "failed"
	case certified < len(m.Capabilities):
		status = "partial"
	}
	reply, err := access.Transact(s.access, r, unchangedSince(r.Context(), current, "certification"), func(tx pgx.Tx, p access.Principal, _ *record) (access.Reply, error) {
		etag, err := s.storeCertification(r.Context(), tx, current, m.withCapabilities(capabilities), defaultSlot, certified, at)
		if err != nil {
			return access.Reply{}, err
		}
		if err = access.Audit(r.Context(), tx, r, p.Actor(), "provider.model.certify", "provider_model", modelID, auditOutcome(certified > 0)); err != nil {
			return access.Reply{}, err
		}
		return access.Detail(certifyReply{ProviderID: id, ModelID: modelID, Status: status, CheckedAt: at, CertifiedCount: certified, AttemptedCount: len(results), Results: results}, etag), nil
	})
	if err == nil && certified == len(m.Capabilities) {
		s.clearValidatedCooldowns(r.Context(), id, defaultSlot)
	}
	return reply, err
}

func (s *Server) setDraftModel(ctx context.Context, tx pgx.Tx, current *record, modelID string, input setModelRequest) error {
	m, err := lockModel(ctx, tx, current.ID, modelID)
	if err != nil {
		return err
	}
	capabilities := m.Capabilities
	if input.Capabilities != nil {
		requested, err := connection.ValidCapabilities(*input.Capabilities)
		if err != nil {
			return err
		}
		existing := map[connection.Capability]storedCapability{}
		for _, c := range m.Capabilities {
			existing[c.Capability] = c
		}
		capabilities = make([]storedCapability, 0, len(requested))
		for _, c := range requested {
			if !probe.ConfigurationCertifiable(current.Connection.Transport(), c) {
				return access.Fail(422, "capability_unavailable", "This connector cannot certify the requested tuple.")
			}
			if kept, ok := existing[c]; ok {
				capabilities = append(capabilities, kept)
				continue
			}
			capabilities = append(capabilities, storedCapability{Capability: connection.Capability{Operation: c.Operation, Surface: c.Surface, Mode: c.Mode}, Source: "declared"})
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

func (s *Server) storeDiscoveredModels(ctx context.Context, tx pgx.Tx, current *record, models []probe.DiscoveredModel, upstream bool, at time.Time) error {
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
	prepared, err := connection.Prepare(ctx, s.ConnectionPolicy(tx), document)
	if err != nil {
		return err
	}
	if err = s.StoreDraftConfiguration(ctx, tx, current.ID, current.Name, prepared); err != nil {
		return err
	}
	if upstream {
		if err = storeProbe(ctx, tx, current.ID, at, true, fmt.Sprintf("Discovered %d models.", len(models))); err != nil {
			return err
		}
	}
	_, err = touch(ctx, tx, current.ID)
	return err
}

type certifyReply struct {
	ProviderID     string          `json:"provider_id"`
	ModelID        string          `json:"model_id"`
	Status         string          `json:"status"`
	CheckedAt      time.Time       `json:"checked_at"`
	CertifiedCount int             `json:"certified_count"`
	AttemptedCount int             `json:"attempted_count"`
	Results        []certifyResult `json:"results"`
}

type certifyResult struct {
	connection.Capability
	Succeeded bool          `json:"succeeded"`
	Detail    string        `json:"detail"`
	ErrorCode *probe.Reason `json:"error_code"`
	ToolCalls *bool         `json:"tool_calls,omitempty"`
}

func (s *Server) certifyCapability(ctx context.Context, target probe.Target, model string, c connection.Capability, fingerprint string, at time.Time) (storedCapability, certifyResult) {
	if err := s.prober.Certify(ctx, target, model, c, probe.BodyLimit); err != nil {
		failure := probe.Classify(err)
		return storedCapability{Capability: c, Source: "declared"}, certifyResult{Capability: c, Detail: failure.Detail, ErrorCode: &failure.Reason}
	}
	toolCalls := c.Operation == "generation" && (c.Surface == "openai" || c.Surface == "anthropic" || c.Surface == "gemini") && (c.Mode == "unary" || c.Mode == "streaming") && s.prober.CertifyToolCalls(ctx, target, model, c.Mode == "streaming") == nil
	return storedCapability{Capability: c, Source: "certified", CertifiedAt: &at, CredentialFingerprint: fingerprint, ToolCalls: toolCalls},
		certifyResult{Capability: c, Succeeded: true, Detail: "Certified.", ToolCalls: &toolCalls}
}

func countCertified(results []certifyResult) int {
	count := 0
	for _, result := range results {
		if result.Succeeded {
			count++
		}
	}
	return count
}

func (s *Server) storeCertification(ctx context.Context, tx pgx.Tx, current *record, model storedModel, defaultSlot slotRow, certified int, at time.Time) (string, error) {
	encoded, err := json.Marshal(model.Capabilities)
	if err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, "UPDATE olp.provider_models SET capabilities=$3 WHERE provider_id=$1 AND id=$2", current.ID, model.ID, encoded); err != nil {
		return "", err
	}
	models, err := enabledModels(ctx, tx, current.ID)
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
	if err = storeProbe(ctx, tx, current.ID, at, certified > 0, detail); err != nil {
		return "", err
	}
	return touch(ctx, tx, current.ID)
}

func storeProbe(ctx context.Context, tx pgx.Tx, id string, at time.Time, succeeded bool, detail string) error {
	_, err := tx.Exec(ctx, "UPDATE olp.providers SET last_probe_at=$2,last_probe_status=$3,last_probe_detail=$4 WHERE id=$1", id, at, probeStatus(succeeded), detail)
	return err
}

// credentialFor prefers the enabled default slot, then another enabled slot.
func (s *Server) credentialFor(ctx context.Context, tx pgx.Tx, p *record) ([]byte, *credentialState, error) {
	slots, err := loadSlots(ctx, tx, p.ID)
	if err != nil {
		return nil, nil, err
	}
	slot := selectProbeSlot(slots, p.Connection.Document())
	if slot == nil {
		return nil, nil, access.Fail(422, "credential_required", "Enable a credential slot before probing this connection.")
	}
	state := &credentialState{ID: slot.CredentialID, Version: slot.CredentialVersion, Revoked: slot.CredentialRevoked}

	if !p.Connection.Document().CredentialRequired() {
		return nil, state, nil
	}
	if state.ID == nil && p.Connection.Document().Grant() {
		return nil, state, access.Fail(422, "credential_required", "Enroll a grant before probing this connection.")
	}
	if state.ID == nil {
		return nil, state, access.Fail(422, "credential_required", "Add a credential before probing this connection.")
	}
	if state.Revoked {
		return nil, state, access.Fail(422, "credential_revoked", "The draft credential was revoked; rotate before probing.")
	}
	if slot.CredentialLapsed {
		return nil, state, access.Fail(422, "credential_lapsed", "The draft credential's grant lapsed; re-enroll it before probing.")
	}
	if err := slot.credentialFits(p.Connection.Document()); err != nil {
		return nil, state, err
	}
	secret, err := s.readCredential(ctx, tx, *state.ID)
	if err != nil {
		return nil, state, err
	}
	return secret, state, nil
}

func selectProbeSlot(slots []slotRow, cfg connection.Configuration) *slotRow {
	var best *slotRow
	usable := func(s *slotRow) bool {
		return !cfg.CredentialRequired() || s.CredentialID != nil && !s.CredentialRevoked && !s.CredentialLapsed
	}
	for i := range slots {
		row := &slots[i]
		if !row.Enabled {
			continue
		}
		if best == nil || usable(row) && !usable(best) || usable(row) == usable(best) && (row.Default && !best.Default || row.Default == best.Default && (row.Priority < best.Priority || row.Priority == best.Priority && row.Position < best.Position)) {
			best = row
		}
	}
	return best
}
