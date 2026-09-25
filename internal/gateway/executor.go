package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// classResourceExhausted marks a bounded gateway-local resource reaching
	// its ceiling: retained dependency bytes, projected delivery bytes,
	// transient event memory, aggregate admitted event work, the stream
	// lifetime, or a durable persistence bound. The provider produced work
	// this proxy could not afford to keep — never provider ill health, so
	// the class feeds no circuit or credential cooldown.
	classResourceExhausted = "resource_exhausted"
)

// Fault origins attribute one failed attempt to the component that owns the
// failure, decided before the retry-taxonomy class feeds health accounting or
// failover. The class alone cannot tell a provider outage from a proxy-local
// defect, and only provider-owned origins may ever charge the shared endpoint
// circuit.
const (
	// faultNativeOutcome is a faithfully delivered native terminal outcome
	// that is itself the report, such as a provider-declared "failed"
	// response; the provider exchange completed and answered.
	faultNativeOutcome = "native_outcome"
	// faultProviderTransport is a connect, deadline or wire-protocol failure
	// of the provider exchange: refused connections, timeouts after dispatch,
	// truncated or malformed provider data.
	faultProviderTransport = "provider_transport"
	// faultProviderDeclared is an error the provider itself declared: an HTTP
	// error status or an in-band error envelope.
	faultProviderDeclared = "provider_declared"
	// faultClientDelivery is the caller's own connection failing; it is never
	// evidence about the upstream.
	faultClientDelivery = "client_delivery"
	// faultProxyCapacity is a gateway-side resource bound — response body,
	// event or continuation bytes — rejecting an otherwise valid exchange.
	faultProxyCapacity = "proxy_capacity"
	// faultProxyPersistence is the gateway failing to durably record its own
	// retained state: response resources, continuations, claims.
	faultProxyPersistence = "proxy_persistence"
	// faultProxyPolicy is a local content-policy refusal after a complete
	// provider exchange.
	faultProxyPolicy = "proxy_policy"
	// faultContract is an admitted contract/profile mismatch or projection
	// defect; it quarantines contract evidence rather than the endpoint.
	faultContract = "contract"
)

// Fault scopes bound how far one failure reaches. Only endpoint- and
// credential-scoped provider faults may charge shared health state; request-
// and contract-scoped faults stay local to the evidence that owns them.
const (
	scopeEndpoint   = "endpoint"
	scopeCredential = "credential"
	scopeContract   = "contract"
	scopeRequest    = "request"
)

// defaultFault derives the fault origin and affected scope a retry-taxonomy
// class implies when the failure carried no explicit attribution. The mapping
// preserves the accounting each class already produced: nothing here widens or
// narrows the health charge a class had before faults were structured.
func defaultFault(class string) (origin, scope string) {
	switch class {
	case classConnect, classTimeout, classProtocol, classAmbiguous:
		return faultProviderTransport, scopeEndpoint
	case classUpstreamServer:
		return faultProviderDeclared, scopeEndpoint
	case classRateLimit, classCredential:
		return faultProviderDeclared, scopeCredential
	case classUpstreamClient, classContextWindow:
		return faultProviderDeclared, scopeRequest
	case classCancelled:
		return faultClientDelivery, scopeRequest
	case classPolicy:
		return faultProxyPolicy, scopeRequest
	case classLimitsUnavailable, classResourceExhausted:
		return faultProxyCapacity, scopeRequest
	}
	return "", ""
}

// endpointScoped reports whether a transport failure actually reached the
// provider; a deadline or refusal before dispatch is request-local evidence.
func endpointScoped(dispatched bool) string {
	if dispatched {
		return scopeEndpoint
	}
	return scopeRequest
}

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
	sourceSummary        *requestSummary
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
	overall      bool                    // the route deadline, not the attempt deadline, expired
	dispatched   bool                    // the request reached the upstream before the failure
	quota        string                  // a quota this gateway enforces rejected the attempt
	contractCode string                  // safe runtime interaction guard violation
	policyCode   string                  // local output policy refusal after upstream completion
	noRetry      bool                    // strict outcome uncertainty must not suggest client retries
	origin       string                  // fault origin; empty falls back to the class default
	scope        string                  // affected scope; empty falls back to the class default
	resource     string                  // the constrained gateway resource a capacity fault names
	exhausted    *interaction.Exhaustion // the bounded local resource that ran out
}

