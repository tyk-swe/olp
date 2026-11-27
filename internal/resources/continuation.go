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

	"github.com/tyk-swe/olp/internal/platform/secrets"
)

const (
	MaxContinuationBytes = 4 << 20
	ContinuationLifetime = 24 * time.Hour
	StateDispatching     = "dispatching"
	StateReady           = "ready"
	StateUnknown         = "outcome_unknown"
)

var (
	ErrSubmission      = errors.New("submission identity is invalid or outside its replay window")
	ErrContract        = errors.New("encrypted resource contract unavailable")
	ErrTransition      = errors.New("resource state transition is not available")
	ErrPayloadTooLarge = errors.New("continuation payload exceeds its bound")
)

type sealedContract struct {
	resource *Resource
	payload  []byte
}

func NewEncrypted(pool *pgxpool.Pool, installation string, keys *secrets.KeyRing) *Store {
	return &Store{pool: pool, installation: installation, keys: keys}
}

func (s *Store) Encrypted() bool { return s != nil && s.keys != nil && s.installation != "" }

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
	return validateContractAt(s.Encrypted(), r, payload, time.Now())
}

func validateContractAt(encrypted bool, r *Resource, payload []byte, now time.Time) error {
	if !encrypted || r == nil || (r.Kind != KindContinuation && r.Kind != KindStrictResponse) {
		return ErrContract
	}
	if r.ContractVersion == nil || len(*r.ContractVersion) == 0 || len(*r.ContractVersion) > 128 {
		return ErrContract
	}
	if len(payload) == 0 || len(payload) > MaxContinuationBytes {
		return ErrPayloadTooLarge
	}
	if r.ExpiresAt == nil || !r.ExpiresAt.After(now) || r.ExpiresAt.After(now.Add(ContinuationLifetime)) {
		return ErrContract
	}
	if len(r.Metadata) > metadataLimit {
		return ErrMetadataTooLarge
	}
	return nil
}

