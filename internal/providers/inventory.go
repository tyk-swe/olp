package providers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
)

func (s *Server) kinds(r *http.Request) (access.Reply, error) {
	if _, err := s.Access.Principal(r, s.Access.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
	return access.OK(map[string]any{"items": kinds}), nil
}

func (s *Server) kindCapabilities(r *http.Request) (access.Reply, error) {
	if _, err := s.Access.Principal(r, s.Access.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
	kind := r.PathValue("provider_kind")
	if kindByName(kind) == nil {
		return access.Reply{}, access.Fail(400, "invalid_provider_kind", "This provider kind is not available.")
	}
	return access.OK(map[string]any{"provider_kind": kind, "capabilities": capabilitiesFor(kind, defaultVendor(kind))}), nil
}

func (s *Server) vendors(r *http.Request) (access.Reply, error) {
	if _, err := s.Access.Principal(r, s.Access.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
	return access.OK(vendors), nil
}

func (s *Server) inventory(r *http.Request) (access.Reply, error) {
	if _, err := s.Access.Principal(r, s.Access.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
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
	if surface != "" && surface != SurfaceOpenAI && surface != "anthropic" && surface != "gemini" {
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
		coalesce(p.configuration->'options'->'models'->m.upstream_model,'{}'::json)
		FROM olp_go.provider_models m JOIN olp_go.providers p ON p.id=m.provider_id
		LEFT JOIN olp_go.provider_revisions r ON r.id=p.active_revision_id
		WHERE m.id<$1 AND ($2='' OR m.upstream_model ILIKE '%'||$2||'%' OR m.display_name ILIKE '%'||$2||'%' OR p.name ILIKE '%'||$2||'%')
		AND ($3::boolean IS NULL OR m.enabled=$3) AND ($4='' OR EXISTS(SELECT 1 FROM jsonb_array_elements(m.capabilities) c WHERE c->>'surface'=$4))
		ORDER BY m.id DESC LIMIT $5`, page.Before, search, enabled, surface, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var m storedModel
		var capabilities, metadata []byte
		var providerID, providerName, providerKind string
		var available bool
		if err = rows.Scan(&m.ID, &m.UpstreamModel, &m.DisplayName, &m.Enabled, &capabilities, &m.DiscoveredAt, &providerID, &providerName, &providerKind, &available, &metadata); err != nil {
			return access.Reply{}, err
		}
		if err = json.Unmarshal(capabilities, &m.Capabilities); err != nil {
			return access.Reply{}, err
		}
		facts, metadataErr := metadataJSON(metadata)
		if metadataErr != nil {
			return access.Reply{}, metadataErr
		}
		items = append(items, map[string]any{"available": available, "metadata": facts, "provider_id": providerID, "provider_name": providerName, "provider_kind": providerKind, "model": modelJSON(m)})
	}
	if err = rows.Err(); err != nil {
		return access.Reply{}, err
	}
	return access.ListReplyBy(items, page, func(item map[string]any) string {
		return item["model"].(map[string]any)["id"].(string)
	}), nil
}

func (s *Server) generations(r *http.Request) (access.Reply, error) {
	if _, err := s.Access.Principal(r, s.Access.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT jsonb_build_object('id',g.id,'sequence',g.sequence,'sha256',g.sha256,'created_by',g.created_by,'created_by_email',u.email,'created_at',g.created_at) FROM olp_go.runtime_releases g JOIN olp_go.users u ON u.id=g.created_by WHERE g.id<$1 ORDER BY g.id DESC LIMIT $2", page.Before, page.Limit+1)
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
	h := s.Access.Handle
	mux.HandleFunc("GET /api/v1/provider-profiles", h(s.profiles))
	mux.HandleFunc("GET /api/v1/operation-dialects", h(s.operationDialects))
	mux.HandleFunc("GET /api/v1/provider-kinds", h(s.kinds))
	mux.HandleFunc("GET /api/v1/provider-kinds/{provider_kind}/capabilities", h(s.kindCapabilities))
	mux.HandleFunc("GET /api/v1/provider-vendors", h(s.vendors))
	mux.HandleFunc("GET /api/v1/provider-models", h(s.inventory))
	mux.HandleFunc("GET /api/v1/runtime-generations", h(s.generations))
	mux.HandleFunc("GET /api/v1/providers", h(s.providers))
	mux.HandleFunc("POST /api/v1/providers", s.Access.HandleWith(1<<20, s.createProvider))
	mux.HandleFunc("GET /api/v1/providers/{provider_id}", h(s.provider))
	mux.HandleFunc("PATCH /api/v1/providers/{provider_id}", s.Access.HandleWith(1<<20, s.updateProvider))
	mux.HandleFunc("POST /api/v1/providers/{provider_id}/activate", h(s.activateProvider))
	mux.HandleFunc("POST /api/v1/providers/{provider_id}/disable", h(s.disableProvider))
	mux.HandleFunc("POST /api/v1/providers/{provider_id}/probe", h(s.probe))
	mux.HandleFunc("POST /api/v1/providers/{provider_id}/discovery", s.Access.HandleWith(1<<20, s.discover))
	mux.HandleFunc("POST /api/v1/providers/{provider_id}/restore-as-draft", h(s.restoreActiveAsDraft))
	mux.HandleFunc("GET /api/v1/providers/{provider_id}/models", h(s.models))
	mux.HandleFunc("PATCH /api/v1/providers/{provider_id}/models/{model_id}", h(s.setModel))
	// Each advertised capability can consume a full probe budget; reserve
	// another management budget for preparation, queueing, and persistence.
	certifyTimeout := time.Duration(len(CapabilityOptions)+1) * probeTimeout
	mux.HandleFunc("POST /api/v1/providers/{provider_id}/models/{model_id}/certify", s.Access.HandleTimeout(65536, certifyTimeout, s.certify))
	mux.HandleFunc("GET /api/v1/providers/{provider_id}/network-credentials", h(s.networkCredentials))
	mux.HandleFunc("POST /api/v1/providers/{provider_id}/network-credentials", s.Access.HandleWith(256<<10, s.createNetworkCredential))
	mux.HandleFunc("POST /api/v1/providers/{provider_id}/network-credentials/{credential_id}/revoke", h(s.revokeNetworkCredential))
	mux.HandleFunc("GET /api/v1/providers/{provider_id}/credentials", h(s.credentials))
	mux.HandleFunc("POST /api/v1/providers/{provider_id}/credentials", s.Access.HandleTimeout(65536, certifyTimeout, s.rotate))
	mux.HandleFunc("POST /api/v1/providers/{provider_id}/credentials/{credential_id}/revoke", h(s.revoke))
	mux.HandleFunc("GET /api/v1/providers/{provider_id}/credential-slots", h(s.slots))
	mux.HandleFunc("PUT /api/v1/providers/{provider_id}/credential-slots/{slot_id}", h(s.writeSlot))
	mux.HandleFunc("POST /api/v1/providers/{provider_id}/credential-slots/{slot_id}/validate", s.Access.HandleTimeout(65536, certifyTimeout, s.validateSlot))
	mux.HandleFunc("GET /api/v1/providers/{provider_id}/revisions", h(s.revisions))
	mux.HandleFunc("GET /api/v1/providers/{provider_id}/revisions/diff", h(s.revisionDiff))
	mux.HandleFunc("GET /api/v1/providers/{provider_id}/revisions/{revision_id}", h(s.revision))
	mux.HandleFunc("GET /api/v1/providers/{provider_id}/revisions/{revision_id}/models", h(s.revisionModels))
	mux.HandleFunc("POST /api/v1/providers/{provider_id}/revisions/{revision_id}/restore-as-draft", h(s.restoreRevisionAsDraft))
}

func (s *Server) profiles(r *http.Request) (access.Reply, error) {
	if _, err := s.Access.Principal(r, s.Access.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
	return access.OK(map[string]any{"items": connectors.Profiles()}), nil
}
