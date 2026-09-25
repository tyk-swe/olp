package interaction

import (
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations/generation"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// ToolContinuation reports whether this plan serves a negotiated client
// continuation contract (ordered tool history with a durable native
// dependency) rather than stateless projection.
func (p *Plan) ToolContinuation() bool {
	return p.mapping != nil && p.mapping.ClientContract != ""
}

// ToolProjection is the compatibility surface callers already consume: an
// ordered client projection over the mapping's registered stream contract.
// The mapping owns reducer use, retention bounds, and completion.
type ToolProjection struct {
	plan       *Plan
	projection generation.Projection
}

func (p *Plan) projectInput(limit int) generation.ProjectInput {
	return generation.ProjectInput{Source: p.source, Prepared: p.prepared, Effective: p.effective, Limit: limit, ValidateEvent: func(event oif.Event) error {
		return p.target.ValidateEvent(event, p.hosted)
	}}
}

// NewToolProjection starts the registered streaming projection for this plan's
// negotiated continuation contract.
func (p *Plan) NewToolProjection(limit int) (*ToolProjection, error) {
	if !p.ToolContinuation() || p.mapping.ProjectEvents == nil {
		return nil, incompatible("state_carrier", "/client_contract", "qualified_continuation_version", "The selected client continuation contract cannot preserve this interaction.")
	}
	projection, err := p.mapping.ProjectEvents(p.projectInput(limit))
	if err != nil {
		return nil, err
	}
	return &ToolProjection{plan: p, projection: projection}, nil
}

// Observe consumes one reducer-admitted native event and returns the projected
// client frames for it.
func (t *ToolProjection) Observe(event oif.Event) ([][]byte, error) {
	return t.projection.Observe(event)
}

// Accounting reports the projection's separated resource counters so callers
// and tests can distinguish retained dependency state, projected replay
// delivery, live transient memory and aggregate admitted event work.
func (t *ToolProjection) Accounting() Accounting {
	return t.projection.Accounting()
}

// Complete finishes the projection over the decoded native completion and
// returns the next-turn dependency plus the recorded client delivery.
func (t *ToolProjection) Complete(completion *openai.Completion, handle string) (*Continuation, Delivery, error) {
	return t.projection.Complete(protocols.CompletionNative(completion, t.plan.route), handle)
}

// ProjectUnary projects one decoded native result through the plan's qualified
// mapping for the negotiated continuation contract.
func (p *Plan) ProjectUnary(completion *openai.Completion, handle string, limit int) (*Continuation, Delivery, error) {
	if p.mapping == nil || p.mapping.ProjectResult == nil {
		return nil, Delivery{}, incompatible("state_carrier", "/client_contract", "qualified_continuation_version", "The selected client continuation contract cannot preserve this interaction.")
	}
	return p.mapping.ProjectResult(p.projectInput(limit), protocols.CompletionNative(completion, p.route), handle)
}
