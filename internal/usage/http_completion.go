package usage

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// HTTPCompletion records the original exchange's terminal observation without
// carrying prices, counters or another financial event. Its identities come
// from the retained generation template, never a newly rendered usage event.
type HTTPCompletion struct {
	EventID, RequestID, APIKeyID  string
	RequestStartedAt, CompletedAt time.Time
	StatusCode                    *int
	ErrorClass                    *string
	LatencyMS                     int64
	FirstByteMS                   *int64
	BudgetBoundary                string
	PayloadCaptured               bool
	Attempt                       HTTPAttemptCompletion
}

// HTTPAttemptCompletion describes only the unfinished final attempt. Previous
// attempts already have final metadata and remain untouched.
type HTTPAttemptCompletion struct {
	ID                        string
	Ordinal                   int
	ProviderID, UpstreamModel string
	StartedAt, CompletedAt    time.Time
	StatusCode                *int
	ErrorClass                *string
	Committed                 bool
	LatencyMS                 int64
	FirstByteMS               *int64
	ClientState               ClientState
}

func (c HTTPCompletion) Validate() error {
	for _, id := range []string{c.EventID, c.RequestID, c.APIKeyID, c.Attempt.ID, c.Attempt.ProviderID} {
		if _, err := uuid.Parse(id); err != nil {
			return fmt.Errorf("%w: HTTP completion identity is malformed", ErrInvalidEvent)
		}
	}
	if c.RequestStartedAt.IsZero() || c.CompletedAt.Before(c.RequestStartedAt) || c.Attempt.StartedAt.IsZero() || c.Attempt.CompletedAt.Before(c.Attempt.StartedAt) || c.Attempt.Ordinal < 1 || c.Attempt.UpstreamModel == "" {
		return fmt.Errorf("%w: HTTP completion timing or target is invalid", ErrInvalidEvent)
	}
	if !validStatus(c.StatusCode) || !validStatus(c.Attempt.StatusCode) || !validBudgetBoundary(c.BudgetBoundary, c.ErrorClass) {
		return fmt.Errorf("%w: HTTP completion status is invalid", ErrInvalidEvent)
	}
	for _, latency := range []int64{c.LatencyMS, c.Attempt.LatencyMS} {
		if _, err := narrowMilliseconds(latency); err != nil {
			return fmt.Errorf("%w: HTTP completion latency is invalid", ErrInvalidEvent)
		}
	}
	for _, latency := range []*int64{c.FirstByteMS, c.Attempt.FirstByteMS} {
		if _, err := narrowOptionalMilliseconds(latency); err != nil {
			return fmt.Errorf("%w: HTTP completion first byte is invalid", ErrInvalidEvent)
		}
	}
	switch c.Attempt.ClientState {
	case "", ClientUnobserved, ClientPartial, ClientActionable, ClientTerminal:
	default:
		return fmt.Errorf("%w: HTTP completion client state is invalid", ErrInvalidEvent)
	}
	return nil
}

// completePendingHTTP changes only HTTP metadata under the resource owner's
// lock. Resource observations continue to own upstream phase and usage.
func completePendingHTTP(event *Event, c HTTPCompletion) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if event.EventID != c.EventID || event.RequestID != c.RequestID || event.APIKeyID != c.APIKeyID || !event.RequestStartedAt.Equal(c.RequestStartedAt) || len(event.Attempts) != c.Attempt.Ordinal {
		return fmt.Errorf("%w: HTTP completion does not own this generation", ErrInvalidEvent)
	}
	final := &event.Attempts[len(event.Attempts)-1]
	if final.ID != c.Attempt.ID || final.ProviderID != c.Attempt.ProviderID || final.UpstreamModel != c.Attempt.UpstreamModel || !final.StartedAt.Equal(c.Attempt.StartedAt) {
		return fmt.Errorf("%w: HTTP completion does not own this attempt", ErrInvalidEvent)
	}
	if !event.HTTPOutcomePending {
		if sameHTTPCompletion(event, c) {
			return nil
		}
		return fmt.Errorf("%w: HTTP completion conflicts with its first final outcome", ErrInvalidEvent)
	}
	event.HTTPOutcomePending = false
	event.RequestCompletedAt, event.StatusCode, event.ErrorClass = c.CompletedAt, c.StatusCode, c.ErrorClass
	event.LatencyMS, event.FirstByteMS = c.LatencyMS, c.FirstByteMS
	event.Committed = c.Attempt.Committed
	event.BudgetBoundary, event.PayloadCaptured = c.BudgetBoundary, c.PayloadCaptured
	final.CompletedAt, final.StatusCode, final.ErrorClass = c.Attempt.CompletedAt, c.Attempt.StatusCode, c.Attempt.ErrorClass
	final.Committed, final.LatencyMS, final.FirstByteMS = c.Attempt.Committed, c.Attempt.LatencyMS, c.Attempt.FirstByteMS
	if final.Routing != nil && final.Routing.Interaction != nil && c.Attempt.ClientState != "" {
		final.Routing.Interaction.ClientState = c.Attempt.ClientState
	}
	_, err := Validate(event)
	return err
}

func sameHTTPCompletion(event *Event, c HTTPCompletion) bool {
	final := &event.Attempts[len(event.Attempts)-1]
	return event.RequestCompletedAt.Equal(c.CompletedAt) && equalOptional(event.StatusCode, c.StatusCode) && equalOptional(event.ErrorClass, c.ErrorClass) &&
		event.LatencyMS == c.LatencyMS && equalOptional(event.FirstByteMS, c.FirstByteMS) && event.BudgetBoundary == c.BudgetBoundary && event.PayloadCaptured == c.PayloadCaptured &&
		final.CompletedAt.Equal(c.Attempt.CompletedAt) && equalOptional(final.StatusCode, c.Attempt.StatusCode) && equalOptional(final.ErrorClass, c.Attempt.ErrorClass) &&
		final.Committed == c.Attempt.Committed && final.LatencyMS == c.Attempt.LatencyMS && equalOptional(final.FirstByteMS, c.Attempt.FirstByteMS) &&
		(c.Attempt.ClientState == "" || final.Routing == nil || final.Routing.Interaction == nil || final.Routing.Interaction.ClientState == c.Attempt.ClientState)
}

func equalOptional[T comparable](a, b *T) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}
