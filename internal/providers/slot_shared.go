package providers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ptr"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/providers/probe"
)

type slotRow struct {
	ID                   string
	ETag                 string
	Default              bool
	Position             int
	Name                 string
	Enabled              bool
	Priority             int
	Weight               int64
	CredentialID         *string
	CredentialVersion    *int
	CredentialRevoked    bool
	CredentialPlugin     string
	CredentialProfile    string
	CredentialLapsed     bool
	GrantGeneration      int64
	CredentialPrincipal  string
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

func defaultSlot(slots []slotRow) (*slotRow, error) {
	index := slices.IndexFunc(slots, func(row slotRow) bool { return row.Default })
	if index < 0 {
		return nil, httpkit.Fail(422, "credential_required", "This connection has no default credential slot.")
	}
	return &slots[index], nil
}

func loadSlots(ctx context.Context, q database.Queryer, providerID string) ([]slotRow, error) {
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

func (row slotRow) allowsModel(model storedModel) bool {
	return model.Enabled && (len(row.Restrictions.AllowedModels) == 0 || slices.Contains(row.Restrictions.AllowedModels, model.UpstreamModel))
}

func (row slotRow) credentialFingerprint(cfg connection.Configuration) string {
	credential := ""
	if cfg.CredentialRequired() {
		credential = ptr.Value(row.CredentialID)
	}
	return cfg.Fingerprint() + ":" + credential
}

// Validation covers exactly the model capabilities this slot can serve. Changes
// to routing preferences or limits do not change the upstream access being tested.
func (row slotRow) validationFingerprint(cfg connection.Configuration, models []storedModel) (string, error) {
	var tuples []string
	for _, model := range models {
		if !row.allowsModel(model) {
			continue
		}
		for _, capability := range model.Capabilities {
			encoded, err := json.Marshal([]string{model.UpstreamModel, capability.Operation, capability.Surface, capability.Mode})
			if err != nil {
				return "", err
			}
			tuples = append(tuples, string(encoded))
		}
	}
	if len(tuples) == 0 {
		return "", nil
	}
	slices.Sort(tuples)
	encoded, err := json.Marshal(tuples)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(encoded)
	return row.credentialFingerprint(cfg) + ":" + hex.EncodeToString(hash[:]), nil
}

func (row slotRow) withCredential(id string) slotRow {
	updated := row
	updated.CredentialID = &id
	return updated
}

func (row slotRow) validationTime(cfg connection.Configuration, models []storedModel) (*time.Time, error) {
	fingerprint, err := row.validationFingerprint(cfg, models)
	if err != nil {
		return nil, err
	}
	if fingerprint != "" && ptr.Value(row.ValidatedFingerprint) == fingerprint {
		return row.ValidatedAt, nil
	}
	return nil, nil
}

func (row slotRow) credentialFits(cfg connection.Configuration) error {
	switch {
	case row.CredentialID == nil || !cfg.CredentialRequired() || cfg.Authenticates(row.CredentialPlugin, row.CredentialProfile):
		return nil
	case !cfg.Grant():
		return httpkit.Fail(422, "credential_mismatch", "Slot "+row.Name+" holds a credential version with a grant, but this provider authenticates with a static credential. Rotate its credential.")
	case row.CredentialPlugin == "":
		return httpkit.Fail(422, "credential_mismatch", "Slot "+row.Name+" holds a pasted credential, but a grant authenticates this provider. Enroll a grant for it.")
	}
	return httpkit.Fail(422, "credential_mismatch", "Slot "+row.Name+" holds a grant that another build or profile of the plugin enrolled. A grant serves only its enrolling build and profile; re-enroll the slot's grant through this provider's profile.")
}

func selectProbeSlot(slots []slotRow, cfg connection.Configuration, model *storedModel) *slotRow {
	var best *slotRow
	usable := func(s *slotRow) bool {
		return !cfg.CredentialRequired() || s.CredentialID != nil && !s.CredentialRevoked && !s.CredentialLapsed
	}
	for i := range slots {
		row := &slots[i]
		if !row.Enabled {
			continue
		}
		if model != nil && (!row.allowsModel(*model) || !usable(row) || row.credentialFits(cfg) != nil) {
			continue
		}
		if best == nil || usable(row) && !usable(best) || usable(row) == usable(best) && (row.Default && !best.Default || row.Default == best.Default && (row.Priority < best.Priority || row.Priority == best.Priority && row.Position < best.Position)) {
			best = row
		}
	}
	return best
}

func checkSlotCredential(cfg connection.Configuration, slot slotRow) error {
	if !cfg.CredentialRequired() {
		return nil
	}
	if slot.CredentialID == nil && cfg.Grant() {
		return httpkit.Fail(422, "credential_required", "Enroll a grant before probing this connection.")
	}
	if slot.CredentialID == nil {
		return httpkit.Fail(422, "credential_required", "Add a credential before probing this connection.")
	}
	if slot.CredentialRevoked {
		return httpkit.Fail(422, "credential_revoked", "The draft credential was revoked; rotate before probing.")
	}
	if slot.CredentialLapsed {
		return httpkit.Fail(422, "credential_lapsed", "The draft credential's grant lapsed; re-enroll it before probing.")
	}
	return slot.credentialFits(cfg)
}

func validateModelAccess(ctx context.Context, prober *probe.Prober, target probe.Target, row slotRow, models []storedModel) error {
	probeCtx, cancel := probe.Budget(ctx)
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
			if err := prober.Certify(probeCtx, target, model.UpstreamModel, capability.Capability); err != nil {
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
