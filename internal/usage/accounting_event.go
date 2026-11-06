package usage

import (
	"bytes"
	"crypto/sha256"
)

type AccountingEvent struct {
	event     *Event
	validated *Validated
	payload   []byte
	digest    [sha256.Size]byte
}

type invalidAccountingEvent struct {
	eventID string
	cause   error
}

func (e *invalidAccountingEvent) Error() string { return e.cause.Error() }
func (e *invalidAccountingEvent) Unwrap() error { return e.cause }

func DecodeAccountingEvent(payload []byte) (AccountingEvent, error) {
	owned := bytes.Clone(payload)
	event, err := Decode(owned)
	if err != nil {
		return AccountingEvent{}, err
	}
	validated, err := Validate(event)
	if err != nil {
		return AccountingEvent{}, &invalidAccountingEvent{eventID: event.EventID, cause: err}
	}
	return AccountingEvent{event: event, validated: validated, payload: owned, digest: sha256.Sum256(owned)}, nil
}

func NewAccountingEvent(event *Event) (AccountingEvent, error) {
	if event == nil {
		return AccountingEvent{}, ErrInvalidEvent
	}
	payload, err := Encode(accountingRequest(event))
	if err != nil {
		return AccountingEvent{}, err
	}
	return DecodeAccountingEvent(payload)
}

func accountingRequest(event *Event) *Event {
	request := *event
	if len(event.Attempts) == 0 {
		return &request
	}
	final := event.Attempts[len(event.Attempts)-1]
	request.ProviderID = &final.ProviderID
	request.UpstreamModel = &final.UpstreamModel
	request.Committed = final.Committed
	return &request
}
