package usage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	ClientState               string
}

func (c HTTPCompletion) validate() error {
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

// CompletePendingHTTP changes only HTTP metadata under the resource owner's
// lock. Resource observations continue to own upstream phase and usage.
func CompletePendingHTTP(event *Event, c HTTPCompletion) error {
	if err := c.validate(); err != nil {
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

// CompleteRequestHTTPTx updates existing history after financial persistence.
// It never inserts history, changes financial evidence or revises the receipt
// payload. Absent history may have expired and is never resurrected.
func CompleteRequestHTTPTx(ctx context.Context, tx pgx.Tx, c HTTPCompletion) error {
	if err := c.validate(); err != nil {
		return err
	}
	var completed, matches bool
	err := tx.QueryRow(ctx, `SELECT completed_at IS NOT NULL,
	 completed_at IS NOT DISTINCT FROM $5::timestamptz AND status_code IS NOT DISTINCT FROM $6::integer
	 AND error_class IS NOT DISTINCT FROM $7::text AND total_latency_ms IS NOT DISTINCT FROM $8::integer
	 AND first_byte_ms IS NOT DISTINCT FROM $9::integer AND budget_boundary=$10 AND payload_captured=$11
	 FROM olp.requests WHERE id=$1 AND started_at=$2 AND api_key_id=$3
	 AND EXISTS (SELECT 1 FROM olp.request_metadata_event_receipts WHERE event_id=$4 AND request_id=$1 AND status='fact_persisted') FOR UPDATE`,
		c.RequestID, c.RequestStartedAt, c.APIKeyID, c.EventID, c.CompletedAt, c.StatusCode, c.ErrorClass, c.LatencyMS, c.FirstByteMS, c.BudgetBoundary, c.PayloadCaptured).Scan(&completed, &matches)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock original HTTP history: %w", err)
	}
	if completed && !matches {
		return fmt.Errorf("%w: HTTP history conflicts with its first final outcome", ErrInvalidEvent)
	}
	a := c.Attempt
	var attemptMatches bool
	err = tx.QueryRow(ctx, `SELECT completed_at IS NOT DISTINCT FROM $8::timestamptz
	 AND status_code IS NOT DISTINCT FROM $9::integer AND error_class IS NOT DISTINCT FROM $10::text
	 AND committed=$11 AND latency_ms IS NOT DISTINCT FROM $12::integer AND first_byte_ms IS NOT DISTINCT FROM $13::integer
	 AND ($14='' OR routing->'interaction' IS NULL OR routing->'interaction'->>'client_state'=$14)
	 FROM olp.attempts WHERE id=$1 AND request_id=$2 AND request_started_at=$3 AND ordinal=$4
	 AND provider_id=$5 AND upstream_model=$6 AND started_at=$7 FOR UPDATE`,
		a.ID, c.RequestID, c.RequestStartedAt, a.Ordinal, a.ProviderID, a.UpstreamModel, a.StartedAt, a.CompletedAt, a.StatusCode, a.ErrorClass, a.Committed, a.LatencyMS, a.FirstByteMS, a.ClientState).Scan(&attemptMatches)
	if err != nil {
		return fmt.Errorf("lock original HTTP attempt: %w", err)
	}
	if completed {
		if !attemptMatches {
			return fmt.Errorf("%w: HTTP attempt conflicts with its first final outcome", ErrInvalidEvent)
		}
		return nil
	}
	if _, err = tx.Exec(ctx, `UPDATE olp.requests SET completed_at=$3,status_code=$4,error_class=$5,total_latency_ms=$6,first_byte_ms=$7,budget_boundary=$8,payload_captured=$9 WHERE id=$1 AND started_at=$2`,
		c.RequestID, c.RequestStartedAt, c.CompletedAt, c.StatusCode, c.ErrorClass, c.LatencyMS, c.FirstByteMS, c.BudgetBoundary, c.PayloadCaptured); err != nil {
		return fmt.Errorf("complete original HTTP history: %w", err)
	}
	if _, err = tx.Exec(ctx, `UPDATE olp.attempts SET completed_at=$2,status_code=$3,error_class=$4,committed=$5,latency_ms=$6,first_byte_ms=$7,
	 routing=CASE WHEN $8<>'' AND routing->'interaction' IS NOT NULL THEN jsonb_set(routing,'{interaction,client_state}',to_jsonb($8::text)) ELSE routing END WHERE id=$1`,
		a.ID, a.CompletedAt, a.StatusCode, a.ErrorClass, a.Committed, a.LatencyMS, a.FirstByteMS, a.ClientState); err != nil {
		return fmt.Errorf("complete original HTTP attempt: %w", err)
	}
	return nil
}
