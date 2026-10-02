package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codexwire"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/runtime"
)

const codeGenerationTimeout = 10 * time.Minute

// CodeForwarder implements raw Codex transport. It never invokes ordinary
// request preparation, credential failover, response transformation or retries.
type CodeForwarder struct{}

func NewCodeForwarder() *CodeForwarder { return &CodeForwarder{} }

func (f *CodeForwarder) RegisterCode(mux *http.ServeMux, s *Server) {
	mux.HandleFunc("/code/{slug}/{operation...}", func(w http.ResponseWriter, r *http.Request) { f.serve(s, w, r) })
}

func (f *CodeForwarder) serve(s *Server, w http.ResponseWriter, r *http.Request) {
	if !s.admit(r.Context()) {
		codeWriteError(w, codemode.Refuse(503, "code_overloaded"))
		return
	}
	defer s.release(r.Context())
	authority, failure := s.authenticate(r, "inference")
	if failure != nil {
		codeWriteError(w, codemode.Refuse(failure.Status, "code_authentication_refused"))
		return
	}
	release := s.Runtime.Release()
	if release == nil || release.Snapshot == nil {
		codeWriteError(w, codemode.Refuse(503, "code_runtime_unavailable"))
		return
	}
	route, ok := release.Snapshot.CodeRoutes[r.PathValue("slug")]
	if !ok {
		codeWriteError(w, codemode.Refuse(404, "code_route_unavailable"))
		return
	}
	if !route.Enabled || !authority.Allows("inference", route.Slug, &route.ProjectID, s.now()) {
		codeRefuse(s, w, r, route, authority.ID, codemode.Refuse(404, "code_route_unavailable"))
		return
	}
	refuse := func(err error) { codeRefuse(s, w, r, route, authority.ID, err) }
	if s.CodeLedger == nil || s.CodeAuthorizer == nil {
		refuse(codemode.Refuse(503, "code_service_unavailable"))
		return
	}
	path := r.PathValue("operation")
	if r.URL.RawPath != "" {
		refuse(codemode.Refuse(400, "code_operation_unsupported"))
		return
	}
	if r.Method == http.MethodGet && path == "responses" && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		f.websocket(s, w, r, release, route, authority.ID)
		return
	}
	if r.Method != http.MethodPost || path != "responses" && path != "responses/compact" {
		refuse(codemode.Refuse(400, "code_operation_unsupported"))
		return
	}
	limit := int64(codexwire.MaxBody)
	if s.cfg.MaxBodyBytes > 0 {
		limit = min(limit, s.cfg.MaxBodyBytes)
	}
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
	body, err := codexwire.Decode(raw, r.Header.Get("Content-Encoding"), limit)
	if err != nil {
		refuse(err)
		return
	}
	observation, err := codexwire.Classify(body, r.Header, path, false)
	if err != nil {
		refuse(err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), codeGenerationTimeout)
	defer cancel()
	attempt, err := f.prepare(s, r.WithContext(ctx), release, route, observation)
	if err != nil {
		refuse(err)
		return
	}
	defer attempt.finish(ctx)
	client, err := f.client(s, r.Context(), release, attempt.config, attempt.permit.Account.ProviderID)
	if err != nil {
		refuse(err)
		return
	}
	defer client.CloseIdleConnections()
	target, err := codeEndpoint(s, attempt.config, path, r.URL.RawQuery)
	if err != nil {
		refuse(err)
		return
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, io.NopCloser(bytes.NewReader(raw)))
	if err != nil {
		refuse(codemode.Refuse(502, "code_connection_invalid"))
		return
	}
	request.ContentLength = int64(len(raw))
	request.Header = attempt.headers(r.Header)
	request.Trailer = codexwire.ForwardHeaders(r.Trailer, true)
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
		if ctx.Err() == nil {
			attempt.health(ctx, "unavailable")
		}
		refuse(codemode.Refuse(502, "code_upstream_unavailable"))
		return
	}
	defer response.Body.Close()
	attempt.response(ctx, response.StatusCode, response.Header)
	for name, values := range codexwire.ForwardHeaders(response.Header, false) {
		w.Header()[name] = values
	}
	w.WriteHeader(response.StatusCode)
	f.copyResponse(w, response, attempt)
	for name, values := range codexwire.ForwardHeaders(response.Trailer, false) {
		w.Header()[http.TrailerPrefix+name] = values
	}
}

type codeAttempt struct {
	server      *Server
	permit      resources.CodePermit
	config      runtime.Configuration
	auth        codemode.Authorization
	lease       *limits.Lease
	dispatched  bool
	usage       codemode.Usage
	terminal    bool
	conflicting bool
}

