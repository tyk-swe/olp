package gateway

import (
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codewire"
	"github.com/tyk-swe/olp/internal/codexwire"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/runtime"
)

const codeGenerationTimeout = 10 * time.Minute

// RegisterCode mounts raw code-mode transport, which requires CodeLedger and
// CodeAuthorizer. A route serves the paths of its accounts' adapters, and
// each request goes to an account whose adapter serves its path. It never
// invokes ordinary request preparation, credential failover, response
// transformation or retries.
func (s *Server) RegisterCode(mux *http.ServeMux) {
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
	refuse := func(err error) { s.codeRefuse(w, r, route, authority.ID, err) }
	if !route.Enabled || !authority.Allows("inference", route.Slug, &route.ProjectID, s.now()) {
		refuse(codemode.Refuse(404, "code_route_unavailable"))
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
	if slices.Contains(adapters, codemode.AdapterCodex) && r.Method == http.MethodGet && path == "responses" && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		s.codeWebSocket(w, r, release, route, authority.ID)
		return
	}
	ingress, ok := codeIngressFor(r.Method, path)
	if !ok || len(release.Snapshot.CodeProviders(route, path)) == 0 {
		if codeProbe(r.Method, path) && len(release.Snapshot.CodeProviders(route, "v1/messages")) != 0 {
			codeWriteError(w, r, codemode.Refuse(404, "code_operation_unsupported"))
			return
		}
		refuse(codemode.Refuse(400, "code_operation_unsupported"))
		return
	}
	limit := s.codeBodyLimit()
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
	observation, err := ingress.classify(body, r.Header)
	if err != nil {
		refuse(err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), codeGenerationTimeout)
	defer cancel()
	attempt, err := s.prepareCode(r.WithContext(ctx), release, route, observation, path, ingress.protocol)
	if err != nil {
		refuse(err)
		return
	}
	attempt.observer = ingress.observer()
	defer attempt.finish(ctx)
	client, err := s.codeClient(r.Context(), release, attempt.config, attempt.permit.Account.ProviderID)
	if err != nil {
		refuse(err)
		return
	}
	defer client.CloseIdleConnections()
	target, err := s.codeEndpoint(attempt.config, attempt.upstream, r.URL.RawQuery)
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

type codeAttempt struct {
	server        *Server
	permit        resources.CodePermit
	config        runtime.Configuration
	auth          codemode.Authorization
	lease         *limits.Lease
	providerLease *limits.Lease
	observer      codewire.Observer
	// upstream is the path the account's adapter serves the client path at.
	upstream string
	// allowance reads subscription allowance from response headers.
	allowance   bool
	dispatched  bool
	usage       codemode.Usage
	terminal    bool
	conflicting bool
	prewarm     bool
}

// prepareCode admits a request on a client path to an account whose adapter
// serves the path, and authorizes it with that adapter's credential.
func (s *Server) prepareCode(r *http.Request, release *runtime.Release, route codemode.Route, observation codemode.Request, path string, protocol codemode.Protocol) (*codeAttempt, error) {
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
	permit, err := s.CodeLedger.Admit(r.Context(), resources.CodeAdmission{
		Route: route, APIKeyID: authority.ID, Operation: observation.Operation,
		Providers: release.Snapshot.CodeProviders(route, path), PreviousResponse: observation.PreviousResponse,
	})
	if err != nil {
		settleKey(r.Context(), lease, false, nil, s.log)
		return nil, err
	}
	a := &codeAttempt{server: s, permit: permit, lease: lease, prewarm: observation.Operation.Name == "prewarm"}
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
	if permit.Pin.AccountID != permit.Account.ID || permit.Pin.Principal != permit.Account.Principal || permit.Authority.ID != authority.ID {
		return nil, codemode.Refuse(503, "code_binding_invalid")
	}
	configuration, exists := release.Snapshot.CodeConnection(route, permit.Account.ProviderID)
	if !exists {
		return nil, codemode.Refuse(503, "code_connection_unpublished")
	}
	// The account's frozen connection names the adapter that serves it.
	vendor, known := codeadapter.ForConnection(configuration.Kind, configuration.AuthMode, configuration.ProfileID)
	upstream, served := vendor.Paths[path]
	if !known || !served {
		return nil, codemode.Refuse(503, "code_configuration_unsupported")
	}
	dispatch := codemode.Dispatch{Adapter: vendor.Adapter, Protocol: protocol}
	a.config, a.upstream, a.allowance = configuration, upstream, vendor.Adapter == codemode.AdapterCodex
	a.providerLease, failure = s.Admission.reserveCodeProvider(r.Context(), permit.Account.ProviderID, configuration, observation.Estimate, codeGenerationTimeout+time.Minute)
	if failure != nil {
		return nil, codemode.Refuse(failure.Status, "code_provider_rate_limited")
	}
	a.auth, err = s.CodeAuthorizer.AuthorizeCode(r.Context(), configuration, permit.Account, dispatch)
	if err != nil {
		return nil, codemode.Refuse(503, "code_account_unavailable")
	}
	if a.auth.Principal == "" || a.auth.Principal != permit.Pin.Principal {
		return nil, codemode.Refuse(409, "code_principal_mismatch")
	}
	// The authorization carries exactly the adapter's credential header for
	// this protocol, and only the other headers the adapter declares.
	credential := codeadapter.CredentialHeader(dispatch.Adapter, dispatch.Protocol)
	for name, values := range a.auth.Headers {
		declared := strings.EqualFold(name, credential) || slices.ContainsFunc(vendor.Extra, func(extra string) bool { return strings.EqualFold(name, extra) })
		if !declared || len(values) != 1 || strings.ContainsAny(values[0], "\r\n") {
			return nil, codemode.Refuse(503, "code_authorization_invalid")
		}
	}
	if a.auth.Headers.Get(credential) == "" {
		return nil, codemode.Refuse(503, "code_authorization_invalid")
	}
	ok = true
	return a, nil
}

func (a *codeAttempt) headers(source http.Header) http.Header {
	h := codewire.ForwardHeaders(source, true)
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

func (a *codeAttempt) observe(o codemode.Observation) {
	if o.ResponseID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = a.server.CodeLedger.ObserveReference(ctx, a.permit.Attempt.ID, o.ResponseID)
		cancel()
	}
	if o.Status != 0 {
		a.response(context.Background(), o.Status, nil)
	}
	if o.Allowance != nil {
		a.observeAllowance(context.Background(), *o.Allowance)
	}
	if o.Outcome != nil {
		a.outcome(context.Background(), *o.Outcome)
	}
	if o.Successful && !a.prewarm {
		a.health(context.Background(), "healthy")
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
	if !a.dispatched {
		a.outcome(ctx, codemode.Outcome{Origin: "gateway", Kind: "rejected"})
	} else if !a.terminal {
		outcome := codemode.Outcome{Origin: "gateway", Kind: "interrupted"}
		if errors.Is(ctx.Err(), context.Canceled) {
			outcome.Origin, outcome.Kind = "client", "canceled"
		}
		a.outcome(ctx, outcome)
	}
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
	settleKey(ctx, a.providerLease, a.dispatched, a.usage.Total, a.server.log)
}

func (a *codeAttempt) outcome(ctx context.Context, outcome codemode.Outcome) {
	if a.permit.Attempt.ID == "" {
		return
	}
	if outcome.ObservedAt.IsZero() {
		outcome.ObservedAt = a.server.now().UTC()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := a.server.CodeLedger.ObserveOutcome(ctx, a.permit.Attempt.ID, outcome); err != nil {
		a.server.log.Warn("code outcome unavailable", "attempt_id", a.permit.Attempt.ID)
	}
}

// transportFailed records a gateway transport failure. Only an upstream close
// that signals the peer's own failure or unavailability cools the account;
// transport noise, like a canceled or expired request, is not the account's
// failure and cannot prove unavailability.
func (a *codeAttempt) transportFailed(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	a.outcome(ctx, codemode.Outcome{Origin: "gateway", Kind: "transport_error"})
	if codeUnavailableClose(err) {
		a.health(ctx, "unavailable")
	}
}

// codeUnavailableClose reports whether err is an upstream WebSocket close whose
// code signals the peer's own failure (internal error, restart, try-again or
// bad gateway) rather than transport noise.
func codeUnavailableClose(err error) bool {
	code := websocket.CloseStatus(err)
	return code >= websocket.StatusInternalError && code <= websocket.StatusBadGateway
}

func (a *codeAttempt) observeAllowance(ctx context.Context, allowance codemode.Allowance) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_ = a.server.CodeLedger.ObserveAllowance(ctx, a.permit.Account.ID, allowance)
}

func (a *codeAttempt) health(ctx context.Context, status string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_ = a.server.CodeLedger.ObserveHealth(ctx, a.permit.Account.ID, status)
}

func (a *codeAttempt) response(ctx context.Context, status int, headers http.Header) {
	outcome := codemode.Outcome{Origin: "upstream", Kind: "headers", UpstreamStatus: &status}
	if status >= 400 {
		outcome.Kind = "rejected"
	}
	a.outcome(ctx, outcome)
	if status == http.StatusUnauthorized {
		a.server.Runtime.CredentialRefused(a.auth.CredentialID, a.auth.GrantGeneration)
	}
	switch {
	case status == 429:
		a.health(ctx, "quota_limited")
	case status == 401 || status == 403 || status >= 500:
		a.health(ctx, "unavailable")
	}
	if !a.allowance {
		return
	}
	if allowance := codexwire.Allowance(headers, a.server.now()); allowance != nil {
		a.observeAllowance(ctx, *allowance)
	}
}

func (s *Server) codeEndpoint(cfg runtime.Configuration, path, query string) (string, error) {
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

func (s *Server) codeClient(ctx context.Context, release *runtime.Release, cfg runtime.Configuration, providerID string) (*http.Client, error) {
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

// codeRefusal is the refusal a client sees for err; failures that are not
// refusals do not leak their detail.
func codeRefusal(err error) *codemode.Refusal {
	if refusal, ok := errors.AsType[*codemode.Refusal](err); ok {
		return refusal
	}
	return &codemode.Refusal{Status: 503, Code: "code_service_unavailable"}
}

// codeWriteError writes a refusal in the envelope of the request's surface:
// Anthropic's for a route's Messages paths, OpenAI's otherwise.
func codeWriteError(w http.ResponseWriter, r *http.Request, err error) {
	refusal := codeRefusal(err)
	surface, kind := requestSurface(r), "code_mode_error"
	if surface == "anthropic" {
		switch refusal.Status {
		case 400, 409, 413, 415, 422:
			kind = "invalid_request_error"
		case 401:
			kind = "authentication_error"
		case 403:
			kind = "permission_error"
		}
	}
	writeSurfaceError(w, &Error{Status: refusal.Status, Code: refusal.Code, Type: kind, Message: refusal.Code}, surface)
}

func (s *Server) codeRefuse(w http.ResponseWriter, r *http.Request, route codemode.Route, keyID string, err error) {
	refusal := codeRefusal(err)
	s.recordCodeRefusal(r.Context(), route, keyID, refusal.Code)
	codeWriteError(w, r, refusal)
}

func (s *Server) recordCodeRefusal(ctx context.Context, route codemode.Route, keyID, code string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_ = s.CodeLedger.RecordRefusal(ctx, route, keyID, code)
}
