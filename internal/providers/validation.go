package providers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"
)

func (row *slotRow) allowsModel(model storedModel) bool {
	return model.Enabled && (len(row.Restrictions.AllowedModels) == 0 || slices.Contains(row.Restrictions.AllowedModels, model.UpstreamModel))
}

func (row *slotRow) credentialFingerprint(cfg *Configuration) string {
	credential := ""
	if cfg.credentialRequired() {
		credential = deref(row.CredentialID)
	}
	return cfg.transportFingerprint() + ":" + credential
}

// Validation covers exactly the model capabilities this slot can serve. Changes
// to routing preferences or limits do not change the upstream access being tested.
func (row *slotRow) validationFingerprint(cfg *Configuration, models []storedModel) string {
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

func (row *slotRow) validationTime(cfg *Configuration, models []storedModel) *time.Time {
	fingerprint := row.validationFingerprint(cfg, models)
	if fingerprint != "" && deref(row.ValidatedFingerprint) == fingerprint {
		return row.ValidatedAt
	}
	return nil
}

// Certification uses the default credential. It also validates that slot once
// every allowed enabled capability has been certified with that same credential.
func (row *slotRow) certificationTime(cfg *Configuration, models []storedModel) *time.Time {
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

func (s *Server) validateModelAccess(ctx context.Context, cfg *Configuration, credential []byte, row *slotRow, models []storedModel) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	checked := 0
	for _, model := range models {
		if !row.allowsModel(model) {
			continue
		}
		if len(model.Capabilities) == 0 {
			return &probeError{Code: "no_capabilities", Detail: "Declare capabilities for the slot's enabled models before validating."}
		}
		for _, capability := range model.Capabilities {
			if err := s.certifyTuple(ctx, cfg, credential, model.UpstreamModel, capabilityInput{capability.Operation, capability.Surface, capability.Mode}, probeBodyLimit); err != nil {
				return err
			}
			checked++
		}
	}
	if checked == 0 {
		return &probeError{Code: "no_enabled_models", Detail: "Select allowed enabled models before validating this credential."}
	}
	return nil
}
