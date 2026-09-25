package interaction

import (
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations/generation"
)

func guardFailure(field, requirement string) error {
	return incompatible("fidelity_protocol_violation", field, requirement, "The provider result does not satisfy the admitted interaction contract.")
}

// ValidateUnary runs before projection. Native codecs retain source extensions;
// the registered dialect owns result grammar and the qualified mapping owns any
// tighter client-contract result guard.
func (p *Plan) ValidateUnary(body []byte) error {
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: p.receipt.Obligations.MaxBodyBytes})
	if err != nil || document.Root().Kind() != oif.Object {
		return guardFailure("/result", "result_grammar")
	}
	// This invokes the actual registered native grammar, never a translator. The
	// executor then uses the admitted client projector once the guard succeeds.
	native, err := p.target.DecodeNative(generation.DecodeInput{Source: p.source, Effective: p.effective, Body: body, Route: "strict-result", MaxBytes: p.receipt.Obligations.MaxBodyBytes})
	if err != nil || native == nil {
		return guardFailure("/result", "native_result_grammar")
	}
	return p.ValidateResult(native.Result)
}

// ValidateResult consumes a result already checked by its native dialect codec.
// The caller can retain observed usage even when a later projection or policy
// guard refuses delivery. No second native decode runs on the gateway hot path.
func (p *Plan) ValidateResult(result oif.Result) error {
	document := result.Source()
	if !document.Valid() || document.Len() > p.receipt.Obligations.MaxBodyBytes || result.Descriptor().Dialect != p.target.Identity {
		return guardFailure("/result", "native_result_grammar")
	}
	if p.mapping != nil && p.mapping.ValidateResult != nil {
		if err := p.mapping.ValidateResult(document); err != nil {
			return err
		}
	}
	if p.template.policy != nil && p.template.policy.HasOutput() {
		if p.target.ResultCoverage == nil {
			return incompatible("policy_conflict", "/result", "output_policy_coverage", "The output policy cannot inspect native opaque or nontext result content.")
		}
		if err := p.target.ResultCoverage(document); err != nil {
			return err
		}
	}
	return nil
}

// ValidateEvent validates individual native event envelopes synchronously before
// any projection. Ordering, terminal state, bounded accumulation and completion
// remain enforced by the dialect-owned reducer behind the registered stream
// grammar, not mutable Plan.
func (p *Plan) ValidateEvent(event oif.Event) error {
	if !p.stream || p.receipt.Class != NativeIdentity && !p.ToolContinuation() {
		return guardFailure("/events", "admitted_event_contract")
	}
	if event.Descriptor().Operation != generation.Contract() || event.Descriptor().Dialect != p.target.Identity {
		return guardFailure("/events", "event_dialect")
	}
	if event.Control() == "" {
		document := event.Source()
		if !document.Valid() || document.Len() > p.receipt.Obligations.MaxEventBytes || document.Root().Kind() != oif.Object {
			return guardFailure("/events", "bounded_event_grammar")
		}
	}
	if p.target.ValidateEvent == nil {
		return guardFailure("/events", "event_grammar")
	}
	return p.target.ValidateEvent(event)
}
