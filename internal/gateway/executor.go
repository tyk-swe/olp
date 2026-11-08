package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/upstream"
	"github.com/tyk-swe/olp/internal/usage"
)

// Failure classes shared with the routing retry taxonomy fixture. Upstream
// exchanges are classified by internal/upstream.
const (
	classSuccess        = "success"
	classConnect        = string(upstream.Connect)
	classTimeout        = string(upstream.Timeout)
	classRateLimit      = string(upstream.RateLimit)
	classUpstreamServer = string(upstream.ServerError)
	classUpstreamClient = string(upstream.ClientError)
	classCredential     = string(upstream.Credential)
	classProtocol       = string(upstream.Protocol)
	classPolicy         = "policy"
	classCancelled      = string(upstream.Cancelled)
	// classAmbiguous marks a side-effecting attempt whose upstream outcome the
	// gateway cannot prove; it never fails over.
	classAmbiguous         = string(upstream.Ambiguous)
	classLimitsUnavailable = "limits_unavailable"
	classContextWindow     = string(upstream.ContextWindow)
	classContentFilter     = string(upstream.ContentFilter)
	// classBudget marks an attempt a spend cap on its connection, credential
	// slot or route refused before dispatch.
	classBudget = "budget"
)

// Canonical defaults retained by existing accounting fixtures.
const (
	operationGeneration = "generation"
	surfaceOpenAI       = "openai"
)

const (
	maxStreamDuration = time.Hour
	errorBodyLimit    = 64 * 1024
)

// applyCredentials authenticates an upstream request and records the credential
// values that must be redacted from provider errors.
func (s *Server) applyCredentials(ctx context.Context, x *call, req *http.Request, cfg connectors.Config, secret, body []byte) error {
	applied, err := s.auth.Apply(ctx, req, cfg, secret, body)
	x.sensitive.Include(applied)
	return err
}

// redacted removes this execution's applied credentials from every field of a
// provider error before it can reach a client, a log, or accounting.
func (x *call) redacted(e *wire.UpstreamError) *wire.UpstreamError {
	return e.Redact(x.sensitive.Redact)
}

// usage is the metering evidence the request ends with: the usage of the
// attempt that served it, which is the last one recorded.
func (x *call) usage() *wire.Usage {
	if len(x.facts) == 0 {
		return nil
	}
	return x.facts[len(x.facts)-1].Usage
}

// delivered records when the first byte of the response payload reached the
// client. Later deliveries keep the first one.
func (x *call) delivered(at time.Time) {
	if x.firstByte == nil {
		elapsed := at.Sub(x.request.startedAt)
		x.firstByte = &elapsed
	}
}

// outcome is the terminal result of the attempt loop.
type outcome struct {
	completion *wire.Completion
	approval   *workflowPause
	err        *Error
	committed  bool
	cancelled  bool
}

// dispatchableAttempts returns the most attempts this request can hand to a
// provider, capped by its requested budget. Each target may be tried through
// each of its usable credential slots, including bounded same-slot retries.
func (s *Server) dispatchableAttempts(x *call) int {
	n := s.walkDispatchable(x, x.attempts, nil)
	if x.mayLeaveRoute() {
		// Fallback and delegating routes may spend the rest of the budget.
		return max(n, x.allowance-x.spent)
	}
	return n
}

// walkDispatchable visits each usable credential slot in the supplied target
// order, including its maximum same-slot retries, up to the remaining attempt
// budget. Cost admission may use price order to cover alternate retry paths.
func (s *Server) walkDispatchable(x *call, attempts []routing.Attempt, visit func(routing.Attempt)) int {
	remaining := x.budget - x.spent
	if remaining <= 0 {
		return 0
	}
	repeats := retryDispatches(x.route)
	available := 0
	for _, attempt := range attempts {
		provider, ok := x.provider(attempt.ProviderID)
		if !ok {
			continue
		}
		for i := range provider.Slots {
			if s.slotAvailable(x, attempt, &provider, &provider.Slots[i]) {
				for range min(repeats, remaining-available) {
					available++
					if visit != nil {
						visit(attempt)
					}
				}
			}
			if available == remaining {
				return available
			}
		}
	}
	return available
}

// retryDispatches bounds one slot's dispatches across the route's retry rules.
func retryDispatches(route *routing.Route) int {
	retries := 0
	for _, rule := range route.Retry {
		retries = max(retries, rule.MaxRetries)
	}
	return 1 + retries
}

