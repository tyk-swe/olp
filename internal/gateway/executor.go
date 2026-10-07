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
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/runtime"
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
	encoded              map[string]encodedRequest
	effectiveOutputs     map[string]effectiveOutput
	sourceSummary        *requestSummary
	request              request
	family               openai.Family
	ingress              string // the caller's wire surface, when it differs from family.Surface()
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

	// primary is the route the caller named. Its deadline and attempt budget
	// bound the request across every route it moves on to; route is the one
	// it is on. spent counts attempts across routes, budget is the ceiling
	// on spent for the current route, and allowance the primary budget.
	primary        *runtime.Route
	planner        planner
	authorize      func(*runtime.Route) *Error
	fixed          bool
	spent          int
	allowance      int
	seed           []byte
	leg            *usage.RouteLeg
	selector       string
	baseline       *usage.Baseline
	visited        []string
	delegators     []*runtime.Route
	frames         []fallbackFrame
	planConditions []string
	fallbacks      []runtime.FallbackStep
	// caps are the supply-side spend caps the request holds reservations
	// against, settled with its key reservation.
	caps []capHold
	// classified holds each classifier route's answer to this request.
	classified map[string]classification
	// mirrors is set where the request may be mirrored to the shadow targets
	// its plan samples, which are kept in shadows.
	mirrors bool
	shadows []runtime.Attempt
	// origin is whom the gateway makes the request for, empty for a caller
	// request; parent is the caller request a shadow request mirrors.
	origin string
	parent string
	// priority is the request's admission class.
	priority string

	policyDecisions []contentpolicy.Decision
	emit            openai.Emit
	estimate        int64
	// sizedInput is the input estimate of a request the gateway reads only the
	// size of: four bytes to a token over its body, as media, Bedrock invoke and
	// Gemini interaction requests are reserved. It is nil for every other
	// request, which is walked, and for lifecycle calls that carry no prompt.
	sizedInput *int64

	historicalSnapshot *runtime.Snapshot
	responseContract   *storedResponseContract
	continuation       *continuationExecution
	pin                *resources.Resource
	pinnedSlot         *runtime.Slot
	providerState      bool
	responseMap        map[string]string

	once       sync.Once
	facts      []AttemptFact
	failure    *Error         // terminal error decided before the attempt loop ran
	firstByte  *time.Duration // request start to the first payload byte the client received
	lease      *limits.Lease  // the API key reservation, settled once the request ends
	dispatched bool           // at least one attempt was handed to a provider
	// responseMetadata is the key's response_metadata policy: the response says
	// how the gateway served it.
	responseMetadata bool
	// attemptCount and attemptVendor describe the attempt now being made, and
	// so the one that serves the response if it succeeds: how many attempts the
	// request has made including this one, and the vendor of its provider. They
	// are recorded as the attempt opens because a stream commits its response
	// before the attempt's fact is appended. A job call, which has no stream,
	// records them with its fact, and a list of jobs once all its polls are in.
	attemptCount  int
	attemptVendor string
	// grantGeneration belongs to the token read for the current attempt.
	grantGeneration int64
	// sensitive holds every credential value applied to an upstream request
	// during this execution; provider-derived text passes through it.
	sensitive egress.Sensitive
}

// applyCredentials authenticates an upstream request and remembers every
// credential value it sent. It is the only way the gateway applies provider
// credentials, so x.redacted covers each value a provider could echo.
func (s *Server) applyCredentials(ctx context.Context, x *execution, req *http.Request, cfg connectors.Config, secret, body []byte) error {
	applied, err := s.auth.Apply(ctx, req, cfg, secret, body)
	x.sensitive.Include(applied)
	return err
}

// redacted removes this execution's applied credentials from every field of a
// provider error before it can reach a client, a log, or accounting.
func (x *execution) redacted(e *openai.UpstreamError) *openai.UpstreamError {
	return e.Redact(x.sensitive.Redact)
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
	n := s.walkDispatchable(x, nil)
	if x.mayLeaveRoute() {
		// Fallback and delegating routes may spend the rest of the budget.
		return max(n, x.allowance-x.spent)
	}
	return n
}

