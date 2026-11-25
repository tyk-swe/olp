package codeforward

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/domain/bodylimit"
	"github.com/tyk-swe/olp/internal/gateway/codeforward/coderesearch"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/release"
)

const researchTimeout = 2 * time.Minute

type researchIngress struct {
	authority keyauthority.Authority
	release   *release.Release
	route     codemode.Route
	service   string
	session   string
	method    string
	body      []byte
}

// serveCodeResearch is deliberately independent of inference admission, model
// selection and conversation pinning. Every HTTP request rechecks live authority.
func (f *Forwarder) serveCodeResearch(w http.ResponseWriter, r *http.Request) {
	refuse := func(err error) { codeWriteError(w, r, err) }
	if !f.gate.Admit(r.Context()) {
		refuse(codemode.Refuse(503, "code_overloaded"))
		return
	}
	defer f.gate.Release(r.Context())
	in, err := f.admitResearchRequest(w, r)
	if err != nil {
		refuse(err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), researchTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	// Request has already restricted notifications to the supported lifecycle
	// and progress methods. They cannot start tool work.
	attempt, err := f.prepareResearch(r, in.release, in.route, in.service, in.authority, strings.HasPrefix(in.method, "notifications/"))
	if err != nil {
		refuse(err)
		return
	}
	defer attempt.finish(ctx)
	lastEvent, upstreamSession, done, err := f.resolveResearchSession(ctx, w, r, in, attempt)
	if err != nil {
		refuse(err)
		return
	}
	if done {
		return
	}
	f.dispatchResearch(ctx, w, r, in, attempt, lastEvent, upstreamSession, refuse)
}

func (f *Forwarder) admitResearchRequest(w http.ResponseWriter, r *http.Request) (researchIngress, error) {
	if !researchOriginAllowed(r) || r.URL.RawPath != "" || r.URL.RawQuery != "" {
		return researchIngress{}, codemode.Refuse(403, "code_research_origin_denied")
	}
	if r.Method != http.MethodPost && r.Method != http.MethodGet && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "POST, GET, DELETE")
		return researchIngress{}, codemode.Refuse(405, "code_research_method_denied")
	}
	authority, failure := f.gate.Authenticate(r, "inference")
	if failure != nil {
		return researchIngress{}, codemode.Refuse(failure.Status, "code_authentication_refused")
	}
	rel := f.runtime.Release()
	if rel == nil || rel.Snapshot() == nil {
		return researchIngress{}, codemode.Refuse(503, "code_research_unavailable")
	}
	route, found := rel.Snapshot().FindCodeRoute(r.PathValue("slug"))
	service := r.PathValue("service")
	if !found || !route.Enabled || !route.Tools.Enables(service) || !authority.Allows("inference", route.Slug, &route.ProjectID, f.now()) {
		return researchIngress{}, codemode.Refuse(404, "code_research_disabled")
	}
	if f.research == nil || f.researchAuth == nil {
		return researchIngress{}, codemode.Refuse(503, "code_research_unavailable")
	}
	session := r.Header.Get("Mcp-Session-Id")
	for _, name := range []string{"Mcp-Session-Id", "Mcp-Protocol-Version"} {
		if values := r.Header.Values(name); len(values) > 1 || len(values) == 1 && !coderesearch.ValidSession(values[0]) {
			return researchIngress{}, codemode.Refuse(400, "code_research_header_invalid")
		}
	}
	body, method, err := f.readResearchBody(w, r, route, service, session)
	if err != nil {
		return researchIngress{}, err
	}
	return researchIngress{authority: authority, release: rel, route: route, service: service, session: session, method: method, body: body}, nil
}

func (f *Forwarder) readResearchBody(w http.ResponseWriter, r *http.Request, route codemode.Route, service, session string) ([]byte, string, error) {
	if r.Method != http.MethodPost {
		return nil, "", nil
	}
	media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if media != "application/json" || r.Header.Get("Content-Encoding") != "" && r.Header.Get("Content-Encoding") != "identity" {
		return nil, "", codemode.Refuse(415, "code_research_content_type_invalid")
	}
	limit := bodylimit.Lower(min(f.codeBodyLimit(), coderesearch.MaxRequest), route.MaxBodyBytes)
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(30 * time.Second))
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	if err != nil || int64(len(raw)) > limit {
		return nil, "", codemode.Refuse(413, "code_research_body_invalid")
	}
	body, method, err := coderesearch.Request(raw, service)
	if err != nil {
		if errors.Is(err, coderesearch.ErrProtocol) {
			return nil, "", codemode.Refuse(400, "code_research_message_invalid")
		}
		return nil, "", err
	}
	if method == "initialize" && session != "" {
		return nil, "", codemode.Refuse(400, "code_research_session_invalid")
	}
	return body, method, nil
}

