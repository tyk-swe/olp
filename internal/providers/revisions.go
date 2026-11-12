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
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/routing"
)

type revisionRow struct {
	ID                string
	ProviderID        string
	Revision          int
	Name              string
	Configuration     connection.Configuration
	Models            []routing.RevisionModel
	Slots             []routing.RevisionSlot
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
	document, err := connection.Decode(configuration)
	if err != nil {
		return nil, err
	}
	v.Configuration = document
	if err := json.Unmarshal(models, &v.Models); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(slots, &v.Slots); err != nil {
		return nil, err
	}
	v.ActivatedAt = v.ActivatedAt.UTC()
	return &v, nil
}

// loadRevision accepts a revision id or number scoped to the provider.
func loadRevision(ctx context.Context, q access.Queryer, providerID, ref string) (*revisionRow, error) {
	if n, err := strconv.Atoi(ref); err == nil {
		return scanRevision(q.QueryRow(ctx, "SELECT "+revisionColumns+" FROM olp.provider_revisions WHERE provider_id=$1 AND revision=$2", providerID, n))
	}
	id, err := access.ParseUUID(ref)
	if err != nil {
		return nil, access.Fail(404, "not_found", "Unknown revision.")
	}
	return scanRevision(q.QueryRow(ctx, "SELECT "+revisionColumns+" FROM olp.provider_revisions WHERE provider_id=$1 AND id=$2", providerID, id))
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

type revisionSummary struct {
	ID                          string    `json:"id"`
	ProviderID                  string    `json:"provider_id"`
	Revision                    int       `json:"revision"`
	Name                        string    `json:"name"`
	ConnectorReady              bool      `json:"connector_ready"`
	ActivatedBy                 string    `json:"activated_by"`
	ActivatedAt                 time.Time `json:"activated_at"`
	ModelCount                  int64     `json:"model_count"`
	EnabledModelCount           int64     `json:"enabled_model_count"`
	CapabilityCount             int64     `json:"capability_count"`
	CertifiedCapabilityCount    int64     `json:"certified_capability_count"`
	HistoricalCredentialVersion *int      `json:"historical_credential_version"`
}

type revisionListItem struct {
	revisionSummary
	Kind string `json:"kind"`
}

type revisionDetail struct {
	revisionSummary
	Configuration connection.Configuration `json:"configuration"`
	SourceETag    string                   `json:"source_etag"`
}

func (v *revisionRow) summary() revisionSummary {
	models, capabilities, certified := v.counts()
	return revisionSummary{ID: v.ID, ProviderID: v.ProviderID, Revision: v.Revision, Name: v.Name, ConnectorReady: true, ActivatedBy: v.ActivatedBy, ActivatedAt: v.ActivatedAt, ModelCount: models, EnabledModelCount: models, CapabilityCount: capabilities, CertifiedCapabilityCount: certified, HistoricalCredentialVersion: v.CredentialVersion}
}

func (s *Server) revisions(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	if _, err = visibleProvider(r.Context(), s.access.Pool, p, id); err != nil {
		return access.Reply{}, err
	}
	rows, err := s.access.Pool.Query(r.Context(), "SELECT "+revisionColumns+" FROM olp.provider_revisions WHERE provider_id=$1 AND id<$2 ORDER BY id DESC LIMIT $3", id, page.Before, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (revisionListItem, error) {
		v, err := scanRevision(row)
		if err != nil {
			return revisionListItem{}, err
		}
		return revisionListItem{revisionSummary: v.summary(), Kind: v.Configuration.Kind}, nil
	})
	if err != nil {
		return access.Reply{}, err
	}
	return access.ListReplyBy(items, page, func(item revisionListItem) string { return item.ID }), nil
}

func (s *Server) revision(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	if _, err = visibleProvider(r.Context(), s.access.Pool, p, id); err != nil {
		return access.Reply{}, err
	}
	v, err := loadRevision(r.Context(), s.access.Pool, id, r.PathValue("revision_id"))
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(revisionDetail{revisionSummary: v.summary(), Configuration: v.Configuration, SourceETag: v.SourceETag}), nil
}

func (s *Server) revisionModels(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	if _, err = visibleProvider(r.Context(), s.access.Pool, p, id); err != nil {
		return access.Reply{}, err
	}
	v, err := loadRevision(r.Context(), s.access.Pool, id, r.PathValue("revision_id"))
	if err != nil {
		return access.Reply{}, err
	}
	items := []modelReply{}
	for _, m := range v.Models {
		if m.ID < page.Before {
			items = append(items, publishedModelReply(m))
		}
	}
	slices.SortFunc(items, func(a, b modelReply) int { return strings.Compare(b.ID, a.ID) })
	return access.ListReplyBy(items, page, modelID), nil
}

func capabilityKey(model string, c routing.RevisionCapability) string {
	return model + ":" + c.Operation + "/" + c.Surface + "/" + c.Mode
}

func (s *Server) revisionDiff(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	if _, err = visibleProvider(r.Context(), s.access.Pool, p, id); err != nil {
		return access.Reply{}, err
	}
	from, err := loadRevision(r.Context(), s.access.Pool, id, r.URL.Query().Get("from"))
	if err != nil {
		return access.Reply{}, err
	}
	to, err := loadRevision(r.Context(), s.access.Pool, id, r.URL.Query().Get("to"))
	if err != nil {
		return access.Reply{}, err
	}
	a, b := from.Configuration, to.Configuration
	fromModels := map[string]routing.RevisionModel{}
	for _, m := range from.Models {
		fromModels[m.UpstreamModel] = m
	}
	toModels := map[string]routing.RevisionModel{}
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
		"network_configuration_changed":  !sameJSON(a.Options.Network, b.Options.Network),
		"profile_changed":                a.ProfileID != b.ProfileID || a.ProfileRevision != b.ProfileRevision,
		"plugin_changed":                 a.PluginDigest() != b.PluginDigest(),
		"plugin_options_changed":         !sameJSON(a.Options.PluginOptions, b.Options.PluginOptions),
		"semantic_configuration_changed": !sameJSON(a.Options.SemanticHeaders, b.Options.SemanticHeaders) || !sameJSON(a.Options.QuerySettings, b.Options.QuerySettings) || !sameJSON(a.Options.OperationDefaults, b.Options.OperationDefaults),
		"serving_binding_changed":        !sameJSON(a.Options.Bindings, b.Options.Bindings) || routing.ObservedPrincipal(from.Slots) != routing.ObservedPrincipal(to.Slots),
		"name_changed":                   from.Name != to.Name,
		"endpoint_changed":               deref(a.Endpoint) != deref(b.Endpoint),
		"cloud_context_changed":          deref(a.CloudRegion) != deref(b.CloudRegion) || deref(a.CloudProject) != deref(b.CloudProject) || !sameJSON(a.BatchStorage, b.BatchStorage),
		"deployment_changed":             deref(a.Deployment) != deref(b.Deployment),
		"api_version_changed":            deref(a.APIVersion) != deref(b.APIVersion),
		"connector_changed":              a.Kind != b.Kind || a.AuthMode != b.AuthMode || !slices.Equal(a.Options.CredentialHeaders, b.Options.CredentialHeaders),
		"credential_changed":             deref(from.CredentialVersion) != deref(to.CredentialVersion),
		"models_added":                   added, "models_removed": removed, "models_changed": changed,
		"capabilities_added": capsAdded, "capabilities_removed": capsRemoved,
	}), nil
}

