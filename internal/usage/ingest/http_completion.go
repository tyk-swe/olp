package ingest

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/usage"
)

// CompleteRequestHTTPTx updates existing history after financial persistence.
// It never inserts history, changes financial evidence or revises the receipt
// payload. Absent history may have expired and is never resurrected.
func CompleteRequestHTTPTx(ctx context.Context, tx pgx.Tx, c usage.HTTPCompletion) error {
	if err := c.Validate(); err != nil {
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
		return fmt.Errorf("%w: HTTP history conflicts with its first final outcome", usage.ErrInvalidEvent)
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
			return fmt.Errorf("%w: HTTP attempt conflicts with its first final outcome", usage.ErrInvalidEvent)
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