// walkDispatchable visits the attempts this request can hand to a provider in
// the order the attempt loop tries them, once for each usable credential slot of
// each, until the request's attempt budget is met. It returns how many it
// visited. Admission walks the same attempts the loop will, so what it reserves
// covers what can be dispatched and no more.
func (s *Server) walkDispatchable(x *execution, visit func(runtime.Attempt)) int {
	remaining := x.budget - x.spent
	available := 0
	for _, attempt := range x.attempts {
		provider, ok := x.snapshot().Providers[attempt.ProviderID]
		if !ok {
			continue
		}
		for i := range provider.Slots {
			if s.slotAvailable(x, attempt, &provider.Slots[i]) {
				available++
				if visit != nil {
					visit(attempt)
				}
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

// execute adapts canonical inference to shared attempt execution. The
// canonical transport retains its first-byte and streaming idle deadlines.
func (s *Server) execute(ctx context.Context, x *execution) *outcome {
	overall := time.Duration(x.named().OverallTimeout) * time.Millisecond
	ctx, cancel := context.WithTimeout(ctx, overall)
	defer cancel()
	out := runAttempts(ctx, s, x, attemptAdapter[*openai.Completion]{
		estimate: x.attemptReservation,
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
	if attempt.Slots != nil {
		ordered = runtime.ArrangeSlots(ordered, attempt.Slots)
	}
	out := make([]runtime.Slot, 0, len(ordered))
	for _, slot := range ordered {
		if !s.slotAvailable(x, attempt, &slot) || (!s.Admission.ready() && (s.health.coolingDown(provider.ID, slot.ID) || s.health.coolingDown(provider.ID, credentialHealthKey(&slot, s.slotGrantGeneration(&slot))))) {
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
	return slot.CredentialID != nil && s.Runtime.Eligibility(*slot.CredentialID) == runtime.Eligible
}

// attemptState tracks why an attempt context ended and what the attempt
// established about its upstream exchange.
type attemptState struct {
	parent            context.Context
	classifier        upstream.Classifier
	reason            atomic.Int32 // 1 first-byte deadline, 2 idle deadline, 3 stream cap
	dispatched        atomic.Bool  // request bytes may have reached the upstream
	status            int          // the unsuccessful status the upstream answered with
	accepted          bool         // the upstream answered with success
	settled           bool         // the upstream's complete result arrived
	credentialRefused bool         // classification before unresolved work becomes ambiguous
}

// trace supplies the dispatch evidence of a net/http request.
func (st *attemptState) trace() *httptrace.ClientTrace {
	return upstream.Trace(&st.dispatched)
}

// evidence is what the attempt has established about its upstream exchange.
func (st *attemptState) evidence() upstream.Evidence {
	return upstream.Evidence{Reached: st.dispatched.Load(), Status: st.status, Accepted: st.accepted, Settled: st.settled}
}

// rejected records the unsuccessful status the upstream answered with and
// classifies it together with the error its body stated.
func (st *attemptState) rejected(status int, stated *openai.UpstreamError) string {
	st.status = status
	e := st.evidence()
	e.Error = stated
	outcome := st.classifier.Classify(e)
	st.credentialRefused = outcome.CredentialRefused
	return string(outcome.Class)
}

// classify classifies an exchange that err ended: an in-band error the
// upstream stated, an interruption, or a broken or malformed transport.
func (st *attemptState) classify(err error, committed bool) string {
	e := st.evidence()
	e.Committed = committed
	if stated, ok := errors.AsType[*openai.UpstreamError](err); ok {
		e.Error = stated
	} else {
		e.Interrupted, e.Err = st.interrupted(err), err
	}
	outcome := st.classifier.Classify(e)
	st.credentialRefused = outcome.CredentialRefused
	return string(outcome.Class)
}

// interrupted reports why the gateway ended the exchange: the caller's
// cancellation or deadline, one of the attempt's own timers, or a failed
// write to the client.
func (st *attemptState) interrupted(err error) error {
	switch {
	case st.parent.Err() != nil:
		return st.parent.Err()
	case st.reason.Load() != 0:
		return context.DeadlineExceeded
	case errors.Is(err, errClientWrite):
		return context.Canceled
	}
	return nil
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
		Leg:                x.leg,
		Selector:           x.selector,
		Baseline:           x.baseline,
	}
	x.recordEstimate(&fact, a)
	x.attemptCount, x.attemptVendor = ordinal, a.VendorID
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
		if x.family == openai.FamilyRealtime {
			if _, ok := x.snapshot().RealtimeTemplate(x.route.Slug, a.TargetID); ok {
				fact.Interaction = &usage.InteractionEvidence{Fidelity: runtime.FidelityStrict, PlanClass: "native_identity", UpstreamState: usage.UpstreamNotSent, ClientState: usage.ClientUnobserved}
			}
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
	cfg := provider.Connector()
	kept := x.takeEncoded()
	fact.Carried = cfg.CarriedByPlugin()
	// A plugin that carries the request reports only whether it was sent, so
	// the attempt must not risk repeating work it may have done.
	st := &attemptState{parent: ctx, classifier: upstream.Classifier{ContextWindow: true, AtMostOnce: fact.Interaction != nil || fact.Carried, Declared: cfg.Classification()}}
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
		f.credentialRefused = st.credentialRefused
		f.acceptance = st.evidence().Acceptance()
		f.accepted = st.accepted
		if x.continuation != nil && x.continuation.resource != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			_ = s.Resources.MarkUnknown(cleanup, x.continuation.resource)
			cancel()
		}

		if fact.Interaction != nil || fact.Carried {
			f.noRetry = f.noRetry || f.dispatched
		}
		if fact.Interaction != nil {
			fact.Interaction.UpstreamState = string(f.acceptance)
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
		body, wire, err = x.encoding(kept, provider, cfg, a.UpstreamModel)
	}
	if err != nil {
		return fail(classProtocol, nil)
	}
	body = cfg.WrapRequest(body, a.UpstreamModel)
	// A profile that forces streaming streams every request, and the gateway
	// aggregates the stream for a caller that did not ask for one.
	aggregated := !x.parsed.Stream && cfg.ForcesStreaming()
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

	endpoint, err := cfg.URL(wire, a.UpstreamModel, x.parsed.Stream || aggregated)
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
	if x.parsed.Stream || aggregated {
		req.Header.Set("Accept", "text/event-stream")
		if wire == "bedrock" || cfg.EventStream() {
			req.Header.Set("Accept", "application/vnd.amazon.eventstream")
		}
	}
	if contract == nil {
		cfg = forwardAnthropicBeta(req.Header, x, wire, cfg)
	}
	if err := s.applySlotCredential(actx, x, req, cfg, slot, body); err != nil {
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
	resp, err := client.Do(req)
	if cfg.CarriedByPlugin() {
		// The plugin reports whether it sent a request it failed; any other
		// failure may have reached the upstream.
		st.dispatched.Store(!errors.Is(err, connectors.ErrNotSent))
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
			if wire == openai.FamilyResponses && (bytes.Contains(frame, []byte("event: response.failed\n")) || bytes.Contains(frame, []byte("event: error\n"))) {
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
		strictResponseIncomplete := false
		var observe func(oif.Event) error
		if contract != nil {
			observe = func(event oif.Event) error {
				if err := contract.ValidateEvent(event); err != nil {
					return err
				}
				if x.strict() && x.family == openai.FamilyResponses {
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
		if contract != nil && contract.ToolContinuation() {
			var projection *interaction.ToolProjection
			projection, err = contract.NewToolProjection(min(resources.MaxContinuationBytes, int(s.cfg.MaxResponseBytes)))
			if err == nil {
				completion, err = protocols.StreamWithEvents(wire, wire, cfg.StreamPayload(resp.Body, int(s.cfg.MaxEventBytes)), int(s.cfg.MaxEventBytes), x.named().Slug, true, func([]byte) error { return nil }, func(event oif.Event) error {
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
				st.settled = true
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
			completion, err = protocols.StreamWithEvents(wire, x.family, cfg.StreamPayload(resp.Body, int(s.cfg.MaxEventBytes)), int(s.cfg.MaxEventBytes), x.named().Slug, x.parsed.IncludeUsage, emit, observe)
		}
		if err == nil && strictResponseIncomplete {
			st.settled = true
			err = &openai.ProtocolError{Detail: "strict Responses stream ended incomplete"}
		}
	} else {
		var raw []byte
		if aggregated {
			raw, err = s.aggregate(&fact, resp, cfg, wire)
		} else if raw, err = io.ReadAll(&countingReader{r: resp.Body, limit: s.cfg.MaxResponseBytes}); err == nil {
			raw = cfg.UnwrapResponse(raw)
		}
		if err == nil {
			if contract != nil {
				var native *openai.Completion
				native, err = protocols.DecodeRequest(wire, wire, raw, x.named().Slug, "", contract.EffectiveRequest())
				if native != nil {
					fact.Usage = native.Usage
				}
				if err == nil {
					st.settled = true
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
					if x.strict() && x.family == openai.FamilyResponses {
						source := native.Native.Source()
						if _, present := source.Lookup("/model"); present {
							model, _ := json.Marshal(x.named().Slug)
							source, err = oif.Apply(source, []oif.Change{{Pointer: "/model", Value: string(model), Origin: oif.IdentityBinding, Reason: "published response model"}})
						}
						if err == nil {
							completion.Body = source.Bytes()
						}
					}
				}
			}
			if err == nil && completion == nil {
				completion, err = protocols.DecodeRequest(wire, x.family, raw, x.named().Slug, protocols.EmbeddingEncoding(x.parsed, provider.ParameterDefaults), x.parsed)
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
		case errors.Is(err, openai.ErrAggregateTooLarge):
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

// aggregate reads the stream that an upstream serving only streams answers a
// non-streaming request with, and returns the dialect's non-streaming result.
// The attempt records the usage the stream stated, also when it fails, as it
// would for a streaming caller.
func (s *Server) aggregate(fact *AttemptFact, resp *http.Response, cfg connectors.Config, wire openai.Family) ([]byte, error) {
	if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType != "text/event-stream" {
		return nil, &openai.ProtocolError{Detail: "the forced stream is not an event stream"}
	}
	limit := int(s.cfg.MaxEventBytes)
	streamed, err := openai.Aggregate(wire, cfg.StreamPayload(resp.Body, limit), limit, int(s.cfg.MaxResponseBytes))
	if streamed == nil {
		return nil, err
	}
	fact.Usage = streamed.Usage
	return streamed.Body, err
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
			env.Route = x.named().Slug
			env.RouteRevisionID = x.named().RevisionID
		}
		if x.origin != "" {
			env.Origin, env.ParentRequestID = x.origin, x.parent
			if usage.Keyless(x.origin) {
				env.KeyID = ""
			}
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

func credentialHealthKey(slot *runtime.Slot, generation int64) string {
	if slot.CredentialID != nil {
		key := "credential:" + *slot.CredentialID
		if generation != 0 {
			key += ":" + strconv.FormatInt(generation, 10)
		}
		return key
	}
	return "credential:ambient"
}
