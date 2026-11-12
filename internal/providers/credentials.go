package providers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/providers/probe"
	"github.com/tyk-swe/olp/internal/routing"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/grants"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routeidentity"
	"github.com/tyk-swe/olp/internal/secretstore"
)

const maxSlots = 64

func (s *Server) credentials(r *http.Request, p access.Principal) (access.Reply, error) {
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
	rows, err := s.Access.Pool.Query(r.Context(), `SELECT jsonb_build_object(
		'id',c.id,'version',c.version,
		'active',EXISTS(SELECT 1 FROM jsonb_array_elements(r.slots) slot WHERE slot->>'credential_id'=c.id::text),
		'draft_selected',EXISTS(SELECT 1 FROM olp.provider_slots d WHERE d.provider_id=p.id AND d.credential_id=c.id),
		'created_at',c.created_at,'revoked_at',c.revoked_at,
		'external_reference',c.external_reference,
		'grant',CASE WHEN c.plugin_digest IS NOT NULL THEN jsonb_build_object(
			'plugin_digest',c.plugin_digest,'principal',c.principal,'facts',c.grant_facts,'expires_at',g.expires_at,'lapsed_at',g.lapsed_at) END)
		FROM olp.provider_credentials c JOIN olp.providers p ON p.id=c.provider_id
		LEFT JOIN olp.provider_revisions r ON r.id=p.active_revision_id
		LEFT JOIN olp.provider_grants g ON g.credential_id=c.id
		WHERE c.provider_id=$1 AND c.id<$2 ORDER BY c.id DESC LIMIT $3`, id, page.Before, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	items, err := access.JSONRows(rows)
	if err != nil {
		return access.Reply{}, err
	}
	return access.ListReply(items, page), nil
}

type rotateRequest struct {
	Credential          string                 `json:"credential"`
	CredentialReference *secretstore.Reference `json:"credential_reference,omitempty"`
}

// rotate validates a new credential against the upstream, records it as the
// next version, and selects it for the draft. Published revisions keep the
// version they were activated with.
func (s *Server) rotate(r *http.Request, _ access.Principal) (access.Reply, error) {
	a := s.Access
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input rotateRequest
	if err = access.DecodeUnique(r, &input, 1<<20); err != nil {
		return access.Reply{}, err
	}
	var plain *string
	if input.Credential != "" {
		plain = &input.Credential
	}
	source, err := decodeCredential(plain, input.CredentialReference)
	if err != nil {
		return access.Reply{}, err
	}
	if source == nil {
		return access.Reply{}, access.Invalid("credential", "Supply a credential or external reference.")
	}
	tx, p, err := a.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	if err = authorizeCredential(p, source); err != nil {
		return access.Reply{}, err
	}
	// The stored reply discloses the provider, so the caller must still reach
	// its project before a replay is served.
	current, err := readProvider(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(current.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	_, replayed, err := a.Replay(r, tx, p, input)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return *replayed, tx.Commit(r.Context())
	}
	if err = access.Match(r, current.ETag); err != nil {
		return access.Reply{}, err
	}
	if !current.Connection.Document().CredentialRequired() {
		return access.Reply{}, access.Fail(422, "credential_forbidden", "This authentication mode takes no stored credential.")
	}
	if current.Connection.Document().Grant() {
		return access.Reply{}, access.Fail(422, "credential_forbidden", grantEnrollmentOnly)
	}
	models, err := enabledModels(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	slots, err := loadSlots(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	defaultSlot := slots[slices.IndexFunc(slots, func(row slotRow) bool { return row.Default })]
	// Do not hold the installation mutation lock during the upstream probe.
	// The commit transaction rechecks authority, replay, and the draft ETag.
	if err = tx.Rollback(r.Context()); err != nil {
		return access.Reply{}, err
	}
	declared := make([]string, len(models))
	for i, model := range models {
		declared[i] = model.UpstreamModel
	}
	credential, err := s.material(r.Context(), source)
	if err != nil {
		return access.Reply{}, err
	}
	defer clear(credential)
	target := current.target(credential, declared...)
	if _, err = s.prober.ListModels(r.Context(), target); err != nil {
		return access.Reply{}, access.Fail(422, "credential_invalid", "The new credential was not accepted by the upstream: "+probe.Classify(err).Detail)
	}
	var validatedAt *time.Time
	if defaultSlot.validationFingerprint(current.Connection.Document(), models) != "" {
		if err = s.validateModelAccess(r.Context(), target, defaultSlot, models); err != nil {
			return access.Reply{}, access.Fail(422, "credential_invalid", "The new credential cannot access the enabled models: "+probe.Classify(err).Detail)
		}
		validatedAt = new(time.Now().UTC())
	}
	return access.Mutate(a, r, input, referencing(r.Context(), id, source), func(tx pgx.Tx, p access.Principal, locked *record) (access.Reply, error) {
		if locked.ETag != current.ETag {
			return access.Reply{}, access.Fail(412, "etag_mismatch", "The connection changed during validation; reload and retry.")
		}
		rotated, err := s.rotateCredential(r.Context(), tx, current, models, defaultSlot, validatedAt, source)
		if err != nil {
			return access.Reply{}, err
		}
		if err = access.Audit(r.Context(), tx, r, p.Actor(), "provider.credential.rotate", "provider_credential", rotated.ID, "success"); err != nil {
			return access.Reply{}, err
		}
		return access.Reply{Status: 201, ETag: rotated.ETag, Body: map[string]any{"provider_id": id, "etag": rotated.ETag, "credential_id": rotated.ID, "credential_version": rotated.Version, "runtime_generation": nil}}, nil
	})
}

// referencing locks a provider for a change that may store an external
// credential reference, which only settings administrators may name.
func referencing(ctx context.Context, id string, source credentialSource) func(pgx.Tx, access.Principal) (*record, error) {
	lock := changeable(ctx, id)
	return func(tx pgx.Tx, p access.Principal) (*record, error) {
		if err := authorizeCredential(p, source); err != nil {
			return nil, err
		}
		return lock(tx, p)
	}
}

// revoke marks a credential version unusable everywhere: drafts, published
// revisions, and gateways that learn it through the authority refresh. It
// ends the version's grant, if it has one.
func (s *Server) revoke(r *http.Request, _ access.Principal) (access.Reply, error) {
	return s.mutation(r, "provider.credential.revoke", nil, func(ctx context.Context, tx pgx.Tx, p access.Principal, current *record) (access.Reply, error) {
		credentialID, err := access.IDParam(r, "credential_id")
		if err != nil {
			return access.Reply{}, err
		}
		revoked, err := s.revokeCredential(ctx, tx, current, credentialID)
		if err != nil {
			return access.Reply{}, err
		}
		return access.Detail(map[string]any{"provider_id": current.ID, "etag": revoked.ETag, "credential_id": credentialID, "credential_version": revoked.Version, "runtime_generation": revoked.Generation}, revoked.ETag), nil
	})
}

type slotInput struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	Enabled             *bool    `json:"enabled"`
	Priority            *int     `json:"priority"`
	Weight              *int64   `json:"weight"`
	CredentialVersionID *string  `json:"credential_version_id"`
	AllowedAPIKeys      []string `json:"allowed_api_keys"`
	AllowedModels       []string `json:"allowed_models"`
	AllowedRoutes       []string `json:"allowed_routes"`
	MaxConcurrency      *int64   `json:"max_concurrency"`
	RequestsPerMinute   *int64   `json:"requests_per_minute"`
	TokensPerMinute     *int64   `json:"tokens_per_minute"`
	routing.Supply
}

func (in slotInput) limits() connection.Limits {
	return connection.Limits{MaxConcurrency: in.MaxConcurrency, RequestsPerMinute: in.RequestsPerMinute, TokensPerMinute: in.TokensPerMinute, Supply: in.Supply}
}

type slotWrite struct {
	Slot                slotInput              `json:"slot"`
	Credential          *string                `json:"credential"`
	CredentialReference *secretstore.Reference `json:"credential_reference,omitempty"`
}

func slotJSON(row slotRow) map[string]any {
	return map[string]any{
		"id": row.ID, "name": row.Name, "enabled": row.Enabled, "priority": row.Priority, "weight": row.Weight,
		"credential_version_id": row.CredentialID,
		"allowed_api_keys":      orEmpty(row.Restrictions.AllowedAPIKeys), "allowed_models": orEmpty(row.Restrictions.AllowedModels), "allowed_routes": orEmpty(row.Restrictions.AllowedRoutes),
		"max_concurrency": row.Limits.MaxConcurrency, "requests_per_minute": row.Limits.RequestsPerMinute, "tokens_per_minute": row.Limits.TokensPerMinute,
		"daily_cost_limit": row.Limits.DailyCostLimit, "monthly_cost_limit": row.Limits.MonthlyCostLimit, "priority_shares": row.Limits.PriorityShares, "saturation_percent": row.Limits.SaturationPercent,
	}
}

func orEmpty(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

func (s *Server) slotList(ctx context.Context, q access.Queryer, current *record) (access.Reply, error) {
	slots, err := loadSlots(ctx, q, current.ID)
	if err != nil {
		return access.Reply{}, err
	}
	activeCredentials := map[string]*string{}
	if current.ActiveRevisionID != nil {
		var encoded []byte
		if err = q.QueryRow(ctx, "SELECT slots FROM olp.provider_revisions WHERE id=$1 AND provider_id=$2", *current.ActiveRevisionID, current.ID).Scan(&encoded); err != nil {
			return access.Reply{}, err
		}
		var published []routing.RevisionSlot
		if err = json.Unmarshal(encoded, &published); err != nil {
			return access.Reply{}, err
		}
		for _, slot := range published {
			activeCredentials[slot.ID] = slot.CredentialID
		}
	}
	models, err := enabledModels(ctx, q, current.ID)
	if err != nil {
		return access.Reply{}, err
	}
	var credentialIDs []string
	for _, id := range activeCredentials {
		if id != nil {
			credentialIDs = append(credentialIDs, *id)
		}
	}
	generations, err := release.ReadGrantGenerations(ctx, q, credentialIDs)
	if err != nil {
		return access.Reply{}, err
	}
	items := make([]map[string]any, 0, len(slots))
	health := map[string]any{}
	for _, row := range slots {
		items = append(items, slotJSON(row))
		validated := row.validationTime(current.Connection.Document(), models)
		health[row.ID] = map[string]any{"revoked": row.CredentialRevoked, "lapsed": row.CredentialLapsed, "active_credential_version_id": activeCredentials[row.ID], "cooling_down": nil, "validated_at": validated, "usage": nil}
	}
	connection := s.quotas(ctx, current.ID, slots, activeCredentials, generations, health)
	return access.Detail(map[string]any{"items": items, "health": health, "etag": current.SlotsETag, "connection_usage": connection}, current.SlotsETag), nil
}

// quotaTimeout bounds one console read of shared quota state. Valkey holds no
// part of the stored slot list, so a slow store costs the page its live
// counters rather than the page itself.
const quotaTimeout = 2 * time.Second

// quotaUsage renders one shared usage snapshot as the management contract's
// ProviderQuotaUsage.
func quotaUsage(u fleet.Usage) map[string]any {
	return map[string]any{"requests_this_minute": u.RequestsThisMinute, "tokens_this_minute": u.TokensThisMinute, "concurrent_requests": u.ConcurrentRequests}
}

// quotas fills the live per-slot counters into health and returns the counters
// of the shared provider connection. Every field it cannot read stays null:
// the stored list is complete without them, so an unreachable or malformed
// quota store is reported to the operator and shown to the console as unknown,
// never as an idle zero and never as a failed request.
func (s *Server) quotas(ctx context.Context, providerID string, slots []slotRow, published map[string]*string, generations map[string]int64, health map[string]any) any {
	if s.Quotas == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, quotaTimeout)
	defer cancel()
	connection, err := s.Quotas.Usage(ctx, limits.ConnectionLookup(providerID))
	if err != nil {
		s.quotaUnavailable(err)
		return nil
	}
	for _, row := range slots {
		entry, ok := health[row.ID].(map[string]any)
		if !ok {
			continue
		}
		usage, err := s.Quotas.Usage(ctx, limits.SlotLookup(row.ID))
		if err != nil {
			s.quotaUnavailable(err)
			break
		}
		// The cooldown is asked of the credential version the published
		// revision dispatches with, because that is the scope the gateway
		// penalises, and of the slot itself.
		var generation int64
		if id := published[row.ID]; id != nil {
			generation = generations[*id]
		}
		cooling, err := s.Quotas.Cooling(ctx, fleet.CredentialScope(providerID, published[row.ID], generation), fleet.SlotScope(row.ID))
		if err != nil {
			s.quotaUnavailable(err)
			break
		}
		entry["usage"], entry["cooling_down"] = quotaUsage(usage), cooling
	}
	return quotaUsage(connection)
}

// quotaUnavailable reports a quota read that failed without changing what the
// caller answers.
func (s *Server) quotaUnavailable(err error) {
	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	log.Warn("shared provider quota state is unavailable", "error", err)
}

func (s *Server) slots(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	current, err := visibleProvider(r.Context(), s.Access.Pool, p, id)
	if err != nil {
		return access.Reply{}, err
	}
	return s.slotList(r.Context(), s.Access.Pool, current)
}

func (in slotInput) normalized(slotID string) (slotInput, error) {
	id, err := slotPathID(in.ID, slotID)
	if err != nil {
		return slotInput{}, err
	}
	if err := access.ValidText("slot.name", in.Name, 100); err != nil {
		return slotInput{}, err
	}
	if in.Priority != nil && (*in.Priority < 0 || *in.Priority > 32767) {
		return slotInput{}, access.Invalid("slot.priority", "Use a priority from 0 to 32767.")
	}
	if in.Weight != nil && (*in.Weight < 1 || *in.Weight > 1000000) {
		return slotInput{}, access.Invalid("slot.weight", "Use a weight from 1 to 1000000.")
	}
	if len(in.AllowedAPIKeys) > 100 || len(in.AllowedRoutes) > 100 || len(in.AllowedModels) > 2000 {
		return slotInput{}, access.Invalid("slot", "Use at most 100 API keys, 100 routes, and 2000 models per slot.")
	}
	keys, err := apiKeyIDs(in.AllowedAPIKeys)
	if err != nil {
		return slotInput{}, err
	}
	for _, route := range in.AllowedRoutes {
		if !routeidentity.ValidSlug(route) {
			return slotInput{}, access.Invalid("slot.allowed_routes", "Use route slugs.")
		}
	}
	for _, model := range in.AllowedModels {
		if err := ValidModelName("slot.allowed_models", model); err != nil {
			return slotInput{}, err
		}
	}
	if err := connection.ValidLimits("slot", in.limits()); err != nil {
		return slotInput{}, err
	}
	versionID, err := credentialVersionID(in.CredentialVersionID)
	if err != nil {
		return slotInput{}, err
	}
	normalized := in
	normalized.ID, normalized.AllowedAPIKeys, normalized.CredentialVersionID = id, keys, versionID
	return normalized, nil
}

func slotPathID(given, path string) (string, error) {
	if given == "" {
		return "", nil
	}
	id, err := access.ParseUUID(given)
	if err != nil || (id != "00000000-0000-0000-0000-000000000000" && id != path) {
		return "", access.Invalid("slot.id", "The slot id must match the path.")
	}
	return id, nil
}

func apiKeyIDs(keys []string) ([]string, error) {
	if keys == nil {
		return nil, nil
	}
	ids := make([]string, len(keys))
	for i, key := range keys {
		id, err := access.ParseUUID(key)
		if err != nil {
			return nil, access.Invalid("slot.allowed_api_keys", "Use API key identifiers.")
		}
		ids[i] = id
	}
	return ids, nil
}

func credentialVersionID(given *string) (*string, error) {
	if given == nil {
		return nil, nil
	}
	id, err := access.ParseUUID(*given)
	if err != nil {
		return nil, access.Invalid("slot.credential_version_id", "Use a credential identifier.")
	}
	return &id, nil
}

func (s *Server) writeSlot(r *http.Request, _ access.Principal) (access.Reply, error) {
	a := s.Access
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	slotID, err := access.IDParam(r, "slot_id")
	if err != nil {
		return access.Reply{}, err
	}
	var request slotWrite
	if err = access.DecodeUnique(r, &request, 1<<20); err != nil {
		return access.Reply{}, err
	}
	slot, err := request.Slot.normalized(slotID)
	if err != nil {
		return access.Reply{}, err
	}
	input := slotWrite{Slot: slot, Credential: request.Credential, CredentialReference: request.CredentialReference}
	source, err := decodeCredential(input.Credential, input.CredentialReference)
	if err != nil {
		return access.Reply{}, err
	}
	if _, external := source.(externalCredential); external && slot.CredentialVersionID != nil {
		return access.Reply{}, access.Invalid("credential_reference", "Choose a new reference or an existing credential version.")
	}
	return access.Mutate(a, r, input, referencing(r.Context(), id, source), func(tx pgx.Tx, p access.Principal, current *record) (access.Reply, error) {
		slots, err := loadSlots(r.Context(), tx, id)
		if err != nil {
			return access.Reply{}, err
		}
		existing := findSlot(slots, slotID)
		if err = matchSlot(r, current, existing); err != nil {
			return access.Reply{}, err
		}
		if err := s.saveDraftSlot(r.Context(), tx, current, slotID, slot, source, existing, len(slots)); err != nil {
			return access.Reply{}, err
		}
		if err = access.Audit(r.Context(), tx, r, p.Actor(), "provider.slot.update", "provider_slot", slotID, "success"); err != nil {
			return access.Reply{}, err
		}
		updated, err := readProvider(r.Context(), tx, id)
		if err != nil {
			return access.Reply{}, err
		}
		result, err := s.slotList(r.Context(), tx, updated)
		if err != nil {
			return access.Reply{}, err
		}
		result.ParentMutation = &access.ParentMutation{Previous: current.ETag, Current: updated.ETag}
		return result, nil
	})
}

func (s *Server) validateSlot(r *http.Request, p access.Principal) (access.Reply, error) {
	a := s.Access
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	slotID, err := access.IDParam(r, "slot_id")
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := a.Pool.Begin(r.Context())
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	current, err := readProvider(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(current.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	slots, err := loadSlots(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	slot := findSlot(slots, slotID)
	if slot == nil {
		return access.Reply{}, pgx.ErrNoRows
	}
	models, err := enabledModels(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	var credential []byte
	if current.Connection.Document().CredentialRequired() {
		if slot.CredentialID == nil {
			return access.Reply{}, access.Fail(422, "credential_required", "This slot has no credential to validate.")
		}
		if slot.CredentialRevoked {
			return access.Reply{}, access.Fail(422, "credential_revoked", "This slot references a revoked credential.")
		}
		if slot.CredentialLapsed {
			return access.Reply{}, access.Fail(422, "credential_lapsed", "This slot's grant lapsed. Re-enroll its grant.")
		}
		if err = slot.credentialFits(current.Connection.Document()); err != nil {
			return access.Reply{}, err
		}
	}
	if err = tx.Rollback(r.Context()); err != nil {
		return access.Reply{}, err
	}
	if current.Connection.Document().CredentialRequired() {
		credential, err = s.readCredential(r.Context(), a.Pool, *slot.CredentialID)
		if err != nil {
			return access.Reply{}, err
		}
		defer clear(credential)
	}
	probeErr := s.validateModelAccess(r.Context(), current.target(credential), *slot, models)
	// Probes run without mutation locks. Only save evidence if its complete
	// input (credential, transport, restrictions, and models) is still current.
	result, err := access.Transact(a, r, unchangedSince(r.Context(), current, "validation"), func(tx pgx.Tx, p access.Principal, locked *record) (access.Reply, error) {
		if err := s.storeSlotValidation(r.Context(), tx, current, slot, models, probeErr == nil); err != nil {
			return access.Reply{}, err
		}
		if err := access.Audit(r.Context(), tx, r, p.Actor(), "provider.slot.validate", "provider_slot", slotID, auditOutcome(probeErr == nil)); err != nil {
			return access.Reply{}, err
		}
		return s.slotList(r.Context(), tx, locked)
	})
	if err != nil {
		return access.Reply{}, err
	}
	if probeErr != nil {
		return result, access.Fail(422, "slot_validation_failed", probe.Classify(probeErr).Detail)
	}
	s.clearValidatedCooldowns(r.Context(), id, *slot)
	return result, nil
}

func (s *Catalogue) saveDraftSlot(ctx context.Context, tx pgx.Tx, current *record, slotID string, slot slotInput, source credentialSource, existing *slotRow, slotCount int) error {
	id := current.ID
	if existing == nil && slotCount >= maxSlots {
		return access.Fail(422, "slot_limit", "A connection can hold at most 64 credential slots.")
	}
	enabled, priority, weight := valueOr(slot.Enabled, true), valueOr(slot.Priority, 0), valueOr(slot.Weight, 1)
	credentialID, err := s.slotCredential(ctx, tx, current, slot, source, existing)
	if err != nil {
		return err
	}
	restrictions, _ := json.Marshal(SlotRestrictions{AllowedAPIKeys: orEmpty(slot.AllowedAPIKeys), AllowedModels: orEmpty(slot.AllowedModels), AllowedRoutes: orEmpty(slot.AllowedRoutes)})
	limits, _ := json.Marshal(slot.limits())
	if existing == nil {
		if _, err = tx.Exec(ctx, "INSERT INTO olp.provider_slots(id,provider_id,is_default,position,name,enabled,priority,weight,credential_id,restrictions,limits) VALUES($1,$2,false,(SELECT coalesce(max(position),0)+1 FROM olp.provider_slots WHERE provider_id=$2),$3,$4,$5,$6,$7,$8,$9)", slotID, id, slot.Name, enabled, priority, weight, credentialID, restrictions, limits); err != nil {
			return err
		}
	} else {
		keepValidation := deref(existing.CredentialID) == deref(credentialID)
		if _, err = tx.Exec(ctx, "UPDATE olp.provider_slots SET name=$3,enabled=$4,priority=$5,weight=$6,credential_id=$7,restrictions=$8,limits=$9,validated_at=CASE WHEN $10 THEN validated_at END,validated_fingerprint=CASE WHEN $10 THEN validated_fingerprint END WHERE id=$1 AND provider_id=$2", slotID, id, slot.Name, enabled, priority, weight, credentialID, restrictions, limits, keepValidation); err != nil {
			return err
		}
	}
	_, err = touchSlots(ctx, tx, id)
	return err
}

func (s *Catalogue) slotCredential(ctx context.Context, tx pgx.Tx, current *record, slot slotInput, source credentialSource, existing *slotRow) (*string, error) {
	switch {
	case source != nil:
		if !current.Connection.Document().CredentialRequired() {
			return nil, access.Fail(422, "credential_forbidden", "This authentication mode takes no stored credential.")
		}
		if current.Connection.Document().Grant() {
			return nil, access.Fail(422, "credential_forbidden", grantEnrollmentOnly)
		}
		stored, _, err := s.storeCredential(ctx, tx, current.ID, source)
		return &stored, err
	case slot.CredentialVersionID != nil:
		var revoked, lapsed bool
		err := tx.QueryRow(ctx, "SELECT c.revoked_at IS NOT NULL,g.lapsed_at IS NOT NULL FROM olp.provider_credentials c LEFT JOIN olp.provider_grants g ON g.credential_id=c.id WHERE c.id=$1 AND c.provider_id=$2", *slot.CredentialVersionID, current.ID).Scan(&revoked, &lapsed)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil, access.Invalid("slot.credential_version_id", "Unknown credential for this connection.")
		case err != nil:
			return nil, err
		case revoked:
			return nil, access.Fail(422, "credential_revoked", "That credential version is revoked.")
		case lapsed:
			return nil, access.Fail(422, "credential_lapsed", "That credential version's grant lapsed. Enroll a new grant.")
		}
		return slot.CredentialVersionID, nil
	case existing != nil:
		return existing.CredentialID, nil
	}
	return nil, nil
}

type credentialRotation struct {
	ID      string
	Version int
	ETag    string
}

func (s *Catalogue) rotateCredential(ctx context.Context, tx pgx.Tx, current *record, models []storedModel, defaultSlot slotRow, validatedAt *time.Time, source credentialSource) (credentialRotation, error) {
	id := current.ID
	credentialID, version, err := s.storeCredential(ctx, tx, id, source)
	if err != nil {
		return credentialRotation{}, err
	}
	fingerprint := defaultSlot.withCredential(credentialID).validationFingerprint(current.Connection.Document(), models)
	if _, err = tx.Exec(ctx, "UPDATE olp.provider_slots SET credential_id=$2,validated_at=$4,validated_fingerprint=$3 WHERE provider_id=$1 AND is_default", id, credentialID, fingerprint, validatedAt); err != nil {
		return credentialRotation{}, err
	}
	etag, err := touchSlots(ctx, tx, id)
	if err != nil {
		return credentialRotation{}, err
	}
	return credentialRotation{ID: credentialID, Version: version, ETag: etag}, nil
}

type revokedCredential struct {
	Version    int
	Generation access.AuthorityGeneration
	ETag       string
}

func (s *Catalogue) revokeCredential(ctx context.Context, tx pgx.Tx, current *record, credentialID string) (revokedCredential, error) {
	var version int
	err := tx.QueryRow(ctx, "UPDATE olp.provider_credentials SET revoked_at=now() WHERE id=$1 AND provider_id=$2 AND revoked_at IS NULL RETURNING version", credentialID, current.ID).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.provider_credentials WHERE id=$1 AND provider_id=$2)", credentialID, current.ID).Scan(&exists); err != nil {
			return revokedCredential{}, err
		}
		if !exists {
			return revokedCredential{}, pgx.ErrNoRows
		}
		return revokedCredential{}, access.Fail(409, "already_revoked", "This credential version is already revoked.")
	}
	if err != nil {
		return revokedCredential{}, err
	}
	if err = grants.Revoke(ctx, tx, credentialID); err != nil {
		return revokedCredential{}, err
	}
	generation, err := access.AdvanceAuthority(ctx, tx)
	if err != nil {
		return revokedCredential{}, err
	}
	var referenced bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.provider_slots WHERE provider_id=$1 AND credential_id=$2)", current.ID, credentialID).Scan(&referenced); err != nil {
		return revokedCredential{}, err
	}
	etag := access.NewID()
	if _, err = tx.Exec(ctx, "UPDATE olp.providers SET etag=$2,slots_etag=$3,draft_dirty=draft_dirty OR $4,updated_at=now() WHERE id=$1", current.ID, etag, access.NewID(), referenced); err != nil {
		return revokedCredential{}, err
	}
	return revokedCredential{Version: version, Generation: generation, ETag: etag}, nil
}

func (s *Catalogue) storeSlotValidation(ctx context.Context, tx pgx.Tx, current *record, slot *slotRow, models []storedModel, succeeded bool) error {
	slotID := slot.ID
	var validatedAt *time.Time
	var fingerprint *string
	if succeeded {
		validatedAt = new(time.Now().UTC())
		fingerprint = new(slot.validationFingerprint(current.Connection.Document(), models))
	}
	if _, err := tx.Exec(ctx, "UPDATE olp.provider_slots SET validated_at=$2,validated_fingerprint=$3 WHERE id=$1", slotID, validatedAt, fingerprint); err != nil {
		return err
	}
	return nil
}
