package code

import (
	"fmt"
	"time"
)

type OutcomeOrigin string

const (
	OriginUpstream OutcomeOrigin = "upstream"
	OriginGateway  OutcomeOrigin = "gateway"
	OriginClient   OutcomeOrigin = "client"
)

type OutcomeKind string

const (
	OutcomeHeaders        OutcomeKind = "headers"
	OutcomeCompleted      OutcomeKind = "completed"
	OutcomeIncomplete     OutcomeKind = "incomplete"
	OutcomeFailed         OutcomeKind = "failed"
	OutcomeRejected       OutcomeKind = "rejected"
	OutcomeInterrupted    OutcomeKind = "interrupted"
	OutcomeCanceled       OutcomeKind = "canceled"
	OutcomeTransportError OutcomeKind = "transport_error"
)

type Outcome struct {
	Origin         OutcomeOrigin
	Kind           OutcomeKind
	UpstreamStatus *int
	ObservedAt     time.Time
}

func UpstreamHeadersOutcome(status int, observedAt time.Time) Outcome {
	kind := OutcomeHeaders
	if status >= 400 {
		kind = OutcomeRejected
	}
	return Outcome{Origin: OriginUpstream, Kind: kind, UpstreamStatus: &status, ObservedAt: observedAt}
}

func UpstreamTerminalOutcome(kind OutcomeKind, status *int, observedAt time.Time) Outcome {
	return Outcome{Origin: OriginUpstream, Kind: kind, UpstreamStatus: status, ObservedAt: observedAt}
}

func GatewayOutcome(kind OutcomeKind, observedAt time.Time) Outcome {
	return Outcome{Origin: OriginGateway, Kind: kind, ObservedAt: observedAt}
}

func ClientCanceledOutcome(observedAt time.Time) Outcome {
	return Outcome{Origin: OriginClient, Kind: OutcomeCanceled, ObservedAt: observedAt}
}

func (o Outcome) WithTime(observedAt time.Time) Outcome {
	if !o.ObservedAt.IsZero() {
		return o
	}
	updated := o
	updated.ObservedAt = observedAt
	return updated
}

func (o Outcome) Validate() error {
	if o.ObservedAt.IsZero() || o.UpstreamStatus != nil && (*o.UpstreamStatus < 100 || *o.UpstreamStatus > 599) {
		return fmt.Errorf("invalid code outcome")
	}
	switch o.Origin {
	case OriginUpstream:
		switch o.Kind {
		case OutcomeHeaders:
			if o.UpstreamStatus == nil {
				return fmt.Errorf("missing upstream status")
			}
		case OutcomeCompleted, OutcomeIncomplete, OutcomeFailed, OutcomeRejected:
		default:
			return fmt.Errorf("invalid upstream outcome")
		}
		if o.Kind == OutcomeCompleted && o.UpstreamStatus != nil && (*o.UpstreamStatus < 200 || *o.UpstreamStatus > 299) ||
			o.Kind == OutcomeRejected && (o.UpstreamStatus == nil || *o.UpstreamStatus < 400) {
			return fmt.Errorf("invalid upstream outcome status")
		}
	case OriginGateway:
		if o.UpstreamStatus != nil || o.Kind != OutcomeInterrupted && o.Kind != OutcomeTransportError && o.Kind != OutcomeRejected {
			return fmt.Errorf("invalid gateway outcome")
		}
	case OriginClient:
		if o.UpstreamStatus != nil || o.Kind != OutcomeCanceled {
			return fmt.Errorf("invalid client outcome")
		}
	default:
		return fmt.Errorf("invalid code outcome origin")
	}
	return nil
}
