package resources

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/ingest"
)

func (s *Store) CompleteGenerationHTTP(ctx context.Context, localID string, completion usage.HTTPCompletion) error {
	parsedKind, id, err := ParseLocalID(localID)
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		var kind, owner, provider string
		var raw []byte
		queryErr := tx.QueryRow(ctx, `SELECT kind,api_key_id::text,provider_id::text,metadata->'pending_usage' FROM olp.provider_resources
	 WHERE id=$1 AND kind IN ('response','strict_response','interaction') FOR UPDATE`, id).Scan(&kind, &owner, &provider, &raw)
		if queryErr != nil && !errors.Is(queryErr, pgx.ErrNoRows) {
			return queryErr
		}
		if queryErr == nil && (parsedKind != kind || owner != completion.APIKeyID || provider != completion.Attempt.ProviderID) {
			return fmt.Errorf("%w: original HTTP completion has a different resource owner", ErrContract)
		}
		if queryErr == nil && hasPendingUsage(raw) {
			pending, err := usage.DecodePendingGeneration(raw)
			if err != nil {
				return err
			}
			completed, err := pending.CompleteHTTP(completion)
			if err != nil {
				return err
			}
			encoded, err := completed.Encode()
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE olp.provider_resources SET metadata=jsonb_set(metadata,'{pending_usage}',$2::jsonb),updated_at=now() WHERE id=$1`, id, encoded)
			return err
		}
		return ingest.CompleteRequestHTTPTx(ctx, tx, completion)
	})
}
