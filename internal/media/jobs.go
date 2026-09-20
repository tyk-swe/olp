package media

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PollGateSeconds is the interval a client poll holds the reconciler off a
// job. The claim query, the staleness summary, and the poll write all bind
// this same gate.
const PollGateSeconds = 5

// Querier is the database surface shared by pool and transaction calls.
type Querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// JobErrorKind classifies durable media-job failures.
type JobErrorKind int

const (
	JobErrorDatabase JobErrorKind = iota
	JobErrorNotFound
	JobErrorPrecondition
	JobErrorUpstreamIdentityConflict
	JobErrorInvalid
)

// JobError is a durable media-job failure.
type JobError struct {
	Kind    JobErrorKind
	Message string
	Err     error
}

func (e *JobError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Message
}

func (e *JobError) Unwrap() error { return e.Err }

// JobHTTPError maps a job failure onto a request-visible error.
func JobHTTPError(err error) *Error {
	var job *JobError
	if !errors.As(err, &job) {
		return Fail(http.StatusServiceUnavailable, "media_job_unavailable", "The media job store is unavailable.")
	}
	switch job.Kind {
	case JobErrorNotFound:
		return Fail(http.StatusNotFound, "media_job_not_found", "The media job does not exist.")
	case JobErrorPrecondition:
		return Fail(http.StatusConflict, "media_job_changed", "The media job changed; refresh and retry.")
	case JobErrorUpstreamIdentityConflict:
		return Fail(http.StatusConflict, "media_job_upstream_conflict",
			"The upstream media job identity conflicts with stored metadata.")
	case JobErrorInvalid:
		return invalidRequest(job.Message)
	default:
		return Fail(http.StatusServiceUnavailable, "media_job_unavailable", "The media job store is unavailable.")
	}
}

// Lifecycle is the durable ownership state of a media job.
type Lifecycle string

const (
	LifecycleCreating             Lifecycle = "creating"
	LifecycleActive               Lifecycle = "active"
	LifecycleCreateAmbiguous      Lifecycle = "create_ambiguous"
	LifecycleCreateCleanupPending Lifecycle = "create_cleanup_pending"
	LifecycleDeletePending        Lifecycle = "delete_pending"
	LifecycleDeleted              Lifecycle = "deleted"
)

// NeedsReconciliation reports whether the lifecycle requires worker work.
func (l Lifecycle) NeedsReconciliation() bool {
	switch l {
	case LifecycleCreating, LifecycleCreateAmbiguous, LifecycleCreateCleanupPending, LifecycleDeletePending:
		return true
	}
	return false
}

// State is the client-visible job state.
type State string

const (
	StateQueued    State = "queued"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
)

// JobRecord is one durable media job.
type JobRecord struct {
	ID                     string
	UpstreamJobID          *string
	APIKeyID               string
	ProviderID             string
	ProviderName           string
	UpstreamModel          string
	RouteSlug              string
	Operation              string
	Surface                string
	State                  State
	Lifecycle              Lifecycle
	ProgressPercent        *float32
	ContentAvailable       bool
	ExpiresAt              *time.Time
	ErrorClass             *string
	CompletedAt            *time.Time
	LastPolledAt           *time.Time
	ReconciliationError    *string
	DeletedAt              *time.Time
	RuntimeGenerationID    string
	ProviderRevisionID     string
	CredentialVersionID    *string
	SlotID                 *string
	ReconciliationClaimID  *string
	ReconciliationAttempts int
	NextReconciliationAt   time.Time
	LastReconciliationAt   *time.Time
	ETag                   string
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// Reservation is the pinned target recorded before a non-idempotent upstream
// create is attempted. No prompt or file metadata is accepted by this API.
type Reservation struct {
	ID                  string
	RuntimeGenerationID string
	ProviderRevisionID  string
	APIKeyID            string
	ProviderID          string
	UpstreamModel       string
	RouteSlug           string
	Operation           string
	Surface             string
	CredentialVersionID *string
	SlotID              *string
}

// JobUpdate carries one upstream poll result.
type JobUpdate struct {
	State            State
	ProgressPercent  *float32
	ContentAvailable bool
	ExpiresAt        *time.Time
	ErrorClass       *string
	LastPolledAt     time.Time
}

// Filters narrow management and client job listings.
type Filters struct {
	APIKeyID      *string
	ProviderID    *string
	RouteSlug     *string
	RouteSlugs    []string
	Operation     *string
	Surface       *string
	State         *State
	Lifecycle     *Lifecycle
	CreatedAfter  *time.Time
	CreatedBefore *time.Time
}

// Order controls client-visible job ordering.
type Order int

const (
	OrderDescending Order = iota
	OrderAscending
)

// Page is one cursor page of job records.
type Page struct {
	Items      []JobRecord
	NextCursor *string
}

// Summary describes the reconciliation backlog for readiness.
type Summary struct {
	Pending         int64
	Stale           int64
	Failed          int64
	OldestPendingAt *time.Time
}

const jobSelect = `SELECT j.id::text, j.upstream_job_id, j.api_key_id::text, j.provider_id::text,
		p.name AS provider_name, j.provider_model, j.route_slug,
		j.operation, j.surface, j.state, j.lifecycle_state,
		j.progress_percent::real AS progress_percent,
		j.content_available, j.expires_at, j.error_class,
		j.completed_at, j.last_polled_at, j.reconciliation_error, j.deleted_at,
		j.runtime_generation_id::text, j.provider_revision_id::text,
		j.credential_version_id::text, j.slot_id::text, j.reconciliation_claim_id::text,
		j.reconciliation_attempts, j.next_reconciliation_at,
		j.last_reconciliation_at, j.etag::text,
		j.created_at, j.updated_at
	FROM olp_go.media_jobs j
	JOIN olp_go.providers p ON p.id = j.provider_id`

func scanJob(row pgx.Row) (JobRecord, error) {
	var j JobRecord
	err := row.Scan(&j.ID, &j.UpstreamJobID, &j.APIKeyID, &j.ProviderID,
		&j.ProviderName, &j.UpstreamModel, &j.RouteSlug,
		&j.Operation, &j.Surface, &j.State, &j.Lifecycle,
		&j.ProgressPercent, &j.ContentAvailable, &j.ExpiresAt, &j.ErrorClass,
		&j.CompletedAt, &j.LastPolledAt, &j.ReconciliationError, &j.DeletedAt,
		&j.RuntimeGenerationID, &j.ProviderRevisionID,
		&j.CredentialVersionID, &j.SlotID, &j.ReconciliationClaimID,
		&j.ReconciliationAttempts, &j.NextReconciliationAt,
		&j.LastReconciliationAt, &j.ETag, &j.CreatedAt, &j.UpdatedAt)
	return j, err
}

func scanJobs(rows pgx.Rows) ([]JobRecord, error) {
	var out []JobRecord
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func dbError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return &JobError{Kind: JobErrorNotFound, Message: "media job was not found"}
	}
	return &JobError{Kind: JobErrorDatabase, Err: err}
}

