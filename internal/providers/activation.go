package providers

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
)

// credentialFits refuses a slot whose credential version the provider can't
// authenticate with (Configuration.Authenticates): a version with a grant
// beneath it for a provider that takes a static credential, a pasted one for
// a provider that authenticates with a grant, or a grant another build or
// profile of the plugin enrolled.
func (row slotRow) credentialFits(cfg connection.Configuration) error {
	switch {
	case row.CredentialID == nil || !cfg.CredentialRequired() || cfg.Authenticates(row.CredentialPlugin, row.CredentialProfile):
		return nil
	case !cfg.Grant():
		return access.Fail(422, "credential_mismatch", "Slot "+row.Name+" holds a credential version with a grant, but this provider authenticates with a static credential. Rotate its credential.")
	case row.CredentialPlugin == "":
		return access.Fail(422, "credential_mismatch", "Slot "+row.Name+" holds a pasted credential, but a grant authenticates this provider. Enroll a grant for it.")
	}
	return access.Fail(422, "credential_mismatch", "Slot "+row.Name+" holds a grant that another build or profile of the plugin enrolled. A grant serves only its enrolling build and profile; re-enroll the slot's grant through this provider's profile.")
}

// observedPrincipal is the principal the slot observes: the one grant
// enrollment observed for its credential version, unless that version serves
// no more because it is revoked or its grant lapsed.
func (row slotRow) observedPrincipal() string {
	if row.CredentialRevoked || row.CredentialLapsed {
		return ""
	}
	return row.CredentialPrincipal
}

// observedFacts are the grant facts the slot's credential version serves
// with, cleared like its principal when the version serves no more.
func (row slotRow) observedFacts() map[string]string {
	if row.CredentialRevoked || row.CredentialLapsed {
		return nil
	}
	return row.CredentialFacts
}

// onePrincipal refuses to activate credential slots that observe different
// principals or serving environments (their grants' facts), naming each
// slot's principal. Every slot of a provider revision serves as one upstream
// account and environment, so re-enrolling that account is a credential
// rotation and several accounts are pooled through several providers.
func onePrincipal(slots []slotRow, cfg connection.Configuration) error {
	if !cfg.Grant() {
		return nil
	}
	var first string
	var firstFacts map[string]string
	var observed []string
	mixed := false
	for i := range slots {
		principal := slots[i].observedPrincipal()
		if principal == "" {
			continue
		}
		facts := slots[i].observedFacts()
		if first == "" {
			first, firstFacts = principal, facts
		}
		mixed = mixed || principal != first || !maps.Equal(facts, firstFacts)
		observed = append(observed, "slot "+slots[i].Name+" observes "+principal)
	}
	if !mixed {
		return nil
	}
	return access.Fail(422, "principal_mismatch", "The credential slots observe different upstream principals or serving environments: "+strings.Join(observed, "; ")+". Every slot of a provider serves one account and environment: re-enroll these slots alike, and pool other accounts through other providers.")
}

func (row slotRow) published(authMode string) routing.RevisionSlot {
	slot := routing.Slot{ID: row.ID, Name: row.Name, Enabled: row.Enabled, Priority: row.Priority, Weight: row.Weight, AllowedModels: row.Restrictions.AllowedModels, AllowedRoutes: row.Restrictions.AllowedRoutes, AllowedAPIKeys: row.Restrictions.AllowedAPIKeys, RequestsPerMinute: row.Limits.RequestsPerMinute, TokensPerMinute: row.Limits.TokensPerMinute, MaxConcurrency: row.Limits.MaxConcurrency, Supply: row.Limits.Supply}
	var principal string
	if connectors.SecretRequired(authMode) {
		slot.Enabled = slot.Enabled && !row.CredentialRevoked
		slot.CredentialID = row.CredentialID
		slot.CredentialVersion = row.CredentialVersion
		principal = row.observedPrincipal()
	}
	return routing.RevisionSlot{Slot: slot, Default: row.Default, ObservedPrincipal: principal}
}

func (s *Server) activateProvider(r *http.Request, _ access.Principal) (access.Reply, error) {
	return s.mutation(r, "provider.activate", nil, func(ctx context.Context, tx pgx.Tx, p access.Principal, current *record) (access.Reply, error) {
		activated, err := s.activateDraft(ctx, tx, p, current)
		if err != nil {
			return access.Reply{}, err
		}
		return access.Detail(map[string]any{"id": current.ID, "state": "active", "etag": activated.ETag, "runtime_generation": activated.Generation}, activated.ETag), nil
	})
}

func (s *Server) disableProvider(r *http.Request, _ access.Principal) (access.Reply, error) {
	return s.mutation(r, "provider.disable", nil, func(ctx context.Context, tx pgx.Tx, p access.Principal, current *record) (access.Reply, error) {
		disabled, err := s.disable(ctx, tx, p, current)
		if err != nil {
			return access.Reply{}, err
		}
		return access.Detail(credentialChange{ProviderID: current.ID, ETag: disabled.ETag, RuntimeGeneration: &disabled.Generation}, disabled.ETag), nil
	})
}

type activatedProvider struct {
	ETag       string
	Generation release.Published
}

