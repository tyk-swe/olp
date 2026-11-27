package resources

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/ingest"
)

var ErrPendingUsage = errors.New("cost owner has unsettled background generation usage")

type pendingUsageUpdate struct {
	id              uuid.UUID
	currentState    string
	nextState       string
	raw             []byte
	terminal        bool
	currentTerminal bool
	evidence        usage.AttemptUsage
	now             time.Time
}

func hasPendingUsage(raw []byte) bool {
	return len(raw) != 0 && string(raw) != "null"
}

func observePendingUsage(raw []byte, evidence usage.AttemptUsage) ([]byte, error) {
	pending, err := usage.DecodePendingGeneration(raw)
	if err != nil {
		return nil, err
	}
	observed, err := pending.Observe(evidence, usage.GenerationPending)
	if err != nil {
		return nil, err
	}
	return observed.Encode()
}

func persistPendingUsage(ctx context.Context, tx pgx.Tx, raw []byte, evidence usage.AttemptUsage, now time.Time, terminal bool) (ingest.Persisted, error) {
	if !hasPendingUsage(raw) {
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

func applyPendingUsageTx(ctx context.Context, tx pgx.Tx, update pendingUsageUpdate) (ingest.Persisted, error) {
	result := ingest.Persisted{Outcome: ingest.PersistOutcomeDuplicate}
	settled := false
	var encoded []byte
	var err error
	if hasPendingUsage(update.raw) && update.terminal {
		result, err = persistPendingUsage(ctx, tx, update.raw, update.evidence, update.now, true)
		if err != nil {
			return ingest.Persisted{}, err
		}
		settled = true
	} else if hasPendingUsage(update.raw) && !update.currentTerminal && update.evidence.Evidence.Observed() {
		encoded, err = observePendingUsage(update.raw, update.evidence)
		if err != nil {
			return ingest.Persisted{}, err
		}
	}
	if settled || encoded != nil || update.nextState != update.currentState {
		_, err = tx.Exec(ctx, `UPDATE olp.provider_resources SET state=$2,
 metadata=CASE WHEN $3 THEN metadata-'pending_usage'
 WHEN $4::jsonb IS NOT NULL THEN jsonb_set(metadata,'{pending_usage}',$4::jsonb)
 ELSE metadata END, updated_at=now() WHERE id=$1`, update.id, update.nextState, settled, encoded)
		if err != nil {
			return ingest.Persisted{}, err
		}
	}
	return result, nil
}
