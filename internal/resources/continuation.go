package resources

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/secrets"
)

const (
	// A complete state includes native history, dependency metadata and delivery.
	// The database separately caps ciphertext, including encryption overhead.
	MaxContinuationBytes = 4 << 20
	ContinuationLifetime = 24 * time.Hour
	StatePending         = "pending"
	StateDispatching     = "dispatching"
	StateReady           = "ready"
	StateUnknown         = "outcome_unknown"
	continuationPurpose  = "provider_continuation"
)

var (
	ErrSubmission      = errors.New("submission identity is invalid or outside its replay window")
	ErrContract        = errors.New("encrypted resource contract unavailable")
	ErrTransition      = errors.New("resource state transition is not available")
	ErrPayloadTooLarge = errors.New("continuation payload exceeds its bound")
)

// NewEncrypted configures the existing resource owner to store recoverable
// contracts with the installation's normal key lifecycle and transactions.
func NewEncrypted(pool *pgxpool.Pool, installation string, keys *secrets.KeyRing) *Store {
	return &Store{pool: pool, installation: installation, keys: keys}
}
func (s *Store) Encrypted() bool { return s != nil && s.keys != nil && s.installation != "" }

// SubmissionID identifies one client invocation, including SDK/network retries.
// Its timestamp prevents an expired, garbage-collected claim from becoming new
// inference. A deliberately new invocation must have a new identity.
func SubmissionID(at time.Time, id uuid.UUID) string {
	return strconv.FormatInt(at.UnixMilli(), 10) + "." + id.String()
}
func ValidateSubmission(value string, now time.Time) error {
	stamp, text, ok := strings.Cut(value, ".")
	millis, err := strconv.ParseInt(stamp, 10, 64)
	id, idErr := uuid.Parse(text)
	if !ok || err != nil || idErr != nil || id == uuid.Nil || SubmissionID(time.UnixMilli(millis), id) != value {
		return ErrSubmission
	}
	at := time.UnixMilli(millis)
	if at.Before(now.Add(-15*time.Minute)) || at.After(now.Add(5*time.Minute)) {
		return ErrSubmission
	}
	return nil
}
func (s *Store) validateContract(r *Resource, payload []byte) error {
	if !s.Encrypted() || r == nil || (r.Kind != KindContinuation && r.Kind != KindStrictResponse) || r.ContractVersion == nil || len(*r.ContractVersion) == 0 || len(*r.ContractVersion) > 128 {
		return ErrContract
	}
	if len(payload) == 0 || len(payload) > MaxContinuationBytes {
		return ErrPayloadTooLarge
	}
	if r.ExpiresAt == nil || !r.ExpiresAt.After(time.Now()) || r.ExpiresAt.After(time.Now().Add(ContinuationLifetime)) {
		return ErrContract
	}
	if len(r.Metadata) > metadataLimit {
		return ErrMetadataTooLarge
	}
	return nil
}

// ClaimContinuation atomically persists the initial encrypted dependencies and
// reserves an owner-wide submission identity. Only the creator may dispatch;
// all other callers receive the existing resource for authorized delivery replay.
// A pending/unknown claim is never permission to repeat provider inference.
func (s *Store) ClaimContinuation(ctx context.Context, r *Resource, payload []byte) (*Resource, bool, error) {
	if err := s.validateContract(r, payload); err != nil {
		return nil, false, err
	}
	if r.Kind != KindContinuation || r.SubmissionID == nil {
		return nil, false, ErrSubmission
	}
	if err := ValidateSubmission(*r.SubmissionID, time.Now()); err != nil {
		return nil, false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)
	if err = s.authorizeOwner(ctx, tx, r.APIKeyID); err != nil {
		return nil, false, err
	}
	if r.ParentID != nil {
		var valid bool
		err = tx.QueryRow(ctx, `SELECT true FROM olp_go.provider_resources WHERE id=$1 AND api_key_id=$2 AND kind=$3 AND state=$4 AND expires_at>now() FOR SHARE`, *r.ParentID, r.APIKeyID, KindContinuation, StateReady).Scan(&valid)
		if err != nil || !valid {
			return nil, false, ErrNotFound
		}
	}
	copy := *r
	copy.UUID = uuid.Must(uuid.NewV7())
	copy.State = StatePending
	// A continuation has no provider resource ID; use its own internal identity.
	copy.UpstreamID = copy.UUID.String()
	if len(copy.Metadata) == 0 {
		copy.Metadata = []byte(`{}`)
	}
	out, err := scan(tx.QueryRow(ctx, `INSERT INTO olp_go.provider_resources
 (id,kind,api_key_id,route_slug,provider_id,provider_revision_id,route_revision_id,slot_id,credential_id,upstream_id,state,metadata,expires_at,contract_version,parent_id,submission_id)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
 ON CONFLICT(api_key_id,submission_id) WHERE submission_id IS NOT NULL DO NOTHING RETURNING `+columns,
		copy.UUID, copy.Kind, copy.APIKeyID, copy.RouteSlug, copy.ProviderID, copy.ProviderRevisionID, copy.RouteRevisionID, copy.SlotID, copy.CredentialID, copy.UpstreamID, copy.State, copy.Metadata, copy.ExpiresAt, copy.ContractVersion, copy.ParentID, copy.SubmissionID))
	if errors.Is(err, pgx.ErrNoRows) {
		out, err = scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM olp_go.provider_resources WHERE api_key_id=$1 AND submission_id=$2 AND kind=$3 AND state<>$4 AND expires_at>now()`, r.APIKeyID, *r.SubmissionID, KindContinuation, StateDeleted))
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		return out, false, err
	}
	if err != nil {
		return nil, false, err
	}
	if err = s.keys.Store(ctx, tx, s.installation, out.UUID.String(), continuationPurpose, payload, out.ExpiresAt); err != nil {
		return nil, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// StartDispatch records the accepted-work boundary before the existing Attempt
// owner calls the provider. Process loss afterward cannot authorize fresh work.
func (s *Store) StartDispatch(ctx context.Context, r *Resource) error {
	return s.transition(ctx, r, StatePending, StateDispatching)
}
func (s *Store) MarkUnknown(ctx context.Context, r *Resource) error {
	return s.transition(ctx, r, StateDispatching, StateUnknown)
}
func (s *Store) transition(ctx context.Context, r *Resource, from, to string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE olp_go.provider_resources SET state=$4,updated_at=now() WHERE id=$1 AND api_key_id=$2 AND state=$3 AND kind=$5 AND expires_at>now()`, r.UUID, r.APIKeyID, from, to, KindContinuation)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrTransition
	}
	return nil
}

