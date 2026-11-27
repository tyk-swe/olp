package resources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/usage"
)

const (
	KindFile           = "file"
	KindBatch          = "batch"
	KindResponse       = "response"
	KindContinuation   = "continuation"
	KindStrictResponse = "strict_response"
	KindInteraction    = "interaction"
	KindStrictFile     = "strict_file"
	KindStrictBatch    = "strict_batch"
	KindAnthropicFile  = "anthropic_file"
	KindAnthropicBatch = "anthropic_batch"
	KindGeminiUpload   = "gemini_upload"
	KindGeminiFile     = "gemini_file"
	KindGeminiBatch    = "gemini_batch"
	KindCloudBatch     = "cloud_batch"
)

const StateDeleted = "deleted"

func ownsPendingUsage(kind string) bool {
	return kind == KindResponse || kind == KindStrictResponse || kind == KindInteraction
}

var (
	ErrNotFound         = errors.New("provider resource not found")
	ErrMetadataTooLarge = errors.New("provider resource metadata exceeds 16 KiB")
)

const metadataLimit = 16384

type Resource struct {
	RetainedAccounting *RetainedAccounting `json:"-"`
	ID                 string              `json:"id"`

	UUID               uuid.UUID       `json:"-"`
	Kind               string          `json:"kind"`
	APIKeyID           string          `json:"api_key_id"`
	RouteSlug          string          `json:"route_slug"`
	ProviderID         string          `json:"provider_id"`
	ProviderRevisionID string          `json:"provider_revision_id"`
	RouteRevisionID    string          `json:"route_revision_id"`
	TargetID           string          `json:"target_id,omitempty"`
	SlotID             string          `json:"slot_id"`
	CredentialID       *string         `json:"credential_id,omitempty"`
	UpstreamID         string          `json:"upstream_id"`
	State              string          `json:"state"`
	Metadata           json.RawMessage `json:"metadata"`
	ExpiresAt          *time.Time      `json:"expires_at,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
	MutationVersion    int64           `json:"-"`
	ContractVersion    *string         `json:"contract_version,omitempty"`
	ParentID           *uuid.UUID      `json:"-"`
	SubmissionID       *string         `json:"-"`
}

func LocalID(kind string, id uuid.UUID) string {
	return kind + "_" + strings.ReplaceAll(id.String(), "-", "")
}

type Store struct {
	pool         *pgxpool.Pool
	installation string
	keys         *secrets.KeyRing
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Begin(ctx context.Context) (pgx.Tx, error) {
	return s.pool.Begin(ctx)
}

const columns = `id,kind,api_key_id,route_slug,provider_id,provider_revision_id,route_revision_id,slot_id,credential_id,upstream_id,state,metadata,expires_at,created_at,updated_at,contract_version,parent_id,submission_id,target_id,mutation_version`

func scan(row pgx.Row) (*Resource, error) {
	var r Resource
	var metadata []byte
	var targetID *string
	err := row.Scan(&r.UUID, &r.Kind, &r.APIKeyID, &r.RouteSlug, &r.ProviderID,
		&r.ProviderRevisionID, &r.RouteRevisionID, &r.SlotID, &r.CredentialID, &r.UpstreamID,
		&r.State, &metadata, &r.ExpiresAt, &r.CreatedAt, &r.UpdatedAt, &r.ContractVersion, &r.ParentID, &r.SubmissionID, &targetID, &r.MutationVersion)
	if err != nil {
		return nil, err
	}
	r.ID = LocalID(r.Kind, r.UUID)
	r.Metadata = metadata
	if targetID != nil {
		r.TargetID = *targetID
	}
	return &r, nil
}

func (s *Store) Put(ctx context.Context, r *Resource) (*Resource, error) {
	return putResource(ctx, s.pool, r)
}

func (s *Store) PutTx(ctx context.Context, tx pgx.Tx, r *Resource) (*Resource, error) {
	return putResource(ctx, tx, r)
}

type resourceRowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func putResource(ctx context.Context, query resourceRowQuerier, r *Resource) (*Resource, error) {
	prepared, err := prepareResourceForPut(r, time.Now())
	if err != nil {
		return nil, err
	}
	out, err := insertResource(ctx, query, &prepared)
	if err != nil {
		return nil, fmt.Errorf("provider resource put: %w", err)
	}
	return out, nil
}

func prepareResourceForPut(r *Resource, now time.Time) (Resource, error) {
	if r == nil || r.TargetID == "" {
		return Resource{}, ErrContract
	}
	switch r.Kind {
	case KindContinuation, KindStrictResponse, KindStrictFile, KindStrictBatch:
		return Resource{}, ErrContract
	}
	prepared := *r
	if len(prepared.Metadata) == 0 {
		prepared.Metadata = json.RawMessage(`{}`)
	}
	if len(prepared.Metadata) > metadataLimit {
		return Resource{}, ErrMetadataTooLarge
	}
	if prepared.ExpiresAt == nil && ownsPendingUsage(prepared.Kind) {
		hasPending, err := metadataHasPendingUsage(prepared.Metadata)
		if err != nil {
			return Resource{}, err
		}
		if hasPending {
			expires := now.Add(ContinuationLifetime)
			prepared.ExpiresAt = &expires
		}
	}
	if prepared.UUID == uuid.Nil {
		id, err := newUUIDv7()
		if err != nil {
			return Resource{}, err
		}
		prepared.UUID = id
	}
	return prepared, nil
}

func metadataHasPendingUsage(raw json.RawMessage) (bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return false, err
	}
	return hasPendingUsage(fields["pending_usage"]), nil
}

func insertResource(ctx context.Context, query resourceRowQuerier, r *Resource) (*Resource, error) {
	return scan(query.QueryRow(ctx, `INSERT INTO olp.provider_resources
		(id,kind,api_key_id,route_slug,provider_id,provider_revision_id,route_revision_id,slot_id,credential_id,upstream_id,state,metadata,expires_at,target_id,parent_id,contract_version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		RETURNING `+columns,
		r.UUID, r.Kind, r.APIKeyID, r.RouteSlug, r.ProviderID, r.ProviderRevisionID,
		r.RouteRevisionID, r.SlotID, r.CredentialID, r.UpstreamID, r.State, r.Metadata, r.ExpiresAt, r.TargetID, r.ParentID, r.ContractVersion))
}

func (s *Store) Get(ctx context.Context, kind, apiKeyID, localID string) (*Resource, error) {
	parsedKind, id, err := ParseLocalID(localID)
	if err != nil || parsedKind != kind {
		return nil, ErrNotFound
	}
	r, err := scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM olp.provider_resources
		WHERE kind=$1 AND id=$2 AND api_key_id=$3 AND state<>$4 AND (expires_at IS NULL OR expires_at>now())`,
		kind, id, apiKeyID, StateDeleted))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("provider resource get: %w", err)
	}
	return r, nil
}

