package usage

import "errors"

// InteractionEvidence is content-free execution evidence on the existing Attempt
// record. Upstream acceptance, client observation and HTTP commitment are
// independent: a lost response may leave work accepted but never observed.
type InteractionEvidence struct {
	Fidelity      string `json:"fidelity"`
	PlanClass     string `json:"plan_class"`
	UpstreamState string `json:"upstream_state"`
	ClientState   string `json:"client_state"`
}

const (
	UpstreamNotSent  = "not-sent"
	UpstreamUnknown  = "outcome-unknown"
	UpstreamAccepted = "accepted"
	UpstreamTerminal = "terminal"
	ClientUnobserved = "unobserved"
	ClientPartial    = "partially-observed"
	ClientActionable = "actionable"
	ClientTerminal   = "terminal"
)

func (e *InteractionEvidence) validate() error {
	if e == nil {
		return nil
	}
	if e.Fidelity != "strict" || e.PlanClass != "native_identity" && e.PlanClass != "qualified_interaction" {
		return errors.New("routing.interaction contract is invalid")
	}
	switch e.UpstreamState {
	case UpstreamNotSent, UpstreamUnknown, UpstreamAccepted, UpstreamTerminal:
	default:
		return errors.New("routing.interaction upstream state is invalid")
	}
	switch e.ClientState {
	case ClientUnobserved, ClientPartial, ClientActionable, ClientTerminal:
	default:
		return errors.New("routing.interaction client state is invalid")
	}
	if e.UpstreamState == UpstreamNotSent && e.ClientState != ClientUnobserved {
		return errors.New("routing.interaction observes unsent work")
	}
	return nil
}
