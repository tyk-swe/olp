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

type Querier interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

type Store struct{ db Querier }

func NewStore(db Querier) *Store { return &Store{db: db} }

const pollGateSeconds = 5

const columns = `j.id::text, j.upstream_job_id, j.api_key_id::text, k.project_id::text, j.provider_id::text,
	p.name, j.provider_model, j.route_slug, j.operation, j.surface, j.state, j.lifecycle_state,
	j.progress_percent::real, j.expires_at, j.error_class, j.completed_at, j.last_polled_at,
	j.reconciliation_error, j.deleted_at, j.runtime_generation_id::text, j.provider_revision_id::text,
	j.credential_version_id::text, j.slot_id::text, j.reconciliation_claim_id::text,
	j.reconciliation_attempts, j.next_reconciliation_at, j.last_reconciliation_at, j.etag::text,
	j.created_at, j.updated_at, j.strict_contract`

func from(source string) string {
	return "SELECT " + columns + " FROM " + source +
		" j JOIN olp.providers p ON p.id = j.provider_id JOIN olp.api_keys k ON k.id = j.api_key_id"
}

func returning(mutation string) string {
	return "WITH changed AS (" + mutation + " RETURNING *) " + from("changed")
}

var selectJobs = from("olp.media_jobs")

const ownedCredential = `NOT EXISTS (SELECT 1 FROM olp.provider_revisions revision
	WHERE revision.id = media_jobs.provider_revision_id AND revision.configuration->>'credential_source' = 'caller')`

const claimSet = `reconciliation_claim_id = gen_random_uuid(),
	reconciliation_claimed_until = $1 + interval '2 minutes',
	last_reconciliation_at = $1,
	next_reconciliation_at = $1 + interval '2 minutes',
	reconciliation_attempts = reconciliation_attempts + 1,
	etag = gen_random_uuid()`

func scan(row pgx.Row) (Record, error) {
	var j Record
	err := row.Scan(&j.ID, &j.UpstreamJobID, &j.APIKeyID, &j.ProjectID, &j.ProviderID,
		&j.ProviderName, &j.UpstreamModel, &j.RouteSlug, &j.Operation, &j.Surface, &j.State, &j.Lifecycle,
		&j.ProgressPercent, &j.ExpiresAt, &j.ErrorClass, &j.CompletedAt, &j.LastPolledAt,
		&j.ReconciliationError, &j.DeletedAt, &j.RuntimeGenerationID, &j.ProviderRevisionID,
		&j.CredentialVersionID, &j.SlotID, &j.ReconciliationClaimID,
		&j.ReconciliationAttempts, &j.NextReconciliationAt, &j.LastReconciliationAt, &j.ETag,
		&j.CreatedAt, &j.UpdatedAt, &j.StrictContract)
	return j, err
}

func scanClaimed(row pgx.Row) (Claimed, error) {
	record, err := scan(row)
	if err != nil {
		return Claimed{}, err
	}
	if record.ReconciliationClaimID == nil {
		return Claimed{}, errors.New("claimed media job has no claim")
	}
	return Claimed{Record: record, claim: *record.ReconciliationClaimID}, nil
}

func storeError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	return &Error{Kind: KindDatabase, Message: "media job store unavailable", Err: err}
}

func text[T ~string](value *T) *string {
	if value == nil {
		return nil
	}
	converted := string(*value)
	return &converted
}

func (s *Store) Get(ctx context.Context, id string) (Record, error) {
	record, err := scan(s.db.QueryRow(ctx, selectJobs+" WHERE j.id = $1", id))
	if err != nil {
		return Record{}, storeError(err)
	}
	return record, nil
}

const listFilter = ` WHERE ($1::uuid[] IS NULL OR k.project_id = ANY($1::uuid[]))
	AND ($2::text[] IS NULL OR j.route_slug = ANY($2::text[]))
	AND ($3::uuid IS NULL OR j.api_key_id = $3::uuid)
	AND ($4::uuid IS NULL OR j.provider_id = $4::uuid)
	AND ($5::text IS NULL OR j.operation = $5::text)
	AND ($6::text IS NULL OR j.surface = $6::text)
	AND ($7::text IS NULL OR j.state = $7::text)
	AND ($8::text IS NULL OR j.lifecycle_state = $8::text)
	AND ($9::timestamptz IS NULL OR j.created_at >= $9::timestamptz)
	AND ($10::timestamptz IS NULL OR j.created_at < $10::timestamptz)`

