package gateway

import (
	"context"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/bodylimit"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codewire"
)

// RegisterCode mounts raw code-mode transport, which requires CodeLedger and
// CodeAuthorizer. A route serves the paths of its accounts' adapters, and
// each request goes to an account whose adapter serves its path. It never
// invokes ordinary request preparation, credential failover, response
// transformation or retries.
func (s *Server) RegisterCode(mux *http.ServeMux) {
	mux.HandleFunc("/code/{slug}/mcp/{service}", s.serveCodeResearch)
	mux.HandleFunc("/code/{slug}/{operation...}", s.serveCode)
}

func (s *Server) serveCode(w http.ResponseWriter, r *http.Request) {
	if !s.admit(r.Context()) {
		codeWriteError(w, r, codemode.Refuse(503, "code_overloaded"))
		return
	}
	defer s.release(r.Context())
	authority, failure := s.authenticate(r, "inference")
	if failure != nil {
		codeWriteError(w, r, codemode.Refuse(failure.Status, "code_authentication_refused"))
		return
	}
	release := s.Runtime.Release()
	if release == nil || release.Snapshot == nil {
		codeWriteError(w, r, codemode.Refuse(503, "code_runtime_unavailable"))
		return
	}
	route, ok := release.Snapshot.CodeRoutes[r.PathValue("slug")]
	if !ok {
		codeWriteError(w, r, codemode.Refuse(404, "code_route_unavailable"))
		return
	}
	refuse := func(err error) { s.codeRefuse(w, r, route, authority.ID, authority.EndUserDigest, err) }
	if !route.Enabled || !authority.Allows("inference", route.Slug, &route.ProjectID, s.now()) {
		refuse(codemode.Refuse(404, "code_route_unavailable"))
		return
	}
	if _, failure = s.parseAttribution(r, authority); failure != nil {
		refuse(codemode.Refuse(failure.Status, "code_"+failure.Code))
		return
	}
	path := r.PathValue("operation")
	if r.URL.RawPath != "" {
		refuse(codemode.Refuse(400, "code_operation_unsupported"))
		return
	}
	adapters := release.Snapshot.CodeAdapters(route)
	if len(adapters) == 0 {
		refuse(codemode.Refuse(503, "code_adapter_unavailable"))
		return
	}
	ingress, ok := codeIngressFor(path)
	if ok && r.Method == http.MethodGet && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") && len(release.Snapshot.CodeProviders(route, path, codeadapter.WebSocket)) != 0 {
		s.codeWebSocket(w, r, release, route, authority, ingress)
		return
	}
	if r.Method != http.MethodPost || !ok || len(release.Snapshot.CodeProviders(route, path, codeadapter.HTTP)) == 0 {
		if codeProbe(r.Method, path) && len(release.Snapshot.CodeProviders(route, "v1/messages", codeadapter.HTTP)) != 0 {
			codeWriteError(w, r, codemode.Refuse(404, "code_operation_unsupported"))
			return
		}
		refuse(codemode.Refuse(400, "code_operation_unsupported"))
		return
	}
	limit := bodylimit.Lower(s.codeBodyLimit(), route.MaxBodyBytes)
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(30 * time.Second))
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		refuse(codemode.Refuse(400, "code_body_invalid"))
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	if len(r.Header.Values("Content-Encoding")) > 1 {
		refuse(codemode.Refuse(415, "code_encoding_unsupported"))
		return
	}
	body, err := codewire.Decode(raw, r.Header.Get("Content-Encoding"), limit)
	if err != nil {
		refuse(err)
		return
	}
	observation, err := classifyCode(ingress, body, r.Header, false)
	if err != nil {
		refuse(err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), codeGenerationTimeout)
	defer cancel()
	attempt, err := s.prepareCode(r.WithContext(ctx), release, route, observation, ingress, codeadapter.HTTP)
	if err != nil {
		refuse(err)
		return
	}
	defer attempt.finish(ctx)
	client, err := s.codeClient(r.Context(), release, attempt.config, attempt.permit.Account.ProviderID)
	if err != nil {
		refuse(err)
		return
	}
	defer client.CloseIdleConnections()
	target, err := s.codeEndpoint(attempt.config, attempt.endpoint.UpstreamPath, r.URL.RawQuery)
	if err != nil {
		refuse(err)
		return
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, &codeRequestBody{data: raw})
	if err != nil {
		refuse(codemode.Refuse(502, "code_connection_invalid"))
		return
	}
	request.ContentLength = int64(len(raw))
	request.Header = attempt.headers(r.Header)
	request.Trailer = codewire.ForwardTrailers(r.Trailer, r.Header, true)
	if len(request.Trailer) != 0 {
		request.ContentLength = -1
		request.Header.Del("Content-Length")
	}
	// A non-rewindable body and a fresh HTTP/1 connection prevent net/http replay.
	request.GetBody = nil
	request.Close = true
	if err := attempt.dispatch(ctx); err != nil {
		refuse(err)
		return
	}
	response, err := client.Do(request)
	if err != nil {
		attempt.transportFailed(ctx, err)
		refuse(codemode.Refuse(502, "code_upstream_unavailable"))
		return
	}
	defer response.Body.Close()
	attempt.response(ctx, response.StatusCode, response.Header)
	maps.Copy(w.Header(), codewire.ForwardHeaders(response.Header, false))
	for name := range codewire.ForwardTrailers(response.Trailer, response.Header, false) {
		w.Header().Add("Trailer", name)
	}
	w.WriteHeader(response.StatusCode)
	attempt.copyResponse(ctx, w, response)
	for name, values := range codewire.ForwardTrailers(response.Trailer, response.Header, false) {
		w.Header()[http.TrailerPrefix+name] = values
	}
}

