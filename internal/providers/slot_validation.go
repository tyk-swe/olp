package providers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/providers/probe"
)

func (s *Server) validateSlot(r *http.Request, p access.Principal) (access.Reply, error) {
	a := s.access
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

func (s *Server) storeSlotValidation(ctx context.Context, tx pgx.Tx, current *record, slot *slotRow, models []storedModel, succeeded bool) error {
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

func (row slotRow) allowsModel(model storedModel) bool {
	return model.Enabled && (len(row.Restrictions.AllowedModels) == 0 || slices.Contains(row.Restrictions.AllowedModels, model.UpstreamModel))
}

func (row slotRow) credentialFingerprint(cfg connection.Configuration) string {
	credential := ""
	if cfg.CredentialRequired() {
		credential = deref(row.CredentialID)
	}
	return cfg.Fingerprint() + ":" + credential
}

// Validation covers exactly the model capabilities this slot can serve. Changes
// to routing preferences or limits do not change the upstream access being tested.
func (row slotRow) validationFingerprint(cfg connection.Configuration, models []storedModel) string {
	var tuples []string
	for _, model := range models {
		if !row.allowsModel(model) {
			continue
		}
		for _, capability := range model.Capabilities {
			encoded, _ := json.Marshal([]string{model.UpstreamModel, capability.Operation, capability.Surface, capability.Mode})
			tuples = append(tuples, string(encoded))
		}
	}
	if len(tuples) == 0 {
		return ""
	}
	slices.Sort(tuples)
	encoded, _ := json.Marshal(tuples)
	hash := sha256.Sum256(encoded)
	return row.credentialFingerprint(cfg) + ":" + hex.EncodeToString(hash[:])
}

func (row slotRow) withCredential(id string) slotRow {
	row.CredentialID = &id
	return row
}

func (row slotRow) validationTime(cfg connection.Configuration, models []storedModel) *time.Time {
	fingerprint := row.validationFingerprint(cfg, models)
	if fingerprint != "" && deref(row.ValidatedFingerprint) == fingerprint {
		return row.ValidatedAt
	}
	return nil
}

// Certification uses the default credential. It also validates that slot once
// every allowed enabled capability has been certified with that same credential.
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

func (s *Server) validateModelAccess(ctx context.Context, target probe.Target, row slotRow, models []storedModel) error {
	ctx, cancel := probe.Budget(ctx)
	defer cancel()
	checked := 0
	for _, model := range models {
		if !row.allowsModel(model) {
			continue
		}
		if len(model.Capabilities) == 0 {
			return &probe.Failure{Reason: probe.NoCapabilities, Detail: "Declare capabilities for the slot's enabled models before validating."}
		}
		for _, capability := range model.Capabilities {
			if err := s.prober.Certify(ctx, target, model.UpstreamModel, capability.Capability, probe.BodyLimit); err != nil {
				return err
			}
			checked++
		}
	}
	if checked == 0 {
		return &probe.Failure{Reason: probe.NoEnabledModels, Detail: "Select allowed enabled models before validating this credential."}
	}
	return nil
}

// A successful live proof clears the shared rejection for the exact version
// and logical slot. Serving processes consult this authority without a stale
// local shadow when distributed coordination is available.
func (s *Server) clearValidatedCooldowns(ctx context.Context, providerID string, slot slotRow) {
	if s.quota == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	scopes := []string{fleet.SlotScope(slot.ID), fleet.CredentialScope(providerID, slot.CredentialID, slot.GrantGeneration)}
	for _, scope := range scopes {
		if err := s.quota.Cooldown(ctx, scope, 0); err != nil {
			s.log.Warn("validated credential cooldown could not be cleared", "provider_id", providerID)
		}
	}
}
