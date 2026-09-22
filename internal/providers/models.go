package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"time"
	"unicode"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/operationregistry"
)

func modelJSON(m storedModel) map[string]any {
	capabilities := make([]map[string]any, 0, len(m.Capabilities))
	for _, c := range m.Capabilities {
		capabilities = append(capabilities, map[string]any{"operation": c.Operation, "surface": c.Surface, "mode": c.Mode, "source": c.Source, "certified_at": c.CertifiedAt})
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
func (s *Server) prepare(r *http.Request, id string) (*record, []byte, error) {
	tx, err := s.Access.Pool.Begin(r.Context())
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(r.Context())
	current, err := load(r.Context(), tx, id, false)
	if err != nil {
		return nil, nil, err
	}
	if err = access.Match(r, current.ETag); err != nil {
		return nil, nil, err
	}
	if err = current.Configuration.Validate(s.Egress); err != nil {
		return nil, nil, err
	}
	credential, _, err := s.credentialFor(r.Context(), tx, current)
	if err != nil {
		return nil, nil, err
	}
	rows, err := tx.Query(r.Context(), "SELECT upstream_model FROM olp_go.provider_models WHERE provider_id=$1 ORDER BY enabled DESC,upstream_model LIMIT 2000", id)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			return nil, nil, err
		}
		current.Configuration.ProbeModels = append(current.Configuration.ProbeModels, model)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return current, credential, nil
}

func (s *Server) probe(r *http.Request) (access.Reply, error) {
	p, err := s.Access.Principal(r, s.Access.Pool, "configure")
	if err != nil {
		return access.Reply{}, err
	}
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	current, credential, err := s.prepare(r, id)
	if err != nil {
		return access.Reply{}, err
	}
	if err := access.ProjectAccess(p, current.ProjectID, true); err != nil {
		return access.Reply{}, err
	}
	at := time.Now().UTC()
	models, err := s.listModelFacts(r.Context(), &current.Configuration, credential)
	var discovered any
	succeeded, detail := err == nil, ""
	if succeeded {
		discovered = len(models)
		detail = fmt.Sprintf("Reached the upstream; %d models listed.", len(models))
	} else {
		detail = classify(err).Detail
	}
	if _, err = s.Access.Pool.Exec(r.Context(), "UPDATE olp_go.providers SET last_probe_at=$2,last_probe_status=$3,last_probe_detail=$4 WHERE id=$1", id, at, probeStatus(succeeded), detail); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(map[string]any{"provider_id": id, "succeeded": succeeded, "checked_at": at, "probe_type": "models", "detail": detail, "discovered_models": discovered}, current.ETag), nil
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

func (s *Server) discover(r *http.Request) (access.Reply, error) {
	a := s.Access
	p, err := a.Principal(r, a.Pool, "configure")
	if err != nil {
		return access.Reply{}, err
	}
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
	type declared struct {
		upstream, display string
		metadata          map[string]json.RawMessage
	}
	var models []declared
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
			models = append(models, declared{m.UpstreamModel, m.DisplayName, nil})
		}
	}
	current, credential, err := s.prepare(r, id)
	if err != nil {
		return access.Reply{}, err
	}
	if err := access.ProjectAccess(p, current.ProjectID, true); err != nil {
		return access.Reply{}, err
	}
	upstream := len(models) == 0
	at := time.Now().UTC()
	if upstream {
		listed, err := s.listModelFacts(r.Context(), &current.Configuration, credential)
		if err != nil {
			pe := classify(err)
			a.Pool.Exec(r.Context(), "UPDATE olp_go.providers SET last_probe_at=$2,last_probe_status='failed',last_probe_detail=$3 WHERE id=$1", id, at, pe.Detail)
			return access.Reply{}, access.Fail(422, "discovery_failed", pe.Detail)
		}
		for _, name := range listed {
			models = append(models, declared{name.Name, name.Display, name.Metadata})
		}
	}
	tx, err := a.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err = a.Principal(r, tx, "configure")
	if err != nil {
		return access.Reply{}, err
	}
	locked, err := load(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	if locked.ETag != current.ETag {
		return access.Reply{}, access.Fail(412, "etag_mismatch", "The connection changed during discovery; reload and retry.")
	}
	for _, m := range models {
		facts := m.metadata
		if facts == nil {
			facts = map[string]json.RawMessage{}
		}
		var existing map[string]json.RawMessage
		_ = json.Unmarshal(locked.Configuration.Options.Models[m.upstream], &existing)
		maps.Copy(facts, existing)
		if len(facts) > 0 {
			encoded, _ := json.Marshal(facts)
			locked.Configuration.Options.Models[m.upstream] = encoded
		}
		if _, err = tx.Exec(r.Context(), "INSERT INTO olp_go.provider_models(id,provider_id,upstream_model,display_name,enabled,capabilities,discovered_at) VALUES($1,$2,$3,$4,false,'[]',$5) ON CONFLICT(provider_id,upstream_model) DO UPDATE SET display_name=excluded.display_name,discovered_at=excluded.discovered_at", access.NewID(), id, m.upstream, m.display, at); err != nil {
			return access.Reply{}, err
		}
	}
	config, _ := json.Marshal(locked.Configuration)
	if _, err = tx.Exec(r.Context(), "UPDATE olp_go.providers SET configuration=$2 WHERE id=$1", id, config); err != nil {
		return access.Reply{}, err
	}
	if upstream {
		if _, err = tx.Exec(r.Context(), "UPDATE olp_go.providers SET last_probe_at=$2,last_probe_status='succeeded',last_probe_detail=$3 WHERE id=$1", id, at, fmt.Sprintf("Discovered %d models.", len(models))); err != nil {
			return access.Reply{}, err
		}
	}
	if _, err = touch(r.Context(), tx, id); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.ID, "provider.discover", "provider", id, "success"); err != nil {
		return access.Reply{}, err
	}
	result, err := s.detailReply(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}

func (s *Server) models(r *http.Request) (access.Reply, error) {
	p, err := s.Access.Principal(r, s.Access.Pool, "read")
	if err != nil {
		return access.Reply{}, err
	}
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	if _, err = checkProvider(r.Context(), s.Access.Pool, p, id, false); err != nil {
		return access.Reply{}, err
	}
	d, err := s.detail(r.Context(), s.Access.Pool, id)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT id::text,upstream_model,display_name,enabled,capabilities,discovered_at FROM olp_go.provider_models WHERE provider_id=$1 AND id<$2 ORDER BY id DESC LIMIT $3", id, page.Before, page.Limit+1)
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
	query := "SELECT id::text,upstream_model,display_name,enabled,capabilities,discovered_at FROM olp_go.provider_models WHERE provider_id=$1 AND id=$2"
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
		supported := operationregistry.Default.Supports(c.Operation, c.Surface, c.Mode)
		for _, option := range CapabilityOptions {
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

func (s *Server) setModel(r *http.Request) (access.Reply, error) {
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
	tx, err := a.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := a.Principal(r, tx, "configure")
	if err != nil {
		return access.Reply{}, err
	}
	current, err := load(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	if err := access.ProjectAccess(p, current.ProjectID, true); err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, current.ETag); err != nil {
		return access.Reply{}, err
	}
	m, err := loadModel(r.Context(), tx, id, modelID, true)
	if err != nil {
		return access.Reply{}, err
	}
	capabilities := m.Capabilities
	if input.Capabilities != nil {
		requested, err := ValidCapabilities(*input.Capabilities)
		if err != nil {
			return access.Reply{}, err
		}
		existing := map[CapabilityInput]storedCapability{}
		for _, c := range m.Capabilities {
			existing[CapabilityInput{c.Operation, c.Surface, c.Mode}] = c
		}
		capabilities = make([]storedCapability, 0, len(requested))
		for _, c := range requested {
			if !configurationCertifiable(&current.Configuration, c) {
				return access.Reply{}, access.Fail(422, "capability_unavailable", "This connector cannot certify the requested tuple.")
			}
			if kept, ok := existing[c]; ok {
				capabilities = append(capabilities, kept)
				continue
			}
			capabilities = append(capabilities, storedCapability{Operation: c.Operation, Surface: c.Surface, Mode: c.Mode, Source: "declared"})
		}
	}
	encoded, _ := json.Marshal(capabilities)
	if _, err = tx.Exec(r.Context(), "UPDATE olp_go.provider_models SET enabled=$3,capabilities=$4 WHERE provider_id=$1 AND id=$2", id, modelID, input.Enabled, encoded); err != nil {
		return access.Reply{}, err
	}
	if _, err = touch(r.Context(), tx, id); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.ID, "provider.model.update", "provider_model", modelID, "success"); err != nil {
		return access.Reply{}, err
	}
	result, err := s.detailReply(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}

func (s *Server) certify(r *http.Request) (access.Reply, error) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	r = r.WithContext(ctx)
	a := s.Access
	p, err := a.Principal(r, a.Pool, "configure")
	if err != nil {
		return access.Reply{}, err
	}
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	modelID, err := access.IDParam(r, "model_id")
	if err != nil {
		return access.Reply{}, err
	}
	current, credential, err := s.prepare(r, id)
	if err != nil {
		return access.Reply{}, err
	}
	if err := access.ProjectAccess(p, current.ProjectID, true); err != nil {
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
	selected := selectProbeSlot(slots, &current.Configuration)
	if selected == nil {
		return access.Reply{}, access.Fail(422, "credential_required", "Enable a credential slot before certifying.")
	}
	defaultSlot := *selected

	at := time.Now().UTC()
	results := make([]map[string]any, 0, len(m.Capabilities))
	certified := 0
	for i := range m.Capabilities {
		c := &m.Capabilities[i]
		err := s.certifyTuple(r.Context(), &current.Configuration, credential, m.UpstreamModel, CapabilityInput{c.Operation, c.Surface, c.Mode}, probeBodyLimit)
		item := map[string]any{"operation": c.Operation, "surface": c.Surface, "mode": c.Mode, "succeeded": err == nil, "detail": "Certified.", "error_code": nil}
		if err == nil {
			certified++
			c.Source, c.CertifiedAt = "certified", &at
			c.CredentialFingerprint = defaultSlot.credentialFingerprint(&current.Configuration)
		} else {
			pe := classify(err)
			item["detail"], item["error_code"] = pe.Detail, pe.Code
			c.Source, c.CertifiedAt = "declared", nil
			c.CredentialFingerprint = ""
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
	tx, err := a.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err = a.Principal(r, tx, "configure")
	if err != nil {
		return access.Reply{}, err
	}
	locked, err := load(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	if locked.ETag != current.ETag {
		return access.Reply{}, access.Fail(412, "etag_mismatch", "The connection changed during certification; reload and retry.")
	}
	encoded, _ := json.Marshal(m.Capabilities)
	if _, err = tx.Exec(r.Context(), "UPDATE olp_go.provider_models SET capabilities=$3 WHERE provider_id=$1 AND id=$2", id, modelID, encoded); err != nil {
		return access.Reply{}, err
	}
	models, err := loadModels(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	if validatedAt := defaultSlot.certificationTime(&current.Configuration, models); validatedAt != nil {
		fingerprint := defaultSlot.validationFingerprint(&current.Configuration, models)
		if _, err = tx.Exec(r.Context(), "UPDATE olp_go.provider_slots SET validated_at=$2,validated_fingerprint=$3 WHERE id=$1", defaultSlot.ID, validatedAt, fingerprint); err != nil {
			return access.Reply{}, err
		}
	}
	detail := fmt.Sprintf("Certified %d of %d capabilities for %s.", certified, len(m.Capabilities), m.UpstreamModel)
	if _, err = tx.Exec(r.Context(), "UPDATE olp_go.providers SET last_probe_at=$2,last_probe_status=$3,last_probe_detail=$4 WHERE id=$1", id, at, probeStatus(certified > 0), detail); err != nil {
		return access.Reply{}, err
	}
	etag, err := touch(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.ID, "provider.model.certify", "provider_model", modelID, auditOutcome(certified > 0)); err != nil {
		return access.Reply{}, err
	}
	result := access.Detail(map[string]any{"provider_id": id, "model_id": modelID, "status": status, "checked_at": at, "certified_count": certified, "attempted_count": len(m.Capabilities), "results": results}, etag)
	reply, err := access.Commit(r, tx, result)
	if err == nil && certified == len(m.Capabilities) {
		s.clearValidatedCooldowns(r.Context(), id, defaultSlot)
	}
	return reply, err
}