// Job returns one record by public ID.
func Job(ctx context.Context, q Querier, id string) (JobRecord, error) {
	j, err := scanJob(q.QueryRow(ctx, jobSelect+" WHERE j.id = $1", id))
	return j, dbError(err)
}

// Jobs returns one cursor page for the management API. The cursor is the
// previous page's last (created_at, id) pair encoded by CursorEncode.
func Jobs(ctx context.Context, q Querier, filters Filters, cursor *Cursor, limit int) (Page, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 200 {
		limit = 200
	}
	query := jobSelect + " WHERE TRUE"
	var args []any
	push := func(clause string, value any) {
		args = append(args, value)
		query += fmt.Sprintf(clause, len(args))
	}
	if filters.APIKeyID != nil {
		push(" AND j.api_key_id = $%d", *filters.APIKeyID)
	}
	if filters.ProviderID != nil {
		push(" AND j.provider_id = $%d", *filters.ProviderID)
	}
	if filters.RouteSlug != nil {
		push(" AND j.route_slug = $%d", *filters.RouteSlug)
	}
	if len(filters.RouteSlugs) > 0 {
		push(" AND j.route_slug = ANY($%d::text[])", filters.RouteSlugs)
	}
	if filters.Operation != nil {
		push(" AND j.operation = $%d", *filters.Operation)
	}
	if filters.Surface != nil {
		push(" AND j.surface = $%d", *filters.Surface)
	}
	if filters.State != nil {
		push(" AND j.state = $%d", string(*filters.State))
	}
	if filters.Lifecycle != nil {
		push(" AND j.lifecycle_state = $%d", string(*filters.Lifecycle))
	}
	if filters.CreatedAfter != nil {
		push(" AND j.created_at >= $%d", *filters.CreatedAfter)
	}
	if filters.CreatedBefore != nil {
		push(" AND j.created_at < $%d", *filters.CreatedBefore)
	}
	if cursor != nil {
		args = append(args, cursor.At, cursor.ID)
		query += fmt.Sprintf(" AND (j.created_at, j.id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, limit+1)
	query += fmt.Sprintf(" ORDER BY j.created_at DESC, j.id DESC LIMIT $%d", len(args))
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return Page{}, dbError(err)
	}
	items, err := scanJobs(rows)
	rows.Close()
	if err != nil {
		return Page{}, dbError(err)
	}
	var next *string
	if len(items) > limit {
		last := items[limit-1]
		encoded := (&Cursor{At: last.CreatedAt, ID: last.ID}).Encode()
		next = &encoded
		items = items[:limit]
	}
	return Page{Items: items, NextCursor: next}, nil
}

// JobsAfterID pages client-visible jobs using the last public job ID as the
// cursor rather than the opaque management timestamp cursor.
func JobsAfterID(ctx context.Context, q Querier, filters Filters, after *string, order Order, limit int) (Page, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 200 {
		limit = 200
	}
	var positionAt *time.Time
	var positionID *string
	if after != nil {
		routes := filters.RouteSlugs
		if routes == nil {
			routes = []string{}
		}
		err := q.QueryRow(ctx, `SELECT created_at, id::text FROM olp_go.media_jobs
			WHERE id = $1
			  AND ($2::uuid IS NULL OR api_key_id = $2)
			  AND (cardinality($3::text[]) = 0 OR route_slug = ANY($3::text[]))
			  AND ($4::text IS NULL OR operation = $4)
			  AND ($5::text IS NULL OR surface = $5)`,
			*after, filters.APIKeyID, routes, filters.Operation, filters.Surface).
			Scan(&positionAt, &positionID)
		if err != nil {
			return Page{}, &JobError{Kind: JobErrorInvalid, Message: "video cursor is invalid"}
		}
	}
	query := jobSelect + " WHERE j.lifecycle_state = 'active'"
	var args []any
	push := func(clause string, value any) {
		args = append(args, value)
		query += fmt.Sprintf(clause, len(args))
	}
	if filters.APIKeyID != nil {
		push(" AND j.api_key_id = $%d", *filters.APIKeyID)
	}
	if filters.ProviderID != nil {
		push(" AND j.provider_id = $%d", *filters.ProviderID)
	}
	if filters.RouteSlug != nil {
		push(" AND j.route_slug = $%d", *filters.RouteSlug)
	}
	if len(filters.RouteSlugs) > 0 {
		push(" AND j.route_slug = ANY($%d::text[])", filters.RouteSlugs)
	}
	if filters.Operation != nil {
		push(" AND j.operation = $%d", *filters.Operation)
	}
	if filters.Surface != nil {
		push(" AND j.surface = $%d", *filters.Surface)
	}
	if filters.State != nil {
		push(" AND j.state = $%d", string(*filters.State))
	}
	if filters.Lifecycle != nil {
		push(" AND j.lifecycle_state = $%d", string(*filters.Lifecycle))
	}
	if filters.CreatedAfter != nil {
		push(" AND j.created_at >= $%d", *filters.CreatedAfter)
	}
	if filters.CreatedBefore != nil {
		push(" AND j.created_at < $%d", *filters.CreatedBefore)
	}
	if positionAt != nil {
		args = append(args, *positionAt, *positionID)
		if order == OrderAscending {
			query += fmt.Sprintf(" AND (j.created_at, j.id) > ($%d, $%d)", len(args)-1, len(args))
		} else {
			query += fmt.Sprintf(" AND (j.created_at, j.id) < ($%d, $%d)", len(args)-1, len(args))
		}
	}
	args = append(args, limit+1)
	if order == OrderAscending {
		query += fmt.Sprintf(" ORDER BY j.created_at ASC, j.id ASC LIMIT $%d", len(args))
	} else {
		query += fmt.Sprintf(" ORDER BY j.created_at DESC, j.id DESC LIMIT $%d", len(args))
	}
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return Page{}, dbError(err)
	}
	items, err := scanJobs(rows)
	rows.Close()
	if err != nil {
		return Page{}, dbError(err)
	}
	var next *string
	if len(items) > limit {
		items = items[:limit]
		next = &items[len(items)-1].ID
	}
	return Page{Items: items, NextCursor: next}, nil
}