type NativeIdentity struct {
	Kind, APIKeyID, RouteSlug, ProviderID, ProviderRevisionID, RouteRevisionID, TargetID, SlotID, UpstreamID string
	CredentialID                                                                                             *string
	ParentID                                                                                                 *uuid.UUID
}

func (r *Resource) NativeIdentity() NativeIdentity {
	return NativeIdentity{Kind: r.Kind, APIKeyID: r.APIKeyID, RouteSlug: r.RouteSlug,
		ProviderID: r.ProviderID, ProviderRevisionID: r.ProviderRevisionID,
		RouteRevisionID: r.RouteRevisionID, TargetID: r.TargetID, SlotID: r.SlotID,
		CredentialID: r.CredentialID, UpstreamID: r.UpstreamID, ParentID: r.ParentID}
}

func (s *Store) GetByUpstream(ctx context.Context, identity NativeIdentity) (*Resource, error) {
	return s.getByUpstream(ctx, s.pool, identity, liveUpstream())
}

func (s *Store) GetByUpstreamIncludingDeleted(ctx context.Context, identity NativeIdentity) (*Resource, error) {
	return s.getByUpstream(ctx, s.pool, identity, newestUpstream())
}

func (s *Store) GetBatchFileByUpstream(ctx context.Context, identity NativeIdentity, batchID uuid.UUID) (*Resource, error) {
	return s.getByUpstream(ctx, s.pool, identity, batchUpstream(batchID))
}

func (s *Store) GetByUpstreamAsOf(ctx context.Context, identity NativeIdentity, at time.Time) (*Resource, error) {
	return s.getByUpstream(ctx, s.pool, identity, upstreamAsOf(at, nil))
}

func (s *Store) GetReferenceOf(ctx context.Context, owner *Resource, identity NativeIdentity) (*Resource, error) {
	return s.getByUpstream(ctx, s.pool, identity, upstreamAsOf(owner.CreatedAt, &owner.UUID))
}

type upstreamChoice struct {
	clause string
	args   []any
}

