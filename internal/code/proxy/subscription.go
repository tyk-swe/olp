package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/coder/websocket"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/research"
	"github.com/tyk-swe/olp/internal/code/session"
	"github.com/tyk-swe/olp/internal/code/vendor"
	"github.com/tyk-swe/olp/internal/code/wire"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/protocols/sse"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/upstream"
)

type attemptCore struct {
	server      *Server
	attemptID   string
	model       string
	publicModel string
	protocol    code.Protocol
	observer    wire.Observer
	dispatched  bool
	terminal    bool
	conflicting bool
	finished    bool
	status      int
	usage       code.Usage
}

func (c *attemptCore) dispatch(ctx context.Context) error {
	if err := c.server.Ledger.MarkDispatched(ctx, c.attemptID); err != nil {
		return code.Refuse(503, "code_dispatch_unavailable")
	}
	c.dispatched = true
	return nil
}

func (c *attemptCore) recordOutcome(ctx context.Context, outcome code.Outcome) {
	if c.attemptID == "" {
		return
	}
	stamped := outcome.WithTime(c.server.Host.Now().UTC())
	step, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := c.server.Ledger.ObserveOutcome(step, c.attemptID, stamped); err != nil && c.server.Host.Log() != nil {
		c.server.Host.Log().Warn("code outcome unavailable", "attempt_id", c.attemptID)
	}
}

func (c *attemptCore) recordReference(responseID string) {
	if responseID == "" || c.attemptID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = c.server.Ledger.ObserveReference(ctx, c.attemptID, responseID)
}

func (c *attemptCore) applyTerminal(o code.Observation) {
	if !o.Terminal {
		return
	}
	if c.terminal && !reflect.DeepEqual(c.usage, o.Usage) {
		c.conflicting = true
	}
	c.terminal = true
	c.usage = o.Usage
	if c.conflicting {
		c.usage = code.Usage{}
	}
}

func (c *attemptCore) settleAttempt(ctx context.Context) {
	if !c.dispatched {
		c.recordOutcome(ctx, code.GatewayOutcome(code.OutcomeRejected, c.server.Host.Now().UTC()))
	} else if !c.terminal {
		outcome := code.GatewayOutcome(code.OutcomeInterrupted, c.server.Host.Now().UTC())
		if errors.Is(ctx.Err(), context.Canceled) {
			outcome = code.ClientCanceledOutcome(c.server.Host.Now().UTC())
		}
		c.recordOutcome(ctx, outcome)
	}
	step, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var err error
	if c.dispatched {
		err = c.server.Ledger.Settle(step, c.attemptID, c.usage)
	} else {
		err = c.server.Ledger.Abort(step, c.attemptID)
		if err != nil {
			err = c.server.Ledger.Settle(step, c.attemptID, code.Usage{})
		}
	}
	if err != nil && c.server.Host.Log() != nil {
		c.server.Host.Log().Warn("code accounting unavailable", "attempt_id", c.attemptID)
	}
}