type attemptFailure struct {
	class        string
	status       int
	committed    bool
	retryAfter   time.Duration
	upstream     *wire.UpstreamError
	acceptance   upstream.Acceptance // what the exchange established about the upstream's work
	accepted     bool                // a successful response began, even if its result could not be metered
	overall      bool                // the route deadline, not the attempt deadline, expired
	dispatched   bool                // the request reached the upstream before the failure
	quota        string              // a quota this gateway enforces rejected the attempt
	contractCode string              // safe runtime interaction guard violation
	policyCode   string              // local output policy refusal after upstream completion
	noRetry      bool                // outcome uncertainty must not suggest client retries
	// credentialRefused survives ambiguity so a grant can refresh without
	// allowing this attempt to fail over.
	credentialRefused bool
	// aggregateTooLarge reports a forced stream whose non-streaming result
	// exceeded the response size limit.
	aggregateTooLarge bool
}

// The quotas that can reject an attempt before it is dispatched.
const (
	quotaConnection = "connection"
	quotaSlot       = "slot"
	// quotaRoute is a route's own spend cap, which every target shares.
	quotaRoute = "route"
)

// billingUncertain reports whether the upstream may have served and billed
// work this attempt cannot account for. Anything already delivered to the
// client was served, and work the upstream may hold is what its acceptance
// leaves unresolved. A successful response may be billable even when its
// result is terminal but unreadable. A stated rejection costs nothing, a 5xx
// may have done the work its status denied, and a failure that never left this
// gateway is free. Exchange evidence, not the class, decides: a declared rule
// changes how the failure is routed, never what was billed. A failure built
// without classified evidence falls back to its class.
func (f *attemptFailure) billingUncertain() bool {
	if f.committed || f.accepted {
		return true
	}
	if f.acceptance != "" {
		return f.acceptance.Unresolved()
	}
	switch f.class {
	case classRateLimit, classUpstreamClient, classCredential, classContextWindow, classContentFilter:
		return false
	}
	return f.dispatched
}

func (f *attemptFailure) toError() (result *Error) {
	defer func() {
		if result != nil && result.Status >= 500 && f.noRetry {
			result.NoRetry = true
		}
	}()
	if f.contractCode != "" {
		return serverError(http.StatusBadGateway, f.contractCode, "The provider result did not satisfy the admitted interaction contract.")
	}
	if f.policyCode == "guardrail_surface_unavailable" {
		return guardrailError(guardrails.ErrSurface)
	}
	if f.policyCode != "" {
		message := "The provider result was blocked by the route's content policy."
		if f.policyCode == "policy_conflict" {
			message = "The route's content policy cannot inspect this provider result."
		}
		return invalidRequest(f.policyCode, message, nil)
	}
	if f.aggregateTooLarge {
		return serverError(http.StatusBadGateway, "upstream_response_too_large", "The upstream streamed a result larger than the gateway's response size limit for a non-streaming request; request a streaming response instead.")
	}
	switch f.class {
	case classLimitsUnavailable:
		return limitsUnavailable()
	case classAmbiguous:
		err := serverError(http.StatusBadGateway, "ambiguous_upstream_result", "The upstream provider may have applied this request; its result could not be confirmed.")
		err.NoRetry = f.noRetry
		return err
	case classCancelled:
		return &Error{Status: 0, Code: "client_cancelled", Message: "The client went away."}
	case classTimeout:
		return serverError(http.StatusGatewayTimeout, "gateway_timeout", "The upstream provider did not respond within the configured deadline.")
	case classConnect:
		return serverError(http.StatusBadGateway, "upstream_unavailable", "The upstream provider could not be reached.")
	case classRateLimit:
		// A quota this gateway enforces was never the upstream's decision, and
		// saying so would send the caller looking at the wrong system.
		switch f.quota {
		case quotaConnection:
			return &Error{Status: http.StatusTooManyRequests, Type: "rate_limit_error", Code: "rate_limit_exceeded", Message: "The provider connection limit was exceeded.", RetryAfter: f.retryAfter}
		case quotaSlot:
			return &Error{Status: http.StatusTooManyRequests, Type: "rate_limit_error", Code: "rate_limit_exceeded", Message: "The provider credential limit was exceeded.", RetryAfter: f.retryAfter}
		}
		return &Error{Status: http.StatusTooManyRequests, Type: "rate_limit_error", Code: "upstream_rate_limit", Message: "The upstream provider is rate limiting requests.", RetryAfter: f.retryAfter}
	case classBudget:
		return serverError(http.StatusServiceUnavailable, "supply_budget_exhausted", "The spend cap of the "+f.quota+" serving this request is exhausted.")
	case classUpstreamServer:
		return serverError(http.StatusBadGateway, "upstream_unavailable", "The upstream provider failed with HTTP "+strconv.Itoa(f.status)+".")
	case classCredential:
		code := "upstream_authentication_failed"
		if f.status == http.StatusForbidden {
			code = "upstream_permission_denied"
		}
		return serverError(http.StatusBadGateway, code, "The upstream provider rejected the configured credential.")
	case classProtocol:
		return serverError(http.StatusBadGateway, "provider_protocol_error", "The upstream provider returned a malformed response.")
	case classContentFilter:
		message := "The upstream provider's content filter refused the request."
		if f.upstream != nil && f.upstream.Message != "" {
			message = f.upstream.Message
		}
		status := f.status
		if !forwardable(status) {
			status = http.StatusBadRequest
		}
		return &Error{Status: status, Type: "invalid_request_error", Code: "content_filter", Message: message}
	case classUpstreamClient, classContextWindow:
		message := "The upstream provider rejected the request."
		if f.upstream != nil && f.upstream.Message != "" {
			message = f.upstream.Message
		}
		if forwardable(f.status) {
			return &Error{Status: f.status, Type: "invalid_request_error", Code: "upstream_rejected", Message: message}
		}
		return serverError(http.StatusBadGateway, "upstream_rejected", message)
	}
	return serverError(http.StatusBadGateway, "upstream_unavailable", "The upstream provider failed.")
}