func (f *CodeForwarder) prepare(s *Server, r *http.Request, release *runtime.Release, route codemode.Route, observation codexwire.Request) (*codeAttempt, error) {
	authority, failure := s.authenticate(r, "inference")
	if failure != nil {
		return nil, codemode.Refuse(failure.Status, "code_authentication_refused")
	}
	if !authority.Allows("inference", route.Slug, &route.ProjectID, s.now()) {
		return nil, codemode.Refuse(403, "code_permission_denied")
	}
	lease, failure := s.Admission.ReserveCodeRate(r.Context(), authority, observation.Estimate, codeGenerationTimeout+time.Minute)
	if failure != nil {
		return nil, codemode.Refuse(failure.Status, "code_rate_limited")
	}
	permit, err := s.CodeLedger.Admit(r.Context(), resources.CodeAdmission{Route: route, APIKeyID: authority.ID, Operation: observation.Operation, PreviousResponse: observation.PreviousResponse})
	if err != nil {
		settleKey(r.Context(), lease, false, nil, s.log)
		return nil, err
	}
	a := &codeAttempt{server: s, permit: permit, lease: lease}
	ok := false
	defer func() {
		if !ok {
			a.finish(r.Context())
		}
	}()
	if !reflect.DeepEqual(authority.Policy, permit.Authority.Policy) {
		settleKey(r.Context(), a.lease, false, nil, s.log)
		a.lease, failure = s.Admission.ReserveCodeRate(r.Context(), permit.Authority, observation.Estimate, codeGenerationTimeout+time.Minute)
		if failure != nil {
			return nil, codemode.Refuse(failure.Status, "code_rate_limited")
		}
	}
	if permit.Binding.AccountID != permit.Account.ID || permit.Binding.Principal != permit.Account.Principal || permit.Authority.ID != authority.ID {
		return nil, codemode.Refuse(503, "code_binding_invalid")
	}
	configuration, exists := release.Snapshot.CodeConnection(route, permit.Account.ProviderID)
	if !exists {
		return nil, codemode.Refuse(503, "code_connection_unpublished")
	}
	a.config = configuration
	a.auth, err = s.CodeAuthorizer.AuthorizeCode(r.Context(), configuration, permit.Account)
	if err != nil {
		return nil, codemode.Refuse(503, "code_account_unavailable")
	}
	if a.auth.Principal == "" || a.auth.Principal != permit.Binding.Principal {
		return nil, codemode.Refuse(409, "code_principal_mismatch")
	}
	for name, values := range a.auth.Headers {
		if !strings.EqualFold(name, "Authorization") && !strings.EqualFold(name, "Chatgpt-Account-Id") || len(values) != 1 || strings.ContainsAny(values[0], "\r\n") {
			return nil, codemode.Refuse(503, "code_authorization_invalid")
		}
	}
	if a.auth.Headers.Get("Authorization") == "" {
		return nil, codemode.Refuse(503, "code_authorization_invalid")
	}
	ok = true
	return a, nil
}

func (a *codeAttempt) headers(source http.Header) http.Header {
	h := codexwire.ForwardHeaders(source, true)
	for name, values := range a.auth.Headers {
		h[name] = append([]string(nil), values...)
	}
	if _, present := h["User-Agent"]; !present {
		h["User-Agent"] = []string{""}
	}
	return h
}

func (a *codeAttempt) dispatch(ctx context.Context) error {
	if err := a.server.CodeLedger.MarkDispatched(ctx, a.permit.Attempt.ID); err != nil {
		return codemode.Refuse(503, "code_dispatch_unavailable")
	}
	a.dispatched = true
	return nil
}

func (a *codeAttempt) observe(o codexwire.Observation) {
	if o.ResponseID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = a.server.CodeLedger.ObserveReference(ctx, a.permit.Attempt.ID, o.ResponseID)
		cancel()
	}
	if o.Status != 0 {
		a.response(context.Background(), o.Status, nil)
	}
	if o.Allowance != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = a.server.CodeLedger.ObserveAllowance(ctx, a.permit.Account.ID, *o.Allowance)
		cancel()
	}
	if !o.Terminal {
		return
	}
	if a.terminal && !reflect.DeepEqual(a.usage, o.Usage) {
		a.conflicting = true
	}
	a.terminal = true
	a.usage = o.Usage
	if a.conflicting {
		a.usage = codemode.Usage{}
	}
}

