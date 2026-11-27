package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s *Store) Reserve(ctx context.Context, input Reservation) (Record, error) {
	_, idErr := uuid.Parse(input.ID)
	_, slotErr := uuid.Parse(input.SlotID)
	if idErr != nil || slotErr != nil || input.UpstreamModel == "" || input.RouteSlug == "" {
		return Record{}, invalid("reservation ID, slot, provider model, and route are required")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Record{}, storeError(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err = tx.Exec(ctx, "SELECT id FROM olp.providers WHERE id = $1 FOR SHARE", input.ProviderID); err != nil {
		return Record{}, storeError(err)
	}
	snapshot, err := releaseSnapshot(ctx, tx, input.RuntimeGenerationID)
	if err != nil {
		return Record{}, storeError(err)
	}
	pin, err := routing.DecodeReleasePin(snapshot, routing.ReleasePinIdentity{ProviderID: input.ProviderID, ProviderRevisionID: input.ProviderRevisionID, RouteSlug: input.RouteSlug, Model: input.UpstreamModel, SlotID: input.SlotID, CredentialID: input.CredentialVersionID})
	if err != nil {
		return Record{}, invalid("the selected historical serving identity is unavailable")
	}
	compatible, err := compatibleConfiguration(ctx, tx, input.ProviderRevisionID, input.ProviderID)
	if err != nil {
		return Record{}, storeError(err)
	}
	unsupported := invalid("the pinned provider authority is unavailable or incompatible with current video support")
	if !compatible {
		return Record{}, unsupported
	}
	var staleAfter *time.Time
	if !input.StaleAfter.IsZero() {
		staleAfter = &input.StaleAfter
	}
	record, err := scan(tx.QueryRow(ctx, `WITH compatible AS (
			SELECT 1 FROM olp.providers provider
			JOIN olp.provider_revisions current ON current.id = provider.active_revision_id
			WHERE provider.id = $3::uuid AND provider.state <> 'disabled'
			  AND (SELECT credential.version FROM olp.provider_credentials credential
			       WHERE credential.id = $13::uuid) IS NOT DISTINCT FROM current.credential_version
			  AND EXISTS (
			    SELECT 1 FROM jsonb_array_elements(current.models) model
			    WHERE model->>'upstream_model' = $4
			      AND NOT EXISTS (
			        SELECT 1 FROM (VALUES ('video_get'), ('video_content'), ('video_delete')) AS required(operation)
			        WHERE NOT EXISTS (
			          SELECT 1 FROM jsonb_array_elements(model->'capabilities') c
			          WHERE c->>'operation' = required.operation
			            AND c->>'surface' = $6
			            AND c->>'mode' = 'unary'
			            AND c->>'source' = 'certified')))
		), changed AS (
			INSERT INTO olp.media_jobs (
				id, api_key_id, provider_id, provider_model, route_slug, operation, surface,
				state, lifecycle_state, runtime_generation_id, provider_revision_id,
				credential_version_id, etag, slot_id, strict_contract, next_reconciliation_at)
			SELECT $1::uuid, $2::uuid, $3::uuid, $4, $5, $14, $6, 'queued', 'creating', $7::uuid, $12::uuid,
			       $8::uuid, gen_random_uuid(), $9::uuid, $10::boolean,
			       COALESCE($11::timestamptz, now() + interval '5 minutes')
			FROM compatible
			RETURNING *)
		`+from("changed"),
		input.ID, input.APIKeyID, input.ProviderID, input.UpstreamModel, input.RouteSlug, input.Surface,
		input.RuntimeGenerationID, input.CredentialVersionID, input.SlotID, input.StrictContract, staleAfter,
		input.ProviderRevisionID, pin.Provider.ActiveCredential, media.OpVideoCreate))
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, unsupported
	}
	if err != nil {
		return Record{}, storeError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return Record{}, storeError(err)
	}
	return record, nil
}

func compatibleConfiguration(ctx context.Context, q pgx.Tx, revisionID, providerID string) (bool, error) {
	var pinned, current []byte
	err := q.QueryRow(ctx, `SELECT pinned.configuration, current.configuration
		FROM olp.providers provider
		JOIN olp.provider_revisions pinned ON pinned.id = $1::uuid AND pinned.provider_id = provider.id
		JOIN olp.provider_revisions current ON current.id = provider.active_revision_id
		WHERE provider.id = $2::uuid`, revisionID, providerID).Scan(&pinned, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	left, err := reservationConfiguration(pinned)
	if err != nil {
		return false, err
	}
	right, err := reservationConfiguration(current)
	if err != nil {
		return false, err
	}
	return bytes.Equal(left, right), nil
}

func reservationConfiguration(source []byte) ([]byte, error) {
	var config routing.Configuration
	if err := json.Unmarshal(source, &config); err != nil {
		return nil, err
	}
	config.Options.Limits = nil
	return json.Marshal(config)
}

func (s *Store) Attach(ctx context.Context, id, upstream string, update Update) (Record, error) {
	if upstream == "" || upstream != strings.TrimSpace(upstream) {
		return Record{}, invalid("upstream job ID cannot be empty")
	}
	record, err := scan(s.db.QueryRow(ctx, returning(`UPDATE olp.media_jobs SET
			upstream_job_id = $2, state = $3, lifecycle_state = 'active',
			progress_percent = $4::real::numeric, expires_at = $5, error_class = $6, last_polled_at = $7,
			reconciliation_error = NULL, next_reconciliation_at = now(), etag = gen_random_uuid()
		WHERE id = $1 AND lifecycle_state = 'creating'`),
		id, upstream, string(update.state), update.progress, update.expires, update.failure, update.polled))
	if err == nil {
		return record, nil
	}
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.ConstraintName == "media_jobs_upstream_unique_idx" {
		return Record{}, &Error{Kind: KindConflict, Message: "upstream job is already attached to another media job"}
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Record{}, storeError(err)
	}
	current, err := s.Get(ctx, id)
	if err != nil {
		return Record{}, err
	}
	if current.Lifecycle == Active && current.UpstreamJobID != nil && *current.UpstreamJobID == upstream {
		return current, nil
	}
	return Record{}, errChanged
}

func (s *Store) refusal(ctx context.Context, id string, fence Fence) error {
	var exists bool
	if err := s.db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.media_jobs WHERE id = $1)", id).Scan(&exists); err != nil {
		return storeError(err)
	}
	switch {
	case !exists:
		return errNotFound
	case fence.claim != nil:
		return errClaimLost
	default:
		return errChanged
	}
}

func (s *Store) mark(ctx context.Context, id string, fence Fence, lifecycle Lifecycle, upstream *string, reason string, from ...Lifecycle) (Record, error) {
	record, err := scan(s.db.QueryRow(ctx, returning(`UPDATE olp.media_jobs SET lifecycle_state = $2,
			upstream_job_id = COALESCE($3, upstream_job_id),
			reconciliation_error = $4, next_reconciliation_at = now(), etag = gen_random_uuid()
		WHERE id = $1 AND lifecycle_state = ANY($5::text[]) AND ($6::uuid IS NULL OR reconciliation_claim_id = $6::uuid)`),
		id, string(lifecycle), upstream, reason, from, fence.claim))
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, s.refusal(ctx, id, fence)
	}
	if err != nil {
		return Record{}, storeError(err)
	}
	return record, nil
}
