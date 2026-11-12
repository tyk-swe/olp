package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/connectors"
)

// gateVerdict is the shared pre-dispatch decision for one candidate slot.
type gateVerdict int

const (
	// gateSkip marks a slot unusable at dispatch time — a credential no longer
	// eligible or cooled since the plan ranked it — so the sibling slot is tried.
	gateSkip gateVerdict = iota
	// gateDenied means the provider circuit refused its half-open probe, a
	// decision every sibling credential on the endpoint shares.
	gateDenied
	// gateRejected carries a quota refusal the request budget pays for.
	gateRejected
	// gateUnmeterable marks a target whose mandatory quota cannot be
	// consulted; it is passed over rather than served unmetered.
	gateUnmeterable
	// gateExpired reports the route deadline is spent; the request stops.
	gateExpired
	// gateAdmitted owns the quota reservation and any half-open probe the
	// dispatch must settle.
	gateAdmitted
)

// gateResult carries one candidate slot's verdict plus the resources or the
// refusal attached to it.
type gateResult struct {
	verdict   gateVerdict
	rejection *attemptFailure // gateRejected: the refusing quota
	hold      *dispatchHold   // gateAdmitted: the resources to settle
	quota     string          // gateUnmeterable: the unreadable cap's scope
}

// dispatchHold owns what one admitted slot holds until its upstream attempt
// settles: the target quota reservation and, while the provider circuit is
// recovering, the exclusive half-open probe.
type dispatchHold struct {
	provider    string
	reservation *targetReservation
	probed      bool
	settled     bool
	budgets     []string
}

// gateAttempt adds request spend caps to the shared slot admission boundary.
// Its hold carries the owners that accounting and settlement must charge.
func (s *Server) gateAttempt(ctx context.Context, x *call, attempt routing.Attempt, provider *routing.Provider, slot *routing.Slot, estimate int64, deadline time.Time) gateResult {
	if x.workflow != nil {
		gate := s.gateSlot(ctx, provider, slot, estimate, deadline, x.priority)
		if gate.hold != nil && !(x.callerCostExempt() && provider.CredentialSource == "caller") {
			for _, owner := range attemptCaps(x.route, provider, slot) {
				if owner.limits != nil {
					gate.hold.budgets = append(gate.hold.budgets, owner.id)
				}
			}
		}
		return gate
	}
	caps := s.holdCaps(ctx, x, attempt, provider, slot, deadline)
	if caps.quota != "" {
		if caps.refusal != nil {
			return gateResult{verdict: gateRejected, rejection: caps.refusal}
		}
		return gateResult{verdict: gateUnmeterable, quota: caps.quota}
	}
	gate := s.gateSlot(ctx, provider, slot, estimate, deadline, x.priority)
	if gate.hold != nil {
		gate.hold.budgets = caps.owners
	}
	return gate
}

// settle reconciles and releases the quota reservation once the attempt has
// ended. It runs at most once so a pre-dispatch release cannot double-refund.
func (h *dispatchHold) settle(ctx context.Context, dispatched bool, actual *int64) {
	if h == nil || h.settled {
		return
	}
	h.settled = true
	h.reservation.settle(ctx, dispatched, actual)
}

// releaseHold abandons an admitted slot before its dispatch: the quota
// reservation is refunded in full and a held half-open probe is released so
// a sibling can still try the recovering endpoint.
func (s *Server) releaseHold(ctx context.Context, hold *dispatchHold) {
	if hold == nil {
		return
	}
	hold.settle(ctx, false, nil)
	if hold.probed {
		hold.probed = false
		s.health.releaseProbe(hold.provider)
	}
}

func (s *Server) settleSession(ctx context.Context, x *call, p *pin) {
	s.finishSessionDebit(ctx, x, x.failure != nil)
	settled := sessionSettlement(x)
	if p != nil {
		s.settlePinHold(ctx, x, p.hold, settled)
	}
	s.settleCaps(ctx, x)
	s.recordCost(x)
	settleKey(ctx, x.lease, x.dispatched, settled, s.log)
}

func sessionSettlement(x *call) *int64 {
	if !x.dispatched {
		return new(int64(0))
	}
	actual := totalTokens(x.usage())
	if len(x.facts) == 0 || !x.facts[len(x.facts)-1].BillingUncertain {
		return actual
	}
	if actual == nil {
		return new(x.estimate)
	}
	return new(max(x.estimate, *actual))
}

// settlePinHold settles a pinned hold that never passed through the attempt
// loop. When it carries the half-open probe, the attempt's outcome lands on
// the circuit through record, or the probe is released when nothing reached
// the upstream, so a pinned call never leaves the circuit wedged half-open.
func (s *Server) settlePinHold(ctx context.Context, x *call, hold *dispatchHold, actual *int64) {
	if hold == nil {
		return
	}
	hold.settle(ctx, x.dispatched, actual)
	if !hold.probed {
		return
	}
	hold.probed = false
	if x.dispatched && len(x.facts) > 0 {
		s.health.record(hold.provider, x.facts[len(x.facts)-1], true)
		return
	}
	s.health.releaseProbe(hold.provider)
}