// CompleteContinuation is the actionability barrier. Ready is published in the
// same commit as the complete encrypted payload. Ready snapshots are immutable;
// a next turn creates a new child instead of consuming or modifying its parent.
func (s *Store) CompleteContinuation(ctx context.Context, r *Resource, payload []byte) error {
	if err := s.validateContract(r, payload); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = s.authorizeOwner(ctx, tx, r.APIKeyID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE olp_go.provider_resources SET state=$4,updated_at=now() WHERE id=$1 AND api_key_id=$2 AND state=$3 AND kind=$5 AND expires_at>now()`, r.UUID, r.APIKeyID, StateDispatching, StateReady, KindContinuation)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrTransition
	}
	if err = s.keys.Store(ctx, tx, s.installation, r.UUID.String(), continuationPurpose, payload, r.ExpiresAt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReadContract returns the encrypted operation-owned document only to its live
// owner. The caller separately enforces route/provider/credential authority and
// contract version, and only exposes delivery when StateReady is present.
func (s *Store) ReadContract(ctx context.Context, kind, owner, localID string) (*Resource, []byte, error) {
	if !s.Encrypted() || (kind != KindContinuation && kind != KindStrictResponse) {
		return nil, nil, ErrContract
	}
	id, err := parseLocal(localID)
	if err != nil || LocalID(kind, id) != localID {
		return nil, nil, ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)
	if err = s.authorizeOwner(ctx, tx, owner); err != nil {
		return nil, nil, err
	}
	r, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM olp_go.provider_resources WHERE id=$1 AND kind=$2 AND api_key_id=$3 AND state<>$4 AND expires_at>now() FOR SHARE`, id, kind, owner, StateDeleted))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	payload, err := s.keys.Read(ctx, tx, s.installation, id.String(), continuationPurpose)
	if err != nil {
		return nil, nil, ErrContract
	}
	if len(payload) == 0 || len(payload) > MaxContinuationBytes {
		return nil, nil, ErrContract
	}
	return r, payload, nil
}
func (s *Store) FindSubmission(ctx context.Context, owner, submission string) (*Resource, []byte, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT id FROM olp_go.provider_resources WHERE api_key_id=$1 AND submission_id=$2 AND kind=$3`, owner, submission, KindContinuation).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	return s.ReadContract(ctx, KindContinuation, owner, LocalID(KindContinuation, id))
}
func (s *Store) authorizeOwner(ctx context.Context, tx pgx.Tx, owner string) error {
	var valid bool
	err := tx.QueryRow(ctx, `SELECT true FROM olp_go.api_keys WHERE id=$1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>now()) FOR SHARE`, owner).Scan(&valid)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("resource owner: %w", err)
	}
	return nil
}

// PutContract publishes a strict provider response mapping and its historical
// contract atomically. No local response identifier may be delivered earlier.
func (s *Store) PutContract(ctx context.Context, r *Resource, payload []byte) (*Resource, error) {
	if err := s.validateContract(r, payload); err != nil {
		return nil, err
	}
	if r.Kind != KindStrictResponse || r.SubmissionID != nil {
		return nil, ErrContract
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err = s.authorizeOwner(ctx, tx, r.APIKeyID); err != nil {
		return nil, err
	}
	copy := *r
	copy.UUID = uuid.Must(uuid.NewV7())
	if len(copy.Metadata) == 0 {
		copy.Metadata = []byte(`{}`)
	}
	out, err := scan(tx.QueryRow(ctx, `INSERT INTO olp_go.provider_resources
 (id,kind,api_key_id,route_slug,provider_id,provider_revision_id,route_revision_id,slot_id,credential_id,upstream_id,state,metadata,expires_at,contract_version,parent_id)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING `+columns,
		copy.UUID, copy.Kind, copy.APIKeyID, copy.RouteSlug, copy.ProviderID, copy.ProviderRevisionID, copy.RouteRevisionID, copy.SlotID, copy.CredentialID, copy.UpstreamID, copy.State, copy.Metadata, copy.ExpiresAt, copy.ContractVersion, copy.ParentID))
	if err != nil {
		return nil, err
	}
	if err = s.keys.Store(ctx, tx, s.installation, out.UUID.String(), continuationPurpose, payload, out.ExpiresAt); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}
