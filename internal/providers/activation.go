package providers

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/platform/secretstore"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
)

func (row slotRow) observedPrincipal() string {
	if row.CredentialRevoked || row.CredentialLapsed {
		return ""
	}
	return row.CredentialPrincipal
}

func (row slotRow) observedFacts() map[string]string {
	if row.CredentialRevoked || row.CredentialLapsed {
		return nil
	}
	return row.CredentialFacts
}

// Every slot of a provider revision serves as one upstream account and
// environment, so re-enrolling that account is a credential rotation and
// several accounts are pooled through several providers.
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
	return httpkit.Fail(422, "principal_mismatch", "The credential slots observe different upstream principals or serving environments: "+strings.Join(observed, "; ")+". Every slot of a provider serves one account and environment: re-enroll these slots alike, and pool other accounts through other providers.")
}

func (row slotRow) published(authMode connectors.AuthMode) routing.RevisionSlot {
	slot := routing.Slot{
		ID: row.ID, Name: row.Name, Enabled: row.Enabled, Priority: row.Priority, Weight: row.Weight,
		AllowedModels: row.Restrictions.AllowedModels, AllowedRoutes: row.Restrictions.AllowedRoutes, AllowedAPIKeys: row.Restrictions.AllowedAPIKeys,
		Limits: row.Limits.Serving(),
	}
	var principal string
	if connectors.SecretRequired(authMode) {
		slot.Enabled = slot.Enabled && !row.CredentialRevoked
		slot.CredentialID = row.CredentialID
		slot.CredentialVersion = row.CredentialVersion
		principal = row.observedPrincipal()
	}
	return routing.RevisionSlot{Slot: slot, Default: row.Default, ObservedPrincipal: principal}
}

type activationReply struct {
	ID                string            `json:"id"`
	State             providerState     `json:"state"`
	ETag              string            `json:"etag"`
	RuntimeGeneration release.Published `json:"runtime_generation"`
}

type activationPreflight struct {
	provider    *record
	slots       []slotRow
	externalRef *secretstore.Reference
}

func checkActivationSlot(cfg connection.Configuration, slots []slotRow) (*slotRow, error) {
	slot := selectProbeSlot(slots, cfg, nil)
	if slot == nil {
		return nil, httpkit.Fail(422, "credential_required", "Enable a credential slot before probing this connection.")
	}
	if err := checkSlotCredential(cfg, *slot); err != nil {
		return nil, err
	}
	return slot, nil
}

func (s *draftService) activationDraftFor(ctx context.Context, id string) func(pgx.Tx, authz.Principal) (activationPreflight, error) {
	lock := changeable(ctx, id)
	return func(tx pgx.Tx, p authz.Principal) (activationPreflight, error) {
		current, err := lock(tx, p)
		if err != nil {
			return activationPreflight{}, err
		}
		slots, err := loadSlots(ctx, tx, id)
		if err != nil {
			return activationPreflight{}, err
		}
		preflight := activationPreflight{provider: current, slots: slots}
		cfg := current.Connection.Document()
		slot, err := checkActivationSlot(cfg, slots)
		if err != nil || !cfg.CredentialRequired() {
			return preflight, nil
		}
		local, extRef, err := s.credentials.readLocalOrRef(ctx, tx, *slot.CredentialID)
		clear(local)
		if err != nil {
			if errors.Is(err, release.ErrCredentialUnavailable) || errors.Is(err, pgx.ErrNoRows) {
				return preflight, nil
			}
			return activationPreflight{}, err
		}
		preflight.externalRef = extRef
		return preflight, nil
	}
}

func (s *draftService) activateProvider(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "provider_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	preflight, replayed, err := access.Preflight(s.access, r, nil, s.activationDraftFor(r.Context(), id))
	if err != nil {
		return httpkit.Reply{}, err
	}
	if replayed != nil {
		return *replayed, nil
	}
	current := preflight.provider
	if err := httpkit.Match(r, current.ETag); err != nil {
		return httpkit.Reply{}, err
	}
	if _, err := checkActivationSlot(current.Connection.Document(), preflight.slots); err != nil {
		return httpkit.Reply{}, err
	}
	if preflight.externalRef != nil {
		secret, err := s.credentials.resolveReference(r.Context(), *preflight.externalRef)
		if err != nil {
			return httpkit.Reply{}, err
		}
		defer clear(secret)
	}
	return access.Mutate(s.access, r, nil, changeable(r.Context(), id), func(tx pgx.Tx, p authz.Principal, locked *record) (httpkit.Reply, access.Event, error) {
		if locked.ETag != current.ETag {
			return httpkit.Reply{}, access.Event{}, httpkit.Fail(412, "etag_mismatch", "The connection changed during activation; reload and retry.")
		}
		activated, err := s.activateDraft(r.Context(), tx, p, locked, preflight.externalRef)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		reply := httpkit.Detail(activationReply{
			ID:                locked.ID,
			State:             stateActive,
			ETag:              activated.ETag,
			RuntimeGeneration: activated.Generation,
		}, activated.ETag)
		return reply, access.Event{Action: "provider.activate", Resource: "provider", ID: id}, nil
	})
}

func (s *draftService) disableProvider(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	return providerMutation(s.access, r, "provider.disable", nil, func(ctx context.Context, tx pgx.Tx, p authz.Principal, current *record) (httpkit.Reply, error) {
		disabled, err := s.disable(ctx, tx, p, current)
		if err != nil {
			return httpkit.Reply{}, err
		}
		return httpkit.Detail(credentialChange{ProviderID: current.ID, ETag: disabled.ETag, RuntimeGeneration: &disabled.Generation}, disabled.ETag), nil
	})
}

type activatedProvider struct {
	ETag       string
	Generation release.Published
}

