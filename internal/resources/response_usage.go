package resources

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/ingest"
)

func (s *Store) ObserveResponseUsage(ctx context.Context, localID, status string, evidence usage.AttemptUsage, now time.Time) (ingest.Persisted, error) {
	kind, id, err := ParseLocalID(localID)
	if err != nil || (kind != KindResponse && kind != KindStrictResponse) {
		return ingest.Persisted{}, ErrNotFound
	}
	return withTxValue(ctx, s.pool, func(tx pgx.Tx) (ingest.Persisted, error) {
		var current string
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT state,metadata->'pending_usage' FROM olp.provider_resources
	WHERE id=$1 AND kind IN ('response','strict_response') AND state<>$2 FOR UPDATE`, id, StateDeleted).Scan(&current, &raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return ingest.Persisted{}, ErrNotFound
		}
		if err != nil {
			return ingest.Persisted{}, err
		}
		terminal := wire.ResponseStatusFinished(status)
		currentTerminal := wire.ResponseStatusFinished(current)
		nextState := status
		if currentTerminal || nextState == "" {
			nextState = current
		}
		return applyPendingUsageTx(ctx, tx, pendingUsageUpdate{
			id:              id,
			currentState:    current,
			nextState:       nextState,
			raw:             raw,
			terminal:        terminal,
			currentTerminal: currentTerminal,
			evidence:        evidence,
			now:             now,
		})
	})
}