func liveUpstream() upstreamChoice {
	return upstreamChoice{` AND state<>$13 AND (expires_at IS NULL OR expires_at>now())`, []any{StateDeleted}}
}

func newestUpstream() upstreamChoice {
	return upstreamChoice{clause: ` ORDER BY (state='deleted') ASC,updated_at DESC,id DESC LIMIT 1`}
}

func batchUpstream(batchID uuid.UUID) upstreamChoice {
	return upstreamChoice{` AND (parent_id=$13 OR (state<>$14 AND (expires_at IS NULL OR expires_at>now())))
 ORDER BY (parent_id IS NOT DISTINCT FROM $13::uuid) DESC,(state=$14) ASC,updated_at DESC,id DESC LIMIT 1`, []any{batchID, StateDeleted}}
}

func upstreamAsOf(at time.Time, owner *uuid.UUID) upstreamChoice {
	return upstreamChoice{` ORDER BY coalesce(parent_id=$14::uuid,false) DESC,
 (created_at<=$13::timestamptz AND (state<>'deleted' OR updated_at>$13::timestamptz)) DESC,
 (state='deleted') ASC,updated_at DESC,id DESC LIMIT 1`, []any{at, owner}}
}

func (s *Store) getByUpstream(ctx context.Context, query secrets.RowQuerier, identity NativeIdentity, choice upstreamChoice) (*Resource, error) {
	if identity.TargetID == "" {
		return nil, ErrContract
	}
	statement := `SELECT ` + columns + ` FROM olp.provider_resources
		WHERE kind=$1 AND api_key_id=$2 AND provider_id=$3 AND provider_revision_id=$4
		AND route_slug=$5 AND route_revision_id=$6 AND target_id=$7 AND slot_id=$8
		AND credential_id IS NOT DISTINCT FROM $9::uuid AND upstream_id=$10
		AND (NOT $12::boolean OR parent_id IS NOT DISTINCT FROM $11::uuid)` + choice.clause
	args := slices.Concat([]any{identity.Kind, identity.APIKeyID, identity.ProviderID, identity.ProviderRevisionID,
		identity.RouteSlug, identity.RouteRevisionID, identity.TargetID, identity.SlotID, identity.CredentialID, identity.UpstreamID, identity.ParentID, slices.Contains(retainedapi.Kinds(), identity.Kind)}, choice.args)
	r, err := scan(query.QueryRow(ctx, statement, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("provider resource get by upstream: %w", err)
	}
	return r, nil
}

func (s *Store) Update(ctx context.Context, localID, state string, metadata json.RawMessage, expiresAt *time.Time) error {
	if len(metadata) > metadataLimit {
		return ErrMetadataTooLarge
	}
	_, id, err := ParseLocalID(localID)
	if err != nil {
		return ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `UPDATE olp.provider_resources
		SET state=COALESCE($2,state), metadata=metadata||COALESCE($3,'{}'::jsonb),
			expires_at=COALESCE($4,expires_at), updated_at=now()
		WHERE id=$1 AND state<>$5 AND (expires_at IS NULL OR expires_at>now())
 AND kind NOT IN ('continuation','strict_file','strict_batch') AND (kind<>'strict_response' OR ($3::jsonb IS NULL AND $4::timestamptz IS NULL))`,
		id, nilIfEmpty(state), nilIfEmpty(string(metadata)), expiresAt, StateDeleted)
	if err != nil {
		return fmt.Errorf("provider resource update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Tombstone(ctx context.Context, localID string) error {
	_, id, err := ParseLocalID(localID)
	if err != nil {
		return ErrNotFound
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		var kind string
		var pending []byte
		err := tx.QueryRow(ctx, `SELECT kind,metadata->'pending_usage' FROM olp.provider_resources WHERE id=$1 AND state<>$2 FOR UPDATE`, id, StateDeleted).Scan(&kind, &pending)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if ownsPendingUsage(kind) {
			if _, err := persistPendingUsage(ctx, tx, pending, usage.AttemptUsage{Evidence: usage.EvidenceUnobservedUncertain}, time.Now(), false); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE olp.provider_resources SET state=$2,
 metadata=CASE WHEN olp.resource_owns_pending_usage(kind) THEN metadata-'pending_usage' ELSE metadata END,
 updated_at=now() WHERE id=$1 AND state<>$2`, id, StateDeleted)
		if err != nil {
			return fmt.Errorf("provider resource tombstone: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		_, err = tx.Exec(ctx, `DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2`, id, secrets.ProviderContinuation)
		return err
	})
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
