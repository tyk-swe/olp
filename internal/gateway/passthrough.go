package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/codewire"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s *Server) passthrough(w http.ResponseWriter, r *http.Request) {
	x := &call{request: s.begin(w, r), family: wire.Passthrough, actor: "api_key", mode: "unary"}
	fail := func(e *Error) {
		x.failure = e
		s.finish(x, nil, e.Status)
		writeSurfaceError(w, e, "openai")
	}
	if !s.admit(r.Context()) {
		fail(overloaded)
		return
	}
	defer s.release(r.Context())
	authority, e := s.authenticate(r, "inference")
	if e != nil {
		fail(e)
		return
	}
	s.bindAuthority(x, authority)
	if x.preferences, x.priority, e = routingControls(r, authority.Policy); e != nil {
		fail(e)
		return
	}
	if x.attribution, e = s.parseAttribution(r, authority); e != nil {
		fail(e)
		return
	}
	if x.request.release == nil || x.request.release.Snapshot == nil {
		fail(serverError(http.StatusServiceUnavailable, "upstream_unavailable", "No serving release is installed."))
		return
	}
	snapshot := x.request.release.Snapshot
	slug := r.PathValue("route")
	route, ok := snapshot.Routes[slug]
	if !ok || !route.IsPassthrough() || route.Passthrough.Dialect != r.PathValue("dialect") || !authority.Allows("inference", slug, route.ProjectID, s.now()) || !slices.Contains(authority.Policy.AllowedRoutes, slug) {
		fail(modelNotFound(slug))
		return
	}
	x.route = &route
	if e := s.reserveState(r.Context(), x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); e != nil {
		fail(e)
		return
	}
	defer s.settleAdmission(r.Context(), x)
	if isWebSocketUpgrade(r) {
		fail(invalidRequest("passthrough_websocket_refused", "Pass-through routes do not accept WebSocket upgrades.", nil))
		return
	}
	if r.URL.IsAbs() || r.URL.Host != "" || r.URL.Fragment != "" {
		fail(invalidRequest("passthrough_path_refused", "The pass-through path is not an anchored slash path.", nil))
		return
	}
	escaped, ok := passthroughSuffix(r)
	if !ok {
		fail(notFoundError("route_not_found", "The pass-through path does not match this route."))
		return
	}
	matched, err := route.Passthrough.Match(r.Method, escaped)
	if err != nil {
		fail(invalidRequest("passthrough_path_refused", "The pass-through path is not an anchored slash path.", nil))
		return
	}
	if !matched {
		fail(notFoundError("route_not_found", "The pass-through path does not match this route."))
		return
	}
	normalized, _ := routing.NormalizePassthroughPath(escaped)
	query, qerr := url.ParseQuery(r.URL.RawQuery)
	if qerr != nil {
		fail(invalidRequest("passthrough_query_refused", "The pass-through query is malformed.", nil))
		return
	}
	for name := range query {
		if !slices.Contains(route.Passthrough.Query, name) {
			fail(invalidRequest("passthrough_query_refused", "Query name `"+name+"` is not allowed on this pass-through route.", nil))
			return
		}
	}
	s.servePassthrough(w, r, x, &route, normalized, query, fail)
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

func passthroughSuffix(r *http.Request) (string, bool) {
	parts := strings.Split(r.URL.EscapedPath(), "/")
	if len(parts) < 6 || parts[1] != "native" || parts[3] != "routes" {
		return "", false
	}
	return "/" + strings.Join(parts[5:], "/"), true
}

