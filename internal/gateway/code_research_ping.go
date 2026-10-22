package gateway

import (
	"context"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/runtime"
)

// answerResearchPing answers only a validated upstream liveness request, under
// the stream's existing admission. Reauthorization fences revocations and
// generation changes without reserving another concurrency slot for the stream.
func (s *Server) answerResearchPing(r *http.Request, release *runtime.Release, route codemode.Route, service string, authority access.Authority, attempt *researchAttempt, session string, reply []byte) error {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	fresh, account, err := s.researchAuthority(r.WithContext(ctx), route, service, authority)
	if err != nil {
		return err
	}
	auth, err := s.researchAuthorization(ctx, attempt.config, account, service)
	if err != nil {
		return err
	}
	if researchBinding(fresh.ID, route, service, account, auth) != attempt.binding {
		return codemode.Refuse(409, "code_research_session_invalid")
	}
	upstream, err := s.CodeResearch.ResearchSession(ctx, session, attempt.binding)
	if err != nil {
		return err
	}
	// A separate raw connection avoids waiting behind the SSE connection when
	// the provider's per-client connection limit is one. This POST is never replayed.
	client, err := s.codeClient(ctx, release, attempt.config, account.ProviderID)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, codemode.ResearchEndpoint(service), &codeRequestBody{data: reply})
	if err != nil {
		return err
	}
	request.ContentLength, request.Close, request.GetBody = int64(len(reply)), true, nil
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Accept-Encoding", "identity")
	if upstream != "" {
		request.Header.Set("Mcp-Session-Id", upstream)
	}
	if protocol := r.Header.Get("Mcp-Protocol-Version"); protocol != "" {
		request.Header.Set("Mcp-Protocol-Version", protocol)
	}
	for name, values := range auth.Headers {
		request.Header[name] = values
	}
	response, err := client.Do(request)
	if err != nil {
		return codemode.Refuse(502, "code_research_upstream_unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted && response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusOK {
		return codemode.Refuse(502, "code_research_ping_refused")
	}
	return nil
}