var (
	listAscending = selectJobs + listFilter +
		` AND ($11::timestamptz IS NULL OR (j.created_at, j.id) > ($11::timestamptz, $12::uuid))
		ORDER BY j.created_at, j.id LIMIT $13`
	listDescending = selectJobs + listFilter +
		` AND ($11::timestamptz IS NULL OR (j.created_at, j.id) < ($11::timestamptz, $12::uuid))
		ORDER BY j.created_at DESC, j.id DESC LIMIT $13`
)

func (s *Store) List(ctx context.Context, q Query) (Page, error) {
	sql := listDescending
	if q.Order == media.OrderAscending {
		sql = listAscending
	}
	var afterAt *time.Time
	var afterID *string
	if q.After != nil {
		afterAt, afterID = &q.After.CreatedAt, &q.After.ID
	}
	rows, err := s.db.Query(ctx, sql, q.Projects.param(), q.Routes.param(), q.APIKeyID, q.ProviderID,
		text(q.Operation), q.Surface, text(q.State), text(q.Lifecycle), q.CreatedAfter, q.CreatedBefore,
		afterAt, afterID, q.Limit+1)
	if err != nil {
		return Page{}, storeError(err)
	}
	records, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Record, error) { return scan(row) })
	if err != nil {
		return Page{}, storeError(err)
	}
	if len(records) > q.Limit {
		return Page{Items: records[:q.Limit], More: true}, nil
	}
	return Page{Items: records}, nil
}

func (s *Store) Position(ctx context.Context, id *string, q Query) (*Position, error) {
	if id == nil {
		return nil, nil
	}
	var position Position
	err := s.db.QueryRow(ctx, `SELECT created_at, id::text FROM olp.media_jobs
		WHERE id = $1
		  AND ($2::uuid IS NULL OR api_key_id = $2::uuid)
		  AND ($3::text[] IS NULL OR route_slug = ANY($3::text[]))
		  AND ($4::text IS NULL OR operation = $4::text)
		  AND ($5::text IS NULL OR surface = $5::text)`,
		*id, q.APIKeyID, q.Routes.param(), text(q.Operation), q.Surface).Scan(&position.CreatedAt, &position.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, invalid("video cursor is invalid")
	}
	if err != nil {
		return nil, storeError(err)
	}
	return &position, nil
}

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
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT id FROM olp.providers WHERE id = $1 FOR SHARE", input.ProviderID); err != nil {
		return Record{}, storeError(err)
	}
	var snapshot []byte
	if err = tx.QueryRow(ctx, "SELECT snapshot FROM olp.runtime_releases WHERE id = $1", input.RuntimeGenerationID).Scan(&snapshot); err != nil {
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

func (s *Store) MarkAmbiguous(ctx context.Context, id string, fence Fence, reason string) (Record, error) {
	return s.mark(ctx, id, fence, CreateAmbiguous, nil, reason, Creating, CreateAmbiguous)
}

func (s *Store) MarkCleanup(ctx context.Context, id string, fence Fence, upstream, reason string) (Record, error) {
	if strings.TrimSpace(upstream) == "" {
		return Record{}, invalid("upstream job ID cannot be empty")
	}
	return s.mark(ctx, id, fence, CleanupPending, &upstream, reason, Creating, CreateAmbiguous, CleanupPending)
}

func (s *Store) settled(ctx context.Context, id string, fence Fence, accept func(Record) bool) (Record, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return Record{}, err
	}
	owned := fence.claim == nil || current.ReconciliationClaimID != nil && *current.ReconciliationClaimID == *fence.claim
	if owned && accept(current) {
		return current, nil
	}
	if fence.claim != nil {
		return Record{}, errClaimLost
	}
	return Record{}, errChanged
}

func (s *Store) BeginDeletion(ctx context.Context, id string, fence Fence) (Record, error) {
	record, err := scan(s.db.QueryRow(ctx, returning(`UPDATE olp.media_jobs SET lifecycle_state = 'delete_pending',
			reconciliation_error = NULL, next_reconciliation_at = now(), etag = gen_random_uuid()
		WHERE id = $1 AND lifecycle_state = 'active' AND ($2::uuid IS NULL OR reconciliation_claim_id = $2::uuid)`),
		id, fence.claim))
	if errors.Is(err, pgx.ErrNoRows) {
		return s.settled(ctx, id, fence, func(current Record) bool {
			return current.Lifecycle == DeletePending || current.Lifecycle == Deleted
		})
	}
	if err != nil {
		return Record{}, storeError(err)
	}
	return record, nil
}

