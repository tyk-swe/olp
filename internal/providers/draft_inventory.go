package providers

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
)

// DraftModel is declared portable model intent. Certification belongs to the
// destination's probe/activation workflow, never to imported intent.
type DraftModel struct {
	UpstreamModel string            `json:"upstream_model"`
	DisplayName   string            `json:"display_name"`
	Enabled       bool              `json:"enabled"`
	Capabilities  []CapabilityInput `json:"capabilities"`
}

// DraftSlot binds validated slot intent to destination-owned identities.
// A caller resolves credentials and local restrictions before staging.
type DraftSlot struct {
	ID           string
	Default      bool
	Position     int
	Name         string
	Enabled      bool
	Priority     int
	Weight       int64
	CredentialID *string
	Restrictions SlotRestrictions
	Limits       Limits
}

// StageDraftInventory owns replacement of a validated provider inventory under
// the caller's authorized installation transaction. Published model/credential
// identities survive staging; retained slots preserve pending enrollments and
// fingerprint-bound validation. Omitted slots discard their encrypted pending
// enrollment state. Every replacement advances parent/collection preconditions.
func StageDraftInventory(ctx context.Context, tx pgx.Tx, providerID string, models []DraftModel, slots []DraftSlot) error {
	desired := make([]string, 0, len(models))
	for _, model := range models {
		desired = append(desired, model.UpstreamModel)
	}
	if _, err := tx.Exec(ctx, "UPDATE olp.provider_models SET enabled=false,capabilities='[]'::jsonb WHERE provider_id=$1 AND NOT (upstream_model = ANY($2::text[]))", providerID, desired); err != nil {
		return err
	}
	for _, model := range models {
		display := model.DisplayName
		if display == "" {
			display = model.UpstreamModel
		}
		declared := make([]storedCapability, 0, len(model.Capabilities))
		for _, capability := range model.Capabilities {
			declared = append(declared, storedCapability{Operation: capability.Operation, Surface: capability.Surface, Mode: capability.Mode, Source: "declared"})
		}
		encoded, err := json.Marshal(declared)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO olp.provider_models(id,provider_id,upstream_model,display_name,enabled,capabilities) VALUES($1,$2,$3,$4,$5,$6)
   ON CONFLICT(provider_id,upstream_model) DO UPDATE SET display_name=excluded.display_name,enabled=excluded.enabled,capabilities=excluded.capabilities`, access.NewID(), providerID, model.UpstreamModel, display, model.Enabled, encoded); err != nil {
			return err
		}
	}
	retained := make([]string, 0, len(slots))
	for _, slot := range slots {
		retained = append(retained, slot.ID)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM olp.secrets WHERE purpose=$3 AND id IN (
  SELECT e.id FROM olp.grant_enrollments e JOIN olp.provider_slots s ON s.id=e.slot_id
  WHERE s.provider_id=$1 AND NOT (s.id=ANY($2::uuid[])))`, providerID, retained, secrets.GrantEnrollment); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM olp.provider_slots WHERE provider_id=$1 AND NOT (id=ANY($2::uuid[]))", providerID, retained); err != nil {
		return err
	}
	// Move existing slots into a temporary disjoint position range so validated
	// reordering/default changes cannot collide with the unique constraints.
	if _, err := tx.Exec(ctx, "UPDATE olp.provider_slots SET is_default=false,position=-position-1 WHERE provider_id=$1", providerID); err != nil {
		return err
	}
	for _, slot := range slots {
		slot.Restrictions.AllowedAPIKeys = orEmpty(slot.Restrictions.AllowedAPIKeys)
		slot.Restrictions.AllowedModels = orEmpty(slot.Restrictions.AllowedModels)
		slot.Restrictions.AllowedRoutes = orEmpty(slot.Restrictions.AllowedRoutes)
		restrictions, err := json.Marshal(slot.Restrictions)
		if err != nil {
			return err
		}
		limits, err := json.Marshal(slot.Limits)
		if err != nil {
			return err
		}
		result, err := tx.Exec(ctx, `INSERT INTO olp.provider_slots(id,provider_id,is_default,position,name,enabled,priority,weight,credential_id,restrictions,limits)
   VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
   ON CONFLICT(id) DO UPDATE SET is_default=excluded.is_default,position=excluded.position,name=excluded.name,
    enabled=excluded.enabled,priority=excluded.priority,weight=excluded.weight,credential_id=excluded.credential_id,
    restrictions=excluded.restrictions,limits=excluded.limits,
    validated_at=CASE WHEN olp.provider_slots.credential_id IS NOT DISTINCT FROM excluded.credential_id THEN olp.provider_slots.validated_at END,
    validated_fingerprint=CASE WHEN olp.provider_slots.credential_id IS NOT DISTINCT FROM excluded.credential_id THEN olp.provider_slots.validated_fingerprint END
   WHERE olp.provider_slots.provider_id=excluded.provider_id`, slot.ID, providerID, slot.Default, slot.Position, slot.Name, slot.Enabled, slot.Priority, slot.Weight, slot.CredentialID, restrictions, limits)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return access.Invalid("slots", "Slot identity belongs to another provider.")
		}
	}
	_, err := tx.Exec(ctx, "UPDATE olp.providers SET etag=$2,slots_etag=$3,draft_dirty=true,updated_at=now() WHERE id=$1", providerID, access.NewID(), access.NewID())
	return err
}
