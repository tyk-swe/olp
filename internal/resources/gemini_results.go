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

// EnsureGeminiResultViews creates both projections before either can be listed
// or deleted. Existing tombstones stay authoritative when a batch is reread.
func (s *Store) EnsureGeminiResultViews(ctx context.Context, fileName string, output, errorView *Resource) (*Resource, *Resource, error) {
	if output == nil || errorView == nil || output.Kind != KindFile || errorView.Kind != KindFile ||
		output.UpstreamID != fileName+"#output" || errorView.UpstreamID != fileName+"#error" ||
		geminiResultScope(output, fileName) != geminiResultScope(errorView, fileName) {
		return nil, nil, ErrContract
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)
	if err = lockGeminiResult(ctx, tx, output, fileName); err != nil {
		return nil, nil, err
	}
	views := []*Resource{output, errorView}
	for i, view := range views {
		mapped, readErr := s.getByUpstream(ctx, tx, view.NativeIdentity(), true)
		if errors.Is(readErr, ErrNotFound) {
			mapped, readErr = s.PutTx(ctx, tx, view)
		}
		if readErr != nil {
			return nil, nil, readErr
		}
		views[i] = mapped
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return views[0], views[1], nil
}

// DetachGeminiResultView removes a single projection while its sibling owns
// the shared object. The last projection remains retryable during settlement;
// once no reference needs the object, the caller can delete it upstream.
func (s *Store) DetachGeminiResultView(ctx context.Context, resource *Resource, fileName string) (detached, pending bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback(ctx)
	if err = lockGeminiResult(ctx, tx, resource, fileName); err != nil {
		return false, false, err
	}
	var state string
	if err = tx.QueryRow(ctx, `SELECT state FROM olp.provider_resources WHERE id=$1 AND kind='file' FOR UPDATE`, resource.UUID).Scan(&state); err != nil {
		return false, false, err
	}
	if state == StateDeleted {
		return false, false, ErrNotFound
	}
	var sibling bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.provider_resources
		WHERE kind='file' AND api_key_id=$1 AND provider_id=$2 AND slot_id=$3
		AND credential_id IS NOT DISTINCT FROM $4::uuid AND id<>$5 AND state<>'deleted'
		AND (expires_at IS NULL OR expires_at>now()) AND upstream_id=ANY($6::text[]))`,
		resource.APIKeyID, resource.ProviderID, resource.SlotID, resource.CredentialID, resource.UUID,
		[]string{fileName + "#output", fileName + "#error"}).Scan(&sibling)
	if err != nil {
		return false, false, err
	}
	if sibling {
		if _, err = tx.Exec(ctx, `UPDATE olp.provider_resources SET state=$2,updated_at=now() WHERE id=$1`, resource.UUID, StateDeleted); err != nil {
			return false, false, err
		}
		return true, false, tx.Commit(ctx)
	}
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.batch_settlements bs
		JOIN olp.provider_resources r ON r.id=bs.resource_id
		WHERE bs.adapter='gemini_managed' AND bs.closed_at IS NULL
		AND bs.api_key_id=$1 AND bs.provider_id=$2 AND bs.slot_id=$3
		AND bs.credential_id IS NOT DISTINCT FROM $4::uuid
		AND ((r.metadata->'provider_batch')#>>'{metadata,output,responsesFile}'=$5
		 OR (r.metadata->'provider_batch')#>>'{metadata,response,responsesFile}'=$5
		 OR (r.metadata->'provider_batch')#>>'{output,responsesFile}'=$5
		 OR (r.metadata->'provider_batch')#>>'{response,responsesFile}'=$5))`,
		resource.APIKeyID, resource.ProviderID, resource.SlotID, resource.CredentialID, fileName).Scan(&pending)
	return false, pending, err
}
