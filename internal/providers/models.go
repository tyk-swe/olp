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
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/providers/probe"
	"github.com/tyk-swe/olp/internal/routing"
)

func (m storedModel) withCapabilities(capabilities []storedCapability) storedModel {
	updated := m
	updated.Capabilities = capabilities
	return updated
}

func (s *modelService) models(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "provider_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if _, err = visibleProvider(r.Context(), s.access.Pool, p, id); err != nil {
		return httpkit.Reply{}, err
	}
	d, err := s.drafts.detail(r.Context(), s.access.Pool, id)
	if err != nil {
		return httpkit.Reply{}, err
	}
	models, err := queryModels(r.Context(), s.access.Pool, "SELECT "+modelColumns+" FROM olp.provider_models WHERE provider_id=$1 AND id<$2 ORDER BY id DESC LIMIT $3", id, page.Before, page.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	items := make([]modelReply, len(models))
	for i, m := range models {
		items[i] = m.reply()
	}
	return httpkit.Detail(struct {
		httpkit.ListBody[modelReply]
		Provider *detail `json:"provider"`
	}{httpkit.ListPage(items, page, modelID), d}, d.ETag), nil
}

type setModelRequest struct {
	Enabled      bool                     `json:"enabled"`
	Capabilities *[]connection.Capability `json:"capabilities"`
}

func readModel(ctx context.Context, q database.Queryer, providerID, modelID string) (storedModel, error) {
	return scanModel(q.QueryRow(ctx, "SELECT "+modelColumns+" FROM olp.provider_models WHERE provider_id=$1 AND id=$2", providerID, modelID))
}

func lockModel(ctx context.Context, tx pgx.Tx, providerID, modelID string) (storedModel, error) {
	return scanModel(tx.QueryRow(ctx, "SELECT "+modelColumns+" FROM olp.provider_models WHERE provider_id=$1 AND id=$2 FOR UPDATE", providerID, modelID))
}

