package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/runtime"
)

const maxCredentialBytes = 8192

type createRequest struct {
	Name          string        `json:"name"`
	Configuration Configuration `json:"configuration"`
	Credential    *string       `json:"credential"`
	DisplayName   *string       `json:"display_name"`
	Model         *string       `json:"model"`
}

type updateRequest struct {
	Name          string        `json:"name"`
	Configuration Configuration `json:"configuration"`
}

func validCredential(value string) error {
	if len(value) == 0 || len(value) > maxCredentialBytes || strings.ContainsAny(value, "\r\n\x00") {
		return access.Invalid("credential", "Use a credential of 1–8192 characters without line breaks.")
	}
	return nil
}

// storeCredential records a new credential version and its sealed secret.
func (s *Server) storeCredential(ctx context.Context, tx pgx.Tx, providerID, secret string) (id string, version int, err error) {
	id = access.NewID()
	if err = tx.QueryRow(ctx, "INSERT INTO olp_go.provider_credentials(id,provider_id,version) VALUES($1,$2,(SELECT coalesce(max(version),0)+1 FROM olp_go.provider_credentials WHERE provider_id=$2)) RETURNING version", id, providerID).Scan(&version); err != nil {
		return "", 0, err
	}
	err = s.Access.Keys.Store(ctx, tx, s.Access.Installation, id, "provider_credential", []byte(secret), nil)
	return id, version, err
}