func (a *codeAttempt) finish(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var err error
	if a.dispatched {
		err = a.server.CodeLedger.Settle(ctx, a.permit.Attempt.ID, a.usage)
	} else {
		err = a.server.CodeLedger.Abort(ctx, a.permit.Attempt.ID)
		if err != nil {
			err = a.server.CodeLedger.Settle(ctx, a.permit.Attempt.ID, codemode.Usage{})
		}
	}
	if err != nil {
		a.server.log.Warn("code accounting unavailable", "attempt_id", a.permit.Attempt.ID)
	}
	settleKey(ctx, a.lease, a.dispatched, a.usage.Total, a.server.log)
}

func (a *codeAttempt) health(ctx context.Context, status string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_ = a.server.CodeLedger.ObserveHealth(ctx, a.permit.Account.ID, status)
}

func (a *codeAttempt) response(ctx context.Context, status int, headers http.Header) {
	switch {
	case status == 429:
		a.health(ctx, "quota_limited")
	case status == 401 || status == 403 || status >= 500:
		a.health(ctx, "unavailable")
	case status >= 200 && status < 300:
		a.health(ctx, "healthy")
	}
	if allowance := codexwire.Allowance(headers, a.server.now()); allowance != nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_ = a.server.CodeLedger.ObserveAllowance(ctx, a.permit.Account.ID, *allowance)
	}
}

func codeEndpoint(s *Server, cfg runtime.Configuration, path, query string) (string, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", codemode.Refuse(502, "code_connection_invalid")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + path
	u.RawPath = ""
	if _, err := s.egress.ValidateEndpoint(u.String()); err != nil {
		return "", codemode.Refuse(502, "code_egress_refused")
	}
	u.RawQuery = query
	return u.String(), nil
}

func (f *CodeForwarder) client(s *Server, ctx context.Context, release *runtime.Release, cfg runtime.Configuration, providerID string) (*http.Client, error) {
	var secret []byte
	if network := cfg.Options.Network; network != nil && network.CredentialID != "" {
		var err error
		secret, err = s.Runtime.NetworkSecret(ctx, release, providerID, network.CredentialID)
		if err != nil {
			return nil, codemode.Refuse(503, "code_network_credential_unavailable")
		}
	}
	client, err := s.egress.RawConnectionClient(cfg.Options.Network, secret, upstreamHeaderTimeout)
	if err != nil {
		return nil, codemode.Refuse(502, "code_connection_invalid")
	}
	return client, nil
}

func (f *CodeForwarder) copyResponse(w http.ResponseWriter, response *http.Response, a *codeAttempt) {
	streaming := strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream")
	encoded := response.Header.Get("Content-Encoding")
	stream := codexwire.NewStream(codexwire.MaxBody, a.observe)
	var observed []byte
	overflow := false
	buffer := make([]byte, 32<<10)
	rc := http.NewResponseController(w)
	for {
		n, err := response.Body.Read(buffer)
		if n > 0 {
			part := buffer[:n]
			if streaming && (encoded == "" || encoded == "identity") {
				stream.Write(part)
			} else if !overflow {
				if len(observed)+n <= codexwire.MaxBody {
					observed = append(observed, part...)
				} else {
					overflow = true
					observed = nil
				}
			}
			_ = rc.SetWriteDeadline(time.Now().Add(responseWriteTimeout))
			if _, err := w.Write(part); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
		if err != nil {
			if err == io.EOF && !overflow && (!streaming || encoded != "" && encoded != "identity") {
				if decoded, err := codexwire.Decode(observed, encoded, codexwire.MaxBody); err == nil {
					if streaming {
						stream.Write(decoded)
					} else {
						a.observe(codexwire.Observe(decoded, true))
					}
				}
			}
			return
		}
	}
}

func codeWriteError(w http.ResponseWriter, err error) {
	var refusal *codemode.Refusal
	if !errors.As(err, &refusal) {
		refusal = &codemode.Refusal{Status: 503, Code: "code_service_unavailable"}
	}
	writeError(w, &Error{Status: refusal.Status, Code: refusal.Code, Type: "code_mode_error", Message: refusal.Code})
}

func codeRefuse(s *Server, w http.ResponseWriter, r *http.Request, route codemode.Route, keyID string, err error) {
	var refusal *codemode.Refusal
	if !errors.As(err, &refusal) {
		refusal = &codemode.Refusal{Status: 503, Code: "code_service_unavailable"}
	}
	if s.CodeLedger != nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
		_ = s.CodeLedger.RecordRefusal(ctx, route, keyID, refusal.Code)
		cancel()
	}
	codeWriteError(w, refusal)
}