func sameJSON(a, b any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}

type ProviderSnapshot struct {
	ID                    string
	Name                  string
	Project               *string
	Kind                  string
	Configuration         []byte
	Network               *string
	RevisionConfiguration []byte
	RevisionModels        []byte
	RevisionSlots         []byte
}

type ModelSnapshot struct {
	ProviderID    string
	UpstreamModel string
	DisplayName   string
	Enabled       bool
	Capabilities  []byte
}

type SlotSnapshot struct {
	ProviderID   string
	Name         string
	ID           string
	Default      bool
	Position     int
	Enabled      bool
	Priority     int
	Weight       int64
	CredentialID *string
	Restrictions []byte
	Limits       []byte
	Usable       *string
	PluginDigest string
	ProfileID    string
}

func ReadProviderSnapshots(ctx context.Context, q access.Queryer) ([]ProviderSnapshot, error) {
	rows, err := q.Query(ctx, `SELECT p.id::text,p.name,pr.name,p.kind,p.configuration,
	(SELECT c.id::text FROM olp.provider_network_credentials c WHERE c.id::text=p.configuration#>>'{options,network,credential_id}' AND c.provider_id=p.id AND c.revoked_at IS NULL),
	r.configuration,r.models,r.slots
	FROM olp.providers p LEFT JOIN olp.projects pr ON pr.id=p.project_id
	LEFT JOIN olp.provider_revisions r ON r.id=p.active_revision_id AND p.state IN ('active','disabled')`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[ProviderSnapshot])
}