func copyHTTPResponse(
	ctx context.Context,
	w http.ResponseWriter,
	response *http.Response,
	core *attemptCore,
	onEvent func(code.Observation),
	onUnary func([]byte),
	onTransportErr func(context.Context, error),
) {
	finishStream := func() {
		if !core.terminal {
			if observation, ok := core.observer.Finish(); ok {
				onEvent(observation)
			}
		}
		if !core.terminal && ctx.Err() == nil {
			core.recordOutcome(ctx, code.UpstreamTerminalOutcome(code.OutcomeIncomplete, nil, core.server.Host.Now().UTC()))
		}
	}
	if core.model != core.publicModel {
		if err := copyRewrittenModelResponse(ctx, w, response, core, onEvent, onUnary, finishStream); err != nil {
			onTransportErr(ctx, err)
		}
		return
	}
	streaming := strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream")
	encoded := response.Header.Get("Content-Encoding")
	events := wire.NewEvents(wire.MaxBody, func(data []byte) { onEvent(core.observer.Event(data)) })
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
				if len(observed)+n <= wire.MaxBody {
					observed = append(observed, part...)
				} else {
					overflow = true
					observed = nil
				}
			}
			_ = rc.SetWriteDeadline(time.Now().Add(responseWriteTimeout))
			if _, writeErr := w.Write(part); writeErr != nil {
				core.recordOutcome(ctx, code.ClientCanceledOutcome(core.server.Host.Now().UTC()))
				return
			}
			if flushErr := rc.Flush(); flushErr != nil {
				core.recordOutcome(ctx, code.ClientCanceledOutcome(core.server.Host.Now().UTC()))
				return
			}
		}
		if err != nil {
			if err == io.EOF && !overflow && (!streaming || encoded != "" && encoded != "identity") {
				if decoded, decodeErr := wire.Decode(observed, encoded, wire.MaxBody); decodeErr == nil {
					if streaming {
						events.Write(decoded)
					} else {
						onUnary(decoded)
					}
				}
			}
			if err == io.EOF {
				finishStream()
			} else if !core.terminal && ctx.Err() == nil {
				core.recordOutcome(ctx, code.GatewayOutcome(code.OutcomeTransportError, core.server.Host.Now().UTC()))
			}
			return
		}
	}
}

func copyRewrittenModelResponse(
	ctx context.Context,
	w http.ResponseWriter,
	response *http.Response,
	core *attemptCore,
	onEvent func(code.Observation),
	onUnary func([]byte),
	finishStream func(),
) error {
	reader, err := wire.OpenDecoder(response.Body, response.Header.Get("Content-Encoding"), wire.MaxBody)
	if err != nil {
		return code.Refuse(502, "code_upstream_invalid")
	}
	defer reader.Close()
	rc := http.NewResponseController(w)
	deliver := func(body []byte) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(responseWriteTimeout))
		if _, err := w.Write(body); err != nil {
			core.recordOutcome(ctx, code.ClientCanceledOutcome(core.server.Host.Now().UTC()))
			return false
		}
		if err := rc.Flush(); err != nil {
			core.recordOutcome(ctx, code.ClientCanceledOutcome(core.server.Host.Now().UTC()))
			return false
		}
		return true
	}
	if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		body, readErr := io.ReadAll(io.LimitReader(reader, wire.MaxBody+1))
		if readErr != nil || len(body) > wire.MaxBody {
			return code.Refuse(502, "code_upstream_invalid")
		}
		onUnary(body)
		rewritten, rewriteErr := wire.RewriteResponseModel(body, core.model, core.publicModel)
		if rewriteErr != nil {
			return rewriteErr
		}
		if deliver(rewritten) {
			finishStream()
		}
		return nil
	}
	decoder := sse.NewRawDecoder(reader, wire.MaxBody)
	for {
		frame, readErr := decoder.Next()
		if readErr == io.EOF {
			finishStream()
			return nil
		}
		if readErr != nil {
			return readErr
		}
		if frame.HasData {
			onEvent(core.observer.Event(frame.Data))
		}
		rewritten, rewriteErr := wire.RewriteEventFrame(frame, core.protocol, core.model, core.publicModel)
		if rewriteErr != nil {
			rewritten = frame.Bytes
		}
		if !deliver(rewritten) {
			return nil
		}
	}
}

type subscriptionAttempt struct {
	core               attemptCore
	permit             session.Permit
	config             routing.Configuration
	vendor             vendor.Vendor
	endpoint           vendor.Endpoint
	auth               code.Authorization
	identityOriginator string
	lease              *admission.Lease
	providerLease      *admission.Lease
	prewarm            bool
	responseHeaders    http.Header
	pendingHealth      *code.HealthObservation
}