// Cursor is the management pagination token (created_at, id).
type Cursor struct {
	At time.Time
	ID string
}

// Encode renders the cursor in its stable wire form.
func (c *Cursor) Encode() string {
	return c.At.UTC().Format("2006-01-02T15:04:05.999999999Z07:00") + "|" + c.ID
}

// DecodeCursor parses a management pagination token.
func DecodeCursor(value string) (*Cursor, error) {
	at, id, ok := strings.Cut(value, "|")
	if !ok {
		return nil, errors.New("invalid cursor")
	}
	parsed, err := time.Parse("2006-01-02T15:04:05.999999999Z07:00", at)
	if err != nil {
		return nil, errors.New("invalid cursor")
	}
	if _, err = uuid.Parse(id); err != nil {
		return nil, errors.New("invalid cursor")
	}
	return &Cursor{At: parsed, ID: id}, nil
}

// ReserveJob persists the public job ID and exact selected target before a
// non-idempotent upstream create is attempted. The insert succeeds only when
// the pinned generation still describes the provider's currently active
// revision, the pinned credential version remains attached to that revision,
// and the model still certifies the full video lifecycle surface.
func ReserveJob(ctx context.Context, pool *pgxpool.Pool, input Reservation) (JobRecord, error) {
	if _, err := uuid.Parse(input.ID); err != nil || input.UpstreamModel == "" || input.RouteSlug == "" {
		return JobRecord{}, &JobError{Kind: JobErrorInvalid,
			Message: "reservation ID, provider model, route, and operation are required"}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return JobRecord{}, dbError(err)
	}
	defer tx.Rollback(ctx)
	// Acquire admission authority before taking the INSERT's fresh snapshot.
	if _, err = tx.Exec(ctx, "SELECT id FROM olp_go.providers WHERE id = $1 FOR SHARE", input.ProviderID); err != nil {
		return JobRecord{}, dbError(err)
	}
	tag, err := tx.Exec(ctx, `WITH authority AS (
			SELECT r.id AS runtime_generation_id, r.snapshot->'providers'->>$3::text AS provider_key,
			       r.snapshot#>'{providers}' -> $3::text AS provider_entry
			FROM olp_go.runtime_releases r
			WHERE r.id = $8::uuid
		), pinned AS (
			SELECT authority.runtime_generation_id,
			       (authority.provider_entry->>'revision_id')::uuid AS provider_revision_id,
			       authority.provider_entry
			FROM authority
			WHERE authority.provider_entry IS NOT NULL
		), compatible AS (
			SELECT pinned.runtime_generation_id, pinned.provider_revision_id
			FROM pinned
			JOIN olp_go.providers provider ON provider.id = $3::uuid
			JOIN olp_go.provider_revisions current ON current.id = provider.active_revision_id
			JOIN olp_go.provider_revisions pinned_revision ON pinned_revision.id = pinned.provider_revision_id
			WHERE provider.state <> 'disabled'
			  AND pinned_revision.provider_id = $3::uuid
			  AND pinned_revision.configuration->>'kind'
			      IS NOT DISTINCT FROM current.configuration->>'kind'
			  AND pinned_revision.configuration->>'endpoint'
			      IS NOT DISTINCT FROM current.configuration->>'endpoint'
			  AND pinned_revision.configuration->>'cloud_region'
			      IS NOT DISTINCT FROM current.configuration->>'cloud_region'
			  AND pinned_revision.configuration->>'cloud_project'
			      IS NOT DISTINCT FROM current.configuration->>'cloud_project'
			  AND pinned_revision.configuration->>'deployment'
			      IS NOT DISTINCT FROM current.configuration->>'deployment'
			  AND pinned_revision.configuration->>'api_version'
			      IS NOT DISTINCT FROM current.configuration->>'api_version'
			  AND pinned_revision.configuration->>'auth_mode'
			      IS NOT DISTINCT FROM current.configuration->>'auth_mode'
			  AND ((pinned_revision.configuration->'options') - 'limits'::text)
			      IS NOT DISTINCT FROM ((current.configuration->'options') - 'limits'::text)
			  AND ($11::uuid IS NULL OR EXISTS (
			       SELECT 1 FROM jsonb_array_elements(pinned.provider_entry->'slots') slot
			       WHERE slot->>'id' = $11::text
			         AND slot->>'credential_id' IS NOT DISTINCT FROM $9::text))
			  AND ($9::uuid IS NULL OR EXISTS (
			       SELECT 1 FROM jsonb_array_elements(pinned.provider_entry->'slots') slot
			       WHERE slot->>'credential_id' = $9::text))
			  AND (SELECT cred.version FROM olp_go.provider_credentials cred
			       WHERE cred.id = (pinned.provider_entry->>'active_credential')::uuid)
			      IS NOT DISTINCT FROM current.credential_version
			  AND EXISTS (
			       SELECT 1 FROM jsonb_array_elements(current.models) model
			       WHERE model->>'upstream_model' = $4
			         AND NOT EXISTS (
			           SELECT required.operation
			           FROM (VALUES ('video_get'), ('video_content'), ('video_delete'))
			                AS required(operation)
			           WHERE NOT EXISTS (
			             SELECT 1 FROM jsonb_array_elements(model->'capabilities') c
			             WHERE c->>'operation' = required.operation
			               AND c->>'surface' = $7
			               AND c->>'mode' = 'unary'
			               AND c->>'source' = 'certified'))))
		INSERT INTO olp_go.media_jobs (
			id, upstream_job_id, api_key_id, provider_id, provider_model,
			route_slug, operation, surface, state, lifecycle_state,
			runtime_generation_id, provider_revision_id, credential_version_id, etag, slot_id
		)
		SELECT $1::uuid, NULL, $2::uuid, $3::uuid, $4, $5, $6, $7, 'queued', 'creating',
		       compatible.runtime_generation_id, compatible.provider_revision_id, $9::uuid, $10::uuid, $11::uuid
		FROM compatible`,
		input.ID, input.APIKeyID, input.ProviderID, input.UpstreamModel,
		input.RouteSlug, input.Operation, input.Surface,
		input.RuntimeGenerationID, input.CredentialVersionID, uuid.Must(uuid.NewV7()), input.SlotID)
	if err != nil {
		return JobRecord{}, dbError(err)
	}
	if tag.RowsAffected() != 1 {
		return JobRecord{}, &JobError{Kind: JobErrorInvalid,
			Message: "the pinned provider authority is unavailable or incompatible with current video support"}
	}
	if err = tx.Commit(ctx); err != nil {
		return JobRecord{}, dbError(err)
	}
	return Job(ctx, pool, input.ID)
}

