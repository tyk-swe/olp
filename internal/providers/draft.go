package providers

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/grants"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/providers/probe"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secretstore"
)

type createRequest struct {
	Name                string                   `json:"name"`
	Configuration       connection.Configuration `json:"configuration"`
	Credential          *string                  `json:"credential"`
	CredentialReference *secretstore.Reference   `json:"credential_reference,omitempty"`
	DisplayName         *string                  `json:"display_name"`
	Model               *string                  `json:"model"`
	ProjectID           *string                  `json:"project_id"`
}

type updateRequest struct {
	Name          string                   `json:"name"`
	Configuration connection.Configuration `json:"configuration"`
}

func (s *Server) createProvider(r *http.Request, _ access.Principal) (access.Reply, error) {
	a := s.access
	var input createRequest
	if err := access.DecodeUnique(r, &input, 1<<20); err != nil {
		return access.Reply{}, err
	}
	credential, err := decodeCredential(input.Credential, input.CredentialReference)
	if err != nil {
		return access.Reply{}, err
	}
	tx, p, err := a.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	if err = authorizeCredential(p, credential); err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := a.Replay(r, tx, p, input)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		// The stored reply carries the created provider, so the caller must
		// still reach its project to receive it.
		if err := a.RequireProject(r.Context(), tx, p, input.ProjectID); err != nil {
			return access.Reply{}, err
		}
		return *replayed, tx.Commit(r.Context())
	}
	created, err := s.createDraft(r.Context(), tx, p, input, credential)
	if err != nil {
		return access.Reply{}, err
	}
	id, etag := created.ID, created.ETag
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "provider.create", "provider", id, "success"); err != nil {
		return access.Reply{}, err
	}
	result := access.Reply{Status: 201, Location: "/api/v1/providers/" + id, ETag: etag, Body: map[string]any{"id": id, "name": input.Name, "kind": created.Kind, "state": stateDraft, "etag": etag, "model": input.Model, "project_id": input.ProjectID}}
	if err = a.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return result, tx.Commit(r.Context())
}

// invalidateEvidence downgrades every certified capability after a transport
// change. Slot evidence is already bound to the transport fingerprint; retaining
// it lets a restored draft reuse validation only when all inputs match again.
func invalidateEvidence(ctx context.Context, tx pgx.Tx, providerID string) error {
	_, err := tx.Exec(ctx, "UPDATE olp.provider_models SET capabilities=(SELECT coalesce(jsonb_agg(c||'{\"source\":\"declared\",\"certified_at\":null}'::jsonb),'[]'::jsonb) FROM jsonb_array_elements(capabilities) c) WHERE provider_id=$1", providerID)
	return err
}

func (s *Server) updateProvider(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input updateRequest
	if err = access.DecodeUnique(r, &input, 1<<20); err != nil {
		return access.Reply{}, err
	}
	if err = access.ValidText("name", input.Name, 100); err != nil {
		return access.Reply{}, err
	}
	return access.Transact(s.access, r, changeable(r.Context(), id), func(tx pgx.Tx, p access.Principal, current *record) (access.Reply, error) {
		if err := access.Match(r, current.ETag); err != nil {
			return access.Reply{}, err
		}
		prepared, err := connection.Prepare(r.Context(), s.ConnectionPolicy(tx), input.Configuration)
		if err != nil {
			return access.Reply{}, err
		}
		if err = s.StoreDraftConfiguration(r.Context(), tx, id, input.Name, prepared); err != nil {
			return access.Reply{}, err
		}
		if err = access.Audit(r.Context(), tx, r, p.Actor(), "provider.update", "provider", id, "success"); err != nil {
			return access.Reply{}, err
		}
		return s.detailReply(r.Context(), tx, id)
	})
}

