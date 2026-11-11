package resources

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/ingest"
)

// ObserveResponseUsage retains partial observations and settles terminal usage
// under the original generation identity. The resource lock serializes polls
// across replicas; accounting and removal of its pending owner commit together.
// Native status owns termination; the protocol's evidence owns completeness.
func (s *Store) ObserveResponseUsage(ctx context.Context, localID, status string, evidence usage.AttemptUsage, now time.Time) (ingest.Persisted, error) {
	id, err := parseLocal(localID)
	if err != nil {
		return ingest.Persisted{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ingest.Persisted{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var current string
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT state,metadata->'pending_usage' FROM olp.provider_resources
	WHERE id=$1 AND kind IN ('response','strict_response') AND state<>$2 FOR UPDATE`, id, StateDeleted).Scan(&current, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return ingest.Persisted{}, ErrNotFound
	}
	if err != nil {
		return ingest.Persisted{}, err
	}
	terminal := wire.ResponseStatusFinished(status)
	currentTerminal := wire.ResponseStatusFinished(current)
	if currentTerminal || status == "" {
		// A later GET or replayed stream cannot reopen already finished work,
		// including foreground work with no deferred accounting owner. An
		// omitted native status supplies no new state or completion proof.
		status = current
	}
	result := ingest.Persisted{Outcome: ingest.PersistOutcomeDuplicate}
	settled := false
	var encoded []byte
	if len(raw) != 0 && string(raw) != "null" && terminal {
		result, err = persistPendingUsage(ctx, tx, raw, evidence, now, true)
		if err != nil {
			return ingest.Persisted{}, err
		}
		settled = true
	} else if len(raw) != 0 && string(raw) != "null" && !currentTerminal && evidence.Evidence.Observed() {
		pending, err := usage.DecodePendingGeneration(raw)
		if err != nil {
			return ingest.Persisted{}, err
		}
		observed, err := pending.Observe(evidence, usage.GenerationPending)
		if err != nil {
			return ingest.Persisted{}, err
		}
		encoded, err = observed.Encode()
		if err != nil {
			return ingest.Persisted{}, err
		}
	}
	if settled || encoded != nil || status != current {
		if _, err = tx.Exec(ctx, `UPDATE olp.provider_resources SET state=$2,
 metadata=CASE WHEN $3 THEN metadata-'pending_usage'
 WHEN $4::jsonb IS NOT NULL THEN jsonb_set(metadata,'{pending_usage}',$4::jsonb)
 ELSE metadata END, updated_at=now() WHERE id=$1`, id, status, settled, encoded); err != nil {
			return ingest.Persisted{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return ingest.Persisted{}, err
	}
	return result, nil
}

// The template contains only accounting metadata. Every owner removes it in
// the same transaction as this persistence, so retries and competing observers
// cannot duplicate the original generation's usage.
func persistPendingUsage(ctx context.Context, tx pgx.Tx, raw []byte, evidence usage.AttemptUsage, now time.Time, terminal bool) (ingest.Persisted, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return ingest.Persisted{Outcome: ingest.PersistOutcomeDuplicate}, nil
	}
	pending, err := usage.DecodePendingGeneration(raw)
	if err != nil {
		return ingest.Persisted{}, err
	}
	state := usage.GenerationUnknown
	if terminal {
		state = usage.GenerationTerminal
	}
	observed, err := pending.Observe(evidence, state)
	if err != nil {
		return ingest.Persisted{}, err
	}
	accepted, err := observed.AccountingEvent(now, state)
	if err != nil {
		return ingest.Persisted{}, err
	}
	return ingest.PersistEventTx(ctx, tx, accepted)
}
