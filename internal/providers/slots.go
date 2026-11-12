package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/grants"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routeidentity"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secretstore"
)

type slotRow struct {
	ID                string
	ETag              string
	Default           bool
	Position          int
	Name              string
	Enabled           bool
	Priority          int
	Weight            int64
	CredentialID      *string
	CredentialVersion *int
	CredentialRevoked bool
	// CredentialPlugin is the digest of the plugin build whose grant
	// enrollment created the credential version, or "" for a pasted one.
	CredentialPlugin    string
	CredentialProfile   string
	CredentialLapsed    bool
	GrantGeneration     int64
	CredentialPrincipal string
	// CredentialFacts are what the credential version reported serving as at
	// enrollment, beyond its principal.
	CredentialFacts      map[string]string
	Restrictions         SlotRestrictions
	Limits               connection.Limits
	ValidatedAt          *time.Time
	ValidatedFingerprint *string
}

type SlotRestrictions struct {
	AllowedAPIKeys []string `json:"allowed_api_keys"`
	AllowedModels  []string `json:"allowed_models"`
	AllowedRoutes  []string `json:"allowed_routes"`
}

func findSlot(slots []slotRow, id string) *slotRow {
	index := slices.IndexFunc(slots, func(row slotRow) bool { return row.ID == id })
	if index < 0 {
		return nil
	}
	return &slots[index]
}

