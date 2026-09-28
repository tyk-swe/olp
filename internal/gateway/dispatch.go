package gateway

import (
	"context"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/runtime"
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
}

// dispatchHold owns what one admitted slot holds until its upstream attempt
// settles: the target quota reservation and, while the provider circuit is
// recovering, the exclusive half-open probe.
type dispatchHold struct {
	provider    string
	reservation *targetReservation
	probed      bool
	settled     bool
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

// cooling reports whether a candidate slot is parked: the shared credential
// or slot cooldown when distributed coordination is configured, or this
// replica's local cooldown when it is not.
func (s *Server) cooling(ctx context.Context, providerID string, slot *runtime.Slot) bool {
	generation := s.slotGrantGeneration(slot)
	if s.Admission.ready() {
		return s.Admission.cooling(ctx, providerID, slot, generation)
	}
	return s.health.coolingDown(providerID, slot.ID) ||
		s.health.coolingDown(providerID, credentialHealthKey(slot, generation))
}

func (s *Server) slotGrantGeneration(slot *runtime.Slot) int64 {
	if slot.CredentialID == nil {
		return 0
	}
	return s.Runtime.GrantGeneration(*slot.CredentialID)
}

// gateSlot is the shared pre-dispatch boundary every new upstream attempt
// crosses — ordinary inference, media, and durable video create alike. A slot
// list filtered before an earlier call is not authority for a later sibling,
// so each candidate is revalidated in slot order: live API and network
// credential eligibility, the route deadline, the distributed or local cooldown,
// connection and slot quota reservations, and the provider circuit's exclusive
// half-open probe. An admitted slot's hold carries the reservation and the probe
// until the attempt settles or releaseHold abandons them before dispatch.
func (s *Server) gateSlot(ctx context.Context, provider *runtime.Provider, slot *runtime.Slot, estimate int64, deadline time.Time) gateResult {
	// Authority can change while an earlier credential attempt is pending.
	if connectors.SecretRequired(provider.AuthMode) && slot.CredentialID != nil && s.Runtime.Eligibility(*slot.CredentialID) != runtime.Eligible {
		return gateResult{verdict: gateSkip}
	}
	if provider.Network != nil && provider.Network.CredentialID != "" && s.Runtime.Eligibility(provider.Network.CredentialID) != runtime.Eligible {
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
	reservation, rejection, skip := s.Admission.reserveTarget(ctx, provider, slot, estimate, ttl)
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
func (s *Server) cooldownFailure(ctx context.Context, providerID string, slot *runtime.Slot, generation int64, failure *attemptFailure) bool {
	if failure.dispatched && slot.CredentialID != nil &&
		(failure.class == classCredential || failure.status == http.StatusUnauthorized) {
		// The upstream refused the credential, whatever rule classified the
		// failure: a grant beneath it is refreshed early, which ends the
		// cooldown (GrantRefreshed).
		s.Runtime.CredentialRefused(*slot.CredentialID, generation)
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
		s.Admission.cooldown(ctx, providerID, slot, generation, credentialCooldown, true)
	case classRateLimit:
		s.health.cooldown(providerID, slot.ID, failure.retryAfter)
		s.Admission.cooldown(ctx, providerID, slot, generation, cooldownDuration(failure.retryAfter), false)
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
	generation := max(0, s.Runtime.GrantGeneration(credentialID)-1)
	s.health.endCooldown(providerID, credentialHealthKey(&runtime.Slot{CredentialID: &credentialID}, generation))
	s.Admission.endCredentialCooldown(context.Background(), providerID, credentialID, generation)
}