func resolveSubscriptionTarget(snapshot *routing.View, route code.Route, providerID, ingressPath string, transport code.Transport) (routing.Configuration, vendor.Vendor, vendor.Endpoint, error) {
	config, exists := snapshot.CodeConnection(route, providerID)
	if !exists {
		return routing.Configuration{}, vendor.Vendor{}, vendor.Endpoint{}, code.Refuse(503, "code_connection_unpublished")
	}
	v, known := vendor.ForConnection(config.Kind, config.AuthMode, config.ProfileID)
	endpoint, served := vendor.RouteEndpoint(route, v, ingressPath)
	if !known || !served || !endpoint.Supports(transport) {
		return routing.Configuration{}, vendor.Vendor{}, vendor.Endpoint{}, code.Refuse(503, "code_configuration_unsupported")
	}
	return config, v, endpoint, nil
}

func (s *Server) finishSubscriptionPrepare(
	r *http.Request,
	rel *release.Release,
	route code.Route,
	observation code.Request,
	operation code.Operation,
	ingress vendor.Endpoint,
	transport code.Transport,
	authority keyauthority.Authority,
	labels map[string]string,
	permit session.Permit,
	lease *admission.Lease,
	recovery *code.Recovery,
) (*subscriptionAttempt, error) {
	model := operation.Model
	if permit.Pin.TargetModel != "" {
		model = permit.Pin.TargetModel
	}
	a := &subscriptionAttempt{
		core:    attemptCore{server: s, attemptID: permit.Attempt.ID, model: model, publicModel: operation.PublicModel()},
		permit:  permit,
		lease:   lease,
		prewarm: observation.Operation.Name == "prewarm",
	}
	ok := false
	defer func() {
		if !ok {
			a.finish(r.Context())
		}
	}()
	if err := BindWorkload(&permit.Authority, authority); err != nil {
		return nil, err
	}
	a.permit.Authority = permit.Authority
	if !permit.Authority.AllowsAttribution(labels) {
		return nil, code.Refuse(409, "code_attribution_changed")
	}
	if !reflect.DeepEqual(authority.Policy, permit.Authority.Policy) || !reflect.DeepEqual(authority.ProjectEndUserPolicy, permit.Authority.ProjectEndUserPolicy) || !reflect.DeepEqual(authority.BudgetGroupID, permit.Authority.BudgetGroupID) || !reflect.DeepEqual(authority.GroupLimits(), permit.Authority.GroupLimits()) {
		s.Host.SettleLease(r.Context(), a.lease, false, nil)
		a.lease = nil
		fresh := permit.Authority
		if failStatus, allowed := s.Host.CheckKeyAddress(r, fresh); !allowed {
			return nil, code.Refuse(failStatus, "code_ip_not_allowed")
		}
		identified, failStatus, validEndUser := s.Host.IdentifyEndUser(r, fresh)
		if !validEndUser {
			return nil, code.Refuse(failStatus, "code_end_user_invalid")
		}
		if identified.EndUserDigest != authority.EndUserDigest {
			return nil, code.Refuse(409, "code_end_user_changed")
		}
		reLeased, failStatus, reserved := s.Host.ReserveRate(r.Context(), research.WithoutCostBudgets(identified), observation.Estimate, GenerationTimeout+time.Minute, route.Slug)
		if !reserved {
			return nil, code.Refuse(failStatus, "code_rate_limited")
		}
		a.lease = reLeased
	}
	if permit.Pin.AccountID != permit.Account.ID || permit.Pin.Principal != permit.Account.Principal || permit.Authority.ID != authority.ID {
		return nil, code.Refuse(503, "code_binding_invalid")
	}
	cfg, v, endpoint, err := resolveSubscriptionTarget(rel.Snapshot(), route, permit.Account.ProviderID, ingress.ClientPath, transport)
	if err != nil {
		return nil, err
	}
	if !v.ServesModel(endpoint.Protocol, model) {
		return nil, code.Refuse(503, "code_configuration_unsupported")
	}
	a.config, a.vendor, a.endpoint = cfg, v, endpoint
	a.core.protocol = endpoint.Protocol
	a.core.observer = wire.NewObserver(endpoint)
	a.identityOriginator = v.CompatibilityOriginator(route)
	providerLease, failStatus, reserved := s.Host.ReserveProvider(r.Context(), permit.Account.ProviderID, cfg, observation.Estimate, GenerationTimeout+time.Minute)
	if !reserved {
		return nil, code.Refuse(failStatus, "code_provider_rate_limited")
	}
	a.providerLease = providerLease
	a.auth, err = s.Authorizer.AuthorizeCode(r.Context(), cfg, permit.Account, code.Dispatch{Adapter: v.Adapter, Protocol: endpoint.Protocol, ClientPath: endpoint.ClientPath})
	if err != nil {
		return nil, code.Refuse(503, "code_account_unavailable")
	}
	if err := v.ValidateAuthorization(endpoint, a.auth, permit.Pin.Principal); err != nil {
		return nil, err
	}
	if recovery != nil {
		if refresh, isRefresh := recovery.CredentialRefresh(); isRefresh && (a.auth.CredentialID != refresh.CredentialID || a.auth.GrantGeneration <= refresh.GrantGeneration) {
			return nil, code.Refuse(409, "code_recovery_unsafe")
		}
	}
	ok = true
	return a, nil
}