func loadSlots(ctx context.Context, q access.Queryer, providerID string) ([]slotRow, error) {
	rows, err := q.Query(ctx, "SELECT s.id::text,s.is_default,s.position,s.name,s.enabled,s.priority,s.weight,s.credential_id::text,c.version,c.revoked_at IS NOT NULL,coalesce(c.plugin_digest,''),coalesce(c.profile_id,''),g.lapsed_at IS NOT NULL,coalesce(g.generation,0),coalesce(c.principal,''),coalesce(c.grant_facts,'{}'),s.restrictions,s.limits,s.validated_at,s.validated_fingerprint,s.etag::text FROM olp.provider_slots s LEFT JOIN olp.provider_credentials c ON c.id=s.credential_id LEFT JOIN olp.provider_grants g ON g.credential_id=s.credential_id WHERE s.provider_id=$1 ORDER BY s.position", providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var slots []slotRow
	for rows.Next() {
		var row slotRow
		var facts, restrictions, limits []byte
		var revoked *bool
		if err = rows.Scan(&row.ID, &row.Default, &row.Position, &row.Name, &row.Enabled, &row.Priority, &row.Weight, &row.CredentialID, &row.CredentialVersion, &revoked, &row.CredentialPlugin, &row.CredentialProfile, &row.CredentialLapsed, &row.GrantGeneration, &row.CredentialPrincipal, &facts, &restrictions, &limits, &row.ValidatedAt, &row.ValidatedFingerprint, &row.ETag); err != nil {
			return nil, err
		}
		row.CredentialRevoked = revoked != nil && *revoked
		if err = json.Unmarshal(facts, &row.CredentialFacts); err == nil {
			err = json.Unmarshal(restrictions, &row.Restrictions)
		}
		if err == nil {
			err = json.Unmarshal(limits, &row.Limits)
		}
		if err != nil {
			return nil, err
		}
		slots = append(slots, row)
	}
	return slots, rows.Err()
}

const maxSlots = 64

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

type slotReply struct {
	ID                  string          `json:"id"`
	Name                string          `json:"name"`
	Enabled             bool            `json:"enabled"`
	Priority            int             `json:"priority"`
	Weight              int64           `json:"weight"`
	CredentialVersionID *string         `json:"credential_version_id"`
	AllowedAPIKeys      []string        `json:"allowed_api_keys"`
	AllowedModels       []string        `json:"allowed_models"`
	AllowedRoutes       []string        `json:"allowed_routes"`
	MaxConcurrency      *int64          `json:"max_concurrency"`
	RequestsPerMinute   *int64          `json:"requests_per_minute"`
	TokensPerMinute     *int64          `json:"tokens_per_minute"`
	DailyCostLimit      *string         `json:"daily_cost_limit"`
	MonthlyCostLimit    *string         `json:"monthly_cost_limit"`
	PriorityShares      *routing.Shares `json:"priority_shares"`
	SaturationPercent   *int64          `json:"saturation_percent"`
}

func (row slotRow) reply() slotReply {
	return slotReply{
		ID: row.ID, Name: row.Name, Enabled: row.Enabled, Priority: row.Priority, Weight: row.Weight,
		CredentialVersionID: row.CredentialID,
		AllowedAPIKeys:      orEmpty(row.Restrictions.AllowedAPIKeys), AllowedModels: orEmpty(row.Restrictions.AllowedModels), AllowedRoutes: orEmpty(row.Restrictions.AllowedRoutes),
		MaxConcurrency: row.Limits.MaxConcurrency, RequestsPerMinute: row.Limits.RequestsPerMinute, TokensPerMinute: row.Limits.TokensPerMinute,
		DailyCostLimit: row.Limits.DailyCostLimit, MonthlyCostLimit: row.Limits.MonthlyCostLimit, PriorityShares: row.Limits.PriorityShares, SaturationPercent: row.Limits.SaturationPercent,
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
	live, connection := s.quotas(ctx, current.ID, slots, activeCredentials, generations)
	items := make([]slotReply, len(slots))
	health := make(map[string]slotHealth, len(slots))
	for i, row := range slots {
		items[i] = row.reply()
		quota := live[row.ID]
		health[row.ID] = slotHealth{
			Revoked: row.CredentialRevoked, Lapsed: row.CredentialLapsed, ActiveCredentialVersionID: activeCredentials[row.ID],
			CoolingDown: quota.coolingDown, ValidatedAt: row.validationTime(current.Connection.Document(), models), Usage: quota.usage,
		}
	}
	return access.Detail(slotList{Items: items, Health: health, ETag: current.SlotsETag, ConnectionUsage: connection}, current.SlotsETag), nil
}

type slotList struct {
	Items           []slotReply           `json:"items"`
	Health          map[string]slotHealth `json:"health"`
	ETag            string                `json:"etag"`
	ConnectionUsage *quotaUsage           `json:"connection_usage"`
}

type slotHealth struct {
	Revoked                   bool        `json:"revoked"`
	Lapsed                    bool        `json:"lapsed"`
	ActiveCredentialVersionID *string     `json:"active_credential_version_id"`
	CoolingDown               *bool       `json:"cooling_down"`
	ValidatedAt               *time.Time  `json:"validated_at"`
	Usage                     *quotaUsage `json:"usage"`
}

// A slow quota store costs the slot list its live counters, never the list.
const quotaTimeout = 2 * time.Second

type quotaUsage struct {
	RequestsThisMinute int64 `json:"requests_this_minute"`
	TokensThisMinute   int64 `json:"tokens_this_minute"`
	ConcurrentRequests int64 `json:"concurrent_requests"`
}

func usageOf(u fleet.Usage) *quotaUsage {
	return &quotaUsage{RequestsThisMinute: u.RequestsThisMinute, TokensThisMinute: u.TokensThisMinute, ConcurrentRequests: u.ConcurrentRequests}
}

type slotQuota struct {
	usage       *quotaUsage
	coolingDown *bool
}

// A counter that cannot be read stays nil, so the console shows it as unknown
// instead of an idle zero or a failed request.
func (s *Server) quotas(ctx context.Context, providerID string, slots []slotRow, published map[string]*string, generations map[string]int64) (map[string]slotQuota, *quotaUsage) {
	if s.quota == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, quotaTimeout)
	defer cancel()
	connection, err := s.quota.Usage(ctx, limits.ConnectionLookup(providerID))
	if err != nil {
		s.quotaUnavailable(err)
		return nil, nil
	}
	live := make(map[string]slotQuota, len(slots))
	for _, row := range slots {
		usage, err := s.quota.Usage(ctx, limits.SlotLookup(row.ID))
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
		cooling, err := s.quota.Cooling(ctx, fleet.CredentialScope(providerID, published[row.ID], generation), fleet.SlotScope(row.ID))
		if err != nil {
			s.quotaUnavailable(err)
			break
		}
		live[row.ID] = slotQuota{usage: usageOf(usage), coolingDown: &cooling}
	}
	return live, usageOf(connection)
}

func (s *Server) quotaUnavailable(err error) {
	s.log.Warn("shared provider quota state is unavailable", "error", err)
}

func (s *Server) slots(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	current, err := visibleProvider(r.Context(), s.access.Pool, p, id)
	if err != nil {
		return access.Reply{}, err
	}
	return s.slotList(r.Context(), s.access.Pool, current)
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
		if err := connection.ValidModelName("slot.allowed_models", model); err != nil {
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
	a := s.access
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

func (s *Server) saveDraftSlot(ctx context.Context, tx pgx.Tx, current *record, slotID string, slot slotInput, source credentialSource, existing *slotRow, slotCount int) error {
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

func (s *Server) slotCredential(ctx context.Context, tx pgx.Tx, current *record, slot slotInput, source credentialSource, existing *slotRow) (*string, error) {
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

// Pool edits keep their collection precondition; individual edits can use the
// slot precondition without borrowing authority over unrelated pool changes.
func matchSlot(r *http.Request, current *record, slot *slotRow) error {
	if r.Header.Get("If-Match") == `"`+current.SlotsETag+`"` || slot == nil {
		return access.Match(r, current.SlotsETag)
	}
	return access.Match(r, slot.ETag)
}

func (s *Server) getSlot(r *http.Request, p access.Principal) (access.Reply, error) {
	providerID, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	slotID, err := access.IDParam(r, "slot_id")
	if err != nil {
		return access.Reply{}, err
	}
	if _, err = visibleProvider(r.Context(), s.access.Pool, p, providerID); err != nil {
		return access.Reply{}, err
	}
	rows, err := loadSlots(r.Context(), s.access.Pool, providerID)
	if err != nil {
		return access.Reply{}, err
	}
	slot := findSlot(rows, slotID)
	if slot == nil {
		return access.Reply{}, access.Fail(404, "slot_not_found", "The credential slot is unavailable.")
	}
	return access.Detail(struct {
		slotReply
		ProviderID string `json:"provider_id"`
		IsDefault  bool   `json:"is_default"`
		ETag       string `json:"etag"`
	}{slot.reply(), providerID, slot.Default, slot.ETag}, slot.ETag), nil
}

func (s *Server) deleteSlot(r *http.Request, _ access.Principal) (access.Reply, error) {
	providerID, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	slotID, err := access.IDParam(r, "slot_id")
	if err != nil {
		return access.Reply{}, err
	}
	input := map[string]string{"provider_id": providerID, "slot_id": slotID}
	return access.Mutate(s.access, r, input, changeable(r.Context(), providerID), func(tx pgx.Tx, p access.Principal, current *record) (access.Reply, error) {
		rows, err := loadSlots(r.Context(), tx, providerID)
		if err != nil {
			return access.Reply{}, err
		}
		slot := findSlot(rows, slotID)
		if slot == nil {
			return access.Reply{}, access.Fail(404, "slot_not_found", "The credential slot is unavailable.")
		}
		if err = matchSlot(r, current, slot); err != nil {
			return access.Reply{}, err
		}
		etag, err := s.deleteDraftSlot(r.Context(), tx, providerID, slotID, slot)
		if err != nil {
			return access.Reply{}, err
		}
		if err = access.Audit(r.Context(), tx, r, p.Actor(), "provider.slot.delete", "provider_slot", slotID, "success"); err != nil {
			return access.Reply{}, err
		}
		return access.Reply{Status: http.StatusNoContent, ParentMutation: &access.ParentMutation{Previous: current.ETag, Current: etag}}, nil
	})
}

func (s *Server) deleteDraftSlot(ctx context.Context, tx pgx.Tx, providerID, slotID string, slot *slotRow) (string, error) {
	var err error
	if slot.Default {
		return "", access.Fail(409, "default_slot_required", "The default credential slot cannot be deleted.")
	}
	if err = grants.DiscardSlotEnrollments(ctx, tx, providerID, slotID); err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, "DELETE FROM olp.provider_slots WHERE id=$1 AND provider_id=$2", slotID, providerID); err != nil {
		return "", err
	}
	return touchSlots(ctx, tx, providerID)
}
