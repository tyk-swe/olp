package resources

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

func geminiResultScope(resource *Resource, fileName string) string {
	credential := ""
	if resource.CredentialID != nil {
		credential = *resource.CredentialID
	}
	return strings.Join([]string{"gemini-result", resource.APIKeyID, resource.ProviderID, resource.SlotID, credential, fileName}, ":")
}

func lockGeminiResult(ctx context.Context, tx pgx.Tx, resource *Resource, fileName string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, geminiResultScope(resource, fileName))
	return err
}

func (s *Store) EnsureGeminiResultViews(ctx context.Context, fileName string, output, errorView *Resource) (*Resource, *Resource, error) {
	if output == nil || errorView == nil || output.Kind != KindFile || errorView.Kind != KindFile ||
		output.UpstreamID != fileName+"#output" || errorView.UpstreamID != fileName+"#error" ||
		geminiResultScope(output, fileName) != geminiResultScope(errorView, fileName) {
		return nil, nil, ErrContract
	}
	views, err := withTxValue(ctx, s.pool, func(tx pgx.Tx) ([2]*Resource, error) {
		if err := lockGeminiResult(ctx, tx, output, fileName); err != nil {
			return [2]*Resource{}, err
		}
		var out [2]*Resource
		for i, view := range [2]*Resource{output, errorView} {
			mapped, readErr := s.getByUpstream(ctx, tx, view.NativeIdentity(), newestUpstream())
			if errors.Is(readErr, ErrNotFound) {
				mapped, readErr = s.PutTx(ctx, tx, view)
			}
			if readErr != nil {
				return [2]*Resource{}, readErr
			}
			out[i] = mapped
		}
		return out, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return views[0], views[1], nil
}

func (s *Store) DetachGeminiResultView(ctx context.Context, resource *Resource, fileName string) (bool, error) {
	if resource == nil {
		return false, ErrContract
	}
	return withTxValue(ctx, s.pool, func(tx pgx.Tx) (bool, error) {
		if err := lockGeminiResult(ctx, tx, resource, fileName); err != nil {
			return false, err
		}
		var state string
		err := tx.QueryRow(ctx, `SELECT state FROM olp.provider_resources WHERE id=$1 AND kind='file' FOR UPDATE`, resource.UUID).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrNotFound
		}
		if err != nil {
			return false, err
		}
		if state == StateDeleted {
			return false, ErrNotFound
		}
		var sibling bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.provider_resources
			WHERE kind='file' AND api_key_id=$1 AND provider_id=$2 AND slot_id=$3
			AND credential_id IS NOT DISTINCT FROM $4::uuid AND id<>$5 AND state<>'deleted'
			AND (expires_at IS NULL OR expires_at>now()) AND upstream_id=ANY($6::text[]))`,
			resource.APIKeyID, resource.ProviderID, resource.SlotID, resource.CredentialID, resource.UUID,
			[]string{fileName + "#output", fileName + "#error"}).Scan(&sibling)
		if err != nil {
			return false, err
		}
		if !sibling {
			return false, nil
		}
		if _, err := tx.Exec(ctx, `UPDATE olp.provider_resources SET state=$2,updated_at=now() WHERE id=$1`, resource.UUID, StateDeleted); err != nil {
			return false, err
		}
		return true, nil
	})
}