func (a *subscriptionAttempt) serveHTTP(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	rel *release.Release,
	route code.Route,
	observation code.Request,
	ingress vendor.Endpoint,
	raw, body []byte,
	recovery *code.Recovery,
	refuse func(error),
) (*code.Recovery, error) {
	payload, err := a.buildPayload(r, ingress, raw, body)
	if err != nil {
		refuse(err)
		return nil, nil
	}
	httpClient, err := a.core.server.Host.ConnectionClient(ctx, rel, a.config, a.permit.Account.ProviderID)
	if err != nil {
		refuse(err)
		return nil, nil
	}
	defer httpClient.CloseIdleConnections()
	target, err := subscriptionEndpointURL(a.core.server.Host, a.config, a.endpoint.Path(payload.stream), payload.query)
	if err != nil {
		refuse(err)
		return nil, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, &oneShotBody{data: payload.body})
	if err != nil {
		refuse(code.Refuse(502, "code_connection_invalid"))
		return nil, nil
	}
	request.ContentLength = int64(len(payload.body))
	request.Header = buildUpstreamHeaders(r.Header, a.auth.Headers, a.identityOriginator)
	if a.core.model != a.core.publicModel {
		request.Header.Del("Content-Encoding")
		request.Header.Del("Content-Length")
		request.Header.Set("Accept-Encoding", "identity")
	}
	if payload.translated || payload.target != "" {
		request.Header.Del("Content-Encoding")
		request.Header.Del("Content-Length")
		request.Header.Del("Anthropic-Version")
		request.Header.Del("Anthropic-Beta")
		request.Header.Set("Accept-Encoding", "identity")
		request.Header.Set("Content-Type", "application/json")
	} else {
		request.Trailer = wire.ForwardTrailers(r.Trailer, r.Header, true)
	}
	if len(request.Trailer) != 0 {
		request.ContentLength = -1
		request.Header.Del("Content-Length")
	}
	request.GetBody = nil
	request.Close = true
	if err := a.core.dispatch(ctx); err != nil {
		refuse(err)
		return nil, nil
	}
	upstreamResponse, err := httpClient.Do(request)
	if err != nil {
		a.transportFailed(ctx, err)
		refuse(code.Refuse(502, "code_upstream_unavailable"))
		return nil, nil
	}
	defer upstreamResponse.Body.Close()
	a.onResponseHeaders(ctx, upstreamResponse.StatusCode, upstreamResponse.Header)
	next, recoveryErr := a.recover(ctx, upstreamResponse, route, recovery, payload.translated)
	if recoveryErr != nil {
		return nil, recoveryErr
	}
	if next != nil {
		return next, nil
	}
	if payload.target == vendor.TargetAntigravity {
		a.antigravityResponse(ctx, w, r, upstreamResponse, observation.Operation.Model, payload)
		return nil, nil
	}
	if payload.translated {
		a.translateResponse(ctx, w, r, upstreamResponse, observation.Operation.Model, payload.stream, protocolFamily(ingress.Protocol), payload.includeUsage)
		return nil, nil
	}
	writeUpstreamResponseHeaders(w, upstreamResponse, a.core.model != a.core.publicModel)
	copyHTTPResponse(ctx, w, upstreamResponse, &a.core, a.observe, a.observeUnary, a.transportFailed)
	writeUpstreamResponseTrailers(w, upstreamResponse)
	return nil, nil
}