func (s *Server) codeBodyLimit() int64 {
	if s.cfg.MaxBodyBytes > 0 {
		return min(codewire.MaxBody, s.cfg.MaxBodyBytes)
	}
	return codewire.MaxBody
}

// codeRequestBody drops the sent request bytes at EOF: the response keeps the
// outbound request reachable for the whole stream. It is never rewound.
type codeRequestBody struct{ data []byte }

func (b *codeRequestBody) Read(p []byte) (int, error) {
	if len(b.data) == 0 {
		b.data = nil
		return 0, io.EOF
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}

func (b *codeRequestBody) Close() error { return nil }

func (a *codeAttempt) copyResponse(ctx context.Context, w http.ResponseWriter, response *http.Response) {
	streaming := strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream")
	encoded := response.Header.Get("Content-Encoding")
	events := codewire.NewEvents(codewire.MaxBody, func(data []byte) { a.observe(a.observer.Event(data)) })
	var observed []byte
	overflow := false
	buffer := make([]byte, 32<<10)
	rc := http.NewResponseController(w)
	for {
		n, err := response.Body.Read(buffer)
		if n > 0 {
			part := buffer[:n]
			if streaming && (encoded == "" || encoded == "identity") {
				events.Write(part)
			} else if !overflow {
				if len(observed)+n <= codewire.MaxBody {
					observed = append(observed, part...)
				} else {
					overflow = true
					observed = nil
				}
			}
			_ = rc.SetWriteDeadline(time.Now().Add(responseWriteTimeout))
			if _, err := w.Write(part); err != nil {
				a.outcome(ctx, codemode.Outcome{Origin: "client", Kind: "canceled"})
				return
			}
			if err := rc.Flush(); err != nil {
				a.outcome(ctx, codemode.Outcome{Origin: "client", Kind: "canceled"})
				return
			}
		}
		if err != nil {
			if err == io.EOF && !overflow && (!streaming || encoded != "" && encoded != "identity") {
				if decoded, err := codewire.Decode(observed, encoded, codewire.MaxBody); err == nil {
					if streaming {
						events.Write(decoded)
					} else {
						a.observe(a.observer.Unary(decoded))
					}
				}
			}
			if err == io.EOF && !a.terminal {
				if o, ok := a.observer.Finish(); ok {
					a.observe(o)
				}
			}
			if !a.terminal && ctx.Err() == nil {
				outcome := codemode.Outcome{Origin: "gateway", Kind: "transport_error"}
				if err == io.EOF {
					outcome.Origin, outcome.Kind = "upstream", "incomplete"
				}
				a.outcome(ctx, outcome)
			}
			return
		}
	}
}