// AttachUpstream binds the accepted upstream identity and first state to a
// creating job. A retry after an ambiguous connection loss still reports
// success when the stored identity already matches.
func AttachUpstream(ctx context.Context, q Querier, id, upstreamJobID string, update JobUpdate) (JobRecord, error) {
	if upstreamJobID == "" || upstreamJobID != trimSpace(upstreamJobID) {
		return JobRecord{}, &JobError{Kind: JobErrorInvalid, Message: "upstream job ID cannot be empty"}
	}
	if err := validateUpdate(update); err != nil {
		return JobRecord{}, err
	}
	row, err := scanJob(q.QueryRow(ctx, `WITH attached AS (
			UPDATE olp_go.media_jobs SET
				upstream_job_id = $2,
				state = $3,
				lifecycle_state = 'active',
				progress_percent = $4::real::numeric,
				content_available = $5,
				expires_at = $6,
				error_class = $7,
				last_polled_at = $8,
				reconciliation_error = NULL,
				etag = $9
			WHERE id = $1 AND lifecycle_state = 'creating'
			RETURNING *
		)
		SELECT j.id::text, j.upstream_job_id, j.api_key_id::text, j.provider_id::text,
			p.name AS provider_name, j.provider_model, j.route_slug,
			j.operation, j.surface, j.state, j.lifecycle_state,
			j.progress_percent::real AS progress_percent,
			j.content_available, j.expires_at, j.error_class,
			j.completed_at, j.last_polled_at, j.reconciliation_error, j.deleted_at,
			j.runtime_generation_id::text, j.provider_revision_id::text,
			j.credential_version_id::text, j.slot_id::text, j.reconciliation_claim_id::text,
			j.reconciliation_attempts, j.next_reconciliation_at,
			j.last_reconciliation_at, j.etag::text, j.created_at, j.updated_at
		FROM attached j JOIN olp_go.providers p ON p.id = j.provider_id`,
		id, upstreamJobID, string(update.State), update.ProgressPercent,
		update.ContentAvailable, update.ExpiresAt, update.ErrorClass,
		update.LastPolledAt, uuid.Must(uuid.NewV7())))
	if err == nil {
		return row, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.ConstraintName == "media_jobs_upstream_unique_idx" {
			return JobRecord{}, &JobError{Kind: JobErrorUpstreamIdentityConflict}
		}
		return JobRecord{}, dbError(err)
	}
	current, loadErr := Job(ctx, q, id)
	if loadErr != nil {
		return JobRecord{}, loadErr
	}
	if current.Lifecycle == LifecycleActive &&
		current.UpstreamJobID != nil && *current.UpstreamJobID == upstreamJobID {
		return current, nil
	}
	return JobRecord{}, &JobError{Kind: JobErrorPrecondition, Message: "media job changed; refresh and retry"}
}

