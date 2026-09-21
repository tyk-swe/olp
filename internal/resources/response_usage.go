package resources

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/tyk-swe/olp/internal/usage"
)

// ReconcileResponseUsage settles a background generation under its original
// request identity. The resource lock serializes polls across all replicas;
// accounting and removal of the pending event commit together.
func (s *Store) ReconcileResponseUsage(ctx context.Context, localID string, evidence usage.AttemptUsage, now time.Time) (usage.Persisted, error) {
	id, err := parseLocal(localID)
	if err != nil {
		return usage.Persisted{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return usage.Persisted{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT metadata->'pending_usage' FROM olp_go.provider_resources
 WHERE id=$1 AND kind='response' FOR UPDATE`, id).Scan(&raw)
	if err != nil {
		return usage.Persisted{}, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return usage.Persisted{Outcome: usage.PersistOutcomeDuplicate}, nil
	}
	var ev usage.Event
	if err = json.Unmarshal(raw, &ev); err != nil {
		return usage.Persisted{}, err
	}
	if len(ev.Attempts) == 0 {
		return usage.Persisted{}, errors.New("background response has no generation attempt")
	}
	ev.RequestCompletedAt, ev.ObservedAt = now, now
	ev.LatencyMS = max(now.Sub(ev.RequestStartedAt).Milliseconds(), 0)
	ev.InputTokens, ev.OutputTokens, ev.CachedInputTokens = evidence.InputTokens, evidence.OutputTokens, evidence.CachedInputTokens
	ev.UsageComplete = evidence.Complete
	final := &ev.Attempts[len(ev.Attempts)-1]
	final.Usage = &evidence
	final.CompletedAt = now
	final.LatencyMS = max(now.Sub(final.StartedAt).Milliseconds(), 0)
	payload, err := usage.Encode(&ev)
	if err != nil {
		return usage.Persisted{}, err
	}
	result, err := usage.PersistEventTx(ctx, tx, &ev, payload)
	if err != nil {
		return usage.Persisted{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE olp_go.provider_resources SET metadata=metadata-'pending_usage', updated_at=now() WHERE id=$1`, id); err != nil {
		return usage.Persisted{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return usage.Persisted{}, err
	}
	return result, nil
}
