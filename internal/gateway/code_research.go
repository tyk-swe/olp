package gateway

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/bodylimit"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/coderesearch"
)

const researchTimeout = 2 * time.Minute

// serveCodeResearch is deliberately independent of inference admission, model
// selection and conversation pinning. Every HTTP request rechecks live authority.
func (s *Server) serveCodeResearch(w http.ResponseWriter, r *http.Request) {
	refuse := func(err error) { codeWriteError(w, r, err) }
	if !s.admit(r.Context()) {
		refuse(codemode.Refuse(503, "code_overloaded"))
		return
	}
	defer s.release(r.Context())
	if !researchOriginAllowed(r) || r.URL.RawPath != "" || r.URL.RawQuery != "" {
		refuse(codemode.Refuse(403, "code_research_origin_denied"))
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodGet && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "POST, GET, DELETE")
		refuse(codemode.Refuse(405, "code_research_method_denied"))
		return
	}
	authority, failure := s.authenticate(r, "inference")
	if failure != nil {
		refuse(codemode.Refuse(failure.Status, "code_authentication_refused"))
		return
	}
	release := s.Runtime.Release()
	if release == nil || release.Snapshot == nil {
		refuse(codemode.Refuse(503, "code_research_unavailable"))
		return
	}
	route, found := release.Snapshot.CodeRoutes[r.PathValue("slug")]
	service := r.PathValue("service")
	if !found || !route.Enabled || !route.Tools.Enables(service) || !authority.Allows("inference", route.Slug, &route.ProjectID, s.now()) {
		refuse(codemode.Refuse(404, "code_research_disabled"))
		return
	}
	if s.CodeResearch == nil || s.ResearchAuth == nil {
		refuse(codemode.Refuse(503, "code_research_unavailable"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), researchTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	session := r.Header.Get("Mcp-Session-Id")
	for _, name := range []string{"Mcp-Session-Id", "Mcp-Protocol-Version"} {
		if values := r.Header.Values(name); len(values) > 1 || len(values) == 1 && !coderesearch.ValidSession(values[0]) {
			refuse(codemode.Refuse(400, "code_research_header_invalid"))
			return
		}
	}
	var body []byte
	var err error
	method := ""
	if r.Method == http.MethodPost {
		media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if media != "application/json" || r.Header.Get("Content-Encoding") != "" && r.Header.Get("Content-Encoding") != "identity" {
			refuse(codemode.Refuse(415, "code_research_content_type_invalid"))
			return
		}
		limit := bodylimit.Lower(min(s.codeBodyLimit(), coderesearch.MaxRequest), route.MaxBodyBytes)
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(30 * time.Second))
		body, err = io.ReadAll(io.LimitReader(r.Body, limit+1))
		_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
		if err != nil || int64(len(body)) > limit {
			refuse(codemode.Refuse(413, "code_research_body_invalid"))
			return
		}
		body, method, err = coderesearch.Request(body, service)
		if err != nil {
			if errors.Is(err, coderesearch.ErrProtocol) {
				err = codemode.Refuse(400, "code_research_message_invalid")
			}
			refuse(err)
			return
		}
		if method == "initialize" && session != "" {
			refuse(codemode.Refuse(400, "code_research_session_invalid"))
			return
		}
	}
	// Request has already restricted notifications to the supported lifecycle
	// and progress methods. They cannot start tool work.
	attempt, err := s.prepareResearch(r, release, route, service, authority, strings.HasPrefix(method, "notifications/"))
	if err != nil {
		refuse(err)
		return
	}
	defer attempt.finish(ctx)
	if method != "initialize" && session == "" {
		refuse(codemode.Refuse(400, "code_research_session_required"))
		return
	}
	lastEvent := ""
	if values := r.Header.Values("Last-Event-Id"); len(values) != 0 {
		if len(values) != 1 || r.Method != http.MethodGet || session == "" {
			refuse(codemode.Refuse(400, "code_research_resume_invalid"))
			return
		}
		lastEvent, err = coderesearch.OpenEventID(session, values[0], attempt.auth.Headers.Get("Authorization"))
		if err != nil {
			refuse(codemode.Refuse(400, "code_research_resume_invalid"))
			return
		}
	}
	upstreamSession := ""
	if session != "" {
		upstreamSession, err = s.CodeResearch.ResearchSession(ctx, session, attempt.binding)
		if err != nil {
			refuse(err)
			return
		}
	}
	if r.Method == http.MethodDelete && upstreamSession == "" {
		// Stateless vendors have no session to terminate. Retire the local
		// authorization state without requiring an upstream DELETE capability.
		if err = s.CodeResearch.DeleteResearchSession(ctx, session, attempt.binding.APIKeyID); err != nil {
			refuse(err)
			return
		}
		attempt.dispatched = true // Count the completed request in rate limits.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	target := codemode.ResearchEndpoint(service)
	if _, err = s.egress.ValidateEndpoint(target); err != nil {
		refuse(codemode.Refuse(502, "code_egress_refused"))
		return
	}
	client, err := s.codeClient(ctx, release, attempt.config, attempt.account.ProviderID)
	if err != nil {
		refuse(err)
		return
	}
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, r.Method, target, &codeRequestBody{data: body})
	if err != nil {
		refuse(codemode.Refuse(502, "code_connection_invalid"))
		return
	}
	request.GetBody, request.Close, request.ContentLength = nil, true, int64(len(body))
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Accept-Encoding", "identity")
	if r.Method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	}
	if value := r.Header.Get("Mcp-Protocol-Version"); value != "" {
		request.Header.Set("Mcp-Protocol-Version", value)
	}
	if lastEvent != "" {
		request.Header.Set("Last-Event-Id", lastEvent)
	}
	if upstreamSession != "" {
		request.Header.Set("Mcp-Session-Id", upstreamSession)
	}
	for name, values := range attempt.auth.Headers {
		request.Header[name] = values
	}
	attempt.dispatched = true
	response, err := client.Do(request)
	if err != nil {
		// Dispatch is never replayed, even if no response headers arrived.
		refuse(codemode.Refuse(502, "code_research_upstream_unavailable"))
		return
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound || r.Method == http.MethodDelete && response.StatusCode >= 200 && response.StatusCode < 300 {
		if session != "" {
			if err = s.CodeResearch.DeleteResearchSession(ctx, session, attempt.binding.APIKeyID); err != nil {
				refuse(codemode.Refuse(503, "code_research_session_unavailable"))
				return
			}
		}
	}
	upstreamID := response.Header.Get("Mcp-Session-Id")
	if upstreamID != "" {
		if !coderesearch.ValidSession(upstreamID) || len(response.Header.Values("Mcp-Session-Id")) != 1 ||
			(method != "initialize" && upstreamID != upstreamSession) {
			refuse(codemode.Refuse(502, "code_research_session_invalid"))
			return
		}
	}
	if method == "initialize" && response.StatusCode == http.StatusOK {
		// An OLP session also fences stateless upstreams: event IDs and stream
		// resumes must not cross caller keys sharing one subscription bearer.
		session, err = s.CodeResearch.CreateResearchSession(ctx, attempt.binding, upstreamID)
		if err != nil {
			refuse(err)
			return
		}
		w.Header().Set("Mcp-Session-Id", session)
	} else if upstreamID != "" && session != "" {
		w.Header().Set("Mcp-Session-Id", session)
	}
	initialized := false
	if method == "initialize" && session != "" {
		defer func() {
			if !initialized {
				// Client cancellation and transport failure must not strand a
				// tentative handshake in the key's bounded session allowance.
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
				defer cancel()
				if err := s.CodeResearch.DeleteResearchSession(cleanup, session, attempt.binding.APIKeyID); err != nil {
					s.log.Warn("research session cleanup unavailable", "session_id", session)
				}
			}
		}()
	}
	forwarding := researchResponse{
		method: method, service: service, resumeKey: attempt.auth.Headers.Get("Authorization"), requestID: coderesearch.MessageID(body),
		answerPing: func(reply []byte) error {
			return s.answerResearchPing(r, release, route, service, authority, attempt, session, reply)
		},
	}
	initialized = s.copyResearchResponse(w, r, response, forwarding)
}
