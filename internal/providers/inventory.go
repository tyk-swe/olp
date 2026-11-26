package providers

import (
	"cmp"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/operationregistry"
	"github.com/tyk-swe/olp/internal/domain/ptr"
	"github.com/tyk-swe/olp/internal/domain/vendors"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/providers/probe"
)

type providerKindsReply struct {
	Items []connection.ListedKind `json:"items"`
}

type kindCapabilitiesReply struct {
	ProviderKind connectors.Kind         `json:"provider_kind"`
	Capabilities []connection.Capability `json:"capabilities"`
}

func (s *inventoryService) kinds(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	return httpkit.OK(providerKindsReply{Items: connection.Kinds()}), nil
}

func (s *inventoryService) kindCapabilities(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	kind := connectors.Kind(r.PathValue("provider_kind"))
	if _, ok := connection.LookupKind(kind); !ok {
		return httpkit.Reply{}, httpkit.Fail(400, "invalid_provider_kind", "This provider kind is not available.")
	}
	return httpkit.OK(kindCapabilitiesReply{ProviderKind: kind, Capabilities: probe.OptionsForKind(kind)}), nil
}

func (s *inventoryService) listVendors(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	return httpkit.OK(connection.Vendors()), nil
}

func (s *inventoryService) inventory(r *http.Request, principal authz.Principal) (httpkit.Reply, error) {
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	query := r.URL.Query()
	search := strings.TrimSpace(query.Get("search"))
	if len(search) > 100 {
		return httpkit.Reply{}, httpkit.Fail(400, "invalid_query", "Use at most 100 search characters.")
	}
	surface := query.Get("surface")
	switch surface {
	case "", "openai", "anthropic", "gemini", "bedrock", "native":
	default:
		return httpkit.Reply{}, httpkit.Fail(400, "invalid_query", "Unknown surface.")
	}
	var enabled *bool
	if raw := query.Get("enabled"); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return httpkit.Reply{}, httpkit.Fail(400, "invalid_query", "enabled must be true or false.")
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
		return httpkit.Reply{}, err
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
		canonical, err := canonicalModel(metadata)
		if err != nil {
			return inventoryItem{}, err
		}
		item.Metadata, item.Model = facts, m.reply()
		item.Lifecycle = s.catalog.Lifecycle(cmp.Or(vendorID, vendors.DefaultFor(item.ProviderKind)), m.UpstreamModel, canonical)
		return item, nil
	})
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.ListReplyBy(items, page, func(item inventoryItem) string { return item.Model.ID }), nil
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

type providerProfilesReply struct {
	Items []connectors.Profile `json:"items"`
}

func (s *inventoryService) profiles(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	pluginProfiles, err := plugins.Profiles(r.Context(), s.access.Pool, s.unconfined)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.OK(providerProfilesReply{Items: ptr.NonNil(slices.Concat(connectors.Profiles(), pluginProfiles))}), nil
}

type operationDialectReply struct {
	ID                string          `json:"id"`
	Revision          string          `json:"revision"`
	Operation         string          `json:"operation"`
	OperationRevision string          `json:"operation_revision"`
	Surface           string          `json:"surface"`
	Mode              string          `json:"mode"`
	Label             string          `json:"label"`
	RequestSchema     json.RawMessage `json:"request_schema"`
	ResultSchema      json.RawMessage `json:"result_schema"`
	Documentation     string          `json:"documentation"`
	Evidence          string          `json:"evidence"`
}

type operationDialectsReply struct {
	Items []operationDialectReply `json:"items"`
}

func (s *inventoryService) operationDialects(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	dialects := operationregistry.Catalog().Dialects()
	items := make([]operationDialectReply, len(dialects))
	for i, d := range dialects {
		items[i] = operationDialectReply{
			ID:                d.Identity.ID,
			Revision:          d.Identity.Revision,
			Operation:         d.Operation.ID,
			OperationRevision: d.Operation.Revision,
			Surface:           d.Surface,
			Mode:              "unary",
			Label:             d.Label,
			RequestSchema:     d.RequestSchema,
			ResultSchema:      d.ResultSchema,
			Documentation:     d.Documentation,
			Evidence:          d.Evidence,
		}
	}
	return httpkit.OK(operationDialectsReply{Items: items}), nil
}
