package providers

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/platform/secretstore"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/providers/probe"
)

type slotValidationInput struct {
	provider *record
	slot     *slotRow
	models   []storedModel
}

type slotValidationSnapshot struct {
	slotValidationInput
	credential []byte
	reference  *secretstore.Reference
}

func (s *slotService) prepareSlotValidation(ctx context.Context, p authz.Principal, id, slotID string) (slotValidationInput, []byte, error) {
	snapshot, err := readTx(ctx, s.access.Pool, func(tx pgx.Tx) (slotValidationSnapshot, error) {
		current, err := readProvider(ctx, tx, id)
		if err != nil {
			return slotValidationSnapshot{}, err
		}
		if err := p.Project(current.ProjectID, authz.Change); err != nil {
			return slotValidationSnapshot{}, err
		}
		slots, err := loadSlots(ctx, tx, id)
		if err != nil {
			return slotValidationSnapshot{}, err
		}
		slot := findSlot(slots, slotID)
		if slot == nil {
			return slotValidationSnapshot{}, pgx.ErrNoRows
		}
		models, err := enabledModels(ctx, tx, id)
		if err != nil {
			return slotValidationSnapshot{}, err
		}
		var local []byte
		var ref *secretstore.Reference
		if cfg := current.Connection.Document(); cfg.CredentialRequired() {
			if slot.CredentialID == nil {
				return slotValidationSnapshot{}, httpkit.Fail(422, "credential_required", "This slot has no credential to validate.")
			}
			if slot.CredentialRevoked {
				return slotValidationSnapshot{}, httpkit.Fail(422, "credential_revoked", "This slot references a revoked credential.")
			}
			if slot.CredentialLapsed {
				return slotValidationSnapshot{}, httpkit.Fail(422, "credential_lapsed", "This slot's grant lapsed. Re-enroll its grant.")
			}
			if err = slot.credentialFits(cfg); err != nil {
				return slotValidationSnapshot{}, err
			}
			local, ref, err = s.credentials.readLocalOrRef(ctx, tx, *slot.CredentialID)
			if err != nil {
				return slotValidationSnapshot{}, err
			}
		}
		return slotValidationSnapshot{
			slotValidationInput: slotValidationInput{provider: current, slot: slot, models: models},
			credential:          local,
			reference:           ref,
		}, nil
	})
	if err != nil {
		return slotValidationInput{}, nil, err
	}
	if snapshot.reference != nil {
		credential, err := s.credentials.resolveReference(ctx, *snapshot.reference)
		if err != nil {
			return slotValidationInput{}, nil, err
		}
		return snapshot.slotValidationInput, credential, nil
	}
	return snapshot.slotValidationInput, snapshot.credential, nil
}

func (s *slotService) validateSlot(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "provider_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	slotID, err := httpkit.IDParam(r, "slot_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	input, credential, err := s.prepareSlotValidation(r.Context(), p, id, slotID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer clear(credential)
	current, slot, models := input.provider, input.slot, input.models
	probeErr := validateModelAccess(r.Context(), s.prober, current.target(credential), *slot, models)
	// Probes run without mutation locks. Only save evidence if its complete
	// input (credential, transport, restrictions, and models) is still current.
	result, err := access.Transact(s.access, r, unchangedSince(r.Context(), current, "validation"), func(tx pgx.Tx, p authz.Principal, locked *record) (httpkit.Reply, access.Event, error) {
		if err := s.storeSlotValidation(r.Context(), tx, current, slot, models, probeErr == nil); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		reply, err := s.slotList(r.Context(), tx, locked)
		return reply, access.Event{Action: "provider.slot.validate", Resource: "provider_slot", ID: slotID, Failed: probeErr != nil}, err
	})
	if err != nil {
		return httpkit.Reply{}, err
	}
	if probeErr != nil {
		return result, httpkit.Fail(422, "slot_validation_failed", probe.Classify(probeErr).Detail)
	}
	clearValidatedCooldowns(r.Context(), s.quota, s.log, id, *slot)
	return result, nil
}

func (s *slotService) storeSlotValidation(ctx context.Context, tx pgx.Tx, current *record, slot *slotRow, models []storedModel, succeeded bool) error {
	slotID := slot.ID
	var validatedAt *time.Time
	var fingerprint *string
	if succeeded {
		validatedAt = new(time.Now().UTC())
		computed, err := slot.validationFingerprint(current.Connection.Document(), models)
		if err != nil {
			return err
		}
		fingerprint = &computed
	}
	if _, err := tx.Exec(ctx, "UPDATE olp.provider_slots SET validated_at=$2,validated_fingerprint=$3 WHERE id=$1", slotID, validatedAt, fingerprint); err != nil {
		return err
	}
	return nil
}

func (row slotRow) certificationTime(cfg connection.Configuration, models []storedModel) *time.Time {
	var at *time.Time
	for _, model := range models {
		if !row.allowsModel(model) {
			continue
		}
		if len(model.Capabilities) == 0 {
			return nil
		}
		for _, capability := range model.Capabilities {
			if capability.Source != "certified" || capability.CertifiedAt == nil || capability.CredentialFingerprint != row.credentialFingerprint(cfg) {
				return nil
			}
			if at == nil || capability.CertifiedAt.Before(*at) {
				at = capability.CertifiedAt
			}
		}
	}
	return at
}

// A successful live proof clears the shared rejection for the exact version
// and logical slot. Serving processes consult this authority without a stale
// local shadow when distributed coordination is available.
func clearValidatedCooldowns(ctx context.Context, quota QuotaSource, log *slog.Logger, providerID string, slot slotRow) {
	if quota == nil {
		return
	}
	cooldownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	scopes := []string{fleet.SlotScope(slot.ID), fleet.CredentialScope(providerID, slot.CredentialID, slot.GrantGeneration)}
	for _, scope := range scopes {
		if err := quota.Cooldown(cooldownCtx, scope, 0); err != nil {
			log.Warn("validated credential cooldown could not be cleared", "provider_id", providerID)
		}
	}
}