func (s *Store) ClaimForDispatch(ctx context.Context, r *Resource, payload []byte) (*Resource, bool, error) {
	if err := s.validateContract(r, payload); err != nil {
		return nil, false, err
	}
	if r.Kind != KindContinuation || r.SubmissionID == nil {
		return nil, false, ErrSubmission
	}
	if r.TargetID == "" {
		return nil, false, ErrContract
	}
	if err := ValidateSubmission(*r.SubmissionID, time.Now()); err != nil {
		return nil, false, err
	}
	id, err := newUUIDv7()
	if err != nil {
		return nil, false, err
	}
	prepared := *r
	prepared.UUID = id
	prepared.State = StateDispatching
	prepared.UpstreamID = id.String()
	if len(prepared.Metadata) == 0 {
		prepared.Metadata = []byte(`{}`)
	}
	ciphertext, err := s.keys.Seal(s.installation, secrets.ProviderContinuation, prepared.UUID.String(), payload)
	if err != nil {
		return nil, false, err
	}
	out, err := scan(s.pool.QueryRow(ctx, `WITH owner AS MATERIALIZED (
 SELECT id FROM olp.api_keys WHERE id=$3 AND revoked_at IS NULL
   AND (expires_at IS NULL OR expires_at>now()) FOR SHARE
), parent AS MATERIALIZED (
 SELECT id FROM olp.provider_resources WHERE id=$15 AND api_key_id=$3
   AND kind=$2 AND state=$19 AND expires_at>now() FOR SHARE
), active AS MATERIALIZED (
 SELECT active_key_version FROM olp.installation WHERE singleton FOR SHARE
), inserted AS (
 INSERT INTO olp.provider_resources
 (id,kind,api_key_id,route_slug,provider_id,provider_revision_id,route_revision_id,slot_id,credential_id,upstream_id,state,metadata,expires_at,contract_version,parent_id,submission_id,target_id)
 SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$21
 FROM owner CROSS JOIN active
 WHERE active.active_key_version=$17
   AND ($15::uuid IS NULL OR EXISTS(SELECT 1 FROM parent))
 ON CONFLICT(api_key_id,submission_id) WHERE submission_id IS NOT NULL DO NOTHING
 RETURNING `+columns+`
), stored AS (
 INSERT INTO olp.secrets(id,purpose,key_version,ciphertext,expires_at)
 SELECT id,$20,$17,$18,expires_at FROM inserted
 ON CONFLICT(id) DO UPDATE SET key_version=excluded.key_version,
   ciphertext=excluded.ciphertext,expires_at=excluded.expires_at RETURNING id
)
SELECT `+columns+` FROM inserted JOIN stored USING(id)`,
		prepared.UUID, prepared.Kind, prepared.APIKeyID, prepared.RouteSlug, prepared.ProviderID, prepared.ProviderRevisionID, prepared.RouteRevisionID, prepared.SlotID, prepared.CredentialID, prepared.UpstreamID, prepared.State, prepared.Metadata, prepared.ExpiresAt, prepared.ContractVersion, prepared.ParentID, prepared.SubmissionID, s.keys.Active, ciphertext, StateReady, secrets.ProviderContinuation, prepared.TargetID))
	if errors.Is(err, pgx.ErrNoRows) {
		existing, claimErr := s.existingClaim(ctx, r)
		return existing, false, claimErr
	}
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func (s *Store) existingClaim(ctx context.Context, r *Resource) (*Resource, error) {
	return withReadTxValue(ctx, s.pool, func(tx pgx.Tx) (*Resource, error) {
		if err := authorizeOwner(ctx, tx, r.APIKeyID); err != nil {
			return nil, err
		}
		if r.ParentID != nil {
			var valid bool
			err := tx.QueryRow(ctx, `SELECT true FROM olp.provider_resources WHERE id=$1 AND api_key_id=$2 AND kind=$3 AND state=$4 AND expires_at>now() FOR SHARE`,
				*r.ParentID, r.APIKeyID, KindContinuation, StateReady).Scan(&valid)
			if err != nil || !valid {
				return nil, ErrNotFound
			}
		}
		out, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM olp.provider_resources WHERE api_key_id=$1 AND submission_id=$2 AND kind=$3 AND state<>$4 AND expires_at>now()`,
			r.APIKeyID, *r.SubmissionID, KindContinuation, StateDeleted))
		if errors.Is(err, pgx.ErrNoRows) {
			var active int
			if keyErr := tx.QueryRow(ctx, `SELECT active_key_version FROM olp.installation WHERE singleton FOR SHARE`).Scan(&active); keyErr != nil {
				return nil, keyErr
			}
			if active != s.keys.Active {
				return nil, errors.New("reload the active master key before writing secrets")
			}
			return nil, ErrNotFound
		}
		return out, err
	})
}

func (s *Store) MarkUnknown(ctx context.Context, r *Resource) error {
	return transitionContinuation(ctx, s.pool, r, StateDispatching, StateUnknown)
}

func transitionContinuation(ctx context.Context, pool *pgxpool.Pool, r *Resource, from, to string) error {
	tag, err := pool.Exec(ctx, `UPDATE olp.provider_resources SET state=$4,updated_at=now() WHERE id=$1 AND api_key_id=$2 AND state=$3 AND kind=$5 AND expires_at>now()`, r.UUID, r.APIKeyID, from, to, KindContinuation)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrTransition
	}
	return nil
}

func (s *Store) CompleteContinuation(ctx context.Context, r *Resource, payload []byte) error {
	if err := s.validateContract(r, payload); err != nil {
		return err
	}
	ciphertext, err := s.keys.Seal(s.installation, secrets.ProviderContinuation, r.UUID.String(), payload)
	if err != nil {
		return err
	}
	var owner, active, updated, stored bool
	err = s.pool.QueryRow(ctx, `WITH owner AS MATERIALIZED (
 SELECT id FROM olp.api_keys WHERE id=$1 AND revoked_at IS NULL
   AND (expires_at IS NULL OR expires_at>now()) FOR SHARE
), active AS MATERIALIZED (
 SELECT active_key_version FROM olp.installation WHERE singleton FOR SHARE
), updated AS (
 UPDATE olp.provider_resources AS r SET state=$4,updated_at=now()
 FROM owner,active
 WHERE r.id=$2 AND r.api_key_id=$1 AND r.state=$3 AND r.kind=$5
   AND r.expires_at>now() AND active.active_key_version=$6 RETURNING r.id
), stored AS (
 INSERT INTO olp.secrets(id,purpose,key_version,ciphertext,expires_at)
 SELECT id,$7,$6,$8,$9 FROM updated
 ON CONFLICT(id) DO UPDATE SET key_version=excluded.key_version,
   ciphertext=excluded.ciphertext,expires_at=excluded.expires_at RETURNING id
)
SELECT EXISTS(SELECT 1 FROM owner),
       EXISTS(SELECT 1 FROM active WHERE active_key_version=$6),
       EXISTS(SELECT 1 FROM updated),EXISTS(SELECT 1 FROM stored)`,
		r.APIKeyID, r.UUID, StateDispatching, StateReady, KindContinuation,
		s.keys.Active, secrets.ProviderContinuation, ciphertext, r.ExpiresAt).Scan(&owner, &active, &updated, &stored)
	if err != nil {
		return err
	}
	if !owner {
		return ErrNotFound
	}
	if !active {
		return errors.New("reload the active master key before writing secrets")
	}
	if !updated {
		return ErrTransition
	}
	if !stored {
		return ErrContract
	}
	return nil
}

func (s *Store) ReadContract(ctx context.Context, kind, owner, localID string) (*Resource, []byte, error) {
	if kind != KindContinuation && kind != KindStrictResponse {
		return nil, nil, ErrContract
	}
	return s.readSealedContract(ctx, kind, owner, localID, MaxContinuationBytes, nil)
}

func (s *Store) readSealedContract(ctx context.Context, kind, owner, localID string, maxBytes int, valid func(*Resource) bool) (*Resource, []byte, error) {
	if !s.Encrypted() {
		return nil, nil, ErrContract
	}
	parsedKind, id, err := ParseLocalID(localID)
	if err != nil || parsedKind != kind {
		return nil, nil, ErrNotFound
	}
	sealed, err := withReadTxValue(ctx, s.pool, func(tx pgx.Tx) (sealedContract, error) {
		if err := authorizeOwner(ctx, tx, owner); err != nil {
			return sealedContract{}, err
		}
		r, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM olp.provider_resources WHERE id=$1 AND kind=$2 AND api_key_id=$3 AND state<>$4 AND expires_at>now() FOR SHARE`, id, kind, owner, StateDeleted))
		if errors.Is(err, pgx.ErrNoRows) {
			return sealedContract{}, ErrNotFound
		}
		if err != nil {
			return sealedContract{}, err
		}
		if valid != nil && !valid(r) {
			return sealedContract{}, ErrContract
		}
		payload, err := s.keys.Read(ctx, tx, s.installation, id.String(), secrets.ProviderContinuation)
		if err != nil || len(payload) == 0 || len(payload) > maxBytes {
			return sealedContract{}, ErrContract
		}
		return sealedContract{resource: r, payload: payload}, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return sealed.resource, sealed.payload, nil
}

func (s *Store) FindSubmission(ctx context.Context, owner, submission string) (*Resource, []byte, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT id FROM olp.provider_resources WHERE api_key_id=$1 AND submission_id=$2 AND kind=$3`, owner, submission, KindContinuation).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	return s.ReadContract(ctx, KindContinuation, owner, LocalID(KindContinuation, id))
}

func authorizeOwner(ctx context.Context, tx pgx.Tx, owner string) error {
	var valid bool
	err := tx.QueryRow(ctx, `SELECT true FROM olp.api_keys WHERE id=$1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>now()) FOR SHARE`, owner).Scan(&valid)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("resource owner: %w", err)
	}
	return nil
}

func (s *Store) PutContract(ctx context.Context, r *Resource, payload []byte) (*Resource, error) {
	if err := s.validateContract(r, payload); err != nil {
		return nil, err
	}
	if r.Kind != KindStrictResponse || r.SubmissionID != nil || r.TargetID == "" {
		return nil, ErrContract
	}
	id, err := newUUIDv7()
	if err != nil {
		return nil, err
	}
	prepared := *r
	prepared.UUID = id
	if len(prepared.Metadata) == 0 {
		prepared.Metadata = []byte(`{}`)
	}
	return withTxValue(ctx, s.pool, func(tx pgx.Tx) (*Resource, error) {
		if err := authorizeOwner(ctx, tx, prepared.APIKeyID); err != nil {
			return nil, err
		}
		return s.insertSealedContract(ctx, tx, &prepared, payload)
	})
}

func (s *Store) insertSealedContract(ctx context.Context, tx pgx.Tx, r *Resource, payload []byte) (*Resource, error) {
	out, err := insertResource(ctx, tx, r)
	if err != nil {
		return nil, err
	}
	if err := s.keys.Store(ctx, tx, s.installation, out.UUID.String(), secrets.ProviderContinuation, payload, out.ExpiresAt); err != nil {
		return nil, err
	}
	return out, nil
}
