package kernel

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/upstream"
)

// Failure classes shared with the routing retry taxonomy fixture. Upstream
// exchanges are classified by internal/upstream.
const (
	ClassSuccess        = events.ClassSuccess
	ClassConnect        = string(upstream.Connect)
	ClassTimeout        = string(upstream.Timeout)
	classRateLimit      = string(upstream.RateLimit)
	ClassUpstreamServer = string(upstream.ServerError)
	classUpstreamClient = string(upstream.ClientError)
	ClassUpstreamClient = classUpstreamClient
	ClassCredential     = string(upstream.Credential)
	ClassProtocol       = string(upstream.Protocol)
	classPolicy         = "policy"
	ClassCancelled      = string(upstream.Cancelled)
	// ClassAmbiguous marks a side-effecting attempt whose upstream outcome the
	// gateway cannot prove; it never fails over.
	ClassAmbiguous         = string(upstream.Ambiguous)
	classLimitsUnavailable = "limits_unavailable"
	ClassLimitsUnavailable = classLimitsUnavailable
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
	MaxStreamDuration = maxStreamDuration
	ErrorBodyLimit    = 64 * 1024
)

// redacted removes this execution's applied credentials from every field of a
// provider error before it can reach a client, a log, or accounting.
func (x *Call) redacted(e *wire.UpstreamError) *wire.UpstreamError {
	return e.Redact(x.sensitive.Redact)
}

// usage is the metering evidence the request ends with: the usage of the
// attempt that served it, which is the last one recorded.
func (x *Call) usage() *wire.Usage {
	if len(x.facts) == 0 {
		return nil
	}
	return x.facts[len(x.facts)-1].Usage
}

// delivered records when the first byte of the response payload reached the
// client. Later deliveries keep the first one.
func (x *Call) Delivered(at time.Time) {
	if x.firstByte == nil {
		elapsed := at.Sub(x.request.startedAt)
		x.firstByte = &elapsed
	}
}

// outcome is the terminal result of the attempt loop.
type Outcome struct {
	Completion *wire.Completion
	approval   *workflowPause
	Err        *fault.Error
	Committed  bool
	Cancelled  bool
}

