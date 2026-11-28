package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ptr"
	"github.com/tyk-swe/olp/internal/domain/routeidentity"
	"github.com/tyk-swe/olp/internal/grants"
	"github.com/tyk-swe/olp/internal/providers/connection"
)

func (in slotInput) normalized(slotID string) (slotInput, error) {
	id, err := slotPathID(in.ID, slotID)
	if err != nil {
		return slotInput{}, err
	}
	if err := httpkit.ValidText("slot.name", in.Name, 100); err != nil {
		return slotInput{}, err
	}
	if in.Priority != nil && (*in.Priority < 0 || *in.Priority > 32767) {
		return slotInput{}, httpkit.Invalid("slot.priority", "Use a priority from 0 to 32767.")
	}
	if in.Weight != nil && (*in.Weight < 1 || *in.Weight > 1000000) {
		return slotInput{}, httpkit.Invalid("slot.weight", "Use a weight from 1 to 1000000.")
	}
	if len(in.AllowedAPIKeys) > 100 || len(in.AllowedRoutes) > 100 || len(in.AllowedModels) > 2000 {
		return slotInput{}, httpkit.Invalid("slot", "Use at most 100 API keys, 100 routes, and 2000 models per slot.")
	}
	keys, err := apiKeyIDs(in.AllowedAPIKeys)
	if err != nil {
		return slotInput{}, err
	}
	for _, route := range in.AllowedRoutes {
		if !routeidentity.ValidSlug(route) {
			return slotInput{}, httpkit.Invalid("slot.allowed_routes", "Use route slugs.")
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
	id, err := httpkit.ParseUUID(given)
	if err != nil || (id != "00000000-0000-0000-0000-000000000000" && id != path) {
		return "", httpkit.Invalid("slot.id", "The slot id must match the path.")
	}
	return id, nil
}

func apiKeyIDs(keys []string) ([]string, error) {
	if keys == nil {
		return nil, nil
	}
	parsed := make([]string, len(keys))
	for i, key := range keys {
		id, err := httpkit.ParseUUID(key)
		if err != nil {
			return nil, httpkit.Invalid("slot.allowed_api_keys", "Use API key identifiers.")
		}
		parsed[i] = id
	}
	return parsed, nil
}

func credentialVersionID(given *string) (*string, error) {
	if given == nil {
		return nil, nil
	}
	id, err := httpkit.ParseUUID(*given)
	if err != nil {
		return nil, httpkit.Invalid("slot.credential_version_id", "Use a credential identifier.")
	}
	return &id, nil
}

func validateSlotCredentialSource(slot slotInput, source credentialSource) error {
	if slot.CredentialVersionID == nil || source == nil {
		return nil
	}
	switch source.(type) {
	case externalCredential:
		return httpkit.Invalid("credential_reference", "Choose a new reference or an existing credential version.")
	case pastedCredential:
		return httpkit.Invalid("credential", "Choose a new credential or an existing credential version.")
	default:
		return httpkit.Invalid("credential", "Choose a new credential or an existing credential version.")
	}
}

func (s *slotService) writeSlot(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	a := s.access
	id, err := httpkit.IDParam(r, "provider_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	slotID, err := httpkit.IDParam(r, "slot_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var request slotWrite
	if err = httpkit.DecodeUnique(r, &request, 1<<20); err != nil {
		return httpkit.Reply{}, err
	}
	slot, err := request.Slot.normalized(slotID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	input := slotWrite{Slot: slot, Credential: request.Credential, CredentialReference: request.CredentialReference}
	source, err := decodeCredential(input.Credential, input.CredentialReference)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err = validateSlotCredentialSource(slot, source); err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(a, r, input, referencing(r.Context(), id, source), func(tx pgx.Tx, p authz.Principal, current *record) (httpkit.Reply, access.Event, error) {
		slots, err := loadSlots(r.Context(), tx, id)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		existing := findSlot(slots, slotID)
		if err = matchSlot(r, current, existing); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err := s.saveDraftSlot(r.Context(), tx, current, slotID, slot, source, existing, len(slots)); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		updated, err := readProvider(r.Context(), tx, id)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		result, err := s.slotList(r.Context(), tx, updated)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		result.ParentMutation = &httpkit.ParentMutation{Previous: current.ETag, Current: updated.ETag}
		return result, access.Event{Action: "provider.slot.update", Resource: "provider_slot", ID: slotID}, nil
	})
}

func (s *slotService) saveDraftSlot(ctx context.Context, tx pgx.Tx, current *record, slotID string, slot slotInput, source credentialSource, existing *slotRow, slotCount int) error {
	id := current.ID
	if existing == nil && slotCount >= maxSlots {
		return httpkit.Fail(422, "slot_limit", "A connection can hold at most 64 credential slots.")
	}
	enabled, priority, weight := ptr.Or(slot.Enabled, true), ptr.Value(slot.Priority), ptr.Or(slot.Weight, 1)
	credentialID, err := s.slotCredential(ctx, tx, current, slot, source, existing)
	if err != nil {
		return err
	}
	restrictions, err := json.Marshal(SlotRestrictions{AllowedAPIKeys: ptr.NonNil(slot.AllowedAPIKeys), AllowedModels: ptr.NonNil(slot.AllowedModels), AllowedRoutes: ptr.NonNil(slot.AllowedRoutes)})
	if err != nil {
		return err
	}
	limits, err := json.Marshal(slot.limits())
	if err != nil {
		return err
	}
	if existing == nil {
		if _, err = tx.Exec(ctx, "INSERT INTO olp.provider_slots(id,provider_id,is_default,position,name,enabled,priority,weight,credential_id,restrictions,limits) VALUES($1,$2,false,(SELECT coalesce(max(position),0)+1 FROM olp.provider_slots WHERE provider_id=$2),$3,$4,$5,$6,$7,$8,$9)", slotID, id, slot.Name, enabled, priority, weight, credentialID, restrictions, limits); err != nil {
			return err
		}
	} else {
		keepValidation := ptr.Value(existing.CredentialID) == ptr.Value(credentialID)
		if _, err = tx.Exec(ctx, "UPDATE olp.provider_slots SET name=$3,enabled=$4,priority=$5,weight=$6,credential_id=$7,restrictions=$8,limits=$9,validated_at=CASE WHEN $10 THEN validated_at END,validated_fingerprint=CASE WHEN $10 THEN validated_fingerprint END WHERE id=$1 AND provider_id=$2", slotID, id, slot.Name, enabled, priority, weight, credentialID, restrictions, limits, keepValidation); err != nil {
			return err
		}
	}
	_, err = touchSlots(ctx, tx, id)
	return err
}

func (s *slotService) slotCredential(ctx context.Context, tx pgx.Tx, current *record, slot slotInput, source credentialSource, existing *slotRow) (*string, error) {
	switch {
	case source != nil:
		if !current.Connection.Document().CredentialRequired() {
			return nil, httpkit.Fail(422, "credential_forbidden", "This authentication mode takes no stored credential.")
		}
		if current.Connection.Document().Grant() {
			return nil, httpkit.Fail(422, "credential_forbidden", grantEnrollmentOnly)
		}
		stored, _, err := s.credentials.storeCredential(ctx, tx, current.ID, source)
		return &stored, err
	case slot.CredentialVersionID != nil:
		var revoked, lapsed bool
		err := tx.QueryRow(ctx, "SELECT c.revoked_at IS NOT NULL,g.lapsed_at IS NOT NULL FROM olp.provider_credentials c LEFT JOIN olp.provider_grants g ON g.credential_id=c.id WHERE c.id=$1 AND c.provider_id=$2", *slot.CredentialVersionID, current.ID).Scan(&revoked, &lapsed)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil, httpkit.Invalid("slot.credential_version_id", "Unknown credential for this connection.")
		case err != nil:
			return nil, err
		case revoked:
			return nil, httpkit.Fail(422, "credential_revoked", "That credential version is revoked.")
		case lapsed:
			return nil, httpkit.Fail(422, "credential_lapsed", "That credential version's grant lapsed. Enroll a new grant.")
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
		return httpkit.Match(r, current.SlotsETag)
	}
	return httpkit.Match(r, slot.ETag)
}

func (s *slotService) getSlot(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	providerID, err := httpkit.IDParam(r, "provider_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	slotID, err := httpkit.IDParam(r, "slot_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	if _, err = visibleProvider(r.Context(), s.access.Pool, p, providerID); err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := loadSlots(r.Context(), s.access.Pool, providerID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	slot := findSlot(rows, slotID)
	if slot == nil {
		return httpkit.Reply{}, httpkit.Fail(404, "slot_not_found", "The credential slot is unavailable.")
	}
	return httpkit.Detail(struct {
		slotReply
		ProviderID string `json:"provider_id"`
		IsDefault  bool   `json:"is_default"`
		ETag       string `json:"etag"`
	}{slot.reply(), providerID, slot.Default, slot.ETag}, slot.ETag), nil
}

func (s *slotService) deleteSlot(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	providerID, err := httpkit.IDParam(r, "provider_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	slotID, err := httpkit.IDParam(r, "slot_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	input := map[string]string{"provider_id": providerID, "slot_id": slotID}
	return access.Mutate(s.access, r, input, changeable(r.Context(), providerID), func(tx pgx.Tx, p authz.Principal, current *record) (httpkit.Reply, access.Event, error) {
		rows, err := loadSlots(r.Context(), tx, providerID)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		slot := findSlot(rows, slotID)
		if slot == nil {
			return httpkit.Reply{}, access.Event{}, httpkit.Fail(404, "slot_not_found", "The credential slot is unavailable.")
		}
		if err = matchSlot(r, current, slot); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		etag, err := s.deleteDraftSlot(r.Context(), tx, providerID, slotID, slot)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Reply{Status: http.StatusNoContent, ParentMutation: &httpkit.ParentMutation{Previous: current.ETag, Current: etag}}, access.Event{Action: "provider.slot.delete", Resource: "provider_slot", ID: slotID}, nil
	})
}

func (s *slotService) deleteDraftSlot(ctx context.Context, tx pgx.Tx, providerID, slotID string, slot *slotRow) (string, error) {
	if slot.Default {
		return "", httpkit.Fail(409, "default_slot_required", "The default credential slot cannot be deleted.")
	}
	if err := grants.DiscardSlotEnrollments(ctx, tx, providerID, slotID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM olp.provider_slots WHERE id=$1 AND provider_id=$2", slotID, providerID); err != nil {
		return "", err
	}
	return touchSlots(ctx, tx, providerID)
}