// Credentials are never restored, so the draft keeps its current slots.
func (s *Server) restoreDraft(ctx context.Context, tx pgx.Tx, current *record, v *revisionRow) (string, error) {
	if err := preserveCredentialBoundary(ctx, tx, current.ID, current.Connection.Document(), v.Configuration); err != nil {
		return "", err
	}
	change, err := connection.Diff(current.Connection.Document(), v.Configuration)
	if err != nil {
		return "", err
	}
	if change.Enrollment {
		if err := grants.CancelProviderPending(ctx, tx, current.ID); err != nil {
			return "", err
		}
	}
	// A revision only ever records evidence gathered against its own
	// transport, so restoring it restores that evidence unchanged; slot
	// validation is keyed by fingerprint and re-evaluates itself on read.
	configuration, err := json.Marshal(v.Configuration)
	if err != nil {
		return "", err
	}
	// Published model identities also anchor route targets. Remove only models
	// that have never been published, and disable the retained rows until the
	// restored revision selects them again. Evidence outside that revision is
	// no longer certified against the restored transport.
	if _, err := tx.Exec(ctx, `DELETE FROM olp.provider_models WHERE provider_id=$1 AND id NOT IN (
		SELECT (m->>'id')::uuid FROM olp.provider_revisions r,jsonb_array_elements(r.models) m WHERE r.provider_id=$1
	)`, current.ID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE olp.provider_models SET enabled=false,
		capabilities=(SELECT coalesce(jsonb_agg(c||'{"source":"declared","certified_at":null}'::jsonb),'[]'::jsonb) FROM jsonb_array_elements(capabilities) c)
		WHERE provider_id=$1`, current.ID); err != nil {
		return "", err
	}
	for _, m := range v.Models {
		capabilities := make([]storedCapability, 0, len(m.Capabilities))
		for _, c := range m.Capabilities {
			capabilities = append(capabilities, storedCapability{Capability: connection.Capability{Operation: c.Operation, Surface: c.Surface, Mode: c.Mode}, Source: c.Source, CertifiedAt: c.CertifiedAt, ToolCalls: c.ToolCalls})
		}
		encoded, _ := json.Marshal(capabilities)
		if _, err := tx.Exec(ctx, `INSERT INTO olp.provider_models(id,provider_id,upstream_model,display_name,enabled,capabilities) VALUES($1,$2,$3,$4,true,$5)
			ON CONFLICT(id) DO UPDATE SET display_name=excluded.display_name,enabled=true,capabilities=excluded.capabilities`, m.ID, current.ID, m.UpstreamModel, m.DisplayName, encoded); err != nil {
			return "", err
		}
	}
	etag := access.NewID()
	dirty := current.ActiveRevisionID == nil || *current.ActiveRevisionID != v.ID
	if !dirty {
		// Restore retains the draft slots, including unpublished credentials
		// and restrictions. Only an exact match to the active slots is clean.
		slots, err := loadSlots(ctx, tx, current.ID)
		if err != nil {
			return "", err
		}
		published := make([]routing.RevisionSlot, 0, len(slots))
		for i := range slots {
			published = append(published, slots[i].published(v.Configuration.AuthMode))
		}
		draftSlots, _ := json.Marshal(published)
		activeSlots, _ := json.Marshal(v.Slots)
		dirty = string(draftSlots) != string(activeSlots)
	}
	_, err = tx.Exec(ctx, "UPDATE olp.providers SET name=$2,kind=$3,configuration=$4,etag=$5,draft_dirty=$6,updated_at=now() WHERE id=$1", current.ID, v.Name, v.Configuration.Kind, configuration, etag, dirty)
	return etag, err
}

func (s *Server) restoreActiveAsDraft(r *http.Request, _ access.Principal) (access.Reply, error) {
	return s.mutation(r, "provider.restore", nil, func(ctx context.Context, tx pgx.Tx, p access.Principal, current *record) (access.Reply, error) {
		if current.ActiveRevisionID == nil {
			return access.Reply{}, access.Fail(409, "invalid_state", "This connection has no activated revision to restore.")
		}
		v, err := loadRevision(ctx, tx, current.ID, *current.ActiveRevisionID)
		if err != nil {
			return access.Reply{}, err
		}
		if _, err = s.restoreDraft(ctx, tx, current, v); err != nil {
			return access.Reply{}, err
		}
		return s.detailReply(ctx, tx, current.ID)
	})
}

func (s *Server) restoreRevisionAsDraft(r *http.Request, _ access.Principal) (access.Reply, error) {
	ref := r.PathValue("revision_id")
	return s.mutation(r, "provider.restore", nil, func(ctx context.Context, tx pgx.Tx, p access.Principal, current *record) (access.Reply, error) {
		v, err := loadRevision(ctx, tx, current.ID, ref)
		if errors.Is(err, pgx.ErrNoRows) {
			return access.Reply{}, access.Fail(404, "not_found", "Unknown revision.")
		}
		if err != nil {
			return access.Reply{}, err
		}
		if _, err = s.restoreDraft(ctx, tx, current, v); err != nil {
			return access.Reply{}, err
		}
		d, err := s.detail(ctx, tx, current.ID)
		if err != nil {
			return access.Reply{}, err
		}
		return access.Detail(map[string]any{"provider": d, "credential_restored": false}, d.ETag), nil
	})
}

type createdProvider struct {
	CreatedDraft
	Kind string
}

func (s *Server) createDraft(ctx context.Context, tx pgx.Tx, p access.Principal, input createRequest, credential credentialSource) (createdProvider, error) {
	var err error
	if err = access.ValidText("name", input.Name, 100); err != nil {
		return createdProvider{}, err
	}
	prepared, err := connection.Prepare(ctx, s.ConnectionPolicy(tx), input.Configuration)
	if err != nil {
		return createdProvider{}, err
	}
	configuration := prepared.Document()
	switch {
	case configuration.Grant() && credential != nil:
		return createdProvider{}, access.Invalid("credential", grantEnrollmentOnly)
	case configuration.CredentialRequired() && !configuration.Grant() && credential == nil:
		return createdProvider{}, access.Invalid("credential", "This authentication mode requires a credential.")
	case !configuration.CredentialRequired() && credential != nil:
		return createdProvider{}, access.Invalid("credential", "This authentication mode takes no stored credential.")
	}
	displayName := cmp.Or(deref(input.DisplayName), deref(input.Model))
	if input.Model != nil {
		if err = connection.ValidModelName("model", *input.Model); err != nil {
			return createdProvider{}, err
		}
		if err = connection.ValidModelName("display_name", displayName); err != nil {
			return createdProvider{}, err
		}
	}
	if err = s.access.RequireProject(ctx, tx, p, input.ProjectID); err != nil {
		return createdProvider{}, err
	}
	created, err := s.CreateDraft(ctx, tx, Draft{Name: input.Name, ProjectID: input.ProjectID, Configuration: prepared, CreatedBy: p.UserID()})
	if err != nil {
		return createdProvider{}, err
	}
	id := created.ID
	var credentialID *string
	if credential != nil {
		stored, _, err := s.storeCredential(ctx, tx, id, credential)
		if err != nil {
			return createdProvider{}, err
		}
		credentialID = &stored
	}
	if _, err = tx.Exec(ctx, "INSERT INTO olp.provider_slots(id,provider_id,is_default,position,name,credential_id) VALUES($1,$2,true,0,'default',$3)", access.NewID(), id, credentialID); err != nil {
		return createdProvider{}, err
	}
	if input.Model != nil {
		transport := prepared.Transport()
		surface, operation := "openai", probe.Operation(transport)
		switch configuration.Kind {
		case connectors.KindAnthropic:
			surface = "anthropic"
		case connectors.KindGemini, connectors.KindVertexAI:
			surface = "gemini"
		}
		if configuration.ProfileID != "" {
			profile, _ := transport.Profile()
			if _, native := protocols.NativeGenerationFamily(profile.OperationDialect("generation")); native && operation == "generation" {
				surface = "native"
			}
			switch profile.Dialect {
			case "openai-chat", "openai-responses":
				// Vertex's OpenAI-compatible profile speaks OpenAI, whatever
				// its kind's native surface.
				surface = "openai"
			case "anthropic-messages":
				surface = "anthropic"
			case "gemini-generate-content":
				surface = "gemini"
			case "bedrock-converse":
				surface = "bedrock"
			case "bedrock-invoke":
				surface, operation = "bedrock", "bedrock_invoke"
			}
		}
		// A dedicated operation profile, such as an embeddings or rerank
		// dialect, declares the tuple it can actually certify.
		mode := "unary"
		if configuration.ProfileID != "" && !probe.Probeable(transport, connection.Capability{Operation: operation, Surface: surface, Mode: mode}) {
			if tuple := probe.DefaultTuple(transport); probe.Probeable(transport, tuple) {
				operation, surface, mode = tuple.Operation, tuple.Surface, tuple.Mode
			}
		}
		capabilities := []storedCapability{{Capability: connection.Capability{Operation: operation, Surface: surface, Mode: mode}, Source: "declared"}}
		if operation == "generation" {
			capabilities = append(capabilities, storedCapability{Capability: connection.Capability{Operation: operation, Surface: surface, Mode: "streaming"}, Source: "declared"})
		}
		declared, _ := json.Marshal(capabilities)
		if _, err = tx.Exec(ctx, "INSERT INTO olp.provider_models(id,provider_id,upstream_model,display_name,enabled,capabilities) VALUES($1,$2,$3,$4,true,$5)", access.NewID(), id, *input.Model, displayName, declared); err != nil {
			return createdProvider{}, err
		}
	}
	return createdProvider{CreatedDraft: created, Kind: configuration.Kind}, nil
}

// The caller must hold the installation lock and current authorization.
func (s *Server) StoreDraftConfiguration(ctx context.Context, tx pgx.Tx, id, name string, prepared connection.Prepared) error {
	next := prepared.Document()
	var encoded []byte
	var previousName string
	if err := tx.QueryRow(ctx, "SELECT name,configuration FROM olp.providers WHERE id=$1 FOR UPDATE", id).Scan(&previousName, &encoded); err != nil {
		return err
	}
	previous, err := connection.Decode(encoded)
	if err != nil {
		return err
	}
	if err := preserveCredentialBoundary(ctx, tx, id, previous, next); err != nil {
		return err
	}
	if err := validateNetworkReference(ctx, tx, id, next.Options.Network); err != nil {
		return err
	}
	change, err := connection.Diff(previous, next)
	if err != nil {
		return err
	}
	if previousName == name && !change.Changed {
		return nil
	}
	if change.Enrollment {
		if err := grants.CancelProviderPending(ctx, tx, id); err != nil {
			return err
		}
	}
	if change.Transport {
		if err := invalidateEvidence(ctx, tx, id); err != nil {
			return err
		}
	}
	after, err := json.Marshal(prepared)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "UPDATE olp.providers SET name=$2,kind=$3,configuration=$4,etag=$5,draft_dirty=true,updated_at=now() WHERE id=$1", id, name, next.Kind, after, access.NewID())
	return err
}

func preserveCredentialBoundary(ctx context.Context, q access.Queryer, providerID string, current, next connection.Configuration) error {
	move := connection.MoveCredentials(current, next)
	if !move.Destination {
		return nil
	}
	grantMigration := move.GrantIdentity
	if grantMigration {
		// The revision is the destination: a grant build binds its upstream
		// and authority origins into the manifest, so a migration may only
		// land on a build declaring the same origins. A missing build or
		// different origins is a destination change, not a migration.
		if err := q.QueryRow(ctx, `SELECT COALESCE((SELECT a.manifest::jsonb->'origins' FROM olp.plugins a WHERE a.digest=$1)
			= (SELECT b.manifest::jsonb->'origins' FROM olp.plugins b WHERE b.digest=$2), false)`,
			current.ProfileRevision, next.ProfileRevision).Scan(&grantMigration); err != nil {
			return err
		}
	}
	var stored bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.provider_credentials
		WHERE provider_id=$1 AND revoked_at IS NULL AND (NOT $2 OR plugin_digest IS NULL))
		OR ($3 AND EXISTS(SELECT 1 FROM olp.provider_network_credentials WHERE provider_id=$1 AND revoked_at IS NULL))`,
		providerID, grantMigration, move.Network).Scan(&stored); err != nil {
		return err
	}
	if stored {
		return access.Fail(422, "credential_destination_changed", "This provider owns unrevoked credential versions bound to its authentication, endpoint, network and profile. Create a new provider or revoke every old credential before supplying credentials for a different destination.")
	}
	return nil
}

