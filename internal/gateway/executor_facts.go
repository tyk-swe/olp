package gateway

import (
	"context"
	"errors"
	"net/http/httptrace"
	"sync/atomic"

	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/upstream"
	"github.com/tyk-swe/olp/internal/usage"
)

// attemptState tracks why an attempt context ended and what the attempt
// established about its upstream exchange.
type attemptState struct {
	parent            context.Context
	classifier        upstream.Classifier
	reason            atomic.Int32 // 1 first-byte deadline, 2 idle deadline, 3 stream cap
	dispatched        atomic.Bool  // request bytes may have reached the upstream
	connected         atomic.Bool
	notSent           atomic.Bool
	status            int  // the unsuccessful status the upstream answered with
	accepted          bool // the upstream answered with success
	settled           bool // the upstream's complete result arrived
	credentialRefused bool // classification before unresolved work becomes ambiguous
}

// trace supplies the dispatch evidence of a net/http request.
func (st *attemptState) trace() *httptrace.ClientTrace {
	trace := upstream.Trace(&st.dispatched)
	trace.GotConn = func(httptrace.GotConnInfo) { st.connected.Store(true) }
	return trace
}

// evidence is what the attempt has established about its upstream exchange.
func (st *attemptState) evidence() upstream.Evidence {
	return upstream.Evidence{Reached: st.dispatched.Load(), Status: st.status, Accepted: st.accepted, Settled: st.settled}
}

// rejected records the unsuccessful status the upstream answered with and
// classifies it together with the error its body stated.
func (st *attemptState) rejected(status int, stated *wire.UpstreamError) string {
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
	if stated, ok := errors.AsType[*wire.UpstreamError](err); ok {
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
	case errors.Is(err, fault.ErrClientWrite):
		return context.Canceled
	}
	return nil
}

// newFact opens the record of one attempt against one credential slot.
func (s *Server) newFact(x *call, a routing.Attempt, slot routing.Slot, ordinal int, pinned ...*routing.Provider) AttemptFact {
	fact := AttemptFact{
		Prompt:   x.prompt,
		Strategy: a.Strategy, PolicyDigest: a.PolicyDigest, VendorID: a.VendorID, Price: a.Price,
		Discount:           a.Discount,
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
	fact.Execution = x.sessionDebit.reference()
	if !x.cardPinned && s.deps.Runtime != nil {
		// A retry can outlive the inputs used to plan the request. Resolve
		// its consumer's card from current inputs and retain it on the fact.
		x.routingInputs = s.deps.Runtime.RoutingInputs()
	}
	fact.cardRev, fact.CardPinned = x.resolveAttemptCardRevision(a.ProviderID, x.route, fact.StartedAt)
	if fact.CardPinned && fact.cardRev != nil {
		fact.CardRevisionID = &fact.cardRev.ID
	}
	provider, known := x.snapshot().FindProvider(a.ProviderID)
	if len(pinned) != 0 && pinned[0] != nil {
		provider, known = *pinned[0], true
	}
	fact.CommitmentPinned = known
	if known {
		timeline := x.snapshot()
		if s.deps.Runtime != nil {
			if release := s.deps.Runtime.Release(); release != nil {
				timeline = release.Snapshot()
			}
		}
		fact.Commitment = timeline.CommitmentAt(a.ProviderID, fact.StartedAt)
		if fact.Commitment != nil && fact.Commitment.ID == "" {
			fact.CommitmentPinned = false
		}
	}
	if known && provider.CredentialSource == "caller" && x.origin != "probe" {
		fact.CredentialSource = "caller"
		fact.BudgetExempt = x.callerCostExempt()
	}
	x.recordEstimate(&fact, a)
	x.attemptCount, x.attemptVendor = ordinal, a.VendorID
	if slot.CredentialID != nil && fact.CredentialSource != "caller" {
		fact.CredentialID = *slot.CredentialID
	}
	if slot.CredentialVersion != nil && fact.CredentialSource != "caller" {
		fact.CredentialVersion = *slot.CredentialVersion
	}
	evidence := x.preparation().attemptEvidence(a)
	fact.GuardrailDecisions = evidence.guardrails
	if x.strict() {
		if x.family == wire.GeminiInteractions || x.family == wire.GeminiLive {
			fact.Interaction = &usage.InteractionEvidence{Fidelity: routing.FidelityStrict, PlanClass: "native_identity", UpstreamState: upstream.NotSent, ClientState: usage.ClientUnobserved}
			return fact
		}
		if x.family == wire.Realtime {
			if _, ok := x.snapshot().RealtimeTemplate(x.route.Slug, a.TargetID); ok {
				fact.Interaction = &usage.InteractionEvidence{Fidelity: routing.FidelityStrict, PlanClass: "native_identity", UpstreamState: upstream.NotSent, ClientState: usage.ClientUnobserved}
			}
			return fact
		}
		if x.family == wire.Batch || x.family == wire.File ||
			x.family == wire.AnthropicFile || x.family == wire.AnthropicBatch ||
			x.family == wire.GeminiFile || x.family == wire.GeminiBatch || x.family == wire.CloudBatch {
			if _, ok := x.snapshot().DurableTemplate(x.route.Slug, a.TargetID); ok {
				fact.Interaction = &usage.InteractionEvidence{Fidelity: routing.FidelityStrict, PlanClass: "native_identity", UpstreamState: upstream.NotSent, ClientState: usage.ClientUnobserved}
			}
			return fact
		}
		if x.media != nil {
			if _, ok := x.snapshot().MediaTemplate(x.route.Slug, a.TargetID, string(x.media.Op())); ok {
				fact.Interaction = &usage.InteractionEvidence{Fidelity: routing.FidelityStrict, PlanClass: "native_identity", UpstreamState: upstream.NotSent, ClientState: usage.ClientUnobserved}
			}
		} else if evidence.planClass != "" {
			fact.Interaction = &usage.InteractionEvidence{Fidelity: routing.FidelityStrict, PlanClass: evidence.planClass, UpstreamState: upstream.NotSent, ClientState: usage.ClientUnobserved}
		}
	}
	return fact
}

// rejectedFact records an attempt a quota refused. Nothing was sent, so the
// attempt carries no status and cannot have been billed by anyone.
func (s *Server) rejectedFact(x *call, a routing.Attempt, slot routing.Slot, ordinal int, rejection *attemptFailure) AttemptFact {
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