func (f *Forwarder) resolveResearchSession(ctx context.Context, w http.ResponseWriter, r *http.Request, in researchIngress, attempt *researchAttempt) (string, string, bool, error) {
	if in.method != "initialize" && in.session == "" {
		return "", "", false, codemode.Refuse(400, "code_research_session_required")
	}
	lastEvent := ""
	if values := r.Header.Values("Last-Event-Id"); len(values) != 0 {
		if len(values) != 1 || r.Method != http.MethodGet || in.session == "" {
			return "", "", false, codemode.Refuse(400, "code_research_resume_invalid")
		}
		opened, err := coderesearch.OpenEventID(in.session, values[0], attempt.auth.Headers.Get("Authorization"))
		if err != nil {
			return "", "", false, codemode.Refuse(400, "code_research_resume_invalid")
		}
		lastEvent = opened
	}
	upstreamSession := ""
	if in.session != "" {
		resolved, err := f.research.ResearchSession(ctx, in.session, attempt.binding)
		if err != nil {
			return "", "", false, err
		}
		upstreamSession = resolved
	}
	if r.Method == http.MethodDelete && upstreamSession == "" {
		// Stateless vendors have no session to terminate. Retire the local
		// authorization state without requiring an upstream DELETE capability.
		if err := f.research.DeleteResearchSession(ctx, in.session, attempt.binding.APIKeyID); err != nil {
			return "", "", false, err
		}
		attempt.dispatched = true // Count the completed request in rate limits.
		w.WriteHeader(http.StatusNoContent)
		return "", "", true, nil
	}
	return lastEvent, upstreamSession, false, nil
}

func (f *Forwarder) dispatchResearch(ctx context.Context, w http.ResponseWriter, r *http.Request, in researchIngress, attempt *researchAttempt, lastEvent, upstreamSession string, refuse func(error)) {
	target := codemode.ResearchEndpoint(in.service)
	if _, err := f.egress.ValidateEndpoint(target); err != nil {
		refuse(codemode.Refuse(502, "code_egress_refused"))
		return
	}
	client, err := f.codeClient(ctx, in.release, attempt.config, attempt.account.ProviderID)
	if err != nil {
		refuse(err)
		return
	}
	defer client.CloseIdleConnections()
	request, err := buildResearchUpstreamRequest(ctx, r, target, in.body, attempt.auth.Headers, lastEvent, upstreamSession)
	if err != nil {
		refuse(err)
		return
	}
	attempt.dispatched = true
	response, err := client.Do(request)
	if err != nil {
		// Dispatch is never replayed, even if no response headers arrived.
		refuse(codemode.Refuse(502, "code_research_upstream_unavailable"))
		return
	}
	defer response.Body.Close()
	session, err := f.applyResearchSessionResponse(ctx, w, r, in, attempt, response, upstreamSession)
	if err != nil {
		refuse(err)
		return
	}
	initialized := false
	if in.method == "initialize" && session != "" {
		defer func() {
			if !initialized {
				// Client cancellation and transport failure must not strand a
				// tentative handshake in the key'f bounded session allowance.
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
				defer cancel()
				if err := f.research.DeleteResearchSession(cleanup, session, attempt.binding.APIKeyID); err != nil {
					f.log.Warn("research session cleanup unavailable", "session_id", session)
				}
			}
		}()
	}
	forwarding := researchResponse{
		method: in.method, service: in.service, resumeKey: attempt.auth.Headers.Get("Authorization"), requestID: coderesearch.MessageID(in.body),
		answerPing: func(reply []byte) error {
			return f.answerResearchPing(r, in.release, in.route, in.service, in.authority, attempt, session, reply)
		},
	}
	initialized = f.copyResearchResponse(w, r, response, forwarding)
}

func buildResearchUpstreamRequest(ctx context.Context, r *http.Request, target string, body []byte, authHeaders http.Header, lastEvent, upstreamSession string) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, r.Method, target, &codeRequestBody{data: body})
	if err != nil {
		return nil, codemode.Refuse(502, "code_connection_invalid")
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
	for name, values := range authHeaders {
		request.Header[name] = values
	}
	return request, nil
}

func (f *Forwarder) applyResearchSessionResponse(ctx context.Context, w http.ResponseWriter, r *http.Request, in researchIngress, attempt *researchAttempt, response *http.Response, upstreamSession string) (string, error) {
	session := in.session
	if response.StatusCode == http.StatusNotFound || r.Method == http.MethodDelete && response.StatusCode >= 200 && response.StatusCode < 300 {
		if session != "" {
			if err := f.research.DeleteResearchSession(ctx, session, attempt.binding.APIKeyID); err != nil {
				return "", codemode.Refuse(503, "code_research_session_unavailable")
			}
		}
	}
	upstreamID := response.Header.Get("Mcp-Session-Id")
	if upstreamID != "" {
		if !coderesearch.ValidSession(upstreamID) || len(response.Header.Values("Mcp-Session-Id")) != 1 ||
			(in.method != "initialize" && upstreamID != upstreamSession) {
			return "", codemode.Refuse(502, "code_research_session_invalid")
		}
	}
	if in.method == "initialize" && response.StatusCode == http.StatusOK {
		// An OLP session also fences stateless upstreams: event IDs and stream
		// resumes must not cross caller keys sharing one subscription bearer.
		created, err := f.research.CreateResearchSession(ctx, attempt.binding, upstreamID)
		if err != nil {
			return "", err
		}
		session = created
		w.Header().Set("Mcp-Session-Id", session)
	} else if upstreamID != "" && session != "" {
		w.Header().Set("Mcp-Session-Id", session)
	}
	return session, nil
}
