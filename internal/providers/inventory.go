package providers

import (
	"cmp"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/providers/probe"
	"github.com/tyk-swe/olp/internal/vendors"
)

func (s *Server) kinds(r *http.Request, _ access.Principal) (access.Reply, error) {
	return access.OK(map[string]any{"items": connection.Kinds()}), nil
}

func (s *Server) kindCapabilities(r *http.Request, _ access.Principal) (access.Reply, error) {
	kind := r.PathValue("provider_kind")
	if _, ok := connection.LookupKind(kind); !ok {
		return access.Reply{}, access.Fail(400, "invalid_provider_kind", "This provider kind is not available.")
	}
	return access.OK(map[string]any{"provider_kind": kind, "capabilities": probe.OptionsForKind(kind)}), nil
}

func (s *Server) listVendors(r *http.Request, _ access.Principal) (access.Reply, error) {
	return access.OK(connection.Vendors()), nil
}

func (s *Server) inventory(r *http.Request, principal access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	query := r.URL.Query()
	search := strings.TrimSpace(query.Get("search"))
	if len(search) > 100 {
		return access.Reply{}, access.Fail(400, "invalid_query", "Use at most 100 search characters.")
	}
	surface := query.Get("surface")
	switch surface {
	case "", "openai", "anthropic", "gemini", "bedrock", "native":
	default:
		return access.Reply{}, access.Fail(400, "invalid_query", "Unknown surface.")
	}
	var enabled *bool
	if raw := query.Get("enabled"); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return access.Reply{}, access.Fail(400, "invalid_query", "enabled must be true or false.")
		}
		enabled = &v
	}
	rows, err := s.access.Pool.Query(r.Context(), `SELECT m.id::text,m.upstream_model,m.display_name,m.enabled,m.capabilities,m.discovered_at,p.id::text,p.name,p.kind,
		p.state='active' AND EXISTS (
			SELECT 1 FROM jsonb_array_elements(r.models) published,jsonb_array_elements(published->'capabilities') c
			WHERE published->>'id'=m.id::text AND c->>'source'='certified' AND ($4='' OR c->>'surface'=$4)
		),
		coalesce(p.configuration->'options'->'models'->m.upstream_model,'{}'::json),
		coalesce(p.configuration->'options'->>'vendor_id','')
		FROM olp.provider_models m JOIN olp.providers p ON p.id=m.provider_id
		LEFT JOIN olp.provider_revisions r ON r.id=p.active_revision_id
		WHERE m.id<$1 AND ($2='' OR m.upstream_model ILIKE '%'||$2||'%' OR m.display_name ILIKE '%'||$2||'%' OR p.name ILIKE '%'||$2||'%')
		AND ($3::boolean IS NULL OR m.enabled=$3) AND ($4='' OR EXISTS(SELECT 1 FROM jsonb_array_elements(m.capabilities) c WHERE c->>'surface'=$4))
		AND ($6 OR p.project_id=ANY($7::uuid[]))
		ORDER BY m.id DESC LIMIT $5`, page.Before, search, enabled, surface, page.Limit+1, principal.AllProjects, principal.ProjectIDs())
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (inventoryItem, error) {
		var m storedModel
		var capabilities, metadata []byte
		var item inventoryItem
		var vendorID string
		if err := row.Scan(&m.ID, &m.UpstreamModel, &m.DisplayName, &m.Enabled, &capabilities, &m.DiscoveredAt, &item.ProviderID, &item.ProviderName, &item.ProviderKind, &item.Available, &metadata, &vendorID); err != nil {
			return inventoryItem{}, err
		}
		if err := json.Unmarshal(capabilities, &m.Capabilities); err != nil {
			return inventoryItem{}, err
		}
		facts, err := connection.MetadataJSON(metadata)
		if err != nil {
			return inventoryItem{}, err
		}
		item.Metadata, item.Model = facts, m.reply()
		item.Lifecycle = s.catalog.Lifecycle(cmp.Or(vendorID, vendors.DefaultFor(item.ProviderKind)), m.UpstreamModel, canonicalModel(metadata))
		return item, nil
	})
	if err != nil {
		return access.Reply{}, err
	}
	return access.ListReplyBy(items, page, func(item inventoryItem) string { return item.Model.ID }), nil
}

type inventoryItem struct {
	Available    bool                       `json:"available"`
	Lifecycle    *catalog.LifecycleView     `json:"lifecycle"`
	Metadata     map[string]json.RawMessage `json:"metadata"`
	Model        modelReply                 `json:"model"`
	ProviderID   string                     `json:"provider_id"`
	ProviderKind string                     `json:"provider_kind"`
	ProviderName string                     `json:"provider_name"`
}

// generations lists installation-wide runtime releases, which span every
// project and name their publishers, so it requires installation reach.
func (s *Server) generations(r *http.Request, principal access.Principal) (access.Reply, error) {
	var err error
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.access.Pool.Query(r.Context(), "SELECT jsonb_build_object('id',g.id,'sequence',g.sequence,'sha256',g.sha256,'created_by',g.created_by,'created_by_email',u.email,'created_at',g.created_at) FROM olp.runtime_releases g JOIN olp.users u ON u.id=g.created_by WHERE g.id<$1 ORDER BY g.id DESC LIMIT $2", page.Before, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	items, err := access.JSONRows(rows)
	if err != nil {
		return access.Reply{}, err
	}
	return access.ListReply(items, page), nil
}

func (s *Server) profiles(r *http.Request, _ access.Principal) (access.Reply, error) {
	pluginProfiles, err := plugins.Profiles(r.Context(), s.access.Pool, s.unconfined)
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(map[string]any{"items": append(connectors.Profiles(), pluginProfiles...)}), nil
}

func (s *Server) operationDialects(r *http.Request, _ access.Principal) (access.Reply, error) {
	items := []map[string]any{}
	for _, d := range operationregistry.Catalog().Dialects() {
		items = append(items, map[string]any{"id": d.Identity.ID, "revision": d.Identity.Revision, "operation": d.Operation.ID, "operation_revision": d.Operation.Revision, "surface": d.Surface, "mode": "unary", "label": d.Label, "request_schema": d.RequestSchema, "result_schema": d.ResultSchema, "documentation": d.Documentation, "evidence": d.Evidence})
	}
	return access.OK(map[string]any{"items": items}), nil
}