type Draft struct {
	Name          string
	ProjectID     *string
	Configuration connection.Prepared
	CreatedBy     string
}

type CreatedDraft struct {
	ID   string
	ETag string
}

func (s *Server) CreateDraft(ctx context.Context, tx pgx.Tx, draft Draft) (CreatedDraft, error) {
	created := CreatedDraft{ID: access.NewID(), ETag: access.NewID()}
	configuration := draft.Configuration.Document()
	if err := validateNetworkReference(ctx, tx, created.ID, configuration.Options.Network); err != nil {
		return CreatedDraft{}, err
	}
	encoded, err := json.Marshal(draft.Configuration)
	if err != nil {
		return CreatedDraft{}, err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by,project_id) VALUES($1,$2,$3,'draft',$4,$5,$6,$7,$8)", created.ID, draft.Name, configuration.Kind, encoded, created.ETag, access.NewID(), draft.CreatedBy, draft.ProjectID); err != nil {
		return CreatedDraft{}, err
	}
	return created, nil
}

// Imported models are never certified. The destination certifies them through
// its own probe and activation workflow.
type DraftModel struct {
	UpstreamModel string                  `json:"upstream_model"`
	DisplayName   string                  `json:"display_name"`
	Enabled       bool                    `json:"enabled"`
	Capabilities  []connection.Capability `json:"capabilities"`
}

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
	Limits       connection.Limits
}

