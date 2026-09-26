package providers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/runtime"
)

const maxSlots = 64

func (s *Server) credentials(r *http.Request) (access.Reply, error) {
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
	rows, err := s.Access.Pool.Query(r.Context(), `SELECT jsonb_build_object(
		'id',c.id,'version',c.version,
		'active',EXISTS(SELECT 1 FROM jsonb_array_elements(r.slots) slot WHERE slot->>'credential_id'=c.id::text),
		'draft_selected',EXISTS(SELECT 1 FROM olp.provider_slots d WHERE d.provider_id=p.id AND d.credential_id=c.id),
		'created_at',c.created_at,'revoked_at',c.revoked_at)
		FROM olp.provider_credentials c JOIN olp.providers p ON p.id=c.provider_id
		LEFT JOIN olp.provider_revisions r ON r.id=p.active_revision_id
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
	Credential string `json:"credential"`
}

// rotate validates a new credential against the upstream, records it as the
// next version, and selects it for the draft. Published revisions keep the
// version they were activated with.
func (s *Server) rotate(r *http.Request) (access.Reply, error) {
	a := s.Access
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input rotateRequest
	if err = access.DecodeUnique(r, &input, 1<<20); err != nil {
		return access.Reply{}, err
	}
	if err = ValidCredential(input.Credential); err != nil {
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
	_, replayed, err := a.Replay(r, tx, p, input)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	current, err := load(r.Context(), tx, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	if err := access.ProjectAccess(p, current.ProjectID, true); err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, current.ETag); err != nil {
		return access.Reply{}, err
	}
	if !current.Configuration.CredentialRequired() {
		return access.Reply{}, access.Fail(422, "credential_forbidden", "This authentication mode takes no stored credential.")
	}
	if err = current.Configuration.Validate(s.Egress); err != nil {
		return access.Reply{}, err
	}
	models, err := loadModels(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	slots, err := loadSlots(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	var defaultSlot slotRow
	for _, row := range slots {
		if row.Default {
			defaultSlot = row
		}
	}
	// Do not hold the installation mutation lock during the upstream probe.
	// The commit transaction rechecks authority, replay, and the draft ETag.
	if err = tx.Rollback(r.Context()); err != nil {
		return access.Reply{}, err
	}
	for _, model := range models {
		current.Configuration.ProbeModels = append(current.Configuration.ProbeModels, model.UpstreamModel)
	}
	if _, err = s.listModels(r.Context(), &current.Configuration, []byte(input.Credential)); err != nil {
		return access.Reply{}, access.Fail(422, "credential_invalid", "The new credential was not accepted by the upstream: "+classify(err).Detail)
	}
	var validatedAt *time.Time
	if defaultSlot.validationFingerprint(&current.Configuration, models) != "" {
		if err = s.validateModelAccess(r.Context(), &current.Configuration, []byte(input.Credential), &defaultSlot, models); err != nil {
			return access.Reply{}, access.Fail(422, "credential_invalid", "The new credential cannot access the enabled models: "+classify(err).Detail)
		}
		validatedAt = new(time.Now().UTC())
	}
	tx, err = a.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err = a.Principal(r, tx, "configure")
	if err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := a.Replay(r, tx, p, input)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	locked, err := load(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	if locked.ETag != current.ETag {
		return access.Reply{}, access.Fail(412, "etag_mismatch", "The connection changed during validation; reload and retry.")
	}
	credentialID, version, err := s.StoreCredential(r.Context(), tx, id, input.Credential)
	if err != nil {
		return access.Reply{}, err
	}
	defaultSlot.CredentialID = &credentialID
	fingerprint := defaultSlot.validationFingerprint(&current.Configuration, models)
	if _, err = tx.Exec(r.Context(), "UPDATE olp.provider_slots SET credential_id=$2,validated_at=$4,validated_fingerprint=$3 WHERE provider_id=$1 AND is_default", id, credentialID, fingerprint, validatedAt); err != nil {
		return access.Reply{}, err
	}
	etag, err := touch(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "UPDATE olp.providers SET slots_etag=$2 WHERE id=$1", id, access.NewID()); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.ID, "provider.credential.rotate", "provider_credential", credentialID, "success"); err != nil {
		return access.Reply{}, err
	}
	result := access.Reply{Status: 201, ETag: etag, Body: map[string]any{"provider_id": id, "etag": etag, "credential_id": credentialID, "credential_version": version, "runtime_generation": nil}}
	if err = a.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}

// revoke marks a credential version unusable everywhere: drafts, published
// revisions, and gateways that learn it through the authority refresh.
func (s *Server) revoke(r *http.Request) (access.Reply, error) {
	return s.mutation(r, "provider.credential.revoke", func(ctx context.Context, tx pgx.Tx, p access.Principal, current *record) (access.Reply, error) {
		credentialID, err := access.IDParam(r, "credential_id")
		if err != nil {
			return access.Reply{}, err
		}
		var version int
		err = tx.QueryRow(ctx, "UPDATE olp.provider_credentials SET revoked_at=now() WHERE id=$1 AND provider_id=$2 AND revoked_at IS NULL RETURNING version", credentialID, current.ID).Scan(&version)
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.provider_credentials WHERE id=$1 AND provider_id=$2)", credentialID, current.ID).Scan(&exists); err != nil {
				return access.Reply{}, err
			}
			if !exists {
				return access.Reply{}, pgx.ErrNoRows
			}
			return access.Reply{}, access.Fail(409, "already_revoked", "This credential version is already revoked.")
		}
		if err != nil {
			return access.Reply{}, err
		}
		generation, err := access.AdvanceAuthority(r, tx)
		if err != nil {
			return access.Reply{}, err
		}
		var referenced bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.provider_slots WHERE provider_id=$1 AND credential_id=$2)", current.ID, credentialID).Scan(&referenced); err != nil {
			return access.Reply{}, err
		}
		etag := access.NewID()
		if _, err = tx.Exec(ctx, "UPDATE olp.providers SET etag=$2,slots_etag=$3,draft_dirty=draft_dirty OR $4,updated_at=now() WHERE id=$1", current.ID, etag, access.NewID(), referenced); err != nil {
			return access.Reply{}, err
		}
		return access.Detail(map[string]any{"provider_id": current.ID, "etag": etag, "credential_id": credentialID, "credential_version": version, "runtime_generation": generation}, etag), nil
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
}

type slotWrite struct {
	Slot       slotInput `json:"slot"`
	Credential *string   `json:"credential"`
}

func slotJSON(row slotRow) map[string]any {
	return map[string]any{
		"id": row.ID, "name": row.Name, "enabled": row.Enabled, "priority": row.Priority, "weight": row.Weight,
		"credential_version_id": row.CredentialID,
		"allowed_api_keys":      orEmpty(row.Restrictions.AllowedAPIKeys), "allowed_models": orEmpty(row.Restrictions.AllowedModels), "allowed_routes": orEmpty(row.Restrictions.AllowedRoutes),
		"max_concurrency": row.Limits.MaxConcurrency, "requests_per_minute": row.Limits.RequestsPerMinute, "tokens_per_minute": row.Limits.TokensPerMinute,
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
		var published []runtime.RevisionSlot
		if err = json.Unmarshal(encoded, &published); err != nil {
			return access.Reply{}, err
		}
		for _, slot := range published {
			activeCredentials[slot.ID] = slot.CredentialID
		}
	}
	models, err := loadModels(ctx, q, current.ID, true)
	if err != nil {
		return access.Reply{}, err
	}
	items := make([]map[string]any, 0, len(slots))
	health := map[string]any{}
	for _, row := range slots {
		items = append(items, slotJSON(row))
		validated := row.validationTime(&current.Configuration, models)
		health[row.ID] = map[string]any{"revoked": row.CredentialRevoked, "active_credential_version_id": activeCredentials[row.ID], "cooling_down": nil, "validated_at": validated, "usage": nil}
	}
	connection := s.quotas(ctx, current.ID, slots, activeCredentials, health)
	return access.Detail(map[string]any{"items": items, "health": health, "etag": current.SlotsETag, "connection_usage": connection}, current.SlotsETag), nil
}

// quotaTimeout bounds one console read of shared quota state. Valkey holds no
// part of the stored slot list, so a slow store costs the page its live
// counters rather than the page itself.
const quotaTimeout = 2 * time.Second

// quotaUsage renders one shared usage snapshot as the management contract's
// ProviderQuotaUsage.
func quotaUsage(u limits.Usage) map[string]any {
	return map[string]any{"requests_this_minute": u.RequestsThisMinute, "tokens_this_minute": u.TokensThisMinute, "concurrent_requests": u.ConcurrentRequests}
}

// quotas fills the live per-slot counters into health and returns the counters
// of the shared provider connection. Every field it cannot read stays null:
// the stored list is complete without them, so an unreachable or malformed
// quota store is reported to the operator and shown to the console as unknown,
// never as an idle zero and never as a failed request.
func (s *Server) quotas(ctx context.Context, providerID string, slots []slotRow, published map[string]*string, health map[string]any) any {
	if s.Quotas == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, quotaTimeout)
	defer cancel()
	connection, err := s.Quotas.ProviderUsage(ctx, limits.ConnectionLookup(providerID))
	if err != nil {
		s.quotaUnavailable(err)
		return nil
	}
	for _, row := range slots {
		entry, ok := health[row.ID].(map[string]any)
		if !ok {
			continue
		}
		usage, err := s.Quotas.ProviderUsage(ctx, limits.SlotLookup(row.ID))
		if err != nil {
			s.quotaUnavailable(err)
			break
		}
		// The cooldown is asked of the credential version the published
		// revision dispatches with, because that is the scope the gateway
		// penalises, and of the slot itself.
		cooling, err := s.Quotas.Cooling(ctx, limits.CredentialScope(providerID, published[row.ID]), limits.SlotScope(row.ID))
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

func (s *Server) slots(r *http.Request) (access.Reply, error) {
	p, err := s.Access.Principal(r, s.Access.Pool, "read")
	if err != nil {
		return access.Reply{}, err
	}
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	current, err := checkProvider(r.Context(), s.Access.Pool, p, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	return s.slotList(r.Context(), s.Access.Pool, current)
}

func validSlot(in *slotInput, slotID string) error {
	if in.ID != "" {
		id, err := access.ParseUUID(in.ID)
		if err != nil || (id != "00000000-0000-0000-0000-000000000000" && id != slotID) {
			return access.Invalid("slot.id", "The slot id must match the path.")
		}
		in.ID = id
	}
	if err := access.ValidText("slot.name", in.Name, 100); err != nil {
		return err
	}
	if in.Priority != nil && (*in.Priority < 0 || *in.Priority > 32767) {
		return access.Invalid("slot.priority", "Use a priority from 0 to 32767.")
	}
	if in.Weight != nil && (*in.Weight < 1 || *in.Weight > 1000000) {
		return access.Invalid("slot.weight", "Use a weight from 1 to 1000000.")
	}
	if len(in.AllowedAPIKeys) > 100 || len(in.AllowedRoutes) > 100 || len(in.AllowedModels) > 2000 {
		return access.Invalid("slot", "Use at most 100 API keys, 100 routes, and 2000 models per slot.")
	}
	for i, key := range in.AllowedAPIKeys {
		id, err := access.ParseUUID(key)
		if err != nil {
			return access.Invalid("slot.allowed_api_keys", "Use API key identifiers.")
		}
		in.AllowedAPIKeys[i] = id
	}
	for _, route := range in.AllowedRoutes {
		if !access.RouteSlug.MatchString(route) {
			return access.Invalid("slot.allowed_routes", "Use route slugs.")
		}
	}
	for _, model := range in.AllowedModels {
		if err := ValidModelName("slot.allowed_models", model); err != nil {
			return err
		}
	}
	if !ValidQuota(Limits{MaxConcurrency: in.MaxConcurrency, RequestsPerMinute: in.RequestsPerMinute, TokensPerMinute: in.TokensPerMinute}) {
		return access.Invalid("slot", "Use positive limits: requests and concurrency at most 2147483647, tokens at most 9007199254740991.")
	}
	if in.CredentialVersionID != nil {
		id, err := access.ParseUUID(*in.CredentialVersionID)
		if err != nil {
			return access.Invalid("slot.credential_version_id", "Use a credential identifier.")
		}
		in.CredentialVersionID = &id
	}
	return nil
}

func (s *Server) writeSlot(r *http.Request) (access.Reply, error) {
	a := s.Access
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	slotID, err := access.IDParam(r, "slot_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input slotWrite
	if err = access.DecodeUnique(r, &input, 1<<20); err != nil {
		return access.Reply{}, err
	}
	if err = validSlot(&input.Slot, slotID); err != nil {
		return access.Reply{}, err
	}
	if input.Credential != nil {
		if err = ValidCredential(*input.Credential); err != nil {
			return access.Reply{}, err
		}
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
	claim, replayed, err := a.Replay(r, tx, p, input)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	current, err := load(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	if err := access.ProjectAccess(p, current.ProjectID, true); err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, current.SlotsETag); err != nil {
		return access.Reply{}, err
	}
	slots, err := loadSlots(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	var existing *slotRow
	for i := range slots {
		if slots[i].ID == slotID {
			existing = &slots[i]
		}
	}
	if existing == nil && len(slots) >= maxSlots {
		return access.Reply{}, access.Fail(422, "slot_limit", "A connection can hold at most 64 credential slots.")
	}
	enabled, priority, weight := true, 0, int64(1)
	if input.Slot.Enabled != nil {
		enabled = *input.Slot.Enabled
	}
	if input.Slot.Priority != nil {
		priority = *input.Slot.Priority
	}
	if input.Slot.Weight != nil {
		weight = *input.Slot.Weight
	}
	var credentialID *string
	switch {
	case input.Credential != nil:
		if !current.Configuration.CredentialRequired() {
			return access.Reply{}, access.Fail(422, "credential_forbidden", "This authentication mode takes no stored credential.")
		}
		stored, _, err := s.StoreCredential(r.Context(), tx, id, *input.Credential)
		if err != nil {
			return access.Reply{}, err
		}
		credentialID = &stored
	case input.Slot.CredentialVersionID != nil:
		var revoked bool
		err = tx.QueryRow(r.Context(), "SELECT revoked_at IS NOT NULL FROM olp.provider_credentials WHERE id=$1 AND provider_id=$2", *input.Slot.CredentialVersionID, id).Scan(&revoked)
		if errors.Is(err, pgx.ErrNoRows) {
			return access.Reply{}, access.Invalid("slot.credential_version_id", "Unknown credential for this connection.")
		}
		if err != nil {
			return access.Reply{}, err
		}
		if revoked {
			return access.Reply{}, access.Fail(422, "credential_revoked", "That credential version is revoked.")
		}
		credentialID = input.Slot.CredentialVersionID
	case existing != nil:
		credentialID = existing.CredentialID
	}
	restrictions, _ := json.Marshal(slotRestrictions{AllowedAPIKeys: orEmpty(input.Slot.AllowedAPIKeys), AllowedModels: orEmpty(input.Slot.AllowedModels), AllowedRoutes: orEmpty(input.Slot.AllowedRoutes)})
	limits, _ := json.Marshal(Limits{MaxConcurrency: input.Slot.MaxConcurrency, RequestsPerMinute: input.Slot.RequestsPerMinute, TokensPerMinute: input.Slot.TokensPerMinute})
	if existing == nil {
		if _, err = tx.Exec(r.Context(), "INSERT INTO olp.provider_slots(id,provider_id,is_default,position,name,enabled,priority,weight,credential_id,restrictions,limits) VALUES($1,$2,false,(SELECT coalesce(max(position),0)+1 FROM olp.provider_slots WHERE provider_id=$2),$3,$4,$5,$6,$7,$8,$9)", slotID, id, input.Slot.Name, enabled, priority, weight, credentialID, restrictions, limits); err != nil {
			return access.Reply{}, err
		}
	} else {
		keepValidation := deref(existing.CredentialID) == deref(credentialID)
		if _, err = tx.Exec(r.Context(), "UPDATE olp.provider_slots SET name=$3,enabled=$4,priority=$5,weight=$6,credential_id=$7,restrictions=$8,limits=$9,validated_at=CASE WHEN $10 THEN validated_at END,validated_fingerprint=CASE WHEN $10 THEN validated_fingerprint END WHERE id=$1 AND provider_id=$2", slotID, id, input.Slot.Name, enabled, priority, weight, credentialID, restrictions, limits, keepValidation); err != nil {
			return access.Reply{}, err
		}
	}
	if _, err = touch(r.Context(), tx, id); err != nil {
		return access.Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "UPDATE olp.providers SET slots_etag=$2 WHERE id=$1", id, access.NewID()); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.ID, "provider.slot.update", "provider_slot", slotID, "success"); err != nil {
		return access.Reply{}, err
	}
	updated, err := load(r.Context(), tx, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	result, err := s.slotList(r.Context(), tx, updated)
	if err != nil {
		return access.Reply{}, err
	}
	if err = a.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}

func (s *Server) validateSlot(r *http.Request) (access.Reply, error) {
	a := s.Access
	p, err := a.Principal(r, a.Pool, "configure")
	if err != nil {
		return access.Reply{}, err
	}
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
	current, err := load(r.Context(), tx, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	if err := access.ProjectAccess(p, current.ProjectID, true); err != nil {
		return access.Reply{}, err
	}
	slots, err := loadSlots(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	var slot *slotRow
	for i := range slots {
		if slots[i].ID == slotID {
			slot = &slots[i]
		}
	}
	if slot == nil {
		return access.Reply{}, pgx.ErrNoRows
	}
	models, err := loadModels(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	var credential []byte
	if current.Configuration.CredentialRequired() {
		if slot.CredentialID == nil {
			return access.Reply{}, access.Fail(422, "credential_required", "This slot has no credential to validate.")
		}
		if slot.CredentialRevoked {
			return access.Reply{}, access.Fail(422, "credential_revoked", "This slot references a revoked credential.")
		}
		if credential, err = a.Keys.Read(r.Context(), tx, a.Installation, *slot.CredentialID, "provider_credential"); err != nil {
			return access.Reply{}, err
		}
	}
	if err = tx.Rollback(r.Context()); err != nil {
		return access.Reply{}, err
	}
	if err = current.Configuration.Validate(s.Egress); err != nil {
		return access.Reply{}, err
	}
	probeErr := s.validateModelAccess(r.Context(), &current.Configuration, credential, slot, models)
	// Probes run without mutation locks. Only save evidence if its complete
	// input (credential, transport, restrictions, and models) is still current.
	tx, err = a.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	if _, err = a.Principal(r, tx, "configure"); err != nil {
		return access.Reply{}, err
	}
	locked, err := load(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	if locked.ETag != current.ETag {
		return access.Reply{}, access.Fail(412, "etag_mismatch", "The connection changed during validation; reload and retry.")
	}
	var validatedAt *time.Time
	var fingerprint *string
	if probeErr == nil {
		validatedAt = new(time.Now().UTC())
		fingerprint = new(slot.validationFingerprint(&current.Configuration, models))
	}
	if _, err = tx.Exec(r.Context(), "UPDATE olp.provider_slots SET validated_at=$2,validated_fingerprint=$3 WHERE id=$1", slotID, validatedAt, fingerprint); err != nil {
		return access.Reply{}, err
	}
	result, err := s.slotList(r.Context(), tx, locked)
	if err != nil {
		return access.Reply{}, err
	}
	result, err = access.Commit(r, tx, result)
	if err == nil && probeErr == nil {
		s.clearValidatedCooldowns(r.Context(), id, *slot)
	}
	if err == nil && probeErr != nil {
		err = access.Fail(422, "slot_validation_failed", classify(probeErr).Detail)
	}
	return result, err
}
