package resources

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/usage"
)

// CompleteGenerationHTTP finalizes the original exchange under the same lock
// used by generation observers and cleanup. Financial settlement may come
// first; in that case only existing usage-owned HTTP history is updated.
func (s *Store) CompleteGenerationHTTP(ctx context.Context, localID string, completion usage.HTTPCompletion) error {
	id, err := parseLocal(localID)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var kind, owner, provider string
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT kind,api_key_id::text,provider_id::text,metadata->'pending_usage' FROM olp.provider_resources
	 WHERE id=$1 AND kind IN ('response','strict_response','interaction') FOR UPDATE`, id).Scan(&kind, &owner, &provider, &raw)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil && (localID != LocalID(kind, id) || owner != completion.APIKeyID || provider != completion.Attempt.ProviderID) {
		return fmt.Errorf("%w: original HTTP completion has a different resource owner", ErrContract)
	}
	if err == nil && len(raw) != 0 && string(raw) != "null" {
		event, err := decodePendingUsage(raw)
		if err != nil {
			return err
		}
		if err = usage.CompletePendingHTTP(event, completion); err != nil {
			return err
		}
		encoded, err := usage.Encode(event)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE olp.provider_resources SET metadata=jsonb_set(metadata,'{pending_usage}',$2::jsonb),updated_at=now() WHERE id=$1`, id, encoded); err != nil {
			return err
		}
	} else if err = usage.CompleteRequestHTTPTx(ctx, tx, completion); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