func (a *subscriptionAttempt) buildPayload(r *http.Request, ingress vendor.Endpoint, raw, body []byte) (translatedPayload, error) {
	requestBody := body
	if a.core.model != a.core.publicModel {
		var err error
		requestBody, err = wire.RewriteRequestModel(body, a.core.publicModel, a.core.model)
		if err != nil {
			return translatedPayload{}, err
		}
	}
	if a.endpoint.Target == vendor.TargetAntigravity {
		return translateAntigravity(requestBody, r, a.core.model, a.auth.ProjectID)
	}
	if a.endpoint.Protocol == ingress.Protocol {
		if a.core.model != a.core.publicModel {
			return translatedPayload{body: requestBody, query: r.URL.RawQuery}, nil
		}
		return translatedPayload{body: raw, query: r.URL.RawQuery}, nil
	}
	if ingress.Protocol == code.ProtocolChat {
		return translateChatToCodex(requestBody, r, a.core.model)
	}
	return translateMessagesToCodex(requestBody, r, a.core.model)
}

func (a *subscriptionAttempt) onResponseHeaders(ctx context.Context, status int, headers http.Header) {
	a.core.status = status
	if headers != nil {
		a.responseHeaders = headers.Clone()
	}
	now := a.core.server.Host.Now().UTC()
	a.core.recordOutcome(ctx, code.UpstreamHeadersOutcome(status, now))
	if status == http.StatusUnauthorized {
		a.core.server.Host.CredentialRefused(a.auth.CredentialID, a.auth.GrantGeneration)
	}
	if status == 429 || status == 401 || status == 403 || status >= 500 {
		health := code.HealthUnavailable
		if status == 429 {
			health = code.HealthQuotaLimited
		}
		var retryAt *time.Time
		if values := headers.Values("Retry-After"); len(values) == 1 {
			if delay, valid := upstream.ParseRetryAfter(values[0], now); valid {
				at := now.Add(delay)
				retryAt = &at
			}
		}
		a.flushPendingHealth(ctx)
		obs := code.NewDegradedObservation(health, now, retryAt)
		a.pendingHealth = &obs
	}
	if a.endpoint.Observation == vendor.ObserveCodex {
		if allowance := wire.CodexAllowance(headers, a.core.server.Host.Now()); allowance != nil {
			a.observeAllowance(ctx, *allowance)
		}
	}
}

func (a *subscriptionAttempt) observeUnary(body []byte) {
	observation := a.core.observer.Unary(body)
	if a.core.status >= 400 && len(observation.RejectionBody) == 0 {
		a.observe(code.Observation{RejectionBody: wire.RejectionBody(body)})
	}
	a.observe(observation)
	if a.core.status >= 400 {
		a.core.terminal = true
	}
}

func (a *subscriptionAttempt) observeEncodedUnary(body []byte, encoding string) {
	if decoded, err := wire.Decode(body, encoding, wire.MaxBody); err == nil {
		a.observeUnary(decoded)
	}
}