// Published model and credential identities survive staging, and retained
// slots keep their pending enrollments and fingerprint-bound validation.
func (s *Server) StageDraftInventory(ctx context.Context, tx pgx.Tx, providerID string, models []DraftModel, slots []DraftSlot) error {
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
			declared = append(declared, storedCapability{Capability: connection.Capability{Operation: capability.Operation, Surface: capability.Surface, Mode: capability.Mode}, Source: "declared"})
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
	if err := grants.DiscardEnrollmentsOutside(ctx, tx, providerID, retained); err != nil {
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
	_, err := touchSlots(ctx, tx, providerID)
	return err
}

type installedPlugins struct {
	q          access.Queryer
	unconfined *plugins.Unconfined
}

func (p installedPlugins) Profile(ctx context.Context, digest, profileID string) (*connectors.PluginProfile, error) {
	plugin, err := plugins.Profile(ctx, p.q, p.unconfined, digest, profileID)
	if refusal, ok := errors.AsType[*plugins.Error](err); ok {
		return nil, &access.Problem{Status: 422, Code: refusal.Code, Detail: refusal.Message, Field: profileField(refusal.Code)}
	}
	return plugin, err
}

func profileField(code string) string {
	if code == plugins.CodeProfileUnknown {
		return "configuration.profile_id"
	}
	return "configuration.profile_revision"
}

func (s *Server) ConnectionPolicy(q access.Queryer) connection.Policy {
	return connection.Policy{Egress: s.egress, Plugins: installedPlugins{q, s.unconfined}}
}