func (s *Server) servePassthrough(w http.ResponseWriter, r *http.Request, x *call, route *routing.Route, normalized string, query url.Values, fail func(*Error)) {
	target := route.Targets[0]
	provider, ok := x.snapshot().Providers[target.ProviderID]
	if !ok || !provider.Enabled {
		fail(serverError(http.StatusServiceUnavailable, "upstream_unavailable", "The bound connection is unavailable."))
		return
	}
	options := s.selectionOptions(r.Context(), x)
	options.CheckSlots = false
	plan, planErr := routing.Select(x.snapshot(), route.Slug, routing.PassthroughOperation, "native", "unary", x.affinity, options)
	if planErr != nil {
		fail(selectionError(planErr, route.Slug))
		return
	}
	var attempt routing.Attempt
	eligible := false
	for _, planned := range plan.Attempts {
		if planned.TargetID == target.ID {
			attempt, eligible = planned, true
			break
		}
	}
	if !eligible {
		fail(selectionError(&routing.SelectionError{Code: routing.NoEligibleTargets}, route.Slug))
		return
	}
	timeout := time.Duration(route.OverallTimeout) * time.Millisecond
	if target.Timeout > 0 {
		timeout = min(timeout, time.Duration(target.Timeout)*time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	_ = http.NewResponseController(w).SetWriteDeadline(deadline)
	limit := s.cfg.MaxBodyBytes
	if route.MaxBodyBytes != nil {
		limit = min(limit, *route.MaxBodyBytes)
	}
	if limit <= 0 {
		limit = 64 * 1024
	}
	var body []byte
	if r.Body != nil && r.Body != http.NoBody {
		controller := http.NewResponseController(w)
		if err := controller.SetReadDeadline(deadline); err != nil {
			fail(serverError(http.StatusInternalServerError, "internal_error", "The request body could not be bounded."))
			return
		}
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, limit+1))
		if err == nil {
			_ = controller.SetReadDeadline(time.Time{})
		}
		if err != nil {
			if ctx.Err() != nil {
				fail((&attemptFailure{class: classTimeout}).toError())
				return
			}
			fail(invalidRequest("invalid_request", "The request body could not be read.", nil))
			return
		}
		if int64(len(body)) > limit {
			fail(bodyTooLarge())
			return
		}
	}
	base, err := s.egress.ValidateEndpoint(provider.Endpoint)
	if err != nil {
		fail(serverError(http.StatusServiceUnavailable, "upstream_unavailable", "The bound connection endpoint is unavailable."))
		return
	}
	upstream := *base
	upstream.Path = strings.TrimSuffix(base.Path, "/") + normalized
	upstream.RawPath = ""
	upstream.RawQuery = query.Encode()
	upstream.Fragment = ""

	defer s.settleCaps(r.Context(), x)
	estimate := max(resourceEstimate, x.estimate)
	var slot routing.Slot
	var hold *dispatchHold
	var factP *AttemptFact
	found := false
	unmeterable := false
slots:
	for _, candidate := range s.slots(x, attempt, &provider) {
		gate := s.gateAttempt(ctx, x, attempt, &provider, &candidate, estimate, deadline)
		switch gate.verdict {
		case gateAdmitted:
			slot, hold, found = candidate, gate.hold, true
			break slots
		case gateRejected:
			x.facts = append(x.facts, s.rejectedFact(x, attempt, candidate, 1, gate.rejection))
			fail(gate.rejection.toError())
			return
		case gateExpired:
			fail((&attemptFailure{class: classTimeout}).toError())
			return
		case gateUnmeterable:
			unmeterable = true
		case gateDenied:
			break slots
		}
	}
	if !found {
		if unmeterable {
			fail(limitsUnavailable())
			return
		}
		if len(provider.Slots) > 0 {
			fail(serverError(http.StatusServiceUnavailable, "upstream_unavailable", "No credential slot is currently able to serve `"+route.Slug+"`."))
			return
		}
	}
	defer func() {
		if x.dispatched && hold != nil {
			x.spendCaps(hold.budgets, attempt)
		}
		s.settlePinHold(r.Context(), x, hold, settledAttemptTokens(factP, estimate))
	}()
	if e := s.reserveSessionCost(ctx, x, x.authority, attempt, time.Until(deadline)); e != nil {
		fail(e)
		return
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, upstream.String(), bytes.NewReader(body))
	if err != nil {
		fail(serverError(http.StatusBadGateway, "upstream_error", "The provider request could not be built."))
		return
	}
	headers := codewire.ForwardHeaders(r.Header, true)
	for _, name := range route.Passthrough.Headers {
		for _, value := range headers.Values(name) {
			req.Header.Add(name, value)
		}
	}
	if ct := headers.Get("Content-Type"); ct != "" && len(body) > 0 {
		req.Header.Set("Content-Type", ct)
	}
	fact := s.newFact(x, attempt, slot, 1, &provider)
	fact.Mode = "unary"
	fact.PricingPinned = attempt.Price != nil
	if hold != nil {
		fact.Budgets = hold.budgets
	}
	x.facts = append(x.facts, fact)
	factP = &x.facts[len(x.facts)-1]
	if err := s.applySlotCredential(ctx, x, req, provider.Connector(), slot, body, &provider, target.ProviderModel); err != nil {
		factP.Class, factP.Duration = classCredential, s.now().Sub(factP.StartedAt)
		factP.recordEvidence(false)
		fail(serverError(http.StatusBadGateway, "upstream_error", "The bound credential could not be applied."))
		return
	}
	client, err := s.providerClient(ctx, x.request.release, &provider, slot)
	if err != nil {
		factP.Class, factP.Duration = classCredential, s.now().Sub(factP.StartedAt)
		factP.recordEvidence(false)
		fail(serverError(http.StatusBadGateway, "upstream_error", "The bound connection could not be reached."))
		return
	}
	x.dispatched = true
	resp, err := client.Do(req)
	if err != nil {
		factP.Duration = s.now().Sub(factP.StartedAt)
		if resp != nil && resp.StatusCode >= 300 && resp.StatusCode < 400 || errors.Is(err, egress.ErrRedirect) || strings.Contains(err.Error(), egress.ErrRedirect.Error()) {
			if resp != nil {
				factP.Status = resp.StatusCode
			}
			factP.Class = classProtocol
			factP.recordEvidence(true)
			fail(serverError(http.StatusBadGateway, "passthrough_redirect_refused", "The provider redirect was refused."))
			return
		}
		factP.Class = classConnect
		if ctx.Err() != nil {
			factP.Class = classTimeout
		}
		factP.recordEvidence(true)
		fail(serverError(http.StatusBadGateway, "upstream_error", "The provider could not complete the request."))
		return
	}
	defer resp.Body.Close()
	received := s.now().Sub(factP.StartedAt)
	factP.FirstByte, factP.Status = &received, resp.StatusCode
	switch resp.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		factP.Class, factP.Duration = classProtocol, s.now().Sub(factP.StartedAt)
		factP.recordEvidence(true)
		fail(serverError(http.StatusBadGateway, "passthrough_redirect_refused", "The provider redirect was refused."))
		return
	}
	responseLimit := s.cfg.MaxResponseBytes
	if responseLimit <= 0 {
		responseLimit = 1 << 20
	}
	if r.Method != http.MethodHead && resp.ContentLength > responseLimit {
		factP.Class, factP.Duration = classProtocol, s.now().Sub(factP.StartedAt)
		factP.recordEvidence(true)
		fail(serverError(http.StatusBadGateway, "upstream_error", "The provider response exceeded the gateway limit."))
		return
	}
	responseBody, responseError := passthroughResponseBody(resp, x.sensitive, responseLimit)
	if responseError != nil {
		factP.Class, factP.Duration = classProtocol, s.now().Sub(factP.StartedAt)
		if ctx.Err() != nil {
			factP.Class = classTimeout
		}
		factP.recordEvidence(true)
		fail(responseError)
		return
	}
	for name, values := range codewire.ForwardHeaders(resp.Header, false) {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if routing.HopByHopOrCredentialHeader(canonical) || x.sensitive.Contains(canonical) {
			continue
		}
		if r.Method != http.MethodHead && resp.StatusCode >= 400 && (canonical == "Content-Encoding" || canonical == "Content-Md5" || canonical == "Digest") {
			continue
		}
		for _, value := range values {
			w.Header().Add(canonical, x.sensitive.Redact(value))
		}
	}
	if r.Method == http.MethodHead && resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	w.WriteHeader(resp.StatusCode)
	x.delivered(s.now())
	readErr, copyErr := copyPassthroughResponse(w, responseBody, responseLimit)
	factP.Duration = s.now().Sub(factP.StartedAt)
	providerSuccess := readErr == nil && (resp.StatusCode >= 200 && resp.StatusCode < 300 || resp.StatusCode == http.StatusNotModified)
	switch {
	case readErr != nil:
		factP.Class = classProtocol
		if ctx.Err() != nil {
			factP.Class = classTimeout
		}
		factP.recordEvidence(true)
	case providerSuccess:
		factP.Class = classSuccess
		factP.observeUsage(&wire.Usage{}, true)
		factP.TokenlessUsage = true
		factP.recordEvidence(false)
	case resp.StatusCode >= 500:
		factP.Class = classUpstreamServer
		factP.recordEvidence(true)
	case resp.StatusCode >= 400:
		factP.Class = classUpstreamClient
		factP.recordEvidence(true)
	default:
		factP.Class = classProtocol
		factP.recordEvidence(true)
	}
	out := &outcome{committed: providerSuccess && copyErr == nil}
	switch {
	case readErr != nil || copyErr != nil:
		out.err = serverError(http.StatusBadGateway, "upstream_error", "The provider response could not be delivered.")
	case !providerSuccess:
		out.err = upstreamError(&attemptFailure{status: resp.StatusCode})
	}
	s.finish(x, out, resp.StatusCode)
	if readErr != nil || copyErr != nil {
		panic(http.ErrAbortHandler)
	}
}

