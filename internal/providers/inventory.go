package providers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
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
	return access.OK(map[string]any{"provider_kind": kind, "capabilities": capabilityOptions}), nil
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
		coalesce(p.configuration->'options'->'models'->m.upstream_model,'{}'::jsonb)
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
		items = append(items, map[string]any{"available": available, "metadata": json.RawMessage(metadata), "provider_id": providerID, "provider_name": providerName, "provider_kind": providerKind, "model": modelJSON(m)})
	}
	if err = rows.Err(); err != nil {
		return access.Reply{}, err
	}
	return access.ListReplyBy(items, page, func(item map[string]any) string {
		return item["model"].(map[string]any)["id"].(string)
	}), nil
}

func (s *Server) health(r *http.Request) (access.Reply, error) {
	if _, err := s.Access.Principal(r, s.Access.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	window := 15
	if raw := r.URL.Query().Get("window_minutes"); raw != "" {
		if window, err = strconv.Atoi(raw); err != nil || window < 1 || window > 1440 {
			return access.Reply{}, access.Fail(400, "invalid_query", "window_minutes must be from 1 to 1440.")
		}
	}
	var stats map[string]HealthStats
	if s.Health != nil {
		stats = s.Health.ProviderHealth(time.Duration(window) * time.Minute)
	}
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT id::text,name,kind,state,last_probe_at,last_probe_status,last_probe_detail FROM olp_go.providers WHERE id<$1 ORDER BY id DESC LIMIT $2", page.Before, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, name, kind, state string
		var probeAt *time.Time
		var probeStatus, probeDetail *string
		if err = rows.Scan(&id, &name, &kind, &state, &probeAt, &probeStatus, &probeDetail); err != nil {
			return access.Reply{}, err
		}
		st := stats[id]
		status := "idle"
		var average any
		var last any
		if st.Attempts > 0 {
			average = float64(st.TotalLatency.Milliseconds()) / float64(st.Attempts)
			last = st.LastAttemptAt.UTC()
			failures := st.Attempts - st.Successes
			switch {
			case failures == 0:
				status = "healthy"
			case failures*2 >= st.Attempts:
				status = "unhealthy"
			default:
				status = "degraded"
			}
		}
		items = append(items, map[string]any{"provider_id": id, "provider_name": name, "provider_kind": kind, "provider_state": state, "status": status, "attempt_count": st.Attempts, "success_count": st.Successes, "rate_limit_count": st.RateLimits, "server_error_count": st.ServerErrors, "transport_error_count": st.TransportErrors, "average_latency_ms": average, "last_attempt_at": last, "last_probe_at": probeAt, "last_probe_status": probeStatus, "last_probe_detail": probeDetail})
	}
	if err = rows.Err(); err != nil {
		return access.Reply{}, err
	}
	reply := access.ListReplyBy(items, page, func(item map[string]any) string { return item["provider_id"].(string) })
	reply.Body.(map[string]any)["window_minutes"] = window
	return reply, nil
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
	mux.HandleFunc("GET /api/v3/provider-kinds", h(s.kinds))
	mux.HandleFunc("GET /api/v3/provider-kinds/{provider_kind}/capabilities", h(s.kindCapabilities))
	mux.HandleFunc("GET /api/v3/provider-vendors", h(s.vendors))
	mux.HandleFunc("GET /api/v3/provider-models", h(s.inventory))
	mux.HandleFunc("GET /api/v3/provider-health", h(s.health))
	mux.HandleFunc("GET /api/v3/runtime-generations", h(s.generations))
	mux.HandleFunc("GET /api/v3/providers", h(s.providers))
	mux.HandleFunc("POST /api/v3/providers", s.Access.HandleWith(1<<20, s.createProvider))
	mux.HandleFunc("GET /api/v3/providers/{provider_id}", h(s.provider))
	mux.HandleFunc("PATCH /api/v3/providers/{provider_id}", s.Access.HandleWith(1<<20, s.updateProvider))
	mux.HandleFunc("POST /api/v3/providers/{provider_id}/activate", h(s.activateProvider))
	mux.HandleFunc("POST /api/v3/providers/{provider_id}/disable", h(s.disableProvider))
	mux.HandleFunc("POST /api/v3/providers/{provider_id}/probe", h(s.probe))
	mux.HandleFunc("POST /api/v3/providers/{provider_id}/discovery", s.Access.HandleWith(1<<20, s.discover))
	mux.HandleFunc("POST /api/v3/providers/{provider_id}/restore-as-draft", h(s.restoreActiveAsDraft))
	mux.HandleFunc("GET /api/v3/providers/{provider_id}/models", h(s.models))
	mux.HandleFunc("PATCH /api/v3/providers/{provider_id}/models/{model_id}", h(s.setModel))
	// Each advertised capability can consume a full probe budget; reserve
	// another management budget for preparation, queueing, and persistence.
	certifyTimeout := time.Duration(len(capabilityOptions)+1) * probeTimeout
	mux.HandleFunc("POST /api/v3/providers/{provider_id}/models/{model_id}/certify", s.Access.HandleTimeout(65536, certifyTimeout, s.certify))
	mux.HandleFunc("GET /api/v3/providers/{provider_id}/credentials", h(s.credentials))
	mux.HandleFunc("POST /api/v3/providers/{provider_id}/credentials", h(s.rotate))
	mux.HandleFunc("POST /api/v3/providers/{provider_id}/credentials/{credential_id}/revoke", h(s.revoke))
	mux.HandleFunc("GET /api/v3/providers/{provider_id}/credential-slots", h(s.slots))
	mux.HandleFunc("PUT /api/v3/providers/{provider_id}/credential-slots/{slot_id}", h(s.writeSlot))
	mux.HandleFunc("POST /api/v3/providers/{provider_id}/credential-slots/{slot_id}/validate", h(s.validateSlot))
	mux.HandleFunc("GET /api/v3/providers/{provider_id}/revisions", h(s.revisions))
	mux.HandleFunc("GET /api/v3/providers/{provider_id}/revisions/diff", h(s.revisionDiff))
	mux.HandleFunc("GET /api/v3/providers/{provider_id}/revisions/{revision_id}", h(s.revision))
	mux.HandleFunc("GET /api/v3/providers/{provider_id}/revisions/{revision_id}/models", h(s.revisionModels))
	mux.HandleFunc("POST /api/v3/providers/{provider_id}/revisions/{revision_id}/restore-as-draft", h(s.restoreRevisionAsDraft))
}