func (a *subscriptionAttempt) observe(o code.Observation) {
	a.core.recordReference(o.ResponseID)
	classification, classified := a.observeCooldownClass(o)
	switch {
	case classified && classification.Scope == code.ScopeModel:
		a.pendingHealth = nil
	case len(o.RejectionBody) > 0:
		a.flushPendingHealth(context.Background())
	}
	if o.Status != 0 && !classified {
		a.onResponseHeaders(context.Background(), o.Status, http.Header{"Retry-After": o.RetryAfter})
		a.flushPendingHealth(context.Background())
	}
	if o.Allowance != nil {
		a.observeAllowance(context.Background(), *o.Allowance)
	}
	if o.AllowanceCharge != nil && a.vendor.SupportsAllowanceCharge && a.core.attemptID != "" {
		step, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = a.core.server.Ledger.ObserveAllowanceCharge(step, a.core.attemptID, o.AllowanceCharge)
		cancel()
	}
	if o.Outcome != nil {
		a.core.recordOutcome(context.Background(), *o.Outcome)
	}
	if o.Successful && !a.prewarm {
		a.observeHealth(context.Background(), code.NewHealthyObservation(a.core.server.Host.Now().UTC()))
	}
	a.core.applyTerminal(o)
}

func (a *subscriptionAttempt) observeAllowance(ctx context.Context, allowance code.Allowance) {
	step, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_ = a.core.server.Ledger.ObserveAllowance(step, a.permit.Account.ID, allowance)
}

func (a *subscriptionAttempt) observeHealth(ctx context.Context, observation code.HealthObservation) {
	step, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_ = a.core.server.Ledger.ObserveHealth(step, a.permit.Account.ID, observation)
}

func (a *subscriptionAttempt) flushPendingHealth(ctx context.Context) {
	if a.pendingHealth == nil {
		return
	}
	pending := *a.pendingHealth
	a.pendingHealth = nil
	a.observeHealth(ctx, pending)
}

func (a *subscriptionAttempt) observeCooldownClass(o code.Observation) (vendor.Classification, bool) {
	if len(o.RejectionBody) == 0 || a.permit.Account.ID == "" {
		return vendor.Classification{}, false
	}
	status := o.Status
	if status == 0 {
		status = a.core.status
	}
	headers := http.Header{"Retry-After": o.RetryAfter}
	if len(o.RetryAfter) == 0 && a.responseHeaders != nil {
		headers = a.responseHeaders
	}
	now := a.core.server.Host.Now().UTC()
	classification, ok := a.vendor.Classify(a.endpoint, status, o.RejectionBody, headers, now)
	if !ok {
		return vendor.Classification{}, false
	}
	if classification.SuppressFallback {
		a.pendingHealth = nil
		return classification, true
	}
	classification = vendor.WithCycleReset(classification, a.permit.Account.CycleAnchor, now)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := a.core.server.Ledger.ObserveCooldownClass(ctx, a.permit.Account.ID, a.core.model, classification, now)
	return classification, err == nil
}

func (a *subscriptionAttempt) transportFailed(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	now := a.core.server.Host.Now().UTC()
	a.core.recordOutcome(ctx, code.GatewayOutcome(code.OutcomeTransportError, now))
	if closeCode := websocket.CloseStatus(err); closeCode >= websocket.StatusInternalError && closeCode <= websocket.StatusBadGateway {
		a.observeHealth(ctx, code.NewDegradedObservation(code.HealthUnavailable, now, nil))
	}
}

func (a *subscriptionAttempt) finish(ctx context.Context) {
	if a.core.finished {
		return
	}
	a.core.finished = true
	a.flushPendingHealth(ctx)
	a.core.settleAttempt(ctx)
	step, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	a.core.server.Host.SettleLease(step, a.lease, a.core.dispatched, a.core.usage.Total)
	a.core.server.Host.SettleLease(step, a.providerLease, a.core.dispatched, a.core.usage.Total)
}

type rejectionBody struct {
	io.Reader
	io.Closer
}