func (s *Store) Refresh(ctx context.Context, id string, fence Fence, update Update) (Record, error) {
	record, err := scan(s.db.QueryRow(ctx, returning(`UPDATE olp.media_jobs SET
			state = $2::text,
			progress_percent = CASE
				WHEN $3::real IS NULL THEN progress_percent
				WHEN progress_percent IS NULL THEN $3::real::numeric
				ELSE GREATEST(progress_percent, $3::real::numeric)
			END,
			expires_at = COALESCE($4, expires_at),
			error_class = COALESCE($5, error_class),
			last_polled_at = $6,
			completed_at = CASE WHEN $2::text IN ('succeeded','failed','cancelled')
				THEN COALESCE(completed_at, $6) ELSE completed_at END,
			next_reconciliation_at = GREATEST(next_reconciliation_at, $6 + $7::int * interval '1 second'),
			etag = gen_random_uuid()
		WHERE id = $1 AND lifecycle_state = 'active'
		  AND ($8::uuid IS NULL OR reconciliation_claim_id = $8::uuid)
		  AND (last_polled_at IS NULL OR last_polled_at <= $6)
		  AND (state = 'queued' OR (state = 'running' AND $2::text <> 'queued') OR state = $2::text)`),
		id, string(update.state), update.progress, update.expires, update.failure, update.polled,
		pollGateSeconds, fence.claim))
	if errors.Is(err, pgx.ErrNoRows) {
		return s.settled(ctx, id, fence, func(current Record) bool {
			return fence.claim == nil || current.Lifecycle == Active
		})
	}
	if err != nil {
		return Record{}, storeError(err)
	}
	return record, nil
}

func (s *Store) Finalize(ctx context.Context, id string, fence Fence) (Record, error) {
	record, err := scan(s.db.QueryRow(ctx, returning(`UPDATE olp.media_jobs
		SET lifecycle_state = 'deleted', deleted_at = COALESCE(deleted_at, now()),
			reconciliation_error = NULL, etag = gen_random_uuid()
		WHERE id = $1 AND ($2::uuid IS NULL OR reconciliation_claim_id = $2::uuid)
		  AND lifecycle_state IN ('creating','create_ambiguous','create_cleanup_pending','delete_pending')`),
		id, fence.claim))
	if errors.Is(err, pgx.ErrNoRows) {
		return s.settled(ctx, id, fence, func(current Record) bool {
			return fence.claim == nil || current.Lifecycle == Deleted
		})
	}
	if err != nil {
		return Record{}, storeError(err)
	}
	return record, nil
}

func (s *Store) Claim(ctx context.Context, now time.Time, limit int) ([]Claimed, error) {
	rows, err := s.db.Query(ctx, `WITH candidates AS (
			SELECT id FROM olp.media_jobs
			WHERE lifecycle_state <> 'deleted'
			  AND `+ownedCredential+`
			  AND next_reconciliation_at <= $1
			  AND (reconciliation_claimed_until IS NULL OR reconciliation_claimed_until <= $1)
			  AND (
			    lifecycle_state IN ('creating','create_ambiguous','create_cleanup_pending','delete_pending')
			    OR (lifecycle_state = 'active'
			        AND upstream_job_id IS NOT NULL
			        AND (
			          (state IN ('queued','running')
			           AND (last_polled_at IS NULL OR last_polled_at <= $1 - $3::int * interval '1 second'))
			          OR expires_at <= $1
			          OR created_at <= $1 - interval '30 days')))
			ORDER BY CASE WHEN lifecycle_state = 'active' THEN 1 ELSE 0 END,
				next_reconciliation_at, created_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT $2
		), claimed AS (
			UPDATE olp.media_jobs SET `+claimSet+`
			FROM candidates c WHERE media_jobs.id = c.id
			RETURNING media_jobs.*
		) `+from("claimed")+` ORDER BY j.created_at, j.id`,
		now, limit, pollGateSeconds)
	if err != nil {
		return nil, storeError(err)
	}
	claimed, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Claimed, error) { return scanClaimed(row) })
	if err != nil {
		return nil, storeError(err)
	}
	return claimed, nil
}

