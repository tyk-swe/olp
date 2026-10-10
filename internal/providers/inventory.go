package providers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/vendors"
)

func (s *Server) kinds(r *http.Request, _ access.Principal) (access.Reply, error) {
	return access.OK(map[string]any{"items": kinds}), nil
}

func (s *Server) kindCapabilities(r *http.Request, _ access.Principal) (access.Reply, error) {
	kind := r.PathValue("provider_kind")
	if kindByName(kind) == nil {
		return access.Reply{}, access.Fail(400, "invalid_provider_kind", "This provider kind is not available.")
	}
	return access.OK(map[string]any{"provider_kind": kind, "capabilities": capabilityOptionsForKind(kind)}), nil
}

func (s *Server) listVendors(r *http.Request, _ access.Principal) (access.Reply, error) {
	return access.OK(vendorCatalogue), nil
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
	case "", SurfaceOpenAI, "anthropic", "gemini", "bedrock", "native":
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
	rows, err := s.Access.Pool.Query(r.Context(), `SELECT m.id::text,m.upstream_model,m.display_name,m.enabled,m.capabilities,m.discovered_at,p.id::text,p.name,p.kind,
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
	items := []map[string]any{}
	for rows.Next() {
		var m storedModel
		var capabilities, metadata []byte
		var providerID, providerName, providerKind, vendorID string
		var available bool
		if err = rows.Scan(&m.ID, &m.UpstreamModel, &m.DisplayName, &m.Enabled, &capabilities, &m.DiscoveredAt, &providerID, &providerName, &providerKind, &available, &metadata, &vendorID); err != nil {
			return access.Reply{}, err
		}
		if err = json.Unmarshal(capabilities, &m.Capabilities); err != nil {
			return access.Reply{}, err
		}
		facts, metadataErr := metadataJSON(metadata)
		if metadataErr != nil {
			return access.Reply{}, metadataErr
		}
		if vendorID == "" {
			vendorID = vendors.DefaultFor(providerKind)
		}
		items = append(items, map[string]any{"available": available, "metadata": facts, "provider_id": providerID, "provider_name": providerName, "provider_kind": providerKind, "model": modelJSON(m),
			"lifecycle": s.Catalog.Lifecycle(vendorID, m.UpstreamModel, canonicalModel(metadata))})
	}
	if err = rows.Err(); err != nil {
		return access.Reply{}, err
	}
	return access.ListReplyBy(items, page, func(item map[string]any) string {
		return item["model"].(map[string]any)["id"].(string)
	}), nil
}

// generations lists installation-wide runtime releases, which span every
// project and name their publishers, so it requires installation reach.
func (s *Server) generations(r *http.Request, principal access.Principal) (access.Reply, error) {
	var err error
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT jsonb_build_object('id',g.id,'sequence',g.sequence,'sha256',g.sha256,'created_by',g.created_by,'created_by_email',u.email,'created_at',g.created_at) FROM olp.runtime_releases g JOIN olp.users u ON u.id=g.created_by WHERE g.id<$1 ORDER BY g.id DESC LIMIT $2", page.Before, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	items, err := access.JSONRows(rows)
	if err != nil {
		return access.Reply{}, err
	}
	return access.ListReply(items, page), nil
}

// Register mounts the provider surface.
func (s *Server) Register(mux *http.ServeMux) {
	s.registerCodeMode(mux)
	s.Access.Route(mux, "GET /api/v1/provider-profiles", s.profiles)
	s.Access.Route(mux, "GET /api/v1/operation-dialects", s.operationDialects)
	s.Access.Route(mux, "GET /api/v1/provider-kinds", s.kinds)
	s.Access.Route(mux, "GET /api/v1/provider-kinds/{provider_kind}/capabilities", s.kindCapabilities)
	s.Access.Route(mux, "GET /api/v1/provider-vendors", s.listVendors)
	s.Access.Route(mux, "GET /api/v1/provider-models", s.inventory)
	s.Access.Route(mux, "GET /api/v1/runtime-generations", s.generations)
	s.Access.Route(mux, "GET /api/v1/providers", s.providers)
	s.Access.Route(mux, "POST /api/v1/providers", s.createProvider, access.MaxBody(1<<20))
	s.Access.Route(mux, "GET /api/v1/providers/{provider_id}", s.provider)
	s.Access.Route(mux, "PATCH /api/v1/providers/{provider_id}", s.updateProvider, access.MaxBody(1<<20))
	s.Access.Route(mux, "DELETE /api/v1/providers/{provider_id}", s.deleteProvider)
	s.Access.Route(mux, "POST /api/v1/providers/{provider_id}/activate", s.activateProvider)
	s.Access.Route(mux, "POST /api/v1/providers/{provider_id}/disable", s.disableProvider)
	// Each advertised capability, or each declared model of a provider without
	// discovery, can consume a full probe budget; reserve another management
	// budget for preparation, queueing, and persistence.
	certifyTimeout := time.Duration(len(CapabilityOptions)+1) * probeTimeout
	s.Access.Route(mux, "POST /api/v1/providers/{provider_id}/probe", s.probe, access.Deadline(certifyTimeout))
	s.Access.Route(mux, "POST /api/v1/providers/{provider_id}/discovery", s.discover, access.MaxBody(1<<20), access.Deadline(certifyTimeout))
	s.Access.Route(mux, "GET /api/v1/providers/{provider_id}/catalog-suggestions", s.catalogSuggestions)
	s.Access.Route(mux, "POST /api/v1/providers/{provider_id}/catalog-suggestions/accept", s.acceptCatalogSuggestions, access.MaxBody(1<<20))
	s.Access.Route(mux, "POST /api/v1/providers/{provider_id}/restore-as-draft", s.restoreActiveAsDraft)
	s.Access.Route(mux, "GET /api/v1/providers/{provider_id}/models", s.models)
	s.Access.Route(mux, "PATCH /api/v1/providers/{provider_id}/models/{model_id}", s.setModel)
	s.Access.Route(mux, "POST /api/v1/providers/{provider_id}/models/{model_id}/certify", s.certify, access.MaxBody(65536), access.Deadline(certifyTimeout))
	s.Access.Route(mux, "GET /api/v1/providers/{provider_id}/network-credentials", s.networkCredentials)
	s.Access.Route(mux, "POST /api/v1/providers/{provider_id}/network-credentials", s.createNetworkCredential, access.MaxBody(256<<10))
	s.Access.Route(mux, "POST /api/v1/providers/{provider_id}/network-credentials/{credential_id}/revoke", s.revokeNetworkCredential)
	s.Access.Route(mux, "GET /api/v1/providers/{provider_id}/credentials", s.credentials)
	s.Access.Route(mux, "POST /api/v1/providers/{provider_id}/credentials", s.rotate, access.MaxBody(65536), access.Deadline(certifyTimeout))
	s.Access.Route(mux, "POST /api/v1/providers/{provider_id}/credentials/{credential_id}/revoke", s.revoke)
	s.Access.Route(mux, "POST /api/v1/providers/{provider_id}/grant-enrollments", s.startGrantEnrollment, access.MaxBody(65536), access.Deadline(grantStepTimeout))
	s.Access.Route(mux, "POST /api/v1/providers/{provider_id}/grant-enrollments/{enrollment_id}/continue", s.continueGrantEnrollment, access.MaxBody(65536), access.Deadline(grantStepTimeout))
	s.Access.Route(mux, "POST /api/v1/providers/{provider_id}/grant-enrollments/{enrollment_id}/poll", s.pollGrantEnrollment, access.MaxBody(65536), access.Deadline(grantStepTimeout))
	s.Access.Route(mux, "DELETE /api/v1/providers/{provider_id}/grant-enrollments/{enrollment_id}", s.cancelGrantEnrollment)
	s.Access.Route(mux, "GET /api/v1/providers/{provider_id}/credential-slots", s.slots)
	s.Access.Route(mux, "GET /api/v1/providers/{provider_id}/credential-slots/{slot_id}", s.getSlot)
	s.Access.Route(mux, "DELETE /api/v1/providers/{provider_id}/credential-slots/{slot_id}", s.deleteSlot)
	s.Access.Route(mux, "PUT /api/v1/providers/{provider_id}/credential-slots/{slot_id}", s.writeSlot)
	s.Access.Route(mux, "POST /api/v1/providers/{provider_id}/credential-slots/{slot_id}/validate", s.validateSlot, access.MaxBody(65536), access.Deadline(certifyTimeout))
	s.Access.Route(mux, "GET /api/v1/providers/{provider_id}/revisions", s.revisions)
	s.Access.Route(mux, "GET /api/v1/providers/{provider_id}/revisions/diff", s.revisionDiff)
	s.Access.Route(mux, "GET /api/v1/providers/{provider_id}/revisions/{revision_id}", s.revision)
	s.Access.Route(mux, "GET /api/v1/providers/{provider_id}/revisions/{revision_id}/models", s.revisionModels)
	s.Access.Route(mux, "POST /api/v1/providers/{provider_id}/revisions/{revision_id}/restore-as-draft", s.restoreRevisionAsDraft)
}

// profiles lists the built-in profiles and those of usable plugins.
func (s *Server) profiles(r *http.Request, _ access.Principal) (access.Reply, error) {
	pluginProfiles, err := plugins.Profiles(r.Context(), s.Access.Pool, s.Unconfined)
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(map[string]any{"items": append(connectors.Profiles(), pluginProfiles...)}), nil
}