func (s *modelService) setModel(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "provider_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	modelID, err := httpkit.IDParam(r, "model_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var input setModelRequest
	if err = httpkit.DecodeUnique(r, &input, 1<<20); err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(s.access, r, changeable(r.Context(), id), func(tx pgx.Tx, p authz.Principal, current *record) (httpkit.Reply, access.Event, error) {
		if err := httpkit.Match(r, current.ETag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err := s.setDraftModel(r.Context(), tx, current, modelID, input); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		reply, err := s.drafts.detailReply(r.Context(), tx, id)
		return reply, access.Event{Action: "provider.model.update", Resource: "provider_model", ID: modelID}, err
	})
}

func (s *modelService) certify(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	a := s.access
	id, err := httpkit.IDParam(r, "provider_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	modelID, err := httpkit.IDParam(r, "model_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	current, err := readProbeProvider(r, p, a.Pool, id)
	if err != nil {
		return httpkit.Reply{}, err
	}
	m, err := readModel(r.Context(), a.Pool, id, modelID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if len(m.Capabilities) == 0 {
		return httpkit.Reply{}, httpkit.Fail(422, "no_capabilities", "Declare at least one capability before certifying.")
	}
	slots, err := loadSlots(r.Context(), a.Pool, id)
	if err != nil {
		return httpkit.Reply{}, err
	}
	cfg := current.Connection.Document()
	slot := selectProbeSlot(slots, cfg, &m)
	if slot == nil {
		return httpkit.Reply{}, httpkit.Fail(422, "credential_required", "Enable the model and configure an enabled credential slot that allows it before certifying.")
	}
	credential, err := s.credentials.probeCredential(r.Context(), a.Pool, cfg, *slot)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer clear(credential)
	target := current.target(credential)

	at := time.Now().UTC()
	// The route deadline gives each capability a probe budget; the last one
	// stays reserved so the results are always recorded.
	probeCtx, cancel := probe.Budget(r.Context())
	defer cancel()
	fingerprint := slot.credentialFingerprint(cfg)
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
	reply, err := access.Transact(s.access, r, unchangedSince(r.Context(), current, "certification"), func(tx pgx.Tx, p authz.Principal, _ *record) (httpkit.Reply, access.Event, error) {
		etag, err := s.storeCertification(r.Context(), tx, current, m.withCapabilities(capabilities), *slot, certified, at)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		event := access.Event{Action: "provider.model.certify", Resource: "provider_model", ID: modelID, Failed: certified == 0}
		return httpkit.Detail(certifyReply{ProviderID: id, ModelID: modelID, Status: status, CheckedAt: at, CertifiedCount: certified, AttemptedCount: len(results), Results: results}, etag), event, nil
	})
	if err == nil && certified == len(m.Capabilities) {
		clearValidatedCooldowns(r.Context(), s.quota, s.log, id, *slot)
	}
	return reply, err
}

func (s *modelService) setDraftModel(ctx context.Context, tx pgx.Tx, current *record, modelID string, input setModelRequest) error {
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
				return httpkit.Fail(422, "capability_unavailable", "This connector cannot certify the requested tuple.")
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

func (s *modelService) storeDiscoveredModels(ctx context.Context, tx pgx.Tx, current *record, models []probe.DiscoveredModel, upstream bool, at time.Time, listing *routing.RevisionListing) error {
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
		endpoint, apiVersion, credentialID, credentialVersion, listedAt := listingValues(listing)
		if _, err = tx.Exec(ctx, `INSERT INTO olp.provider_models(id,provider_id,upstream_model,display_name,enabled,capabilities,discovered_at,discovered_listing_endpoint,discovered_listing_api_version,discovered_listing_credential_id,discovered_listing_credential_version,discovered_listing_at) VALUES($1,$2,$3,$4,false,'[]',$5,$6,$7,$8,$9,$10)
			ON CONFLICT(provider_id,upstream_model) DO UPDATE SET display_name=excluded.display_name,discovered_at=excluded.discovered_at,
			discovered_listing_endpoint=coalesce(excluded.discovered_listing_endpoint,olp.provider_models.discovered_listing_endpoint),
			discovered_listing_api_version=coalesce(excluded.discovered_listing_api_version,olp.provider_models.discovered_listing_api_version),
			discovered_listing_credential_id=coalesce(excluded.discovered_listing_credential_id,olp.provider_models.discovered_listing_credential_id),
			discovered_listing_credential_version=coalesce(excluded.discovered_listing_credential_version,olp.provider_models.discovered_listing_credential_version),
			discovered_listing_at=coalesce(excluded.discovered_listing_at,olp.provider_models.discovered_listing_at)`, ids.New(), current.ID, m.Name, m.Display, at, endpoint, apiVersion, credentialID, credentialVersion, listedAt); err != nil {
			return err
		}
	}
	document.Options.Models = metadata
	prepared, err := connection.Prepare(ctx, s.drafts.connectionPolicy(tx), document)
	if err != nil {
		return err
	}
	if _, err = s.drafts.storeDraftConfiguration(ctx, tx, current.ID, current.Name, prepared); err != nil {
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

func (s *modelService) certifyCapability(ctx context.Context, target probe.Target, model string, c connection.Capability, fingerprint string, at time.Time) (storedCapability, certifyResult) {
	if err := s.prober.Certify(ctx, target, model, c); err != nil {
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

func (s *modelService) storeCertification(ctx context.Context, tx pgx.Tx, current *record, model storedModel, slot slotRow, certified int, at time.Time) (string, error) {
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
	if validatedAt := slot.certificationTime(current.Connection.Document(), models); validatedAt != nil {
		fingerprint, err := slot.validationFingerprint(current.Connection.Document(), models)
		if err != nil {
			return "", err
		}
		if _, err = tx.Exec(ctx, "UPDATE olp.provider_slots SET validated_at=$2,validated_fingerprint=$3 WHERE id=$1", slot.ID, validatedAt, fingerprint); err != nil {
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