func readTranslatedRejection(ctx context.Context, body io.ReadCloser, limit int64) ([]byte, error) {
	type readResult struct {
		body []byte
		err  error
	}
	result := make(chan readResult, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(body, limit))
		result <- readResult{body: data, err: err}
	}()
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case received := <-result:
		return received.body, received.err
	case <-timer.C:
		_ = body.Close()
		return nil, context.DeadlineExceeded
	case <-ctx.Done():
		_ = body.Close()
		return nil, ctx.Err()
	}
}

func (a *subscriptionAttempt) recover(ctx context.Context, response *http.Response, route code.Route, previous *code.Recovery, translated bool) (*code.Recovery, error) {
	ordinal := 1
	if previous != nil {
		ordinal = previous.Number + 1
	}
	if route.InitialRecoveryAttempts < 1 || response.StatusCode != 401 && response.StatusCode != 429 || ctx.Err() != nil {
		return nil, nil
	}
	body, err := func() ([]byte, error) {
		if translated {
			return readTranslatedRejection(ctx, response.Body, (64<<10)+1)
		}
		return io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	}()
	response.Body = rejectionBody{Reader: io.MultiReader(bytes.NewReader(body), response.Body), Closer: response.Body}
	if err != nil || len(body) > 64<<10 {
		return nil, nil
	}
	rejection, proven := a.vendor.InitialRejection(a.endpoint, response.StatusCode, body, response.Header, a.core.server.Host.Now())
	if !proven {
		return nil, nil
	}
	if err := a.core.server.Ledger.ConfirmNonexecution(ctx, a.core.attemptID, response.StatusCode); err != nil {
		return nil, nil
	}
	rejection = vendor.RejectionWithCycleReset(rejection, a.permit.Account.CycleAnchor, a.core.server.Host.Now().UTC())
	zero := int64(0)
	a.core.usage, a.core.terminal = code.Usage{Total: &zero}, true
	recorded := true
	if rejection.Class == upstream.RateLimit {
		_, recorded = a.observeCooldownClass(code.Observation{Status: response.StatusCode, RejectionBody: body, RetryAfter: response.Header.Values("Retry-After")})
		if recorded {
			a.pendingHealth = nil
		}
	}
	a.finish(ctx)
	if !recorded {
		return nil, nil
	}
	if rejection.Class == upstream.Credential {
		if err := a.core.server.Ledger.ObserveCooldown(ctx, a.permit.Account.ID, rejection.RetryAt); err != nil {
			return nil, nil
		}
	}
	if !a.permit.InitialRecovery {
		return nil, code.Refuse(409, "code_recovery_unsafe")
	}
	if ordinal > route.InitialRecoveryAttempts {
		return nil, nil
	}
	if rejection.Class == upstream.Credential && a.auth.CredentialID != "" && a.auth.GrantGeneration > 0 && a.refreshed(ctx) {
		rec := code.NewCredentialRecovery(a.core.attemptID, rejection.Envelope, ordinal, code.CredentialRefreshTarget{
			AccountID: a.permit.Account.ID, CredentialID: a.auth.CredentialID,
			GrantGeneration: a.auth.GrantGeneration, RejectedUntil: rejection.RetryAt,
		})
		return &rec, nil
	}
	rec := code.NewRebindRecovery(a.core.attemptID, rejection.Envelope, ordinal)
	return &rec, nil
}

func (a *subscriptionAttempt) refreshed(ctx context.Context) bool {
	refreshCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	dispatch := code.Dispatch{Adapter: a.vendor.Adapter, Protocol: a.endpoint.Protocol, ClientPath: a.endpoint.ClientPath}
	for {
		auth, err := a.core.server.Authorizer.AuthorizeCode(refreshCtx, a.config, a.permit.Account, dispatch)
		if err == nil && auth.CredentialID == a.auth.CredentialID && auth.GrantGeneration > a.auth.GrantGeneration && a.vendor.ValidateAuthorization(a.endpoint, auth, a.permit.Pin.Principal) == nil {
			return true
		}
		select {
		case <-refreshCtx.Done():
			return false
		case <-ticker.C:
		}
	}
}
