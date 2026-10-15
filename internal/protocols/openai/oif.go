package openai

import (
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/sse"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

// LiftEvent is shared by the family stream adapters. Framing remains owned by
// the transport decoder; JSON ambiguity is rejected before dialect projection.
func LiftEvent(family wire.Format, data, name string, sequence uint64, maxBytes int) (oif.Event, error) {
	d := wire.Descriptor(family, true)
	if data == "[DONE]" {
		return oif.ControlEvent(d, name, data, sequence), nil
	}
	source, err := oif.ParseJSON([]byte(data), oif.Limits{MaxBytes: maxBytes})
	if err != nil {
		return oif.Event{}, &wire.ProtocolError{Detail: err.Error()}
	}
	return oif.NewEvent(d, source, name, sequence)
}

func LiftSSE(family wire.Format, frame sse.Frame, sequence uint64, maxBytes int) (oif.Event, error) {
	name := ""
	if frame.Event != nil {
		name = *frame.Event
	}
	event, err := LiftEvent(family, frame.Data, name, sequence, maxBytes)
	if err != nil {
		return event, err
	}
	framing := oif.Framing{}
	if frame.ID != nil {
		framing.ID = *frame.ID
		framing.HasID = true
	}
	if frame.RetryMS != nil {
		framing.RetryMillis = *frame.RetryMS
		framing.HasRetry = true
	}
	return event.WithFraming(framing), nil
}

func liftResult(family wire.Format, body []byte) (oif.Result, error) {
	doc, err := oif.ParseJSON(body, oif.Limits{})
	if err != nil {
		return oif.Result{}, &wire.ProtocolError{Detail: err.Error()}
	}
	return oif.NewResult(wire.Descriptor(family, false), doc, oif.Complete)
}