func (s *Server) providers(r *http.Request) (access.Reply, error) {
	if _, err := s.Access.Principal(r, s.Access.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	if len(search) > 100 {
		return access.Reply{}, access.Invalid("search", "Use at most 100 characters.")
	}
	rows, err := s.Access.Pool.Query(r.Context(), detailQuery+" WHERE p.id<$1 AND ($2='' OR p.name ILIKE '%'||$2||'%') ORDER BY p.id DESC LIMIT $3", page.Before, search, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		d, err := s.scanDetail(rows)
		if err != nil {
			return access.Reply{}, err
		}
		items = append(items, d.summary())
	}
	if err = rows.Err(); err != nil {
		return access.Reply{}, err
	}
	return access.ListReply(items, page), nil
}

func (s *Server) provider(r *http.Request) (access.Reply, error) {
	if _, err := s.Access.Principal(r, s.Access.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	return s.detailReply(r.Context(), s.Access.Pool, id)
}

func (s *Server) createProvider(r *http.Request) (access.Reply, error) {
	a := s.Access
	var input createRequest
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	tx, err := a.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := a.Principal(r, tx, "configure")
	if err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := a.Replay(r, tx, p, input)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	if err = access.ValidText("name", input.Name, 100); err != nil {
		return access.Reply{}, err
	}
	input.Configuration.normalize()
	if err = input.Configuration.validate(s.Egress); err != nil {
		return access.Reply{}, err
	}
	if input.Configuration.credentialRequired() && input.Credential == nil {
		return access.Reply{}, access.Invalid("credential", "This authentication mode requires a credential.")
	}
	if !input.Configuration.credentialRequired() && input.Credential != nil {
		return access.Reply{}, access.Invalid("credential", "The none authentication mode takes no credential.")
	}
	if input.Credential != nil {
		if err = validCredential(*input.Credential); err != nil {
			return access.Reply{}, err
		}
	}
	if input.Model != nil {
		if err = validModelName("model", *input.Model); err != nil {
			return access.Reply{}, err
		}
		if input.DisplayName == nil || *input.DisplayName == "" {
			input.DisplayName = input.Model
		}
		if err = validModelName("display_name", *input.DisplayName); err != nil {
			return access.Reply{}, err
		}
	}
	id, etag := access.NewID(), access.NewID()
	configuration, err := json.Marshal(input.Configuration)
	if err != nil {
		return access.Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp_go.providers(id,name,kind,state,configuration,etag,slots_etag,created_by) VALUES($1,$2,$3,'draft',$4,$5,$6,$7)", id, input.Name, input.Configuration.Kind, configuration, etag, access.NewID(), p.ID); err != nil {
		return access.Reply{}, err
	}
	var credentialID *string
	if input.Credential != nil {
		stored, _, err := s.storeCredential(r.Context(), tx, id, *input.Credential)
		if err != nil {
			return access.Reply{}, err
		}
		credentialID = &stored
	}
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp_go.provider_slots(id,provider_id,is_default,position,name,credential_id) VALUES($1,$2,true,0,'default',$3)", access.NewID(), id, credentialID); err != nil {
		return access.Reply{}, err
	}
	if input.Model != nil {
		declared, _ := json.Marshal([]storedCapability{{Operation: OperationGeneration, Surface: SurfaceOpenAI, Mode: ModeUnary, Source: "declared"}, {Operation: OperationGeneration, Surface: SurfaceOpenAI, Mode: ModeStreaming, Source: "declared"}})
		if _, err = tx.Exec(r.Context(), "INSERT INTO olp_go.provider_models(id,provider_id,upstream_model,display_name,enabled,capabilities) VALUES($1,$2,$3,$4,true,$5)", access.NewID(), id, *input.Model, *input.DisplayName, declared); err != nil {
			return access.Reply{}, err
		}
	}
	if err = access.Audit(r.Context(), tx, r, p.ID, "provider.create", "provider", id, "success"); err != nil {
		return access.Reply{}, err
	}
	result := access.Reply{Status: 201, Location: "/api/v3/providers/" + id, ETag: etag, Body: map[string]any{"id": id, "name": input.Name, "kind": input.Configuration.Kind, "state": "draft", "etag": etag, "model": input.Model}}
	if err = a.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}

// invalidateEvidence downgrades every certified capability after a transport
// change. Slot evidence is already bound to the transport fingerprint; retaining
// it lets a restored draft reuse validation only when all inputs match again.
func invalidateEvidence(ctx context.Context, tx pgx.Tx, providerID string) error {
	_, err := tx.Exec(ctx, "UPDATE olp_go.provider_models SET capabilities=(SELECT coalesce(jsonb_agg(c||'{\"source\":\"declared\",\"certified_at\":null}'::jsonb),'[]'::jsonb) FROM jsonb_array_elements(capabilities) c) WHERE provider_id=$1", providerID)
	return err
}

func (s *Server) updateProvider(r *http.Request) (access.Reply, error) {
	a := s.Access
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input updateRequest
	if err = access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	tx, err := a.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := a.Principal(r, tx, "configure")
	if err != nil {
		return access.Reply{}, err
	}
	current, err := load(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, current.ETag); err != nil {
		return access.Reply{}, err
	}
	if err = access.ValidText("name", input.Name, 100); err != nil {
		return access.Reply{}, err
	}
	input.Configuration.normalize()
	if err = input.Configuration.validate(s.Egress); err != nil {
		return access.Reply{}, err
	}
	if input.Configuration.transportFingerprint() != current.Configuration.transportFingerprint() {
		if err = invalidateEvidence(r.Context(), tx, id); err != nil {
			return access.Reply{}, err
		}
	}
	configuration, err := json.Marshal(input.Configuration)
	if err != nil {
		return access.Reply{}, err
	}
	etag := access.NewID()
	if _, err = tx.Exec(r.Context(), "UPDATE olp_go.providers SET name=$2,kind=$3,configuration=$4,etag=$5,draft_dirty=true,updated_at=now() WHERE id=$1", id, input.Name, input.Configuration.Kind, configuration, etag); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.ID, "provider.update", "provider", id, "success"); err != nil {
		return access.Reply{}, err
	}
	result, err := s.detailReply(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}

// mutation runs a locked, replay-protected provider mutation that needs
// If-Match and Idempotency-Key.
func (s *Server) mutation(r *http.Request, action string, fn func(ctx context.Context, tx pgx.Tx, p access.Principal, current *record) (access.Reply, error)) (access.Reply, error) {
	a := s.Access
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := a.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := a.Principal(r, tx, "configure")
	if err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := a.Replay(r, tx, p, nil)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	current, err := load(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, current.ETag); err != nil {
		return access.Reply{}, err
	}
	result, err := fn(r.Context(), tx, p, current)
	if err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.ID, action, "provider", id, "success"); err != nil {
		return access.Reply{}, err
	}
	if err = a.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}