type attemptCompletion struct {
	fact        AttemptFact
	failure     *attemptFailure
	authority   *Error
	attempt     routing.Attempt
	estimate    int64
	finishLease func() *Error
}

func (h *dispatchHold) begin(ctx context.Context, s *Server, x *call, attempt routing.Attempt, provider *routing.Provider, slot routing.Slot) (bool, *Error) {
	if !x.servingAllowed(provider, attempt.TargetID, attempt.UpstreamModel, slot, true) {
		s.releaseHold(ctx, h)
		return false, nil
	}
	if x.workflow == nil {
		return true, nil
	}
	authority, err := s.workflowAuthority(ctx, x, x.route, "")
	if err != nil {
		s.releaseHold(ctx, h)
		return false, err
	}
	x.sessionDebit = nil
	caps := attemptCaps(x.route, provider, &slot)
	if x.callerCostExempt() && provider.CredentialSource == "caller" {
		caps = [3]cappedOwner{}
	}
	if err := s.reserveSessionDebit(ctx, x, *authority, attempt, caps[:]); err != nil {
		s.releaseHold(ctx, h)
		return false, err
	}
	return true, nil
}

func (h *dispatchHold) authorize(ctx context.Context, s *Server, x *call) (context.Context, func() *Error, *Error) {
	if x.authority.TaskCapability != nil {
		admitter, ok := s.deps.Runtime.(interface {
			AdmitTaskCapabilityAttempt(context.Context, keyauthority.Authority, string, string, string, *string, string) error
		})
		var err error
		if ok {
			err = admitter.AdmitTaskCapabilityAttempt(ctx, x.authority, "inference", x.route.Slug, x.route.RevisionID, x.route.ProjectID, x.request.accountingID())
		}
		if !ok || err != nil {
			if errors.Is(err, release.ErrTaskAttemptLimit) {
				return ctx, nil, &Error{Status: http.StatusTooManyRequests, Type: "rate_limit_error", Code: "task_limit_exceeded", Message: "The task attempt limit was exceeded."}
			}
			return ctx, nil, permissionError("permission_denied", "The task capability is no longer authorized.")
		}
	}
	if x.workflow == nil {
		return ctx, func() *Error { return nil }, nil
	}
	dispatchCtx, finish := s.monitorWorkflowAuthority(ctx, x, "")
	return dispatchCtx, finish, nil
}

func (h *dispatchHold) dispatchContext(ctx context.Context, s *Server, x *call) (context.Context, func() *Error) {
	if x.workflow == nil {
		return ctx, func() *Error { return nil }
	}
	return s.monitorWorkflowModelLease(ctx, x.sessionDebit)
}

func (h *dispatchHold) complete(ctx context.Context, s *Server, x *call, completion attemptCompletion) (*Error, bool) {
	dispatched := completion.failure == nil || completion.failure.dispatched
	x.dispatched = x.dispatched || dispatched
	x.stepDispatched = x.stepDispatched || dispatched
	h.settle(ctx, dispatched, settledAttemptTokens(&completion.fact, completion.estimate))
	if dispatched {
		x.spendCaps(h.budgets, completion.attempt)
	}
	x.facts = append(x.facts, completion.fact)
	s.health.record(h.provider, completion.fact, h.probed)
	h.probed = false
	if x.workflow == nil {
		return completion.authority, false
	}
	if completion.failure == nil {
		settlementErr := s.settleWorkflowModelAttempt(ctx, x, completion.fact)
		leaseErr := completion.finishLease()
		if settlementErr != nil {
			return settlementErr, false
		}
		if leaseErr != nil {
			return leaseErr, false
		}
		return completion.authority, false
	}
	var releaseErr *Error
	if completion.failure.dispatched {
		s.markSessionUnknown(ctx, x)
	} else {
		releaseErr = s.abandonWorkflowDebit(ctx, x)
	}
	leaseErr := completion.finishLease()
	if releaseErr != nil {
		return releaseErr, false
	}
	if leaseErr != nil {
		return leaseErr, false
	}
	if completion.authority != nil {
		return completion.authority, false
	}
	if completion.failure.dispatched {
		return completion.failure.toError(), completion.failure.class == classCancelled
	}
	return nil, false
}

// cooling reports whether a candidate slot is parked: the shared credential
// or slot cooldown when distributed coordination is configured, or this
// replica's local cooldown when it is not.
func (s *Server) cooling(ctx context.Context, providerID string, slot *routing.Slot) bool {
	generation := s.slotGrantGeneration(slot)
	if s.deps.Admission.ready() {
		return s.deps.Admission.cooling(ctx, providerID, slot, generation)
	}
	return s.health.coolingDown(providerID, slot.ID) ||
		s.health.coolingDown(providerID, credentialHealthKey(slot, generation))
}

func (s *Server) slotGrantGeneration(slot *routing.Slot) int64 {
	if slot.CredentialID == nil {
		return 0
	}
	return s.deps.Runtime.GrantGeneration(*slot.CredentialID)
}