func (s *Store) ClaimOne(ctx context.Context, id string, now time.Time) (Claimed, bool, error) {
	claimed, err := scanClaimed(s.db.QueryRow(ctx, returning(`UPDATE olp.media_jobs SET `+claimSet+`
		WHERE id = $2 AND lifecycle_state <> 'deleted'
		  AND (reconciliation_claimed_until IS NULL OR reconciliation_claimed_until <= $1)
		  AND `+ownedCredential), now, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Claimed{}, false, nil
	}
	if err != nil {
		return Claimed{}, false, storeError(err)
	}
	return claimed, true, nil
}

func (s *Store) Extend(ctx context.Context, job Claimed, until time.Time) (bool, error) {
	tag, err := s.db.Exec(ctx, `UPDATE olp.media_jobs SET
			reconciliation_claimed_until = GREATEST(reconciliation_claimed_until, $3),
			next_reconciliation_at = GREATEST(next_reconciliation_at, $3),
			etag = gen_random_uuid()
		WHERE id = $1 AND reconciliation_claim_id = $2`, job.ID, job.claim, until)
	if err != nil {
		return false, storeError(err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) Finish(ctx context.Context, job Claimed, next time.Time, failure *string) error {
	if failure != nil && (*failure == "" || len(*failure) > 120) {
		return invalid("reconciliation error class must contain 1-120 bytes")
	}
	tag, err := s.db.Exec(ctx, `UPDATE olp.media_jobs SET
			reconciliation_claim_id = NULL,
			reconciliation_claimed_until = NULL,
			next_reconciliation_at = $3,
			reconciliation_error = $4,
			reconciliation_attempts = CASE WHEN $4::text IS NULL THEN 0 ELSE reconciliation_attempts END,
			etag = gen_random_uuid()
		WHERE id = $1 AND reconciliation_claim_id = $2`, job.ID, job.claim, next, failure)
	if err != nil {
		return storeError(err)
	}
	if tag.RowsAffected() != 1 {
		return s.refusal(ctx, job.ID, Unfenced)
	}
	return nil
}

func (s *Store) Summary(ctx context.Context, now time.Time) (Summary, error) {
	var summary Summary
	err := s.db.QueryRow(ctx, `SELECT
			COUNT(*) FILTER (WHERE lifecycle_state NOT IN ('active','deleted'))::bigint,
			COUNT(*) FILTER (
				WHERE (lifecycle_state = 'creating' AND next_reconciliation_at < $1::timestamptz)
				   OR (lifecycle_state NOT IN ('creating','active','deleted')
				       AND next_reconciliation_at < $1::timestamptz - interval '1 minute')
				   OR (lifecycle_state = 'active'
				       AND state IN ('queued','running')
				       AND next_reconciliation_at < $1::timestamptz - interval '1 minute'
				       AND (last_polled_at IS NULL
				            OR last_polled_at <= $1::timestamptz - $2::int * interval '1 second')))::bigint,
			COUNT(*) FILTER (WHERE reconciliation_error IS NOT NULL)::bigint,
			MIN(created_at) FILTER (WHERE lifecycle_state NOT IN ('active','deleted'))
		FROM olp.media_jobs WHERE lifecycle_state <> 'deleted' AND `+ownedCredential,
		now, pollGateSeconds).Scan(&summary.Pending, &summary.Stale, &summary.Failed, &summary.OldestPendingAt)
	if err != nil {
		return Summary{}, storeError(err)
	}
	return summary, nil
}

func (s *Store) RouteAuthority(ctx context.Context, slug string) (*string, routing.RouteFidelity, error) {
	var project *string
	var fidelity []byte
	if err := s.db.QueryRow(ctx, `SELECT r.project_id::text, v.fidelity FROM olp.routes r
		JOIN olp.route_revisions v ON v.id = r.latest_revision_id WHERE r.slug = $1`, slug).Scan(&project, &fidelity); err != nil {
		return nil, routing.RouteFidelity{}, storeError(err)
	}
	current, err := routing.DecodeFidelity(fidelity)
	if err != nil {
		return nil, routing.RouteFidelity{}, err
	}
	return project, current, nil
}