func trimSpace(value string) string {
	start, end := 0, len(value)
	for start < end && (value[start] == ' ' || value[start] == '\t' || value[start] == '\n' || value[start] == '\r') {
		start++
	}
	for end > start && (value[end-1] == ' ' || value[end-1] == '\t' || value[end-1] == '\n' || value[end-1] == '\r') {
		end--
	}
	return value[start:end]
}

// updateLifecycle applies one lifecycle transition. When claimID is set the
// transition lands only while that claim still owns the row, so a stale worker
// cannot mutate a job another worker reclaimed.
func updateLifecycle(ctx context.Context, q Querier, id string, lifecycle Lifecycle,
	upstreamJobID *string, reconciliationError string, allowed []Lifecycle, claimID *string) (JobRecord, error) {
	allowedStrings := make([]string, 0, len(allowed))
	for _, value := range allowed {
		allowedStrings = append(allowedStrings, string(value))
	}
	query := `UPDATE olp_go.media_jobs SET lifecycle_state = $2,
			upstream_job_id = COALESCE($3, upstream_job_id),
			reconciliation_error = $4, next_reconciliation_at = now(), etag = $5
		WHERE id = $1 AND lifecycle_state = ANY($6::text[])`
	args := []any{id, string(lifecycle), upstreamJobID, reconciliationError, uuid.Must(uuid.NewV7()), allowedStrings}
	if claimID != nil {
		args = append(args, *claimID)
		query += " AND reconciliation_claim_id = $7"
	}
	tag, err := q.Exec(ctx, query, args...)
	if err != nil {
		return JobRecord{}, dbError(err)
	}
	if tag.RowsAffected() == 0 {
		if claimID != nil {
			return JobRecord{}, claimRefusal(ctx, q, id)
		}
		return JobRecord{}, missingOrChanged(ctx, q, id)
	}
	return Job(ctx, q, id)
}

// claimRefusal classifies a claim-fenced mutation that changed no row. A
// genuinely missing row keeps missing-record reporting; any other refusal
// means the claimed work item moved on — the lease changed hands or the
// lifecycle drifted past the transition — and the worker hands off.
func claimRefusal(ctx context.Context, q Querier, id string) error {
	var exists bool
	if err := q.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp_go.media_jobs WHERE id=$1)", id).Scan(&exists); err != nil {
		return &JobError{Kind: JobErrorDatabase, Err: err}
	}
	if !exists {
		return &JobError{Kind: JobErrorNotFound, Message: "media job was not found"}
	}
	return errClaimLost
}

func missingOrChanged(ctx context.Context, q Querier, id string) *JobError {
	var exists bool
	if err := q.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp_go.media_jobs WHERE id=$1)", id).Scan(&exists); err != nil {
		return &JobError{Kind: JobErrorDatabase, Err: err}
	}
	if exists {
		return &JobError{Kind: JobErrorPrecondition, Message: "media job changed; refresh and retry"}
	}
	return &JobError{Kind: JobErrorNotFound, Message: "media job was not found"}
}

// MarkCreateAmbiguous records that the create outcome is unknown upstream.
func MarkCreateAmbiguous(ctx context.Context, q Querier, id, reconciliationError string) (JobRecord, error) {
	return updateLifecycle(ctx, q, id, LifecycleCreateAmbiguous, nil, reconciliationError,
		[]Lifecycle{LifecycleCreating, LifecycleCreateAmbiguous}, nil)
}

// markCreateAmbiguousClaimed is MarkCreateAmbiguous fenced on the worker's
// reconciliation claim.
func markCreateAmbiguousClaimed(ctx context.Context, q Querier, id, claimID, reconciliationError string) (JobRecord, error) {
	return updateLifecycle(ctx, q, id, LifecycleCreateAmbiguous, nil, reconciliationError,
		[]Lifecycle{LifecycleCreating, LifecycleCreateAmbiguous}, &claimID)
}

// MarkCreateCleanupPending records an upstream identity whose attach failed;
// the reconciler must delete it before the job can be marked deleted.
func MarkCreateCleanupPending(ctx context.Context, q Querier, id, upstreamJobID, reconciliationError string) (JobRecord, error) {
	if trimSpace(upstreamJobID) == "" {
		return JobRecord{}, &JobError{Kind: JobErrorInvalid, Message: "upstream job ID cannot be empty"}
	}
	return updateLifecycle(ctx, q, id, LifecycleCreateCleanupPending, &upstreamJobID, reconciliationError,
		[]Lifecycle{LifecycleCreating, LifecycleCreateAmbiguous, LifecycleCreateCleanupPending}, nil)
}