func passthroughResponseBody(response *http.Response, sensitive egress.Sensitive, limit int64) (io.Reader, *Error) {
	if response.StatusCode < 400 || response.Request != nil && response.Request.Method == http.MethodHead {
		return response.Body, nil
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, serverError(http.StatusBadGateway, "upstream_error", "The provider error response could not be read.")
	}
	decoded, err := codewire.Decode(raw, response.Header.Get("Content-Encoding"), limit)
	if err != nil {
		return nil, serverError(http.StatusBadGateway, "upstream_error", "The provider error response could not be decoded within the gateway limit.")
	}
	redacted := sensitive.Redact(string(decoded))
	if int64(len(redacted)) > limit {
		return nil, serverError(http.StatusBadGateway, "upstream_error", "The provider error response exceeded the gateway limit.")
	}
	return strings.NewReader(redacted), nil
}

func copyPassthroughResponse(w http.ResponseWriter, source io.Reader, limit int64) (error, error) {
	bounded := &io.LimitedReader{R: source, N: limit + 1}
	buffer := make([]byte, 32<<10)
	controller := http.NewResponseController(w)
	var deliveryErr error
	for {
		n, readErr := bounded.Read(buffer)
		if bounded.N == 0 {
			return io.ErrShortBuffer, deliveryErr
		}
		if n > 0 && deliveryErr == nil {
			written, err := w.Write(buffer[:n])
			switch {
			case err != nil:
				deliveryErr = err
			case written != n:
				deliveryErr = io.ErrShortWrite
			default:
				deliveryErr = controller.Flush()
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil, deliveryErr
		}
		if readErr != nil {
			return readErr, deliveryErr
		}
	}
}