type storedCapability struct {
	Operation             string     `json:"operation"`
	Surface               string     `json:"surface"`
	Mode                  string     `json:"mode"`
	Source                string     `json:"source"`
	CertifiedAt           *time.Time `json:"certified_at"`
	CredentialFingerprint string     `json:"credential_fingerprint,omitempty"`
}

type storedModel struct {
	ID            string             `json:"id"`
	UpstreamModel string             `json:"upstream_model"`
	DisplayName   string             `json:"display_name"`
	Enabled       bool               `json:"enabled"`
	Capabilities  []storedCapability `json:"capabilities"`
	DiscoveredAt  *time.Time         `json:"discovered_at"`
}

func loadModels(ctx context.Context, q access.Queryer, providerID string, enabledOnly bool) ([]storedModel, error) {
	rows, err := q.Query(ctx, "SELECT id::text,upstream_model,display_name,enabled,capabilities,discovered_at FROM olp_go.provider_models WHERE provider_id=$1 AND (enabled OR NOT $2) ORDER BY upstream_model", providerID, enabledOnly)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var models []storedModel
	for rows.Next() {
		var m storedModel
		var capabilities []byte
		if err = rows.Scan(&m.ID, &m.UpstreamModel, &m.DisplayName, &m.Enabled, &capabilities, &m.DiscoveredAt); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(capabilities, &m.Capabilities); err != nil {
			return nil, err
		}
		if m.Capabilities == nil {
			m.Capabilities = []storedCapability{}
		}
		models = append(models, m)
	}
	return models, rows.Err()
}

type slotRow struct {
	ID                   string
	Default              bool
	Position             int
	Name                 string
	Enabled              bool
	Priority             int
	Weight               int64
	CredentialID         *string
	CredentialVersion    *int
	CredentialRevoked    bool
	Restrictions         slotRestrictions
	Limits               Limits
	ValidatedAt          *time.Time
	ValidatedFingerprint *string
}

type slotRestrictions struct {
	AllowedAPIKeys []string `json:"allowed_api_keys"`
	AllowedModels  []string `json:"allowed_models"`
	AllowedRoutes  []string `json:"allowed_routes"`
}