// markCreateCleanupPendingClaimed is MarkCreateCleanupPending fenced on the
// worker's reconciliation claim.
func markCreateCleanupPendingClaimed(ctx context.Context, q Querier, id, claimID, upstreamJobID, reconciliationError string) (JobRecord, error) {
	if trimSpace(upstreamJobID) == "" {
		return JobRecord{}, &JobError{Kind: JobErrorInvalid, Message: "upstream job ID cannot be empty"}
	}
	return updateLifecycle(ctx, q, id, LifecycleCreateCleanupPending, &upstreamJobID, reconciliationError,
		[]Lifecycle{LifecycleCreating, LifecycleCreateAmbiguous, LifecycleCreateCleanupPending}, &claimID)
}

// BeginDeletion persists delete intent before contacting the pinned upstream
// target. Repeated calls return the same pending/deleted tombstone.
func BeginDeletion(ctx context.Context, q Querier, id string) (JobRecord, error) {
	tag, err := q.Exec(ctx, `UPDATE olp_go.media_jobs SET lifecycle_state = 'delete_pending',
			reconciliation_error = NULL, next_reconciliation_at = now(), etag = $2
		WHERE id = $1 AND lifecycle_state = 'active'`, id, uuid.Must(uuid.NewV7()))
	if err != nil {
		return JobRecord{}, dbError(err)
	}
	record, err := Job(ctx, q, id)
	if err != nil {
		return JobRecord{}, err
	}
	if tag.RowsAffected() == 1 ||
		record.Lifecycle == LifecycleDeletePending || record.Lifecycle == LifecycleDeleted {
		return record, nil
	}
	return JobRecord{}, &JobError{Kind: JobErrorPrecondition, Message: "media job changed; refresh and retry"}
}

// beginDeletionClaimed persists delete intent only while claimID owns the
// job. A concurrent client delete may already have moved the claimed row to
// delete_pending or deleted; that record is returned so the worker finishes
// the upstream confirmation. Any other refusal hands the job off.
func beginDeletionClaimed(ctx context.Context, q Querier, id, claimID string) (JobRecord, error) {
	tag, err := q.Exec(ctx, `UPDATE olp_go.media_jobs SET lifecycle_state = 'delete_pending',
			reconciliation_error = NULL, next_reconciliation_at = now(), etag = $3
		WHERE id = $1 AND reconciliation_claim_id = $2 AND lifecycle_state = 'active'`,
		id, claimID, uuid.Must(uuid.NewV7()))
	if err != nil {
		return JobRecord{}, dbError(err)
	}
	record, err := Job(ctx, q, id)
	if err != nil {
		return JobRecord{}, err
	}
	if tag.RowsAffected() == 1 {
		return record, nil
	}
	if record.ReconciliationClaimID == nil || *record.ReconciliationClaimID != claimID {
		return JobRecord{}, errClaimLost
	}
	if record.Lifecycle == LifecycleDeletePending || record.Lifecycle == LifecycleDeleted {
		return record, nil
	}
	return JobRecord{}, errClaimLost
}

// AllowsRefreshTransition reports whether an upstream poll may move the
// stored state; terminal states are immutable.
func AllowsRefreshTransition(current, incoming State) bool {
	switch current {
	case StateQueued:
		return true
	case StateRunning:
		return incoming != StateQueued
	case StateSucceeded:
		return incoming == StateSucceeded
	case StateFailed:
		return incoming == StateFailed
	case StateCancelled:
		return incoming == StateCancelled
	}
	return false
}

func validateUpdate(update JobUpdate) error {
	if update.ContentAvailable && update.State != StateSucceeded {
		return &JobError{Kind: JobErrorInvalid, Message: "content is available only for a succeeded job"}
	}
	if update.ErrorClass != nil && update.State != StateFailed {
		return &JobError{Kind: JobErrorInvalid, Message: "an error class is valid only for a failed job"}
	}
	if update.ProgressPercent != nil &&
		(*update.ProgressPercent < 0 || *update.ProgressPercent > 100 ||
			*update.ProgressPercent != *update.ProgressPercent) {
		return &JobError{Kind: JobErrorInvalid, Message: "progress must be a finite percentage from 0 through 100"}
	}
	return nil
}

// RefreshJob applies an upstream poll result. Polls are serialized per job;
// stale results and state regressions are ignored while terminal states
// remain immutable.
func RefreshJob(ctx context.Context, pool *pgxpool.Pool, id string, update JobUpdate) (JobRecord, error) {
	return refreshJob(ctx, pool, id, nil, update)
}

// refreshJobClaimed applies a poll result only while claimID owns the job, so
// a stale worker's late response cannot overwrite the current owner's state.
func refreshJobClaimed(ctx context.Context, pool *pgxpool.Pool, id, claimID string, update JobUpdate) (JobRecord, error) {
	return refreshJob(ctx, pool, id, &claimID, update)
}