// attribute fills in fault origin and scope from the resolved class when the
// failure site did not attribute them, then copies the fault evidence onto
// the fact. A bounded-resource exhaustion names the budget that ran out.
func (f *attemptFailure) attribute(fact *AttemptFact) {
	if f.origin == "" || f.scope == "" {
		origin, scope := defaultFault(f.class)
		if f.origin == "" {
			f.origin = origin
		}
		if f.scope == "" {
			f.scope = scope
		}
	}
	resource := f.resource
	if resource == "" && f.exhausted != nil {
		resource = f.exhausted.Resource
	}
	fact.FaultOrigin, fact.FaultScope, fact.FaultResource = f.origin, f.scope, resource
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
	// A quota this gateway enforces was never the upstream's decision, and
	// saying so would send the caller looking at the wrong system.
	switch f.quota {
	case quotaConnection:
		return &Error{Status: http.StatusTooManyRequests, Type: "rate_limit_error", Code: "rate_limit_exceeded", Message: "The provider connection limit was exceeded.", RetryAfter: f.retryAfter}
	case quotaSlot:
		return &Error{Status: http.StatusTooManyRequests, Type: "rate_limit_error", Code: "rate_limit_exceeded", Message: "The provider credential limit was exceeded.", RetryAfter: f.retryAfter}
	}
	// A bounded local resource names itself and its budget category rather
	// than claiming the provider malfunctioned; exhaustion is explicit and
	// never provider ill health.
	if f.class == classResourceExhausted {
		resource, category := "stream_work_bytes", string(interaction.LimitEventWork)
		if f.exhausted != nil {
			resource, category = f.exhausted.Resource, string(f.exhausted.Category)
		}
		return serverError(http.StatusBadGateway, "resource_exhausted", "The provider result exceeded the gateway's bounded "+category+" budget: "+resource+".")
	}
	if f.contractCode != "" {
		return serverError(http.StatusBadGateway, f.contractCode, "The provider result did not satisfy the admitted interaction contract.")
	}
	// A proxy-local fault names the actual constrained resource or the
	// durability step that failed; it never claims the provider returned
	// malformed data.
	switch f.origin {
	case faultProxyCapacity:
		resource := f.resource
		if resource == "" {
			resource = "resource"
		}
		return serverError(http.StatusServiceUnavailable, "proxy_resource_exhausted", "The provider exchange exceeded the gateway's "+resource+" limit.")
	case faultProxyPersistence:
		return serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The gateway could not durably record the provider outcome.")
	case faultContract:
		if f.class == classProtocol && f.contractCode == "" {
			return serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider exchange could not be projected under the admitted contract.")
		}
	}
	if f.policyCode != "" {
		message := "The provider result was blocked by the route's content policy."
		if f.policyCode == "policy_conflict" {
			message = "The route's content policy cannot inspect this provider result."
		}
		return invalidRequest(f.policyCode, message, nil)
	}
	// A provider-declared failure delivered in-band is the provider's own
	// outcome report, not malformed data this gateway failed to read.
	if f.origin == faultProviderDeclared && f.class == classProtocol && f.upstream != nil {
		return upstreamError(f)
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

// classify attributes an in-flight attempt error before the retry taxonomy is
// consulted. A deadline or refusal that never reached the provider is
// request-local; the gateway's own byte, work and lifetime ceilings are
// resource exhaustion, never provider ill health; a truncated or malformed
// provider stream remains a provider transport fault.
func (st *attemptState) classify(err error, committed bool) *attemptFailure {
	switch {
	case st.parent.Err() != nil:
		if errors.Is(st.parent.Err(), context.DeadlineExceeded) {
			return &attemptFailure{class: classTimeout, origin: faultProviderTransport, scope: endpointScoped(st.dispatched.Load())}
		}
		return &attemptFailure{class: classCancelled, origin: faultClientDelivery, scope: scopeRequest}
	case st.reason.Load() != 0:
		if st.reason.Load() == 3 {
			// The stream lifetime cap is a proxy-local bound: the provider
			// delivered for a full hour, which is exhausted lifetime, not
			// ill health.
			return &attemptFailure{
				class:  classResourceExhausted,
				origin: faultProxyCapacity,
				scope:  scopeRequest,
				exhausted: &interaction.Exhaustion{
					Resource: "stream_lifetime_seconds", Category: interaction.LimitTime, Limit: int(maxStreamDuration / time.Second),
				},
			}
		}
		// First-byte and idle deadlines measure an exchange this gateway
		// already opened against the endpoint; both are endpoint evidence.
		return &attemptFailure{class: classTimeout, origin: faultProviderTransport, scope: scopeEndpoint}
	case errors.Is(err, errClientWrite):
		return &attemptFailure{class: classCancelled, origin: faultClientDelivery, scope: scopeRequest}
	}
	var pe *openai.ProtocolError
	var exhaustion *interaction.Exhaustion
	switch {
	case errors.As(err, &exhaustion):
		// A bounded byte/work budget is exhausted the same whether or not
		// earlier frames were committed.
		return &attemptFailure{class: classResourceExhausted, origin: faultProxyCapacity, scope: scopeRequest, exhausted: exhaustion}
	case errors.Is(err, openai.ErrEventTooLarge):
		return &attemptFailure{
			class:     classResourceExhausted,
			origin:    faultProxyCapacity,
			scope:     scopeRequest,
			exhausted: &interaction.Exhaustion{Resource: "event_bytes", Category: interaction.LimitBytes},
		}
	case errors.Is(err, errResponsePersistence):
		// Retained-state durability failed locally; the provider exchange
		// itself produced no fault evidence.
		return &attemptFailure{class: classProtocol, origin: faultProxyPersistence, scope: scopeRequest}
	case errors.Is(err, errResponseMapping):
		// The provider's own frame data was malformed or drifted from the
		// accepted identity; that is endpoint evidence, not a local defect.
		return &attemptFailure{class: classProtocol, origin: faultProviderTransport, scope: scopeEndpoint}
	case errors.Is(err, errFrameProjection):
		// The gateway's own projection of a provider frame failed; that is
		// contract evidence, never provider transport evidence.
		return &attemptFailure{class: classProtocol, origin: faultContract, scope: scopeContract}
	case errors.As(err, &pe):
		return &attemptFailure{class: classProtocol, origin: faultProviderTransport, scope: scopeEndpoint}
	case committed:
		return &attemptFailure{class: classProtocol, origin: faultProviderTransport, scope: scopeEndpoint}
	}
	return &attemptFailure{class: classConnect, origin: faultProviderTransport, scope: scopeEndpoint}
}

func endpointPath(family openai.Family) string {
	if family == openai.FamilyResponses {
		return "/responses"
	}
	return "/chat/completions"
}

// responsesTerminalStatus returns the provider-declared terminal status a
// client-facing Responses frame carries, or "" when the frame is not one of
// the terminal lifecycle events.
func responsesTerminalStatus(frame []byte) string {
	const marker = "event: response."
	i := bytes.Index(frame, []byte(marker))
	if i < 0 {
		return ""
	}
	rest := frame[i+len(marker):]
	end := bytes.IndexByte(rest, '\n')
	if end < 0 {
		return ""
	}
	switch string(rest[:end]) {
	case "completed":
		return "completed"
	case "incomplete":
		return "incomplete"
	case "failed":
		return "failed"
	case "cancelled":
		return "cancelled"
	}
	return ""
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
	rejection.attribute(&fact)
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
		f.attribute(&fact)
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
		// The egress policy refused the configured endpoint before any byte
		// was sent; that is this gateway's configuration, not a signal the
		// provider produced.
		return fail(classConnect, &attemptFailure{origin: faultContract, scope: scopeContract})
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
		return fail(classProtocol, &attemptFailure{origin: faultContract, scope: scopeContract})
	}
	deadline, _ := ctx.Deadline()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return fail(classTimeout, &attemptFailure{overall: true, origin: faultProxyCapacity, scope: scopeRequest, resource: "request deadline"})
	}
	timeout := min(a.Timeout, remaining)

	actx, cancel := context.WithCancel(attemptCtx)
	defer cancel()
	firstByte := time.AfterFunc(timeout, func() { st.reason.CompareAndSwap(0, 1); cancel() })
	defer firstByte.Stop()

	endpoint, err := cfg.URL(wire, a.UpstreamModel, x.parsed.Stream)
	if err != nil {
		return fail(classProtocol, &attemptFailure{origin: faultContract, scope: scopeContract})
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(actx, st.trace()), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fail(classConnect, &attemptFailure{origin: faultContract, scope: scopeRequest})
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
			f := st.classify(err, false)
			return fail(f.class, f)
		}
		// Signing the request is local machinery; the provider never saw it.
		return fail(classCredential, &attemptFailure{origin: faultContract, scope: scopeCredential})
	}

	client, err := s.providerClient(actx, x.request.release, provider, slot)
	if err != nil {
		return fail(classCredential, &attemptFailure{origin: faultContract, scope: scopeCredential})
	}
	if contract != nil && contract.ToolContinuation() {
		if err := s.claimToolWork(actx, x, contract, a, slot); err != nil {
			if errors.Is(err, resources.ErrPayloadTooLarge) {
				// The continuation payload itself overflowed its durable
				// bound: a named local resource ceiling, not provider
				// evidence.
				return fail(classResourceExhausted, &attemptFailure{
					noRetry: true,
					origin:  faultProxyCapacity,
					scope:   scopeRequest,
					exhausted: &interaction.Exhaustion{
						Resource: "continuation_payload_bytes", Category: interaction.LimitPersistence, Limit: resources.MaxContinuationBytes,
					},
				})
			}
			return fail(classProtocol, &attemptFailure{contractCode: "continuation_unavailable", noRetry: true, origin: faultProxyPersistence, scope: scopeRequest})
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		f := st.classify(err, false)
		return fail(f.class, f)
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
		f := &attemptFailure{status: resp.StatusCode, upstream: openai.ParseErrorBody(raw), origin: faultProviderDeclared}
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
	nativeStatus := ""
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
					// The mapping layer already owns its fault: malformed or
					// drifting provider data is endpoint evidence, durability
					// is proxy persistence, and only an otherwise unattributed
					// failure is this gateway's projection defect.
					switch {
					case errors.Is(mapErr, errResponsePersistence),
						errors.Is(mapErr, errResponseMapping),
						errors.Is(mapErr, errFrameProjection):
						return mapErr
					default:
						return fmt.Errorf("%w: %w", errFrameProjection, mapErr)
					}
				}
				frame = mapped
			}
			if x.family == openai.FamilyResponses {
				// The provider's own terminal status is outcome evidence;
				// "incomplete" is a valid terminal, never a fault.
				if status := responsesTerminalStatus(frame); status != "" {
					nativeStatus = status
				}
			}
			if wire == openai.FamilyResponses && (bytes.Contains(frame, []byte("event: response.failed\n")) || bytes.Contains(frame, []byte("event: error\n"))) {
				var err error
				frame, err = redactFailedResponseFrame(frame, credentialValues)
				if err != nil {
					return fmt.Errorf("%w: %v", errFrameProjection, err)
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
		var observe func(oif.Event) error
		if contract != nil {
			observe = func(event oif.Event) error {
				if err := contract.ValidateEvent(event); err != nil {
					return err
				}
				if x.strict() && x.family == openai.FamilyResponses {
					if kind, present := event.Source().Root().Lookup("type"); present {
						// The provider's own terminal status is outcome
						// evidence, never a protocol violation.
						if text, ok := kind.Text(); ok {
							switch text {
							case "response.completed", "response.incomplete", "response.failed", "response.cancelled":
								nativeStatus = strings.TrimPrefix(text, "response.")
							}
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
					if x.family == openai.FamilyResponses && native.Native.Source().Valid() {
						if status, present := native.Native.Source().Lookup("/status"); present {
							if text, ok := status.Text(); ok {
								nativeStatus = text
							}
						}
					}
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
					if x.strict() && x.family == openai.FamilyResponses {
						source := native.Native.Source()
						if _, present := source.Lookup("/model"); present {
							model, _ := json.Marshal(x.route.Slug)
							source, err = oif.Apply(source, []oif.Change{{Pointer: "/model", Value: string(model), Origin: oif.IdentityBinding, Reason: "published response model"}})
						}
						if err == nil {
							completion.Body = source.Bytes()
						}
					}
				}
			}
			if err == nil && completion == nil {
				completion, err = protocols.DecodeRequest(wire, x.family, raw, x.route.Slug, protocols.EmbeddingEncoding(x.parsed, provider.ParameterDefaults), x.parsed)
			}
		}
	}
	fact.NativeStatus = nativeStatus
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
		var exhausted *interaction.Exhaustion
		switch {
		case errors.As(err, &exhausted):
			f.exhausted = exhausted
			return fail(classResourceExhausted, f)
		case errors.Is(err, resources.ErrPayloadTooLarge):
			f.exhausted = &interaction.Exhaustion{Resource: "continuation_payload_bytes", Category: interaction.LimitPersistence, Limit: resources.MaxContinuationBytes}
			return fail(classResourceExhausted, f)
		case errors.Is(err, errResponseTooLarge):
			f.exhausted = &interaction.Exhaustion{Resource: "response_bytes", Category: interaction.LimitBytes, Limit: int(s.cfg.MaxResponseBytes)}
			return fail(classResourceExhausted, f)
		case errors.Is(err, openai.ErrEventTooLarge):
			f.exhausted = &interaction.Exhaustion{Resource: "event_bytes", Category: interaction.LimitBytes, Limit: int(s.cfg.MaxEventBytes)}
			return fail(classResourceExhausted, f)
		case errors.As(err, &violation):
			// The provider's exchange completed; the projection of its
			// result broke the admitted contract, which is a defect of
			// this gateway's contract layer, not provider evidence.
			f.contractCode = "fidelity_protocol_violation"
			f.origin, f.scope = faultContract, scopeContract
			return fail(classProtocol, f)
		case errors.As(err, &ue):
			f.upstream = ue
			ue.Message = redactCredentials(ue.Message, credentialValues)
			class, status := inBandFailure(ue, committed)
			f.status = status
			// An in-band error envelope is the provider's own terminal
			// report; the class still bounds which scope it may charge.
			f.origin = faultProviderDeclared
			return fail(class, f)
		}
		f = st.classify(err, committed)
		f.status = resp.StatusCode
		f.committed = committed
		return fail(f.class, f)
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

// errFrameProjection marks a failure in the gateway's own frame mapping or
// credential redaction — a contract defect, not provider wire evidence.
var errFrameProjection = errors.New("provider frame could not be projected faithfully")

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
