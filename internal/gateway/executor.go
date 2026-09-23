package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/providerinvoke"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

// Failure classes shared with the routing retry taxonomy fixture.
const (
	classSuccess        = "success"
	classConnect        = "connect"
	classTimeout        = "timeout"
	classRateLimit      = "rate_limit"
	classUpstreamServer = "upstream_server"
	classUpstreamClient = "upstream_client"
	classCredential     = "credential"
	classProtocol       = "protocol"
	classPolicy         = "policy"
	classCancelled      = "cancelled"
	// classAmbiguous marks a side-effecting attempt whose upstream outcome the
	// gateway cannot prove; it never fails over.
	classAmbiguous         = "ambiguous"
	classLimitsUnavailable = "limits_unavailable"
	classContextWindow     = "context_window"
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

// execution is one inference request flowing through the attempt loop.
type execution struct {
	unary                *unaryExecution
	semanticHeaders      http.Header
	semanticQuery        url.Values
	semanticQueryInvalid bool
	serving              *interaction.ServingIdentity
	servingSlot          string
	servingBinding       string
	preparedProviders    map[string]preparedProvider
	request              request
	family               openai.Family
	parsed               *openai.Request
	media                *media.Request
	actor                string
	keyID                string
	budgetGroupID        *string
	attribution          map[string]string
	userID               string
	affinity             []byte
	authority            access.Authority
	route                *runtime.Route
	mode                 string
	attempts             []runtime.Attempt
	budget               int
	preferences          *runtime.Preferences
	decisions            []runtime.Decision
	policy               runtime.EffectivePolicy

	policyDecisions []contentpolicy.Decision
	emit            openai.Emit
	estimate        int64

	historicalSnapshot *runtime.Snapshot
	responseContract   *storedResponseContract
	continuation       *continuationExecution
	pin                *resources.Resource
	pinnedSlot         *runtime.Slot
	pinnedSecret       []byte
	providerState      bool
	responseMap        map[string]string

	once       sync.Once
	facts      []AttemptFact
	failure    *Error         // terminal error decided before the attempt loop ran
	firstByte  *time.Duration // request start to the first payload byte the client received
	lease      *limits.Lease  // the API key reservation, settled once the request ends
	dispatched bool           // at least one attempt was handed to a provider
}

// usage is the metering evidence the request ends with: the usage of the
// attempt that served it, which is the last one recorded.
func (x *execution) usage() *openai.Usage {
	if len(x.facts) == 0 {
		return nil
	}
	return x.facts[len(x.facts)-1].Usage
}

// delivered records when the first byte of the response payload reached the
// client. Later deliveries keep the first one.
func (x *execution) delivered(at time.Time) {
	if x.firstByte == nil {
		elapsed := at.Sub(x.request.startedAt)
		x.firstByte = &elapsed
	}
}

// outcome is the terminal result of the attempt loop.
type outcome struct {
	completion *openai.Completion
	err        *Error
	committed  bool
	cancelled  bool
}

// dispatchableAttempts returns the most attempts this request can hand to a
// provider, capped by its requested budget. Each target may be tried through
// each of its usable credential slots.
func (s *Server) dispatchableAttempts(x *execution) int {
	remaining := x.budget
	available := 0
	for _, attempt := range x.attempts {
		provider, ok := x.snapshot().Providers[attempt.ProviderID]
		if !ok {
			continue
		}
		for i := range provider.Slots {
			if s.slotAvailable(x, attempt, &provider.Slots[i]) {
				available++
			}
			if available == remaining {
				return available
			}
		}
	}
	return available
}

type attemptFailure struct {
	class        string
	status       int
	committed    bool
	retryAfter   time.Duration
	upstream     *openai.UpstreamError
	overall      bool   // the route deadline, not the attempt deadline, expired
	dispatched   bool   // the request reached the upstream before the failure
	quota        string // a quota this gateway enforces rejected the attempt
	contractCode string // safe runtime interaction guard violation
	policyCode   string // local output policy refusal after upstream completion
	noRetry      bool   // strict outcome uncertainty must not suggest client retries
}

// The quotas that can reject an attempt before it is dispatched.
const (
	quotaConnection = "connection"
	quotaSlot       = "slot"
)

// billingUncertain reports whether the upstream may have served and billed
// work this attempt cannot account for. Anything already delivered to the
// client was served, and a request that reached the upstream may have been
// processed in full even though its result never came back. A rejection the
// upstream stated — a bad request, an exhausted quota, a refused credential —
// costs nothing, and neither does a failure that never left this gateway.
// The phase, not the class, decides: classConnect covers every transport
// failure here, including a connection lost long after the request was sent.
func (f *attemptFailure) billingUncertain() bool {
	if f.committed {
		return true
	}
	switch f.class {
	case classRateLimit, classUpstreamClient, classCredential, classContextWindow:
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
	if f.policyCode != "" {
		message := "The provider result was blocked by the route's content policy."
		if f.policyCode == "policy_conflict" {
			message = "The route's content policy cannot inspect this provider result."
		}
		return invalidRequest(f.policyCode, message, nil)
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

// execute adapts canonical inference to shared attempt execution. The
// canonical transport retains its first-byte and streaming idle deadlines.
func (s *Server) execute(ctx context.Context, x *execution) *outcome {
	overall := time.Duration(x.route.OverallTimeout) * time.Millisecond
	ctx, cancel := context.WithTimeout(ctx, overall)
	defer cancel()
	out := runAttempts(ctx, s, x, attemptAdapter[*openai.Completion]{
		estimate: func(provider *runtime.Provider) int64 {
			return x.providerEstimate(provider)
		},
		dispatch: func(ctx context.Context, attempt runtime.Attempt, provider *runtime.Provider, slot runtime.Slot, ordinal int) (AttemptFact, *openai.Completion, *attemptFailure) {
			return s.attempt(ctx, x, attempt, provider, slot, ordinal)
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
func (s *Server) slots(x *execution, attempt runtime.Attempt, provider *runtime.Provider) []runtime.Slot {
	if x.pinnedSlot != nil {

		if !s.slotAvailable(x, attempt, x.pinnedSlot) {
			return nil
		}
		return []runtime.Slot{*x.pinnedSlot}
	}
	ordered := runtime.SelectSlots(*provider, attempt.UpstreamModel, *x.route, x.keyID, x.operationName(), x.surfaceName(), x.mode, x.affinity)
	out := make([]runtime.Slot, 0, len(ordered))
	for _, slot := range ordered {
		if !s.slotAvailable(x, attempt, &slot) || (!s.Admission.ready() && (s.health.coolingDown(provider.ID, slot.ID) || s.health.coolingDown(provider.ID, credentialHealthKey(&slot)))) {
			continue
		}
		out = append(out, slot)
	}

	return out
}

func (s *Server) slotAvailable(x *execution, attempt runtime.Attempt, slot *runtime.Slot) bool {
	if !slot.Allows(attempt.UpstreamModel, x.route.Slug, x.keyID) {
		return false
	}
	provider := x.snapshot().Providers[attempt.ProviderID]
	if !connectors.SecretRequired(provider.AuthMode) {
		return true
	}
	if slot.CredentialID == nil || s.Runtime.Revoked(*slot.CredentialID) {
		return false
	}
	if _, ok := x.request.release.Credential(*slot.CredentialID); ok {
		return true
	}
	return x.pinnedSlot != nil && slot.ID == x.pinnedSlot.ID && x.pinnedSecret != nil
}

// attemptState tracks why an attempt context ended.
type attemptState struct {
	parent     context.Context
	reason     atomic.Int32 // 1 first-byte deadline, 2 idle deadline, 3 stream cap
	dispatched atomic.Bool
	upstream   atomic.Int32 // 0 not sent, 1 outcome unknown, 2 accepted, 3 terminal
}

// trace conservatively marks dispatch once writing has begun or a response
// arrives. A body write failure does not prove that the upstream saw nothing;
// only a failure before writing is safely refundable.
func (st *attemptState) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		// A failed body write can still leave work at the upstream.
		WroteHeaders: func() { st.dispatched.Store(true); st.upstream.CompareAndSwap(0, 1) },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				st.dispatched.Store(true)
				st.upstream.CompareAndSwap(0, 1)
			}
		},
		GotFirstResponseByte: func() { st.dispatched.Store(true); st.upstream.CompareAndSwap(0, 1) },
	}
}

func (st *attemptState) classify(err error, committed bool) string {
	switch {
	case st.parent.Err() != nil:
		if errors.Is(st.parent.Err(), context.DeadlineExceeded) {
			return classTimeout
		}
		return classCancelled
	case st.reason.Load() != 0:
		return classTimeout
	case errors.Is(err, errClientWrite):
		return classCancelled
	case committed:
		return classProtocol
	}
	var pe *openai.ProtocolError
	if errors.As(err, &pe) || errors.Is(err, openai.ErrEventTooLarge) {
		return classProtocol
	}
	return classConnect
}

func endpointPath(family openai.Family) string {
	if family == openai.FamilyResponses {
		return "/responses"
	}
	return "/chat/completions"
}

// newFact opens the record of one attempt against one credential slot.
func (s *Server) newFact(x *execution, a runtime.Attempt, slot runtime.Slot, ordinal int) AttemptFact {
	fact := AttemptFact{
		Strategy: a.Strategy, PolicyDigest: a.PolicyDigest, VendorID: a.VendorID, Price: a.Price,
		Ordinal:            ordinal,
		TargetID:           a.TargetID,
		ProviderID:         a.ProviderID,
		ProviderRevisionID: a.ProviderRevisionID,
		UpstreamModel:      a.UpstreamModel,
		SlotID:             slot.ID,
		Mode:               x.mode,
		StartedAt:          s.now(),
	}
	if slot.CredentialID != nil {
		fact.CredentialID = *slot.CredentialID
	}
	if slot.CredentialVersion != nil {
		fact.CredentialVersion = *slot.CredentialVersion
	}
	if x.strict() {
		provider := x.snapshot().Providers[a.ProviderID]
		if x.family == openai.FamilyGeminiInteractions || x.family == openai.FamilyGeminiLive {
			fact.Interaction = &usage.InteractionEvidence{Fidelity: runtime.FidelityStrict, PlanClass: "native_identity", UpstreamState: usage.UpstreamNotSent, ClientState: usage.ClientUnobserved}
			return fact
		}
		if x.family == openai.FamilyBatch || x.family == openai.FamilyFile {
			if _, ok := x.snapshot().DurableTemplate(x.route.Slug, a.TargetID); ok {
				fact.Interaction = &usage.InteractionEvidence{Fidelity: runtime.FidelityStrict, PlanClass: "native_identity", UpstreamState: usage.UpstreamNotSent, ClientState: usage.ClientUnobserved}
			}
			return fact
		}
		if x.media != nil {
			if _, ok := x.snapshot().MediaTemplate(x.route.Slug, a.TargetID, x.media.Op); ok {
				fact.Interaction = &usage.InteractionEvidence{Fidelity: runtime.FidelityStrict, PlanClass: "native_identity", UpstreamState: usage.UpstreamNotSent, ClientState: usage.ClientUnobserved}
			}
		} else if x.unary != nil {
			if plan, err := x.unaryPlan(&provider, a.UpstreamModel); err == nil {
				fact.Interaction = &usage.InteractionEvidence{Fidelity: runtime.FidelityStrict, PlanClass: plan.Receipt().Class, UpstreamState: usage.UpstreamNotSent, ClientState: usage.ClientUnobserved}
			}
		} else if prepared, err := x.preparedProvider(&provider, a.UpstreamModel); err == nil && prepared.plan != nil {
			fact.Interaction = &usage.InteractionEvidence{Fidelity: runtime.FidelityStrict, PlanClass: prepared.plan.Receipt().Class, UpstreamState: usage.UpstreamNotSent, ClientState: usage.ClientUnobserved}
		}
	}
	return fact
}

// rejectedFact records an attempt a quota refused. Nothing was sent, so the
// attempt carries no status and cannot have been billed by anyone.
func (s *Server) rejectedFact(x *execution, a runtime.Attempt, slot runtime.Slot, ordinal int, rejection *attemptFailure) AttemptFact {
	fact := s.newFact(x, a, slot, ordinal)
	fact.Class = rejection.class
	fact.Duration = s.now().Sub(fact.StartedAt)
	if rejection.retryAfter > 0 {
		retry := rejection.retryAfter
		fact.RetryAfter = &retry
	}
	fact.recordEvidence(false)
	return fact
}

// attempt performs one upstream call with one credential.
func (s *Server) attempt(ctx context.Context, x *execution, a runtime.Attempt, provider *runtime.Provider, slot runtime.Slot, ordinal int) (AttemptFact, *openai.Completion, *attemptFailure) {
	fact := s.newFact(x, a, slot, ordinal)
	st := &attemptState{parent: ctx}
	attemptCtx, atr := x.request.trace.Attempt(ctx, provider.Kind, a.ProviderRevisionID, a.UpstreamModel)
	finishTrace := func() {
		if atr == nil {
			return
		}
		if u := fact.Usage; u != nil {
			atr.RecordUsage(&u.InputTokens, &u.OutputTokens, u.CachedInputTokens, u.MediaUnits)
		}
		atr.Finish(fact.Class, fact.Status)
	}
	fail := func(class string, f *attemptFailure) (AttemptFact, *openai.Completion, *attemptFailure) {
		if f == nil {
			f = &attemptFailure{}
		}
		f.dispatched = st.dispatched.Load()
		if x.continuation != nil && x.continuation.resource != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			_ = s.Resources.MarkUnknown(cleanup, x.continuation.resource)
			cancel()
		}

		if fact.Interaction != nil {
			f.noRetry = f.dispatched
			fact.Interaction.UpstreamState = st.upstreamState()
			if f.dispatched && st.upstream.Load() != 3 && (class == classConnect || class == classTimeout || class == classUpstreamServer) {
				class = classAmbiguous
				f.noRetry = true
			}
		}
		f.class = class
		fact.Class = class
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
	cfg := provider.Connector()
	var body []byte
	var wire openai.Family
	var contract *interaction.Plan
	if provider.ProfileID != "" || x.strict() || x.route.ContentPolicy != nil {
		prepared, prepareErr := x.preparedProvider(provider, a.UpstreamModel)
		err = prepareErr
		if err == nil {
			for _, decision := range prepared.policyDecisions {
				recordDecision(x, decision)
			}
			body, wire = prepared.invocation.Prepared.Document().Bytes(), prepared.invocation.Wire
			contract = prepared.plan
			if contract != nil {
				cfg = contract.Config()
			}
		}
	} else {
		body, wire, err = providerinvoke.Encode(x.parsed, cfg, a.UpstreamModel, provider.ParameterDefaults)
	}
	if err != nil {
		return fail(classProtocol, nil)
	}
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

	endpoint, err := cfg.URL(wire, a.UpstreamModel, x.parsed.Stream)
	if err != nil {
		return fail(classProtocol, nil)
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(actx, st.trace()), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fail(classConnect, nil)
	}
	atr.InjectUpstream(req.Header, x.request.trace.PropagateUpstream())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "olp-go/gateway")
	req.Header.Set("Accept", "application/json")
	if x.parsed.Stream {
		req.Header.Set("Accept", "text/event-stream")
		if wire == "bedrock" || cfg.EventStream() {
			req.Header.Set("Accept", "application/vnd.amazon.eventstream")
		}
	}
	var secret []byte
	if slot.CredentialID != nil {
		secret, _ = x.request.release.Credential(*slot.CredentialID)
		if secret == nil && x.pinnedSlot != nil && slot.ID == x.pinnedSlot.ID {
			secret = x.pinnedSecret
		}
	}
	credentialValues, err := s.auth.Apply(actx, req, cfg, secret, body)
	if err != nil {
		if actx.Err() != nil {
			return fail(st.classify(err, false), nil)
		}
		return fail(classCredential, nil)
	}

	client, err := s.providerClient(actx, x.request.release, provider, slot)
	if err != nil {
		return fail(classCredential, nil)
	}
	if contract != nil && contract.ToolContinuation() {
		if err := s.claimToolWork(actx, x, contract, a, slot); err != nil {
			return fail(classProtocol, &attemptFailure{contractCode: "continuation_unavailable", noRetry: true})
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fail(st.classify(err, false), nil)
	}
	defer resp.Body.Close()
	// The response status is the first thing the upstream sends back.
	received := s.now().Sub(fact.StartedAt)
	fact.FirstByte = &received
	fact.Status = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode < 500 {
			st.upstream.Store(3)
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		f := &attemptFailure{status: resp.StatusCode, upstream: openai.ParseErrorBody(raw)}
		if f.upstream != nil {
			f.upstream.Message = redactCredentials(f.upstream.Message, credentialValues)
		}
		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return fail(classCredential, f)
		case resp.StatusCode == http.StatusTooManyRequests:
			f.retryAfter = retryAfter(resp.Header.Get("Retry-After"), s.now())
			return fail(classRateLimit, f)
		case resp.StatusCode >= 500:
			return fail(classUpstreamServer, f)
		case contextWindowError(f.upstream):
			return fail(classContextWindow, f)
		}
		return fail(classUpstreamClient, f)
	}
	st.upstream.Store(2)

	var completion *openai.Completion
	committed := false
	actionable := false
	if x.parsed.Stream {
		mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		if mediaType != "text/event-stream" && !((wire == "bedrock" || cfg.EventStream()) && mediaType == "application/vnd.amazon.eventstream") {
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
			if x.providerState {
				mapped, mapErr := s.mapStreamResponseFrame(ctx, x, &fact, frame)
				if mapErr != nil {
					return mapErr
				}
				frame = mapped
			}
			if !committed {
				committed = true
				firstByte.Stop()
				watchdog = time.AfterFunc(a.Timeout, func() { st.reason.CompareAndSwap(0, 2); cancel() })
			} else {
				watchdog.Reset(a.Timeout)
			}
			if len(frame) > int(s.cfg.MaxEventBytes) {
				return openai.ErrEventTooLarge
			}
			err := x.emit(frame)
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
		var observe func(oif.Event) error
		if contract != nil {
			observe = func(event oif.Event) error {
				if err := contract.ValidateEvent(event); err != nil {
					return err
				}
				actionable = actionable || eventActionable(event)
				return nil
			}
		}
		if contract != nil && contract.ToolContinuation() {
			var projection *interaction.ToolProjection
			projection, err = contract.NewToolProjection(min(resources.MaxContinuationBytes, int(s.cfg.MaxResponseBytes)))
			if err == nil {
				completion, err = protocols.StreamWithEvents(wire, wire, cfg.StreamPayload(resp.Body, int(s.cfg.MaxEventBytes)), int(s.cfg.MaxEventBytes), x.route.Slug, true, func([]byte) error { return nil }, func(event oif.Event) error {
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
						x.continuation.emitted++
					}
					return nil
				})
			}
			if err == nil {
				st.upstream.Store(3)
				var state *interaction.Continuation
				var delivery interaction.Delivery
				state, delivery, err = projection.Complete(completion, x.continuation.resource.ID)
				if err == nil {
					err = s.validateToolDelivery(delivery)
				}
				if err == nil {
					err = s.commitToolDelivery(ctx, x, state, delivery)
				}
				if err == nil {
					for _, frame := range delivery.Frames[x.continuation.emitted:] {
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
			completion, err = protocols.StreamWithEvents(wire, x.family, cfg.StreamPayload(resp.Body, int(s.cfg.MaxEventBytes)), int(s.cfg.MaxEventBytes), x.route.Slug, x.parsed.IncludeUsage, emit, observe)
		}
	} else {
		limited := &countingReader{r: resp.Body, limit: s.cfg.MaxResponseBytes}
		raw, readErr := io.ReadAll(limited)
		if readErr != nil {
			err = readErr
		} else {
			if contract != nil {
				var native *openai.Completion
				native, err = protocols.DecodeRequest(wire, wire, raw, x.route.Slug, "", contract.EffectiveRequest())
				if native != nil {
					fact.Usage = native.Usage
				}
				if err == nil {
					st.upstream.Store(3)
					err = contract.ValidateResult(native.Native)
				}
				if err == nil && contract.ToolContinuation() {
					var state *interaction.Continuation
					var delivery interaction.Delivery
					state, delivery, err = contract.ProjectUnary(native, x.continuation.resource.ID, min(resources.MaxContinuationBytes, int(s.cfg.MaxResponseBytes)))
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
				if err == nil && wire == x.family {
					completion = native
				}
			}
			if err == nil && completion == nil {
				completion, err = protocols.DecodeRequest(wire, x.family, raw, x.route.Slug, protocols.EmbeddingEncoding(x.parsed, provider.ParameterDefaults), x.parsed)
			}
		}
	}
	if completion != nil {
		fact.Usage = completion.Usage
		if !x.parsed.Stream && (wire != x.family || x.family == openai.FamilyEmbeddings || x.family == openai.FamilyRerank) && int64(len(completion.Body)) > s.cfg.MaxResponseBytes {
			err = errResponseTooLarge
		}
	}
	if err != nil {
		f := &attemptFailure{status: resp.StatusCode, committed: committed}
		var ue *openai.UpstreamError
		var violation *interaction.Error
		switch {
		case errors.As(err, &violation):
			f.contractCode = "fidelity_protocol_violation"
			return fail(classProtocol, f)
		case errors.Is(err, errResponseTooLarge):
			return fail(classProtocol, f)
		case errors.As(err, &ue):
			f.upstream = ue
			ue.Message = redactCredentials(ue.Message, credentialValues)
			class, status := inBandFailure(ue, committed)
			f.status = status
			return fail(class, f)
		}
		return fail(st.classify(err, committed), f)
	}
	fact.Class = classSuccess
	st.upstream.Store(3)
	if fact.Interaction != nil {
		fact.Interaction.UpstreamState = usage.UpstreamTerminal
		if x.parsed.Stream {
			fact.Interaction.ClientState = usage.ClientTerminal
		}
	}
	fact.Committed = committed
	fact.Duration = s.now().Sub(fact.StartedAt)
	// A success carrying no usage was still served and billed upstream, with
	// nothing this gateway can meter.
	fact.recordEvidence(true)
	finishTrace()
	return fact, completion, nil
}

// countingReader enforces the buffered unary response byte limit.
type countingReader struct {
	r        io.Reader
	limit    int64
	read     int64
	exceeded bool
}

var errResponseTooLarge = errors.New("upstream response exceeds byte limit")

func (c *countingReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if c.exceeded {
		return 0, errResponseTooLarge
	}
	if c.read == c.limit {
		// Reaching the limit is valid if the upstream ends here. Read at most
		// one additional byte to distinguish EOF from an oversized response.
		var extra [1]byte
		n, err := c.r.Read(extra[:])
		if n > 0 {
			c.exceeded = true
			return 0, errResponseTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > c.limit-c.read {
		p = p[:c.limit-c.read]
	}
	n, err := c.r.Read(p)
	c.read += int64(n)
	return n, err
}

// retryAfter parses a Retry-After header as seconds or an HTTP date.
func retryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		for _, c := range value {
			if c < '0' || c > '9' {
				return 0
			}
		}
		const maxSeconds = uint64((1<<63 - 1) / time.Second)
		return time.Duration(min(seconds, maxSeconds)) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(at.Sub(now), 0)
	}
	return 0
}

// finish emits the terminal envelope exactly once.
func (s *Server) finish(x *execution, out *outcome, status int) {
	x.once.Do(func() {
		completedAt := s.now()
		env := Envelope{
			RequestID:       x.request.id,
			AccountingID:    x.request.accountingID(),
			ClientIP:        x.request.clientIP,
			Actor:           x.actor,
			KeyID:           x.keyID,
			BudgetGroupID:   x.budgetGroupID,
			Attribution:     x.attribution,
			PolicyDecisions: x.policyDecisions,
			UserID:          x.userID,
			Family:          string(x.family),
			Mode:            x.mode,
			Operation:       x.operationName(),
			Surface:         x.surfaceName(),
			Outcome:         "failure",
			Status:          status,
			StartedAt:       x.request.startedAt,
			CompletedAt:     completedAt,
			Duration:        completedAt.Sub(x.request.startedAt),
			FirstByte:       x.firstByte,
			Attempts:        x.facts,
		}
		if x.request.release != nil {
			env.ReleaseSequence = x.request.release.Sequence
			if x.request.release.Snapshot != nil {
				env.RuntimeGenerationID = x.request.release.Snapshot.Generation.ID
			}
		}
		if x.route != nil {
			env.Route = x.route.Slug
			env.RouteRevisionID = x.route.RevisionID
		}
		env.Usage = x.usage()
		switch {
		case out != nil && out.err != nil:
			env.ErrorClass = out.err.Code
		case out == nil && x.failure != nil:
			env.ErrorClass = x.failure.Code
		}
		if out != nil {
			env.Committed = out.committed
			switch {
			case out.cancelled:
				env.Outcome = "cancelled"
				env.Status = 0
			case out.err == nil:
				env.Outcome = "success"
			}
		}
		if x.request.trace != nil {
			var firstByte, total time.Duration
			if x.firstByte != nil {
				firstByte = *x.firstByte
			}
			total = env.Duration
			x.request.trace.RecordInferenceContext(env.Surface, env.Operation, env.Route, env.KeyID, env.RuntimeGenerationID)
			x.request.trace.RecordTerminal(env.Status, env.ErrorClass, len(x.facts), firstByte, total)
		}
		s.Sink.Terminal(env)
	})
}

func credentialHealthKey(slot *runtime.Slot) string {
	if slot.CredentialID != nil {
		return "credential:" + *slot.CredentialID
	}
	return "credential:ambient"
}
