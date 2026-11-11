package usage

import (
	"errors"

	"github.com/tyk-swe/olp/internal/upstream"
)

// InteractionEvidence is content-free execution evidence on the existing Attempt
// record. Upstream acceptance, client observation and HTTP commitment are
// independent: a lost response may leave work accepted but never observed.
type InteractionEvidence struct {
	Fidelity      string              `json:"fidelity"`
	PlanClass     string              `json:"plan_class"`
	UpstreamState upstream.Acceptance `json:"upstream_state"`
	ClientState   ClientState         `json:"client_state"`
}

type ClientState string

const (
	ClientUnobserved ClientState = "unobserved"
	ClientPartial    ClientState = "partially-observed"
	ClientActionable ClientState = "actionable"
	ClientTerminal   ClientState = "terminal"
)

func (e *InteractionEvidence) validate() error {
	if e == nil {
		return nil
	}
	if e.Fidelity != "strict" || e.PlanClass != "native_identity" && e.PlanClass != "qualified_interaction" {
		return errors.New("routing.interaction contract is invalid")
	}
	switch e.UpstreamState {
	case upstream.NotSent, upstream.Unknown, upstream.Accepted, upstream.Terminal:
	default:
		return errors.New("routing.interaction upstream state is invalid")
	}
	switch e.ClientState {
	case ClientUnobserved, ClientPartial, ClientActionable, ClientTerminal:
	default:
		return errors.New("routing.interaction client state is invalid")
	}
	if e.UpstreamState == upstream.NotSent && e.ClientState != ClientUnobserved {
		return errors.New("routing.interaction observes unsent work")
	}
	return nil
}