// dispatchableAttempts returns the most attempts this request can hand to a
// provider, capped by its requested budget. Each target may be tried through
// each of its usable credential slots, including bounded same-slot retries.
func (s *Server) dispatchableAttempts(x *Call) int {
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
func (s *Server) walkDispatchable(x *Call, attempts []routing.Attempt, visit func(routing.Attempt)) int {
	remaining := x.budget - x.spent
	if remaining <= 0 {
		return 0
	}
	repeats := retryDispatches(x.Route)
	available := 0
	for _, attempt := range attempts {
		provider, ok := x.Snapshot().FindProvider(attempt.ProviderID)
		if !ok {
			continue
		}
		for i := range provider.Slots {
			if s.slotAvailable(x, attempt, &provider.Slots[i]) {
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

type AttemptFailure struct {
	class        string
	Status       int
	committed    bool
	retryAfter   time.Duration
	upstream     *wire.UpstreamError
	Acceptance   upstream.Acceptance // what the exchange established about the upstream's work
	accepted     bool                // a successful response began, even if its result could not be metered
	overall      bool                // the route deadline, not the attempt deadline, expired
	dispatched   bool                // the request reached the upstream before the failure
	quota        string              // a quota this gateway enforces rejected the attempt
	dimension    limits.Dimension    // the exhausted dimension of that quota, when it named one
	contractCode string              // safe runtime interaction guard violation
	policyCode   string              // local output guardrail refusal after upstream completion
	noRetry      bool                // outcome uncertainty must not suggest client retries
	// credentialRefused survives ambiguity so a grant can refresh without
	// allowing this attempt to fail over.
	credentialRefused bool
	// aggregateTooLarge reports a forced stream whose non-streaming result
	// exceeded the response size limit.
	aggregateTooLarge bool
}

type AttemptFailureSpec struct {
	Class      string
	Status     int
	Committed  bool
	RetryAfter time.Duration
	Upstream   *wire.UpstreamError
	Acceptance upstream.Acceptance
	Accepted   bool
	Overall    bool
	Dispatched bool
}

func NewAttemptFailure(spec AttemptFailureSpec) *AttemptFailure {
	return &AttemptFailure{
		class:      spec.Class,
		Status:     spec.Status,
		committed:  spec.Committed,
		retryAfter: spec.RetryAfter,
		upstream:   spec.Upstream,
		Acceptance: spec.Acceptance,
		accepted:   spec.Accepted,
		overall:    spec.Overall,
		dispatched: spec.Dispatched,
	}
}

func (f *AttemptFailure) Class() string          { return f.class }
func (f *AttemptFailure) Committed() bool        { return f.committed }
func (f *AttemptFailure) Dispatched() bool       { return f.dispatched }
func (f *AttemptFailure) Quota() string          { return f.quota }
func (f *AttemptFailure) BillingUncertain() bool { return f.billingUncertain() }

func (f *AttemptFailure) rejectedWithoutWork() bool {
	if f == nil {
		return false
	}
	if f.Acceptance == upstream.NotSent {
		return true
	}
	if f.Acceptance != upstream.Terminal || f.accepted {
		return false
	}
	switch f.Status {
	case 400, 401, 403, 404, 405, 413, 415, 422, 429:
		return true
	}
	return false
}

// billingUncertain reports whether the upstream may have served and billed
// work this attempt cannot account for. Anything already delivered to the
// client was served, and work the upstream may hold is what its acceptance
// leaves unresolved. A successful response may be billable even when its
// result is terminal but unreadable. A stated rejection costs nothing, a 5xx
// may have done the work its status denied, and a failure that never left this
// gateway is free. Exchange evidence, not the class, decides: a declared rule
// changes how the failure is routed, never what was billed. A failure built
// without classified evidence falls back to its class.
func (f *AttemptFailure) billingUncertain() bool {
	if f.committed || f.accepted {
		return true
	}
	if f.Acceptance != "" {
		return f.Acceptance.Unresolved()
	}
	switch f.class {
	case classRateLimit, classUpstreamClient, ClassCredential, classContextWindow, classContentFilter:
		return false
	}
	return f.dispatched
}

func (f *AttemptFailure) Fault() (result *fault.Error) {
	defer func() {
		if result != nil && result.Status >= 500 && f.noRetry {
			result.NoRetry = true
		}
	}()
	if f.contractCode != "" {
		return fault.ServerError(http.StatusBadGateway, f.contractCode, "The provider result did not satisfy the admitted interaction contract.")
	}
	if f.policyCode == string(guardrails.ErrSurface) {
		return guardrailError(guardrails.ErrSurface)
	}
	if f.policyCode != "" {
		message := "The provider result was blocked by the route's guardrails."
		if f.policyCode == "policy_conflict" {
			message = "The route's guardrails cannot inspect this provider result."
		}
		return fault.InvalidRequest(f.policyCode, message, nil)
	}
	if f.aggregateTooLarge {
		return fault.ServerError(http.StatusBadGateway, "upstream_response_too_large", "The upstream streamed a result larger than the gateway's response size limit for a non-streaming request; request a streaming response instead.")
	}
	switch f.class {
	case classLimitsUnavailable:
		return quota.LimitsUnavailable()
	case ClassAmbiguous:
		err := fault.ServerError(http.StatusBadGateway, "ambiguous_upstream_result", "The upstream provider may have applied this request; its result could not be confirmed.")
		err.NoRetry = f.noRetry
		return err
	case ClassCancelled:
		return &fault.Error{Status: 0, Code: "client_cancelled", Message: "The client went away."}
	case ClassTimeout:
		return fault.ServerError(http.StatusGatewayTimeout, "gateway_timeout", "The upstream provider did not respond within the configured deadline.")
	case ClassConnect:
		return fault.ServerError(http.StatusBadGateway, "upstream_unavailable", "The upstream provider could not be reached.")
	case classRateLimit:
		// A quota this gateway enforces was never the upstream's decision, and
		// saying so would send the caller looking at the wrong system.
		switch f.quota {
		case quota.Connection:
			return &fault.Error{Status: http.StatusTooManyRequests, Type: "rate_limit_error", Code: "rate_limit_exceeded", Message: "The provider connection " + f.exceededLimit() + " was exceeded.", RetryAfter: f.retryAfter}
		case quota.Slot:
			return &fault.Error{Status: http.StatusTooManyRequests, Type: "rate_limit_error", Code: "rate_limit_exceeded", Message: "The provider credential " + f.exceededLimit() + " was exceeded.", RetryAfter: f.retryAfter}
		case quota.SharedGroup:
			return &fault.Error{Status: http.StatusTooManyRequests, Type: "rate_limit_error", Code: "rate_limit_exceeded", Message: "The shared provider token quota " + f.exceededLimit() + " was exceeded.", RetryAfter: f.retryAfter}
		}
		return &fault.Error{Status: http.StatusTooManyRequests, Type: "rate_limit_error", Code: "upstream_rate_limit", Message: "The upstream provider is rate limiting requests.", RetryAfter: f.retryAfter}
	case classBudget:
		return fault.ServerError(http.StatusServiceUnavailable, "supply_budget_exhausted", "The spend cap of the "+f.quota+" serving this request is exhausted.")
	case ClassUpstreamServer:
		return fault.ServerError(http.StatusBadGateway, "upstream_unavailable", "The upstream provider failed with HTTP "+strconv.Itoa(f.Status)+".")
	case ClassCredential:
		code := "upstream_authentication_failed"
		if f.Status == http.StatusForbidden {
			code = "upstream_permission_denied"
		}
		return fault.ServerError(http.StatusBadGateway, code, "The upstream provider rejected the configured credential.")
	case ClassProtocol:
		return fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The upstream provider returned a malformed response.")
	case classContentFilter:
		message := "The upstream provider's content filter refused the request."
		if f.upstream != nil && f.upstream.Message != "" {
			message = f.upstream.Message
		}
		status := f.Status
		if !forwardable(status) {
			status = http.StatusBadRequest
		}
		return &fault.Error{Status: status, Type: "invalid_request_error", Code: "content_filter", Message: message}
	case classUpstreamClient, classContextWindow:
		message := "The upstream provider rejected the request."
		if f.upstream != nil && f.upstream.Message != "" {
			message = f.upstream.Message
		}
		if forwardable(f.Status) {
			return &fault.Error{Status: f.Status, Type: "invalid_request_error", Code: "upstream_rejected", Message: message}
		}
		return fault.ServerError(http.StatusBadGateway, "upstream_rejected", message)
	}
	return fault.ServerError(http.StatusBadGateway, "upstream_unavailable", "The upstream provider failed.")
}

func (f *AttemptFailure) exceededLimit() string {
	switch f.dimension {
	case limits.DimensionInputTokens:
		return "input token limit"
	case limits.DimensionOutputTokens:
		return "output token limit"
	}
	return "limit"
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
func (s *Server) executeModelStep(ctx context.Context, x *Call, source *wire.Request, current keyauthority.Authority) *Outcome {
	if x.generation.firstStep == nil {
		x.generation.parsed = source
		out := s.execute(ctx, x)
		x.generation.firstStep = &modelStepAccounting{factEnd: len(x.facts), estimate: x.estimate}
		return out
	}
	if source.Route != x.named().Slug || source.Family != x.generation.parsed.Family || x.cache != nil && x.cache.result != "bypass" {
		return &Outcome{Err: fault.InvalidRequest("invalid_request", "The model step must use the original route and format without response caching.", nil)}
	}
	stepCtx, cancel := x.routeContext(ctx)
	defer cancel()
	first := x.generation.firstStep
	actual := x.settledTokensFor(x.facts[:first.factEnd], first.estimate)
	if err := x.lease.Reconcile(stepCtx, *actual); err != nil {
		if e := s.Deps.Admission.Outage(x.authority.ID, false, err); e != nil {
			return &Outcome{Err: e}
		}
	}
	x.generation.parsed = source
	x.routeLeg = routeLeg{affinity: x.seed}
	x.allowance = 0
	x.walk = routing.RouteWalk{}
	x.classified = nil
	if e := s.prepare(stepCtx, x, x.authorize); e != nil {
		return &Outcome{Err: e}
	}
	x.estimate = requestEstimate(x)
	authority := cacheAuthority(x.AdmissionAuthority(current), false, true, false)
	reservation := quota.KeyReservationEstimate(x.estimate, s.dispatchableAttempts(x))
	lease, e := s.Deps.Admission.ReserveKeyCosted(stepCtx, &authority, x.clientSurface(), reservation, time.Duration(x.named().OverallTimeout)*time.Millisecond, quota.CostReservation{}, x.limitRoute())
	if e != nil {
		return &Outcome{Err: e}
	}
	if e := s.ReserveFallbackCost(stepCtx, x); e != nil {
		quota.SettleKey(ctx, lease, false, nil, s.Log)
		return &Outcome{Err: e}
	}
	start := len(x.facts)
	x.stepDispatched = false
	out := s.execute(stepCtx, x)
	quota.SettleKey(ctx, lease, x.stepDispatched, x.settledTokensFor(x.facts[start:], x.estimate), s.Log)
	return out
}

// execute adapts canonical inference to shared attempt execution. The
// canonical transport retains its first-byte and streaming idle deadlines.
func (s *Server) execute(ctx context.Context, x *Call) *Outcome {
	if x.cacheHit() {
		return s.replayCache(ctx, x)
	}
	ctx, cancel := x.routeContext(ctx)
	defer cancel()
	out := runAttempts(ctx, s, x, attemptAdapter[*wire.Completion]{
		demand: x.attemptDemand,
		dispatch: func(ctx context.Context, attempt routing.Attempt, provider *routing.Provider, slot routing.Slot, fact events.AttemptFact) (events.AttemptFact, *wire.Completion, *AttemptFailure) {
			return s.attempt(ctx, x, attempt, provider, slot, fact)
		},
	})
	return &Outcome{
		Completion: out.result,
		Err:        out.err,
		Committed:  out.committed,
		Cancelled:  out.cancelled,
	}
}

// slots returns the credential slots usable for this attempt, ordered by
// priority then by deterministic weighted rendezvous on the request's
// affinity so sibling keys spread across a pool.
func (s *Server) slots(x *Call, attempt routing.Attempt, provider *routing.Provider) []routing.Slot {
	if x.pinnedSlot != nil {

		if !s.slotAvailable(x, attempt, x.pinnedSlot) {
			return nil
		}
		return []routing.Slot{*x.pinnedSlot}
	}
	ordered := routing.SelectSlots(*provider, attempt.UpstreamModel, *x.Route, x.keyID, x.OperationName(), x.surfaceName(), x.mode, x.affinity)
	if attempt.Slots != nil {
		ordered = routing.ArrangeSlots(ordered, attempt.Slots)
	}
	out := make([]routing.Slot, 0, len(ordered))
	for _, slot := range ordered {
		if !s.slotAvailable(x, attempt, &slot) || (!s.Deps.Admission.Ready() && (s.health.CoolingDown(provider.ID, slot.ID) || s.health.CoolingDown(provider.ID, credentialHealthKey(&slot, s.slotGrantGeneration(&slot))))) {
			continue
		}
		out = append(out, slot)
	}

	return out
}

func (s *Server) slotAvailable(x *Call, attempt routing.Attempt, slot *routing.Slot) bool {
	if !slot.Allows(attempt.UpstreamModel, x.Route.Slug, x.keyID) {
		return false
	}
	provider := x.Snapshot().Provider(attempt.ProviderID)
	if !connectors.SecretRequired(provider.AuthMode) {
		return true
	}
	return slot.CredentialID != nil && s.Deps.Runtime.Eligibility(*slot.CredentialID) == routing.Eligible
}