// forwardable reports whether an upstream client-error status is returned to
// the caller unchanged; every other upstream rejection is a gateway failure.
func forwardable(status int) bool {
	switch status {
	case 400, 404, 405, 409, 413, 415, 422:
		return true
	}
	return false
}

// A call owns mutexes, pinned authority, and request-wide accounting, so model steps update it in place.
func (s *Server) executeModelStep(ctx context.Context, x *call, source *wire.Request, current keyauthority.Authority) *outcome {
	if x.generation.firstStep == nil {
		x.generation.parsed = source
		out := s.execute(ctx, x)
		x.generation.firstStep = &modelStepAccounting{factEnd: len(x.facts), estimate: x.estimate}
		return out
	}
	if source.Route != x.named().Slug || source.Family != x.generation.parsed.Family || x.cache != nil && x.cache.result != "bypass" {
		return &outcome{err: invalidRequest("invalid_request", "The model step must use the original route and format without response caching.", nil)}
	}
	stepCtx, cancel := x.routeContext(ctx)
	defer cancel()
	first := x.generation.firstStep
	actual := x.settledTokensFor(x.facts[:first.factEnd], first.estimate)
	if err := x.lease.Reconcile(stepCtx, *actual); err != nil {
		if e := s.deps.Admission.outage(x.authority.ID, false, err); e != nil {
			return &outcome{err: e}
		}
	}
	x.generation.parsed = source
	x.routeLeg = routeLeg{affinity: x.seed}
	x.allowance = 0
	x.walk = routing.RouteWalk{}
	x.classified = nil
	if e := s.prepare(stepCtx, x, x.authorize); e != nil {
		return &outcome{err: e}
	}
	x.estimate = requestEstimate(x)
	authority := cacheAuthority(x.admissionAuthority(current), false, true, false)
	reservation := keyReservationEstimate(x.estimate, s.dispatchableAttempts(x))
	lease, e := s.deps.Admission.reserveKeyCosted(stepCtx, &authority, x.clientSurface(), reservation, time.Duration(x.named().OverallTimeout)*time.Millisecond, costReservation{}, x.limitRoute())
	if e != nil {
		return &outcome{err: e}
	}
	if e := s.reserveFallbackCost(stepCtx, x); e != nil {
		settleKey(ctx, lease, false, nil, s.log)
		return &outcome{err: e}
	}
	start := len(x.facts)
	x.stepDispatched = false
	out := s.execute(stepCtx, x)
	settleKey(ctx, lease, x.stepDispatched, x.settledTokensFor(x.facts[start:], x.estimate), s.log)
	return out
}

// execute adapts canonical inference to shared attempt execution. The
// canonical transport retains its first-byte and streaming idle deadlines.
func (s *Server) execute(ctx context.Context, x *call) *outcome {
	if x.cacheHit() {
		return s.replayCache(ctx, x)
	}
	ctx, cancel := x.routeContext(ctx)
	defer cancel()
	out := runAttempts(ctx, s, x, attemptAdapter[*wire.Completion]{
		estimate: x.attemptReservation,
		dispatch: func(ctx context.Context, attempt routing.Attempt, provider *routing.Provider, slot routing.Slot, fact AttemptFact) (AttemptFact, *wire.Completion, *attemptFailure) {
			return s.attempt(ctx, x, attempt, provider, slot, fact)
		},
	})
	return &outcome{
		completion: out.result,
		err:        out.err,
		committed:  out.committed,
		cancelled:  out.cancelled,
	}
}