func loadSlots(ctx context.Context, q access.Queryer, providerID string) ([]slotRow, error) {
	rows, err := q.Query(ctx, "SELECT s.id::text,s.is_default,s.position,s.name,s.enabled,s.priority,s.weight,s.credential_id::text,c.version,c.revoked_at IS NOT NULL,s.restrictions,s.limits,s.validated_at,s.validated_fingerprint FROM olp_go.provider_slots s LEFT JOIN olp_go.provider_credentials c ON c.id=s.credential_id WHERE s.provider_id=$1 ORDER BY s.position", providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var slots []slotRow
	for rows.Next() {
		var row slotRow
		var restrictions, limits []byte
		var revoked *bool
		if err = rows.Scan(&row.ID, &row.Default, &row.Position, &row.Name, &row.Enabled, &row.Priority, &row.Weight, &row.CredentialID, &row.CredentialVersion, &revoked, &restrictions, &limits, &row.ValidatedAt, &row.ValidatedFingerprint); err != nil {
			return nil, err
		}
		row.CredentialRevoked = revoked != nil && *revoked
		if err = json.Unmarshal(restrictions, &row.Restrictions); err == nil {
			err = json.Unmarshal(limits, &row.Limits)
		}
		if err != nil {
			return nil, err
		}
		slots = append(slots, row)
	}
	return slots, rows.Err()
}

func (row *slotRow) published(authMode string) runtime.RevisionSlot {
	slot := runtime.Slot{ID: row.ID, Name: row.Name, Enabled: row.Enabled, Priority: row.Priority, Weight: row.Weight, AllowedModels: row.Restrictions.AllowedModels, AllowedRoutes: row.Restrictions.AllowedRoutes, AllowedAPIKeys: row.Restrictions.AllowedAPIKeys, RequestsPerMinute: row.Limits.RequestsPerMinute, TokensPerMinute: row.Limits.TokensPerMinute, MaxConcurrency: row.Limits.MaxConcurrency}
	if authMode != AuthNone {
		slot.Enabled = slot.Enabled && !row.CredentialRevoked
		slot.CredentialID = row.CredentialID
		slot.CredentialVersion = row.CredentialVersion
	}
	return runtime.RevisionSlot{Slot: slot, Default: row.Default}
}

func (s *Server) activateProvider(r *http.Request) (access.Reply, error) {
	return s.mutation(r, "provider.activate", func(ctx context.Context, tx pgx.Tx, p access.Principal, current *record) (access.Reply, error) {
		if err := current.Configuration.validate(s.Egress); err != nil {
			return access.Reply{}, err
		}
		if _, _, err := s.credentialFor(ctx, tx, current); err != nil {
			return access.Reply{}, err
		}
		models, err := loadModels(ctx, tx, current.ID, true)
		if err != nil {
			return access.Reply{}, err
		}
		if len(models) == 0 {
			return access.Reply{}, access.Fail(422, "no_enabled_models", "Enable and certify at least one model before activating.")
		}
		published := make([]runtime.RevisionModel, 0, len(models))
		for _, m := range models {
			if len(m.Capabilities) == 0 {
				return access.Reply{}, access.Fail(422, "certification_required", "Model "+m.UpstreamModel+" declares no capabilities.")
			}
			rm := runtime.RevisionModel{ID: m.ID, UpstreamModel: m.UpstreamModel, DisplayName: m.DisplayName}
			for _, c := range m.Capabilities {
				if c.Source != "certified" {
					return access.Reply{}, access.Fail(422, "certification_required", "Model "+m.UpstreamModel+" has an uncertified "+c.Operation+"/"+c.Surface+"/"+c.Mode+" capability.")
				}
				rm.Capabilities = append(rm.Capabilities, runtime.RevisionCapabilty{Operation: c.Operation, Surface: c.Surface, Mode: c.Mode, Source: c.Source, CertifiedAt: c.CertifiedAt})
			}
			published = append(published, rm)
		}
		slots, err := loadSlots(ctx, tx, current.ID)
		if err != nil {
			return access.Reply{}, err
		}
		var credentialVersion *int
		revisionSlots := make([]runtime.RevisionSlot, 0, len(slots))
		for i := range slots {
			slot := slots[i].published(current.Configuration.AuthMode)
			if slot.Enabled && current.Configuration.credentialRequired() && slots[i].validationFingerprint(&current.Configuration, models) != "" {
				if slot.CredentialID == nil {
					return access.Reply{}, access.Fail(422, "credential_required", "Slot "+slot.Name+" has no credential.")
				}
				if slots[i].validationTime(&current.Configuration, models) == nil {
					return access.Reply{}, access.Fail(422, "slot_validation_required", "Validate slot "+slot.Name+" for its enabled models before activating.")
				}
			}
			if slot.Default {
				credentialVersion = slot.CredentialVersion
			}
			revisionSlots = append(revisionSlots, slot)
		}
		modelsJSON, _ := json.Marshal(published)
		slotsJSON, _ := json.Marshal(revisionSlots)
		configuration, _ := json.Marshal(current.Configuration)
		revisionID, etag := access.NewID(), access.NewID()
		var revision int
		if err = tx.QueryRow(ctx, "INSERT INTO olp_go.provider_revisions(id,provider_id,revision,name,configuration,models,slots,credential_version,source_etag,activated_by) VALUES($1,$2,(SELECT coalesce(max(revision),0)+1 FROM olp_go.provider_revisions WHERE provider_id=$2),$3,$4,$5,$6,$7,$8,$9) RETURNING revision", revisionID, current.ID, current.Name, configuration, modelsJSON, slotsJSON, credentialVersion, current.ETag, p.ID).Scan(&revision); err != nil {
			return access.Reply{}, err
		}
		if _, err = tx.Exec(ctx, "UPDATE olp_go.providers SET state='active',active_revision=$2,active_revision_id=$3,draft_dirty=false,etag=$4,updated_at=now() WHERE id=$1", current.ID, revision, revisionID, etag); err != nil {
			return access.Reply{}, err
		}
		generation, err := runtime.Publish(ctx, tx, p.ID)
		if err != nil {
			return access.Reply{}, err
		}
		return access.Detail(map[string]any{"id": current.ID, "state": "active", "etag": etag, "runtime_generation": generation}, etag), nil
	})
}

func (s *Server) disableProvider(r *http.Request) (access.Reply, error) {
	return s.mutation(r, "provider.disable", func(ctx context.Context, tx pgx.Tx, p access.Principal, current *record) (access.Reply, error) {
		if current.State != "active" {
			return access.Reply{}, access.Fail(409, "invalid_state", "Only active connections can be disabled.")
		}
		etag := access.NewID()
		if _, err := tx.Exec(ctx, "UPDATE olp_go.providers SET state='disabled',etag=$2,updated_at=now() WHERE id=$1", current.ID, etag); err != nil {
			return access.Reply{}, err
		}
		generation, err := runtime.Publish(ctx, tx, p.ID)
		if err != nil {
			return access.Reply{}, err
		}
		return access.Detail(map[string]any{"provider_id": current.ID, "etag": etag, "credential_id": nil, "credential_version": nil, "runtime_generation": generation}, etag), nil
	})
}

type revisionRow struct {
	ID                string
	ProviderID        string
	Revision          int
	Name              string
	Configuration     Configuration
	Models            []runtime.RevisionModel
	Slots             []runtime.RevisionSlot
	CredentialVersion *int
	SourceETag        string
	ActivatedBy       string
	ActivatedAt       time.Time
}

const revisionColumns = "id::text,provider_id::text,revision,name,configuration,models,slots,credential_version,source_etag::text,activated_by::text,activated_at"

func scanRevision(row pgx.Row) (*revisionRow, error) {
	var v revisionRow
	var configuration, models, slots []byte
	if err := row.Scan(&v.ID, &v.ProviderID, &v.Revision, &v.Name, &configuration, &models, &slots, &v.CredentialVersion, &v.SourceETag, &v.ActivatedBy, &v.ActivatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(configuration, &v.Configuration); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(models, &v.Models); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(slots, &v.Slots); err != nil {
		return nil, err
	}
	v.Configuration.normalize()
	v.ActivatedAt = v.ActivatedAt.UTC()
	return &v, nil
}

// loadRevision accepts a revision id or number scoped to the provider.
func loadRevision(ctx context.Context, q access.Queryer, providerID, ref string) (*revisionRow, error) {
	if n, err := strconv.Atoi(ref); err == nil {
		return scanRevision(q.QueryRow(ctx, "SELECT "+revisionColumns+" FROM olp_go.provider_revisions WHERE provider_id=$1 AND revision=$2", providerID, n))
	}
	id, err := access.ParseUUID(ref)
	if err != nil {
		return nil, access.Fail(404, "not_found", "Unknown revision.")
	}
	return scanRevision(q.QueryRow(ctx, "SELECT "+revisionColumns+" FROM olp_go.provider_revisions WHERE provider_id=$1 AND id=$2", providerID, id))
}

func (v *revisionRow) counts() (models, capabilities, certified int64) {
	for _, m := range v.Models {
		models++
		for _, c := range m.Capabilities {
			capabilities++
			if c.Source == "certified" {
				certified++
			}
		}
	}
	return models, capabilities, certified
}

func (v *revisionRow) summary() map[string]any {
	models, capabilities, certified := v.counts()
	return map[string]any{"id": v.ID, "provider_id": v.ProviderID, "revision": v.Revision, "name": v.Name, "kind": v.Configuration.Kind, "connector_ready": true, "activated_by": v.ActivatedBy, "activated_at": v.ActivatedAt, "model_count": models, "enabled_model_count": models, "capability_count": capabilities, "certified_capability_count": certified, "historical_credential_version": v.CredentialVersion}
}

func (v *revisionRow) detail() map[string]any {
	m := v.summary()
	delete(m, "kind")
	m["configuration"] = v.Configuration
	m["source_etag"] = v.SourceETag
	return m
}

func (s *Server) revisions(r *http.Request) (access.Reply, error) {
	if _, err := s.Access.Principal(r, s.Access.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	if _, err = load(r.Context(), s.Access.Pool, id, false); err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT "+revisionColumns+" FROM olp_go.provider_revisions WHERE provider_id=$1 AND id<$2 ORDER BY id DESC LIMIT $3", id, page.Before, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		v, err := scanRevision(rows)
		if err != nil {
			return access.Reply{}, err
		}
		items = append(items, v.summary())
	}
	if err = rows.Err(); err != nil {
		return access.Reply{}, err
	}
	return access.ListReply(items, page), nil
}

func (s *Server) revision(r *http.Request) (access.Reply, error) {
	if _, err := s.Access.Principal(r, s.Access.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	v, err := loadRevision(r.Context(), s.Access.Pool, id, r.PathValue("revision_id"))
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(v.detail()), nil
}

func (s *Server) revisionModels(r *http.Request) (access.Reply, error) {
	if _, err := s.Access.Principal(r, s.Access.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	v, err := loadRevision(r.Context(), s.Access.Pool, id, r.PathValue("revision_id"))
	if err != nil {
		return access.Reply{}, err
	}
	items := []map[string]any{}
	for _, m := range v.Models {
		if m.ID >= page.Before {
			continue
		}
		capabilities := []map[string]any{}
		for _, c := range m.Capabilities {
			capabilities = append(capabilities, map[string]any{"operation": c.Operation, "surface": c.Surface, "mode": c.Mode, "source": c.Source, "certified_at": c.CertifiedAt})
		}
		items = append(items, map[string]any{"id": m.ID, "upstream_model": m.UpstreamModel, "display_name": m.DisplayName, "enabled": true, "capabilities": capabilities, "discovered_at": nil})
	}
	slices.SortFunc(items, func(a, b map[string]any) int { return strings.Compare(b["id"].(string), a["id"].(string)) })
	if len(items) > page.Limit+1 {
		items = items[:page.Limit+1]
	}
	return access.ListReply(items, page), nil
}

func capabilityKey(model string, c runtime.RevisionCapabilty) string {
	return model + ":" + c.Operation + "/" + c.Surface + "/" + c.Mode
}

func (s *Server) revisionDiff(r *http.Request) (access.Reply, error) {
	if _, err := s.Access.Principal(r, s.Access.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	from, err := loadRevision(r.Context(), s.Access.Pool, id, r.URL.Query().Get("from"))
	if err != nil {
		return access.Reply{}, err
	}
	to, err := loadRevision(r.Context(), s.Access.Pool, id, r.URL.Query().Get("to"))
	if err != nil {
		return access.Reply{}, err
	}
	a, b := from.Configuration, to.Configuration
	fromModels := map[string]runtime.RevisionModel{}
	for _, m := range from.Models {
		fromModels[m.UpstreamModel] = m
	}
	toModels := map[string]runtime.RevisionModel{}
	for _, m := range to.Models {
		toModels[m.UpstreamModel] = m
	}
	added, removed, changed, capsAdded, capsRemoved := []string{}, []string{}, []string{}, []string{}, []string{}
	fromCaps, toCaps := map[string]bool{}, map[string]bool{}
	for name, m := range fromModels {
		for _, c := range m.Capabilities {
			fromCaps[capabilityKey(name, c)] = true
		}
		if _, ok := toModels[name]; !ok {
			removed = append(removed, name)
		}
	}
	for name, m := range toModels {
		for _, c := range m.Capabilities {
			toCaps[capabilityKey(name, c)] = true
		}
		before, ok := fromModels[name]
		if !ok {
			added = append(added, name)
			continue
		}
		x, _ := json.Marshal(before)
		y, _ := json.Marshal(m)
		if string(x) != string(y) {
			changed = append(changed, name)
		}
	}
	for key := range toCaps {
		if !fromCaps[key] {
			capsAdded = append(capsAdded, key)
		}
	}
	for key := range fromCaps {
		if !toCaps[key] {
			capsRemoved = append(capsRemoved, key)
		}
	}
	for _, list := range []*[]string{&added, &removed, &changed, &capsAdded, &capsRemoved} {
		slices.Sort(*list)
	}
	return access.OK(map[string]any{
		"from_revision": from.Revision, "to_revision": to.Revision,
		"name_changed":          from.Name != to.Name,
		"endpoint_changed":      deref(a.Endpoint) != deref(b.Endpoint),
		"cloud_context_changed": deref(a.CloudRegion) != deref(b.CloudRegion) || deref(a.CloudProject) != deref(b.CloudProject),
		"deployment_changed":    deref(a.Deployment) != deref(b.Deployment),
		"api_version_changed":   deref(a.APIVersion) != deref(b.APIVersion),
		"connector_changed":     a.Kind != b.Kind || a.AuthMode != b.AuthMode || !slices.Equal(a.Options.CredentialHeaders, b.Options.CredentialHeaders),
		"credential_changed":    deref(from.CredentialVersion) != deref(to.CredentialVersion),
		"models_added":          added, "models_removed": removed, "models_changed": changed,
		"capabilities_added": capsAdded, "capabilities_removed": capsRemoved,
	}), nil
}

func deref[T comparable](v *T) T {
	var zero T
	if v == nil {
		return zero
	}
	return *v
}

// restoreDraft rewrites the draft configuration and models from a revision.
// Credentials are never restored; the draft keeps its current slots.
func (s *Server) restoreDraft(ctx context.Context, tx pgx.Tx, current *record, v *revisionRow) (string, error) {
	// A revision only ever records evidence gathered against its own
	// transport, so restoring it restores that evidence unchanged; slot
	// validation is keyed by fingerprint and re-evaluates itself on read.
	configuration, _ := json.Marshal(v.Configuration)
	// Published model identities also anchor route targets. Remove only models
	// that have never been published, and disable the retained rows until the
	// restored revision selects them again. Evidence outside that revision is
	// no longer certified against the restored transport.
	if _, err := tx.Exec(ctx, `DELETE FROM olp_go.provider_models WHERE provider_id=$1 AND id NOT IN (
		SELECT (m->>'id')::uuid FROM olp_go.provider_revisions r,jsonb_array_elements(r.models) m WHERE r.provider_id=$1
	)`, current.ID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE olp_go.provider_models SET enabled=false,
		capabilities=(SELECT coalesce(jsonb_agg(c||'{"source":"declared","certified_at":null}'::jsonb),'[]'::jsonb) FROM jsonb_array_elements(capabilities) c)
		WHERE provider_id=$1`, current.ID); err != nil {
		return "", err
	}
	for _, m := range v.Models {
		capabilities := make([]storedCapability, 0, len(m.Capabilities))
		for _, c := range m.Capabilities {
			capabilities = append(capabilities, storedCapability{Operation: c.Operation, Surface: c.Surface, Mode: c.Mode, Source: c.Source, CertifiedAt: c.CertifiedAt})
		}
		encoded, _ := json.Marshal(capabilities)
		if _, err := tx.Exec(ctx, `INSERT INTO olp_go.provider_models(id,provider_id,upstream_model,display_name,enabled,capabilities) VALUES($1,$2,$3,$4,true,$5)
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
		published := make([]runtime.RevisionSlot, 0, len(slots))
		for i := range slots {
			published = append(published, slots[i].published(v.Configuration.AuthMode))
		}
		draftSlots, _ := json.Marshal(published)
		activeSlots, _ := json.Marshal(v.Slots)
		dirty = string(draftSlots) != string(activeSlots)
	}
	_, err := tx.Exec(ctx, "UPDATE olp_go.providers SET name=$2,kind=$3,configuration=$4,etag=$5,draft_dirty=$6,updated_at=now() WHERE id=$1", current.ID, v.Name, v.Configuration.Kind, configuration, etag, dirty)
	return etag, err
}

func (s *Server) restoreActiveAsDraft(r *http.Request) (access.Reply, error) {
	return s.mutation(r, "provider.restore", func(ctx context.Context, tx pgx.Tx, p access.Principal, current *record) (access.Reply, error) {
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

func (s *Server) restoreRevisionAsDraft(r *http.Request) (access.Reply, error) {
	ref := r.PathValue("revision_id")
	return s.mutation(r, "provider.restore", func(ctx context.Context, tx pgx.Tx, p access.Principal, current *record) (access.Reply, error) {
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