// gateSlot is the shared pre-dispatch boundary every new upstream attempt
// crosses — ordinary inference, media, and durable video create alike. A slot
// list filtered before an earlier call is not authority for a later sibling,
// so each candidate is revalidated in slot order: live API and network
// credential eligibility, the route deadline, the distributed or local cooldown,
// connection and slot quota reservations, and the provider circuit's exclusive
// half-open probe. An admitted slot's hold carries the reservation and the probe
// until the attempt settles or releaseHold abandons them before dispatch.
func (s *Server) gateSlot(ctx context.Context, provider *routing.Provider, slot *routing.Slot, estimate int64, deadline time.Time, priority string) gateResult {
	// Authority can change while an earlier credential attempt is pending.
	if connectors.SecretRequired(provider.AuthMode) && slot.CredentialID != nil && s.deps.Runtime.Eligibility(*slot.CredentialID) != routing.Eligible {
		return gateResult{verdict: gateSkip}
	}
	if provider.Network != nil && provider.Network.CredentialID != "" && s.deps.Runtime.Eligibility(provider.Network.CredentialID) != routing.Eligible {
		return gateResult{verdict: gateSkip}
	}
	// attempt.Timeout bounds first-byte/idle waits, not the lifetime of a
	// stream. Hold concurrency through the overall deadline.
	ttl := time.Until(deadline)
	if ttl <= 0 {
		// The route deadline is spent, so there is no window left to reserve
		// and nothing further to try.
		return gateResult{verdict: gateExpired}
	}
	// A cooldown another replica recorded is read here rather than while the
	// slots are ranked, so a limiter answering slowly costs one round trip
	// per slot actually tried instead of one per candidate slot.
	if s.cooling(ctx, provider.ID, slot) {
		return gateResult{verdict: gateSkip}
	}
	return acquireDispatchHold(ctx, s, provider, slot, estimate, ttl, priority)
}

func acquireDispatchHold(ctx context.Context, s *Server, provider *routing.Provider, slot *routing.Slot, estimate int64, ttl time.Duration, priority string) gateResult {
	reservation, rejection, skip := s.deps.Admission.reserveTarget(ctx, provider, slot, estimate, ttl, priority)
	if skip {
		return gateResult{verdict: gateUnmeterable}
	}
	if rejection != nil {
		return gateResult{verdict: gateRejected, rejection: rejection}
	}
	probed, granted := s.health.claim(provider.ID)
	if !granted {
		reservation.settle(ctx, false, nil)
		return gateResult{verdict: gateDenied}
	}
	return gateResult{verdict: gateAdmitted, hold: &dispatchHold{provider: provider.ID, reservation: reservation, probed: probed}}
}

// cooldownFailure parks the credential or slot that produced a retryable
// upstream failure and reports whether the provider's remaining slots should
// be skipped: credential and rate-limit failures stay slot-scoped while every
// other failure belongs to the endpoint the siblings share.
func (s *Server) cooldownFailure(ctx context.Context, providerID string, slot *routing.Slot, generation int64, failure *attemptFailure, caller ...bool) bool {
	if len(caller) != 0 && caller[0] && (failure.class == classCredential || failure.class == classRateLimit || failure.credentialRefused) {
		return true
	}
	if failure.dispatched && slot.CredentialID != nil &&
		(failure.credentialRefused || failure.class == classCredential || failure.status == http.StatusUnauthorized) {
		// The upstream refused the credential, whatever rule classified the
		// failure: a grant beneath it is refreshed early, which ends the
		// cooldown (GrantRefreshed).
		s.deps.Runtime.CredentialRefused(*slot.CredentialID, generation)
	}
	switch failure.class {
	case classCredential:
		if generation != s.slotGrantGeneration(slot) {
			return false
		}
		// The poll may install a replacement while the shared store is
		// recording this cooldown. Its generation keeps that write scoped
		// to the refused token, including across gateway replicas.
		s.health.cooldown(providerID, credentialHealthKey(slot, generation), credentialCooldown)
		s.deps.Admission.cooldown(ctx, providerID, slot, generation, credentialCooldown, true)
	case classRateLimit:
		s.health.cooldown(providerID, slot.ID, failure.retryAfter)
		s.deps.Admission.cooldown(ctx, providerID, slot, generation, cooldownDuration(failure.retryAfter), false)
	default:
		return true
	}
	return false
}

// GrantRefreshed retires the previous token's cooldown. Serving uses the new
// generation's scope immediately, so this cleanup may wait on the shared
// store without holding up the refreshed token.
func (s *Server) GrantRefreshed(providerID, credentialID string) {
	// Cleanup is best-effort: older generations are already ineligible and
	// their cooldowns expire. A delayed notifier must never clear a refusal
	// of the newly served token.
	generation := max(0, s.deps.Runtime.GrantGeneration(credentialID)-1)
	s.health.endCooldown(providerID, credentialHealthKey(&routing.Slot{CredentialID: &credentialID}, generation))
	s.deps.Admission.endCredentialCooldown(context.Background(), providerID, credentialID, generation)
}
