package resources

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/usage"
)

// ObserveResponseUsage retains partial observations and settles terminal usage
// under the original generation identity. The resource lock serializes polls
// across replicas; accounting and removal of its pending owner commit together.
// Native status owns termination; the protocol's evidence owns completeness.
func (s *Store) ObserveResponseUsage(ctx context.Context, localID, status string, evidence usage.AttemptUsage, now time.Time) (usage.Persisted, error) {
	id, err := parseLocal(localID)
	if err != nil {
		return usage.Persisted{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return usage.Persisted{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var current string
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT state,metadata->'pending_usage' FROM olp.provider_resources
	WHERE id=$1 AND kind IN ('response','strict_response') AND state<>$2 FOR UPDATE`, id, StateDeleted).Scan(&current, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return usage.Persisted{}, ErrNotFound
	}
	if err != nil {
		return usage.Persisted{}, err
	}
	terminal := responseStateFinished(status)
	currentTerminal := responseStateFinished(current)
	if currentTerminal || status == "" {
		// A later GET or replayed stream cannot reopen already finished work,
		// including foreground work with no deferred accounting owner. An
		// omitted native status supplies no new state or completion proof.
		status = current
	}
	result := usage.Persisted{Outcome: usage.PersistOutcomeDuplicate}
	settled := false
	var encoded []byte
	if len(raw) != 0 && string(raw) != "null" && terminal {
		result, err = persistPendingUsage(ctx, tx, raw, evidence, now, true)
		if err != nil {
			return usage.Persisted{}, err
		}
		settled = true
	} else if len(raw) != 0 && string(raw) != "null" && !currentTerminal && evidence.Observed {
		event, err := decodePendingUsage(raw)
		if err != nil {
			return usage.Persisted{}, err
		}
		if err := observePendingUsage(event, evidence, false); err != nil {
			return usage.Persisted{}, err
		}
		encoded, err = usage.Encode(event)
		if err != nil {
			return usage.Persisted{}, err
		}
	}
	if settled || encoded != nil || status != current {
		if _, err = tx.Exec(ctx, `UPDATE olp.provider_resources SET state=$2,
 metadata=CASE WHEN $3 THEN metadata-'pending_usage'
 WHEN $4::jsonb IS NOT NULL THEN jsonb_set(metadata,'{pending_usage}',$4::jsonb)
 ELSE metadata END, updated_at=now() WHERE id=$1`, id, status, settled, encoded); err != nil {
			return usage.Persisted{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return usage.Persisted{}, err
	}
	return result, nil
}

func responseStateFinished(status string) bool {
	switch status {
	case "completed", "cancelled", "failed", "incomplete":
		return true
	}
	return false
}

// The template contains only accounting metadata. Every owner removes it in
// the same transaction as this persistence, so retries and competing observers
// cannot duplicate the original generation's usage.
func persistPendingUsage(ctx context.Context, tx pgx.Tx, raw []byte, evidence usage.AttemptUsage, now time.Time, terminal bool) (usage.Persisted, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return usage.Persisted{Outcome: usage.PersistOutcomeDuplicate}, nil
	}
	ev, err := decodePendingUsage(raw)
	if err != nil {
		return usage.Persisted{}, err
	}
	if len(ev.Attempts) == 0 {
		return usage.Persisted{}, errors.New("deferred generation has no attempt")
	}
	if err := observePendingUsage(ev, evidence, terminal); err != nil {
		return usage.Persisted{}, err
	}
	// The resource outlives the submission exchange. Delayed observation must
	// not turn its captured acceptance into a days-long HTTP request or rewrite
	// that exchange's outcome. The resource state owns native terminal timing.
	ev.ObservedAt = now
	final := &ev.Attempts[len(ev.Attempts)-1]
	if final.Routing != nil && final.Routing.Interaction != nil {
		if terminal {
			final.Routing.Interaction.UpstreamState = usage.UpstreamTerminal
		} else {
			final.Routing.Interaction.UpstreamState = usage.UpstreamUnknown
		}
	}
	payload, err := usage.Encode(ev)
	if err != nil {
		return usage.Persisted{}, err
	}
	return usage.PersistEventTx(ctx, tx, ev, payload)
}

// Pending owners retain exactly the current versioned accounting envelope.
// Unknown versions or absent required evidence may not be reinterpreted before
// deleting their only owner.
func decodePendingUsage(raw []byte) (*usage.Event, error) {
	event, err := usage.Decode(raw)
	if err != nil {
		return nil, err
	}
	if _, err = usage.Validate(event); err != nil {
		return nil, err
	}
	return event, nil
}