func ReadModelSnapshots(ctx context.Context, q access.Queryer) ([]ModelSnapshot, error) {
	rows, err := q.Query(ctx, `SELECT provider_id::text,upstream_model,display_name,enabled,capabilities FROM olp.provider_models ORDER BY provider_id,upstream_model`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[ModelSnapshot])
}

func ReadSlotSnapshots(ctx context.Context, q access.Queryer) ([]SlotSnapshot, error) {
	rows, err := q.Query(ctx, `SELECT s.provider_id::text,s.name,s.id::text,s.is_default,s.position,s.enabled,s.priority,s.weight,s.credential_id::text,s.restrictions,s.limits,
	c.id::text,coalesce(c.plugin_digest,''),coalesce(c.profile_id,'')
	FROM olp.provider_slots s LEFT JOIN olp.provider_credentials c ON c.id=s.credential_id AND c.revoked_at IS NULL
	AND NOT EXISTS (SELECT 1 FROM olp.provider_grants g WHERE g.credential_id=c.id AND g.lapsed_at IS NOT NULL)
	ORDER BY s.provider_id,s.position`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[SlotSnapshot])
}

type ProjectProvider struct {
	ID            string
	Configuration connection.Configuration
}

type RevisionLabels struct {
	Provider string
	Project  string
	Models   []routing.RevisionModel
}

func ReadProjectProvider(ctx context.Context, q access.Queryer, name, projectID string) (ProjectProvider, bool, error) {
	var provider ProjectProvider
	err := q.QueryRow(ctx, `SELECT id::text,configuration FROM olp.providers WHERE lower(name)=lower($1) AND project_id=$2`, name, projectID).Scan(&provider.ID, &provider.Configuration)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectProvider{}, false, nil
	}
	return provider, err == nil, err
}

func ReadActiveRevision(ctx context.Context, q access.Queryer, name, projectID string) (routing.ProviderRevision, bool, error) {
	var current routing.ProviderRevision
	err := q.QueryRow(ctx, `SELECT p.id::text,v.id::text,p.name,p.state,p.project_id::text,v.configuration,v.models,v.slots FROM olp.providers p JOIN olp.provider_revisions v ON v.id=p.active_revision_id WHERE lower(p.name)=lower($1) AND p.project_id=$2 AND p.state='active'`, name, projectID).Scan(&current.ID, &current.RevisionID, &current.Name, &current.State, &current.ProjectID, &current.Configuration, &current.Models, &current.Slots)
	if errors.Is(err, pgx.ErrNoRows) {
		return routing.ProviderRevision{}, false, nil
	}
	return current, err == nil, err
}

func ReadRevisionLabels(ctx context.Context, q access.Queryer, revisionID, providerID string) (RevisionLabels, error) {
	var revision RevisionLabels
	err := q.QueryRow(ctx, `SELECT p.name,j.name,v.models FROM olp.provider_revisions v JOIN olp.providers p ON p.id=v.provider_id JOIN olp.projects j ON j.id=p.project_id WHERE v.id=$1 AND p.id=$2`, revisionID, providerID).Scan(&revision.Provider, &revision.Project, &revision.Models)
	return revision, err
}