// refreshJob applies one poll result inside a single transaction: the row is
// locked, the optional claim fence and the lifecycle are checked, and the
// update lands under the same lock. Poll results only ever apply to an active
// job — a late poll must not undo delete intent or rewrite a tombstone.
func refreshJob(ctx context.Context, pool *pgxpool.Pool, id string, claimID *string, update JobUpdate) (JobRecord, error) {
	if err := validateUpdate(update); err != nil {
		return JobRecord{}, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return JobRecord{}, dbError(err)
	}
	defer tx.Rollback(ctx)
	current, err := scanJob(tx.QueryRow(ctx, jobSelect+" WHERE j.id = $1 FOR UPDATE OF j", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return JobRecord{}, &JobError{Kind: JobErrorNotFound, Message: "media job was not found"}
	}
	if err != nil {
		return JobRecord{}, dbError(err)
	}
	if claimID != nil &&
		(current.ReconciliationClaimID == nil || *current.ReconciliationClaimID != *claimID) {
		return JobRecord{}, errClaimLost
	}
	if current.Lifecycle != LifecycleActive {
		if claimID != nil {
			return JobRecord{}, errClaimLost
		}
		if err = tx.Commit(ctx); err != nil {
			return JobRecord{}, dbError(err)
		}
		return current, nil
	}
	stale := current.LastPolledAt != nil && current.LastPolledAt.After(update.LastPolledAt)
	if stale || !AllowsRefreshTransition(current.State, update.State) {
		if err = tx.Commit(ctx); err != nil {
			return JobRecord{}, dbError(err)
		}
		return current, nil
	}
	if _, err = tx.Exec(ctx, `UPDATE olp_go.media_jobs SET
			state = $2,
			progress_percent = CASE
				WHEN $3::real IS NULL THEN progress_percent
				WHEN progress_percent IS NULL THEN $3::real::numeric
				ELSE GREATEST(progress_percent, $3::real::numeric)
			END,
			content_available = content_available OR $4,
			expires_at = COALESCE($5, expires_at),
			error_class = COALESCE($6, error_class),
			last_polled_at = $7,
			completed_at = CASE WHEN $2 IN ('succeeded','failed','cancelled')
				THEN COALESCE(completed_at, $7) ELSE completed_at END,
			next_reconciliation_at = GREATEST(next_reconciliation_at, $7 + $8::int * interval '1 second'),
			etag = $9
		WHERE id = $1`,
		id, string(update.State), update.ProgressPercent, update.ContentAvailable,
		update.ExpiresAt, update.ErrorClass, update.LastPolledAt,
		PollGateSeconds, uuid.Must(uuid.NewV7())); err != nil {
		return JobRecord{}, dbError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return JobRecord{}, dbError(err)
	}
	return Job(ctx, pool, id)
}

// FinalizeDeletion applies the metadata-only tombstone once the upstream
// delete is accepted. Polls may rotate the ETag while the upstream delete is
// in flight, so optimistic locking is unsafe at this point.
func FinalizeDeletion(ctx context.Context, q Querier, id string) (bool, error) {
	tag, err := q.Exec(ctx, `UPDATE olp_go.media_jobs
		SET lifecycle_state = 'deleted', deleted_at = COALESCE(deleted_at, now()),
			reconciliation_error = NULL, content_available = false, etag = $2
		WHERE id = $1
		  AND lifecycle_state IN ('creating','create_ambiguous','create_cleanup_pending','delete_pending')`,
		id, uuid.Must(uuid.NewV7()))
	if err != nil {
		return false, dbError(err)
	}
	return tag.RowsAffected() == 1, nil
}

// finalizeDeletionClaimed applies the tombstone only while claimID owns the
// job and its lifecycle still permits deletion, so a confirmed upstream
// delete cannot be finalized by a worker that lost the lease. An existing
// tombstone under the same claim is a successful no-op.
func finalizeDeletionClaimed(ctx context.Context, q Querier, id, claimID string) error {
	tag, err := q.Exec(ctx, `UPDATE olp_go.media_jobs
		SET lifecycle_state = 'deleted', deleted_at = COALESCE(deleted_at, now()),
			reconciliation_error = NULL, content_available = false, etag = $3
		WHERE id = $1
		  AND reconciliation_claim_id = $2
		  AND lifecycle_state IN ('creating','create_ambiguous','create_cleanup_pending','delete_pending')`,
		id, claimID, uuid.Must(uuid.NewV7()))
	if err != nil {
		return dbError(err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var lifecycle string
	var currentClaim *string
	err = q.QueryRow(ctx, `SELECT lifecycle_state, reconciliation_claim_id::text
		FROM olp_go.media_jobs WHERE id = $1`, id).Scan(&lifecycle, &currentClaim)
	if err != nil {
		return dbError(err)
	}
	if currentClaim != nil && *currentClaim == claimID && lifecycle == string(LifecycleDeleted) {
		return nil
	}
	return errClaimLost
}

// ClaimJobs claims a bounded cross-replica batch for autonomous lifecycle
// work. The database lease is deliberately longer than an ordinary route
// deadline so an expired lease can be recovered after process death.
func ClaimJobs(ctx context.Context, q Querier, now time.Time, limit int) ([]JobRecord, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 32 {
		limit = 32
	}
	claimID := uuid.Must(uuid.NewV7())
	rows, err := q.Query(ctx, `WITH candidates AS (
			SELECT id FROM olp_go.media_jobs
			WHERE lifecycle_state <> 'deleted'
			  AND next_reconciliation_at <= $1
			  AND (reconciliation_claimed_until IS NULL
			       OR reconciliation_claimed_until <= $1)
			  AND (
			    lifecycle_state IN ('create_ambiguous','create_cleanup_pending','delete_pending')
			    OR (lifecycle_state = 'creating'
			        AND updated_at <= $1 - interval '5 minutes')
			    OR (lifecycle_state = 'active'
			        AND upstream_job_id IS NOT NULL
			        AND (
			          (state IN ('queued','running')
			           AND (last_polled_at IS NULL
			                OR last_polled_at <= $1 - $4::int * interval '1 second'))
			          OR expires_at <= $1
			          OR created_at <= $1 - interval '30 days')))
			ORDER BY CASE WHEN lifecycle_state = 'active' THEN 1 ELSE 0 END,
				next_reconciliation_at, created_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT $2
		), claimed AS (
			UPDATE olp_go.media_jobs j SET
				reconciliation_claim_id = $3,
				reconciliation_claimed_until = $1 + interval '2 minutes',
				last_reconciliation_at = $1,
				next_reconciliation_at = $1 + interval '2 minutes',
				reconciliation_attempts = reconciliation_attempts + 1,
				etag = $5
			FROM candidates c WHERE j.id = c.id
			RETURNING j.*
		)
		SELECT c.id::text, c.upstream_job_id, c.api_key_id::text, c.provider_id::text,
			p.name AS provider_name, c.provider_model, c.route_slug,
			c.operation, c.surface, c.state, c.lifecycle_state,
			c.progress_percent::real AS progress_percent,
			c.content_available, c.expires_at, c.error_class,
			c.completed_at, c.last_polled_at, c.reconciliation_error, c.deleted_at,
			c.runtime_generation_id::text, c.provider_revision_id::text,
			c.credential_version_id::text, c.slot_id::text, c.reconciliation_claim_id::text,
			c.reconciliation_attempts, c.next_reconciliation_at,
			c.last_reconciliation_at, c.etag::text, c.created_at, c.updated_at
		FROM claimed c JOIN olp_go.providers p ON p.id = c.provider_id
		ORDER BY c.created_at, c.id`,
		now, limit, claimID, PollGateSeconds, uuid.Must(uuid.NewV7()))
	if err != nil {
		return nil, dbError(err)
	}
	items, err := scanJobs(rows)
	rows.Close()
	return items, dbError(err)
}

// ExtendClaim revalidates ownership of a claimed job and extends its lease so
// it covers the upstream call that is about to start.
func ExtendClaim(ctx context.Context, q Querier, id, claimID string, until time.Time) (bool, error) {
	tag, err := q.Exec(ctx, `UPDATE olp_go.media_jobs SET
			reconciliation_claimed_until = GREATEST(reconciliation_claimed_until, $3),
			next_reconciliation_at = GREATEST(next_reconciliation_at, $3),
			etag = $4
		WHERE id = $1 AND reconciliation_claim_id = $2`,
		id, claimID, until, uuid.Must(uuid.NewV7()))
	if err != nil {
		return false, dbError(err)
	}
	return tag.RowsAffected() == 1, nil
}

// FinishReconciliation releases one lease and records only a bounded error
// class. Provider bodies and request content are never accepted here.
func FinishReconciliation(ctx context.Context, q Querier, id, claimID string, nextAttemptAt time.Time, errorClass *string) error {
	if errorClass != nil && (*errorClass == "" || len(*errorClass) > 120) {
		return &JobError{Kind: JobErrorInvalid, Message: "reconciliation error class must contain 1-120 bytes"}
	}
	tag, err := q.Exec(ctx, `UPDATE olp_go.media_jobs SET
			reconciliation_claim_id = NULL,
			reconciliation_claimed_until = NULL,
			next_reconciliation_at = $3,
			reconciliation_error = $4,
			reconciliation_attempts = CASE WHEN $4::text IS NULL THEN 0 ELSE reconciliation_attempts END,
			etag = $5
		WHERE id = $1 AND reconciliation_claim_id = $2`,
		id, claimID, nextAttemptAt, errorClass, uuid.Must(uuid.NewV7()))
	if err != nil {
		return dbError(err)
	}
	if tag.RowsAffected() != 1 {
		return missingOrChanged(ctx, q, id)
	}
	return nil
}

// ReconciliationSummary reports the backlog behind readiness reporting.
func ReconciliationSummary(ctx context.Context, q Querier, now time.Time) (Summary, error) {
	var s Summary
	err := q.QueryRow(ctx, `SELECT COUNT(*) FILTER (
				WHERE lifecycle_state NOT IN ('active','deleted'))::bigint,
			COUNT(*) FILTER (
				WHERE (lifecycle_state = 'creating' AND updated_at < $1::timestamptz - interval '5 minutes')
				   OR (lifecycle_state NOT IN ('creating','active','deleted')
				       AND next_reconciliation_at < $1::timestamptz - interval '1 minute')
				   OR (lifecycle_state = 'active'
				       AND state IN ('queued','running')
				       AND next_reconciliation_at < $1::timestamptz - interval '1 minute'
				       AND (last_polled_at IS NULL
				            OR last_polled_at <= $1::timestamptz - $2::int * interval '1 second')))::bigint,
			COUNT(*) FILTER (
				WHERE lifecycle_state <> 'deleted' AND reconciliation_error IS NOT NULL)::bigint,
			MIN(created_at) FILTER (WHERE lifecycle_state NOT IN ('active','deleted'))
		FROM olp_go.media_jobs WHERE lifecycle_state <> 'deleted'`,
		now, PollGateSeconds).
		Scan(&s.Pending, &s.Stale, &s.Failed, &s.OldestPendingAt)
	return s, dbError(err)
}

// PendingJobs lists one key's unreconciled jobs; used by diagnostics.
func PendingJobs(ctx context.Context, q Querier, apiKeyID string, limit int) ([]JobRecord, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 32 {
		limit = 32
	}
	rows, err := q.Query(ctx, jobSelect+` WHERE j.api_key_id = $1
		AND j.lifecycle_state IN ('create_cleanup_pending','delete_pending')
		ORDER BY j.updated_at ASC, j.id ASC LIMIT $2`, apiKeyID, limit)
	if err != nil {
		return nil, dbError(err)
	}
	items, err := scanJobs(rows)
	rows.Close()
	return items, dbError(err)
}