func (s *Server) activateDraft(ctx context.Context, tx pgx.Tx, p access.Principal, current *record) (activatedProvider, error) {
	// The plugin a draft pinned may since have become unusable, such as
	// an unconfined plugin in a deployment that no longer enables them.
	prepared, err := connection.Prepare(ctx, s.ConnectionPolicy(tx), current.Connection.Document())
	if err != nil {
		return activatedProvider{}, err
	}
	configuration := prepared.Document()
	if err := validateNetworkReference(ctx, tx, current.ID, configuration.Options.Network); err != nil {
		return activatedProvider{}, err
	}
	if _, _, err := s.credentialFor(ctx, tx, current); err != nil {
		return activatedProvider{}, err
	}
	models, err := enabledModels(ctx, tx, current.ID)
	if err != nil {
		return activatedProvider{}, err
	}
	if len(models) == 0 {
		return activatedProvider{}, access.Fail(422, "no_enabled_models", "Enable and certify at least one model before activating.")
	}
	published := make([]routing.RevisionModel, 0, len(models))
	for _, m := range models {
		if len(m.Capabilities) == 0 {
			return activatedProvider{}, access.Fail(422, "certification_required", "Model "+m.UpstreamModel+" declares no capabilities.")
		}
		rm := routing.RevisionModel{ID: m.ID, UpstreamModel: m.UpstreamModel, DisplayName: m.DisplayName}
		for _, c := range m.Capabilities {
			if c.Source != "certified" {
				return activatedProvider{}, access.Fail(422, "certification_required", "Model "+m.UpstreamModel+" has an uncertified "+c.Operation+"/"+c.Surface+"/"+c.Mode+" capability.")
			}
			rm.Capabilities = append(rm.Capabilities, routing.RevisionCapability{Operation: c.Operation, Surface: c.Surface, Mode: c.Mode, Source: c.Source, CertifiedAt: c.CertifiedAt, ToolCalls: c.ToolCalls})
		}
		published = append(published, rm)
	}
	slots, err := loadSlots(ctx, tx, current.ID)
	if err != nil {
		return activatedProvider{}, err
	}
	if err = onePrincipal(slots, configuration); err != nil {
		return activatedProvider{}, err
	}
	var credentialVersion *int
	revisionSlots := make([]routing.RevisionSlot, 0, len(slots))
	for i := range slots {
		slot := slots[i].published(configuration.AuthMode)
		if err := slots[i].credentialFits(configuration); err != nil {
			return activatedProvider{}, err
		}
		if slot.Enabled && configuration.CredentialRequired() && slots[i].validationFingerprint(configuration, models) != "" {
			if slot.CredentialID == nil {
				return activatedProvider{}, access.Fail(422, "credential_required", "Slot "+slot.Name+" has no credential.")
			}
			if slots[i].validationTime(configuration, models) == nil {
				return activatedProvider{}, access.Fail(422, "slot_validation_required", "Validate slot "+slot.Name+" for its enabled models before activating.")
			}
		}
		if slot.Default {
			credentialVersion = slot.CredentialVersion
		}
		revisionSlots = append(revisionSlots, slot)
	}
	modelsJSON, _ := json.Marshal(published)
	slotsJSON, _ := json.Marshal(revisionSlots)
	configurationJSON, err := json.Marshal(prepared)
	if err != nil {
		return activatedProvider{}, err
	}
	revisionID, etag := access.NewID(), access.NewID()
	var revision int
	if err = tx.QueryRow(ctx, "INSERT INTO olp.provider_revisions(id,provider_id,revision,name,configuration,models,slots,credential_version,source_etag,activated_by) VALUES($1,$2,(SELECT coalesce(max(revision),0)+1 FROM olp.provider_revisions WHERE provider_id=$2),$3,$4,$5,$6,$7,$8,$9) RETURNING revision", revisionID, current.ID, current.Name, configurationJSON, modelsJSON, slotsJSON, credentialVersion, current.ETag, p.UserID()).Scan(&revision); err != nil {
		return activatedProvider{}, err
	}
	if _, err = tx.Exec(ctx, "UPDATE olp.providers SET state='active',active_revision=$2,active_revision_id=$3,draft_dirty=false,etag=$4,updated_at=now() WHERE id=$1", current.ID, revision, revisionID, etag); err != nil {
		return activatedProvider{}, err
	}
	if err = s.activated(ctx, tx, current.ID, p.UserID()); err != nil {
		return activatedProvider{}, err
	}
	generation, err := release.Publish(ctx, tx, p.UserID())
	if err != nil {
		return activatedProvider{}, err
	}
	return activatedProvider{ETag: etag, Generation: generation}, nil
}

func (s *Server) disable(ctx context.Context, tx pgx.Tx, p access.Principal, current *record) (activatedProvider, error) {
	if current.State != "active" {
		return activatedProvider{}, access.Fail(409, "invalid_state", "Only active connections can be disabled.")
	}
	etag := access.NewID()
	if _, err := tx.Exec(ctx, "UPDATE olp.providers SET state='disabled',etag=$2,updated_at=now() WHERE id=$1", current.ID, etag); err != nil {
		return activatedProvider{}, err
	}
	generation, err := release.Publish(ctx, tx, p.UserID())
	if err != nil {
		return activatedProvider{}, err
	}
	return activatedProvider{ETag: etag, Generation: generation}, nil
}