func (s *draftService) verifyActivationCredential(ctx context.Context, tx pgx.Tx, cfg connection.Configuration, slots []slotRow, resolvedRef *secretstore.Reference) error {
	slot, err := checkActivationSlot(cfg, slots)
	if err != nil {
		return err
	}
	if !cfg.CredentialRequired() {
		return nil
	}
	local, extRef, err := s.credentials.readLocalOrRef(ctx, tx, *slot.CredentialID)
	defer clear(local)
	if err != nil {
		return err
	}
	if extRef != nil && (resolvedRef == nil || *extRef != *resolvedRef) {
		return httpkit.Fail(412, "etag_mismatch", "The connection changed during activation; reload and retry.")
	}
	return nil
}

func buildRevisionModels(models []storedModel) ([]routing.RevisionModel, error) {
	if len(models) == 0 {
		return nil, httpkit.Fail(422, "no_enabled_models", "Enable and certify at least one model before activating.")
	}
	published := make([]routing.RevisionModel, 0, len(models))
	for _, m := range models {
		if len(m.Capabilities) == 0 {
			return nil, httpkit.Fail(422, "certification_required", "Model "+m.UpstreamModel+" declares no capabilities.")
		}
		rm := routing.RevisionModel{ID: m.ID, UpstreamModel: m.UpstreamModel, DisplayName: m.DisplayName, Listing: m.Listing}
		for _, c := range m.Capabilities {
			if c.Source != "certified" {
				return nil, httpkit.Fail(422, "certification_required", "Model "+m.UpstreamModel+" has an uncertified "+c.Operation+"/"+c.Surface+"/"+c.Mode+" capability.")
			}
			rm.Capabilities = append(rm.Capabilities, routing.RevisionCapability{Operation: c.Operation, Surface: c.Surface, Mode: c.Mode, Source: c.Source, CertifiedAt: c.CertifiedAt, ToolCalls: c.ToolCalls})
		}
		published = append(published, rm)
	}
	return published, nil
}

func buildRevisionSlots(configuration connection.Configuration, models []storedModel, slots []slotRow) ([]routing.RevisionSlot, *int, error) {
	if err := onePrincipal(slots, configuration); err != nil {
		return nil, nil, err
	}
	var credentialVersion *int
	revisionSlots := make([]routing.RevisionSlot, 0, len(slots))
	for i := range slots {
		slot := slots[i].published(configuration.AuthMode)
		if err := slots[i].credentialFits(configuration); err != nil {
			return nil, nil, err
		}
		if slot.Enabled && configuration.CredentialRequired() {
			fingerprint, err := slots[i].validationFingerprint(configuration, models)
			if err != nil {
				return nil, nil, err
			}
			if fingerprint != "" {
				if slot.CredentialID == nil {
					return nil, nil, httpkit.Fail(422, "credential_required", "Slot "+slot.Name+" has no credential.")
				}
				validatedAt, err := slots[i].validationTime(configuration, models)
				if err != nil {
					return nil, nil, err
				}
				if validatedAt == nil {
					return nil, nil, httpkit.Fail(422, "slot_validation_required", "Validate slot "+slot.Name+" for its enabled models before activating.")
				}
			}
		}
		if slot.Default {
			credentialVersion = slot.CredentialVersion
		}
		revisionSlots = append(revisionSlots, slot)
	}
	return revisionSlots, credentialVersion, nil
}

func (s *draftService) activateDraft(ctx context.Context, tx pgx.Tx, p authz.Principal, current *record, resolvedRef *secretstore.Reference) (activatedProvider, error) {
	// The plugin a draft pinned may since have become unusable, such as
	// an unconfined plugin in a deployment that no longer enables them.
	prepared, err := connection.Prepare(ctx, s.connectionPolicy(tx), current.Connection.Document())
	if err != nil {
		return activatedProvider{}, err
	}
	configuration := prepared.Document()
	if err := validateNetworkReference(ctx, tx, current.ID, configuration.Options.Network); err != nil {
		return activatedProvider{}, err
	}
	slots, err := loadSlots(ctx, tx, current.ID)
	if err != nil {
		return activatedProvider{}, err
	}
	if err := s.verifyActivationCredential(ctx, tx, configuration, slots, resolvedRef); err != nil {
		return activatedProvider{}, err
	}
	models, err := enabledModels(ctx, tx, current.ID)
	if err != nil {
		return activatedProvider{}, err
	}
	published, err := buildRevisionModels(models)
	if err != nil {
		return activatedProvider{}, err
	}
	revisionSlots, credentialVersion, err := buildRevisionSlots(configuration, models, slots)
	if err != nil {
		return activatedProvider{}, err
	}
	modelsJSON, err := json.Marshal(published)
	if err != nil {
		return activatedProvider{}, err
	}
	slotsJSON, err := json.Marshal(revisionSlots)
	if err != nil {
		return activatedProvider{}, err
	}
	configurationJSON, err := json.Marshal(prepared)
	if err != nil {
		return activatedProvider{}, err
	}
	revisionID, etag := ids.New(), ids.New()
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

func (s *draftService) disable(ctx context.Context, tx pgx.Tx, p authz.Principal, current *record) (activatedProvider, error) {
	if current.State != stateActive {
		return activatedProvider{}, httpkit.Fail(409, "invalid_state", "Only active connections can be disabled.")
	}
	etag := ids.New()
	if _, err := tx.Exec(ctx, "UPDATE olp.providers SET state='disabled',etag=$2,updated_at=now() WHERE id=$1", current.ID, etag); err != nil {
		return activatedProvider{}, err
	}
	generation, err := release.Publish(ctx, tx, p.UserID())
	if err != nil {
		return activatedProvider{}, err
	}
	return activatedProvider{ETag: etag, Generation: generation}, nil
}