// slots returns the credential slots usable for this attempt, ordered by
// priority then by deterministic weighted rendezvous on the request's
// affinity so sibling keys spread across a pool.
func (s *Server) slots(x *call, attempt routing.Attempt, provider *routing.Provider) []routing.Slot {
	if x.pinnedSlot != nil {
		if !s.slotAvailable(x, attempt, provider, x.pinnedSlot) {
			return nil
		}
		return []routing.Slot{*x.pinnedSlot}
	}
	ordered := plannedSlots(x, attempt, provider)
	out := make([]routing.Slot, 0, len(ordered))
	for _, slot := range ordered {
		if !s.slotAvailable(x, attempt, provider, &slot) || (!s.deps.Admission.ready() && (s.health.coolingDown(provider.ID, slot.ID) || s.health.coolingDown(provider.ID, credentialHealthKey(&slot, s.slotGrantGeneration(&slot))))) {
			continue
		}
		out = append(out, slot)
	}
	return out
}

func plannedSlots(x *call, attempt routing.Attempt, provider *routing.Provider) []routing.Slot {
	if attempt.Slots != nil {
		return routing.ArrangeSlots(provider.Slots, attempt.Slots)
	}
	return routing.SelectSlots(*provider, attempt.UpstreamModel, *x.route, x.keyID, x.operationName(), x.surfaceName(), x.mode, x.affinity)
}

func (s *Server) slotAvailable(x *call, attempt routing.Attempt, provider *routing.Provider, slot *routing.Slot) bool {
	if !slot.Allows(attempt.UpstreamModel, x.route.Slug, x.keyID) {
		return false
	}
	if !connectors.SecretRequired(provider.AuthMode) {
		return true
	}
	return slot.CredentialID != nil && s.deps.Runtime.Eligibility(*slot.CredentialID) == routing.Eligible
}

// attempt performs one upstream call with one credential.
func (s *Server) attempt(ctx context.Context, x *call, a routing.Attempt, provider *routing.Provider, slot routing.Slot, fact AttemptFact) (AttemptFact, *wire.Completion, *attemptFailure) {
	if x.guardrailEngine != nil {
		x.guardrailCtx = context.WithValue(ctx, guardrailCallerKey{}, x)
	}
	cfg := provider.Connector()
	fact.Carried = cfg.CarriedByPlugin()
	// A plugin that carries the request reports only whether it was sent, so
	// the attempt must not risk repeating work it may have done.
	resolved := x.resolvedGenerationResources()
	conversationBound := resolved["/conversation"] != "" || resolved["/conversation/id"] != ""
	retainedResponse := x.family == wire.Responses && x.providerState && (responseStoreRequested(x.generation.parsed) || conversationBound)
	st := &attemptState{parent: ctx, classifier: upstream.Classifier{ContextWindow: true, AtMostOnce: fact.Interaction != nil || fact.Carried || retainedResponse, Declared: cfg.Classification()}}
	var parallel *parallelGuardrail
	nativeDeliveryClass := ""
	attemptCtx, atr := x.request.trace.Attempt(ctx, provider.Kind, a.ProviderRevisionID, a.UpstreamModel)
	finishTrace := func() {
		if atr == nil {
			return
		}
		if u := fact.Usage; u != nil {
			atr.RecordUsage(&u.InputTokens, &u.OutputTokens, u.CachedInputTokens, u.MediaUnits)
		}
		atr.RecordResponse(fact.ResponseModel, fact.FinishReasons)
		atr.Finish(fact.Class, fact.Status)
	}
	fail := func(class string, f *attemptFailure) (AttemptFact, *wire.Completion, *attemptFailure) {
		if f == nil {
			f = &attemptFailure{}
		}
		if parallel != nil {
			if guardErr := parallel.join(x, true); guardErr != nil {
				class = classPolicy
				f.policyCode = guardrailError(guardErr).Code
				f.noRetry = true
				f.overall = true
			}
		}
		f.dispatched = st.dispatched.Load() || (x.workflow != nil && st.connected.Load() && !st.notSent.Load())
		f.credentialRefused = st.credentialRefused
		f.acceptance = st.evidence().Acceptance()
		f.accepted = st.accepted
		if x.generation.continuation != nil && x.generation.continuation.resource != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			_ = s.deps.Resources.MarkUnknown(cleanup, x.generation.continuation.resource)
			cancel()
		}

		if fact.Interaction != nil || fact.Carried || (retainedResponse && f.acceptance.Unresolved()) {
			f.noRetry = f.noRetry || f.dispatched
		}
		if fact.Interaction != nil {
			fact.Interaction.UpstreamState = string(f.acceptance)
		}
		f.class = class
		fact.Class = class
		if nativeDeliveryClass != "" {
			fact.Class = nativeDeliveryClass
		} else if fact.localRefusal && st.settled {
			fact.Class = classSuccess
		}
		fact.Committed = f.committed
		fact.Duration = s.now().Sub(fact.StartedAt)
		if f.retryAfter > 0 {
			retry := f.retryAfter
			fact.RetryAfter = &retry
		}
		fact.recordEvidence(f.billingUncertain())
		finishTrace()
		return fact, nil, f
	}

	_, err := s.egress.ValidateEndpoint(provider.Endpoint)
	if err != nil {
		return fail(classConnect, nil)
	}
	var body []byte
	var encodedWire wire.Format
	var streamCandidates int64
	var contract *interaction.Plan
	if provider.ProfileID != "" || x.strict() || x.route.ContentPolicy != nil || x.guardrailEngine != nil {
		prepared, prepareErr := x.preparedProvider(provider, a.TargetID, a.UpstreamModel)
		err = prepareErr
		if err == nil {
			x.guardrailInput = prepared.guardrailSegments
			for _, decision := range prepared.policyDecisions {
				recordDecision(x, decision)
			}
			body, encodedWire = prepared.invocation.Prepared.Document().Bytes(), prepared.invocation.Wire
			native, sourceErr := wire.NewRequest(encodedWire, x.generation.parsed.Route, x.generation.parsed.Stream, prepared.invocation.Prepared.Document())
			if sourceErr != nil {
				return fail(classProtocol, nil)
			}
			_, streamCandidates = estimate.OutputBounds(native, nil)
			contract = prepared.plan
			if contract != nil {
				cfg = contract.Config()
			}
		}
	} else {
		body, encodedWire, streamCandidates, err = x.encoding(provider, cfg, a.TargetID, a.UpstreamModel)
	}
	if err != nil {
		return fail(classProtocol, nil)
	}
	body = cfg.WrapRequest(body, a.UpstreamModel)
	// A profile that forces streaming streams every request, and the gateway
	// aggregates the stream for a caller that did not ask for one.
	aggregated := !x.generation.parsed.Stream && cfg.ForcesStreaming()
	deadline, _ := ctx.Deadline()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return fail(classTimeout, &attemptFailure{overall: true})
	}
	timeout := min(a.Timeout, remaining)

	actx, cancel := context.WithCancel(attemptCtx)
	defer cancel()
	firstByte := time.AfterFunc(timeout, func() { st.reason.CompareAndSwap(0, 1); cancel() })
	defer firstByte.Stop()

	endpoint, err := cfg.URL(encodedWire, a.UpstreamModel, x.generation.parsed.Stream || aggregated)
	if err != nil {
		return fail(classProtocol, nil)
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(actx, st.trace()), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fail(classConnect, nil)
	}
	atr.InjectUpstream(req.Header, x.request.trace.PropagateUpstream())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "olp/gateway")
	req.Header.Set("Accept", "application/json")
	if x.generation.parsed.Stream || aggregated {
		req.Header.Set("Accept", "text/event-stream")
		if encodedWire == "bedrock" || cfg.EventStream() {
			req.Header.Set("Accept", "application/vnd.amazon.eventstream")
		}
	}
	if contract == nil {
		cfg = forwardAnthropicBeta(req.Header, x, encodedWire, cfg)
	}
	if err := s.applySlotCredential(actx, x, req, cfg, slot, body, provider, a.UpstreamModel); err != nil {
		switch {
		case actx.Err() != nil:
			return fail(st.classify(err, false), nil)
		case errors.Is(err, connectors.ErrSigningUnavailable):
			// The plugin couldn't sign the request, which says nothing
			// about the credential: the upstream is out of reach from
			// here, like one the gateway can't connect to.
			return fail(classConnect, nil)
		}
		return fail(classCredential, nil)
	}

	var client *http.Client
	if cfg.CarriedByPlugin() {
		client = cfg.CarrierClient(s.cfg.Carrier, x.sensitive.Values())
	} else if client, err = s.providerClient(actx, x.request.release, provider, slot); err != nil {
		return fail(classCredential, nil)
	}
	if contract != nil && contract.ToolContinuation() {
		if err := s.claimToolWork(actx, x, contract, a, slot); err != nil {
			return fail(classProtocol, &attemptFailure{contractCode: "continuation_unavailable", noRetry: true})
		}
	}
	parallel = x.startParallel(actx, cancel)
	if parallel != nil {
		defer parallel.cancel()
	}
	if x.workflow != nil {
		if e := s.markSessionDispatched(actx, x); e != nil {
			return fail(classConnect, &attemptFailure{contractCode: e.Code, noRetry: true})
		}
	}
	resp, err := client.Do(req)
	if cfg.CarriedByPlugin() {
		// The plugin reports whether it sent a request it failed; any other
		// failure may have reached the upstream.
		st.dispatched.Store(!errors.Is(err, connectors.ErrNotSent))
		st.notSent.Store(errors.Is(err, connectors.ErrNotSent))
	}
	if err != nil {
		return fail(st.classify(err, false), nil)
	}
	defer resp.Body.Close()
	// The response status is the first thing the upstream sends back.
	received := s.now().Sub(fact.StartedAt)
	fact.FirstByte = &received
	fact.Status = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		status, raw := cfg.Rejection(resp.StatusCode, raw)
		f := &attemptFailure{status: status, upstream: x.redacted(openai.ParseErrorBody(raw))}
		class := st.rejected(status, f.upstream)
		if class == classRateLimit {
			f.retryAfter = upstream.RetryAfter(resp.Header.Get("Retry-After"), s.now())
		}
		return fail(class, f)
	}
	st.accepted = true

	var completion *wire.Completion
	committed := false
	actionable := false
	if x.generation.parsed.Stream {
		mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		if mediaType != "text/event-stream" && !((encodedWire == "bedrock" || cfg.EventStream()) && mediaType == "application/vnd.amazon.eventstream") {
			return fail(classProtocol, &attemptFailure{status: resp.StatusCode})
		}
		streamCap := time.AfterFunc(maxStreamDuration, func() { st.reason.CompareAndSwap(0, 3); cancel() })
		defer streamCap.Stop()
		var watchdog *time.Timer
		defer func() {
			if watchdog != nil {
				watchdog.Stop()
			}
		}()
		emit := func(frame []byte) error {
			if e := parallel.join(x, terminalGuardrailFrame(x.family, frame)); e != nil {
				return e
			}
			if x.providerState {
				mapped, mapErr := s.mapStreamResponseFrame(ctx, x, &fact, frame, encodedWire != wire.Responses)
				if mapErr != nil {
					return mapErr
				}
				frame = mapped
			}
			if x.family == wire.Responses {
				mapped, mapErr := s.mapGenerationReferenceFrame(ctx, x, responseNativeIdentity(x, &fact, ""), frame)
				if mapErr != nil {
					return &responseLocalRefusal{cause: mapErr}
				}
				frame = mapped
			}
			if encodedWire == wire.Responses && (bytes.Contains(frame, []byte("event: response.failed\n")) || bytes.Contains(frame, []byte("event: error\n"))) {
				var err error
				frame, err = redactFailedResponseFrame(frame, x.sensitive)
				if err != nil {
					return err
				}
			}
			if !committed {
				committed = true
				firstByte.Stop()
				watchdog = time.AfterFunc(a.Timeout, func() { st.reason.CompareAndSwap(0, 2); cancel() })
			} else {
				watchdog.Reset(a.Timeout)
			}
			if len(frame) > int(s.cfg.MaxEventBytes) {
				return wire.ErrEventTooLarge
			}
			err := x.generation.emit(frame)
			if err == nil && x.capture != nil {
				x.captureDeliveredFrame(frame)
			}
			if fact.Interaction != nil {
				if fact.Interaction.ClientState == usage.ClientUnobserved {
					fact.Interaction.ClientState = usage.ClientPartial
				}
				if actionable {
					// A failed write can have exposed a complete call before losing
					// the remaining frame. Do not infer non-actionability from error.
					fact.Interaction.ClientState = usage.ClientActionable
				}
			}
			if err == nil && fact.FirstOutput == nil && protocols.MeaningfulFrame(x.family, frame) {
				elapsed := s.now().Sub(fact.StartedAt)
				fact.FirstOutput = &elapsed
			}
			return err
		}
		deliver := emit
		var guarded *guardrailStream
		if x.guardrailEngine.HasPhase(guardrails.OutputStream) || x.guardrailEngine.HasAction(guardrails.Input, guardrails.Mask) {
			guarded, err = newGuardrailStream(x)
			if err != nil {
				return fail(classPolicy, &attemptFailure{policyCode: guardrailError(err).Code, overall: true, noRetry: true})
			}
			emit = func(frame []byte) error {
				frames, e := guarded.push(actx, frame)
				if e != nil {
					return e
				}
				for _, f := range frames {
					if e = deliver(f); e != nil {
						return e
					}
				}
				return nil
			}
		}
		strictResponseIncomplete := false
		var observe func(oif.Event) error
		if contract != nil {
			observe = func(event oif.Event) error {
				if err := contract.ValidateEvent(event); err != nil {
					return err
				}
				if x.strict() && x.family == wire.Responses {
					if kind, present := event.Source().Root().Lookup("type"); present {
						if text, ok := kind.Text(); ok && text == "response.incomplete" {
							strictResponseIncomplete = true
						}
					}
				}
				actionable = actionable || eventActionable(event)
				return nil
			}
		}
		if encodedWire == wire.Responses && x.providerState {
			contractObserve := observe
			observe = func(event oif.Event) error {
				if err := s.observeStreamResponseEvent(ctx, x, &fact, event); err != nil {
					return err
				}
				if contractObserve != nil {
					return contractObserve(event)
				}
				return nil
			}
		}
		if contract != nil && contract.ToolContinuation() {
			var projection *interaction.ToolProjection
			projection, err = contract.NewToolProjection(min(resources.MaxContinuationBytes, int(s.cfg.MaxResponseBytes)))
			if err == nil {
				completion, err = protocols.StreamWithEvents(encodedWire, encodedWire, cfg.StreamPayload(resp.Body, int(s.cfg.MaxEventBytes)), int(s.cfg.MaxEventBytes), x.named().Slug, true, func([]byte) error { return nil }, func(event oif.Event) error {
					frames, e := projection.Observe(event)
					if e != nil {
						return e
					}
					if watchdog != nil {
						watchdog.Reset(a.Timeout)
					}
					for _, frame := range frames {
						if e = emit(interaction.ContinuationFrame(frame)); e != nil {
							return e
						}
						x.generation.continuation.emitted++
					}
					return nil
				})
			}
			if err == nil {
				st.settled = true
				var state *interaction.Continuation
				var delivery interaction.Delivery
				state, delivery, err = projection.Complete(completion, x.generation.continuation.resource.ID)
				if err == nil {
					err = s.validateToolDelivery(delivery)
				}
				if err == nil {
					err = s.commitToolDelivery(ctx, x, state, delivery)
				}
				if err == nil {
					for _, frame := range delivery.Frames[x.generation.continuation.emitted:] {
						actionable = actionable || interaction.ContinuationActionable(frame)
						if err = emit(interaction.ContinuationFrame(frame)); err != nil {
							break
						}
					}
					if err == nil {
						err = emit([]byte("data: [DONE]\n\n"))
					}
				}
			}
		} else {
			completion, err = protocols.StreamWithEvents(encodedWire, x.family, cfg.StreamPayload(resp.Body, int(s.cfg.MaxEventBytes)), int(s.cfg.MaxEventBytes), x.named().Slug, x.generation.parsed.IncludeUsage, emit, observe, streamCandidates)
		}
		if guarded != nil {
			if err == nil {
				var frames [][]byte
				frames, err = guarded.finish(actx)
				if err == nil {
					for _, frame := range frames {
						if err = deliver(frame); err != nil {
							break
						}
					}
				}
			}
			x.recordGuardrails(guarded.stream.Decisions())
		}
		if err == nil && strictResponseIncomplete {
			st.settled = true
			err = &wire.ProtocolError{Detail: "strict Responses stream ended incomplete"}
		}
	} else {
		var raw []byte
		if aggregated {
			raw, err = s.aggregate(&fact, resp, cfg, encodedWire)
		} else if raw, err = io.ReadAll(&countingReader{r: resp.Body, limit: s.cfg.MaxResponseBytes}); err == nil {
			raw = cfg.UnwrapResponse(raw)
		}
		if err == nil {
			if contract != nil {
				var native *wire.Completion
				native, err = protocols.DecodeRequest(encodedWire, encodedWire, raw, x.named().Slug, "", contract.EffectiveRequest())
				if native != nil {
					if encodedWire == wire.Responses {
						fact.observeUsage(native.Usage, native.UsageComplete)
					} else {
						fact.Usage = native.Usage
					}
				}
				if err == nil {
					st.settled = true
					err = contract.ValidateResult(native.Native)
				}
				if err == nil && contract.ToolContinuation() {
					var state *interaction.Continuation
					var delivery interaction.Delivery
					state, delivery, err = contract.ProjectUnary(native, x.generation.continuation.resource.ID, min(resources.MaxContinuationBytes, int(s.cfg.MaxResponseBytes)))
					if err == nil {
						err = s.validateToolDelivery(delivery)
					}
					if err == nil {
						err = s.commitToolDelivery(ctx, x, state, delivery)
					}
					if err == nil {
						completion = native
						completion.Body = delivery.Body
					}
				}
				if err == nil && encodedWire == x.family {
					completion = native
					if x.strict() && x.family == wire.Responses {
						source := native.Native.Source()
						if _, present := source.Lookup("/model"); present {
							model, _ := json.Marshal(x.named().Slug)
							source, err = oif.Apply(source, []oif.Change{{Pointer: "/model", Value: string(model), Origin: oif.IdentityBinding, Reason: "published response model"}})
							if err != nil {
								err = &responseLocalRefusal{cause: err}
							}
						}
						if err == nil {
							completion.Body = source.Bytes()
						}
					}
				}
			}
			if err == nil && completion == nil {
				completion, err = protocols.DecodeRequest(encodedWire, x.family, raw, x.named().Slug, protocols.EmbeddingEncoding(x.generation.parsed, provider.ParameterDefaults), x.generation.parsed)
			}
		}
	}
	if guardErr := parallel.join(x, true); guardErr != nil {
		err = guardErr
	}
	if err == nil && completion != nil && !x.generation.parsed.Stream && x.guardrailEngine != nil {
		completion.Body, err = x.inspectGuardrailOutput(completion.Body)
	}
	if completion != nil {
		if x.generation.parsed.Stream || encodedWire == wire.Responses {
			fact.observeUsage(completion.Usage, completion.UsageComplete)
		} else {
			fact.Usage = completion.Usage
		}
		fact.ResponseModel = completion.ProviderModel
		if completion.FinishReason != "" {
			fact.FinishReasons = []string{completion.FinishReason}
		}
		if !x.generation.parsed.Stream && (encodedWire != x.family || x.family == wire.Embeddings || x.family == wire.Rerank) && int64(len(completion.Body)) > s.cfg.MaxResponseBytes {
			err = errResponseTooLarge
		}
	}
	if err != nil {
		f := &attemptFailure{status: resp.StatusCode, committed: committed}
		if errors.Is(err, errClientWrite) {
			if terminal, nativeFailure := completion.NativeResponseOutcome(); terminal {
				// The native result arrived before its client write failed.
				// Keep that provider outcome independent of the interruption
				// returned to the caller, including a native failure payload.
				st.settled = true
				// A retained create still owns accepted work even when its
				// terminal result never reached the original caller.
				f.noRetry = retainedResponse && st.dispatched.Load()
				nativeDeliveryClass = classSuccess
				if nativeFailure != nil {
					nativeDeliveryClass = string(st.classifier.Classify(upstream.Evidence{Settled: true, Error: nativeFailure}).Class)
				}
				return fail(st.classify(err, committed), f)
			}
		}
		_, localMapping := errors.AsType[*responseLocalRefusal](err)
		if localMapping || openai.IsResponseProjectionError(err) {
			fact.localRefusal = true
			f.overall, f.noRetry = true, st.dispatched.Load()
			terminal, nativeFailure := completion.NativeResponseOutcome()
			st.settled = st.settled || terminal
			if nativeFailure != nil {
				fact.localRefusal = false
				// A known failed stateless generation remains retryable. An
				// accepted retained create keeps its original resource owner.
				f.overall = retainedResponse || fact.GenerationUsageDeferred
				f.noRetry = f.overall && st.dispatched.Load()
				f.upstream = x.redacted(nativeFailure)
				class := st.classify(nativeFailure, committed)
				f.status = inBandStatus(class)
				return fail(class, f)
			}
			return fail(classAmbiguous, f)
		}
		var guardFailure *guardrails.Error
		var ue *wire.UpstreamError
		var violation *oif.Incompatibility
		switch {
		case errors.As(err, &guardFailure):
			f.policyCode = guardFailure.Code
			f.overall = true
			f.noRetry = true
			return fail(classPolicy, f)
		case errors.As(err, &violation):
			f.contractCode = "fidelity_protocol_violation"
			return fail(classProtocol, f)
		case errors.Is(err, errResponseTooLarge):
			return fail(classProtocol, f)
		case errors.Is(err, wire.ErrAggregateTooLarge):
			f.aggregateTooLarge = true
			return fail(classProtocol, f)
		case errors.As(err, &ue):
			f.upstream = x.redacted(ue)
			class := st.classify(err, committed)
			f.status = inBandStatus(class)
			return fail(class, f)
		}
		return fail(st.classify(err, committed), f)
	}
	fact.Class = classSuccess
	if fact.Interaction != nil {
		fact.Interaction.UpstreamState = usage.UpstreamTerminal
		if x.generation.parsed.Stream {
			fact.Interaction.ClientState = usage.ClientTerminal
		}
	}
	fact.Committed = committed
	fact.Duration = s.now().Sub(fact.StartedAt)
	// Moderation intentionally has no usage counters. Other successful work
	// without usage may have been billed with nothing this gateway can meter.
	fact.recordEvidence(x.operationName() != "moderation")
	finishTrace()
	return fact, completion, nil
}
