package resources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

// Strict and transformed resources keep separate kinds because their storage
// contracts differ: a strict kind stores an encrypted native payload under a
// contract version, while a transformed kind stores metadata only.
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

// Only retained generation owners carry gateway-authored deferred usage.
// Other resource metadata may contain opaque native fields with the same name.
func ownsPendingUsage(kind string) bool {
	return kind == KindResponse || kind == KindStrictResponse || kind == KindInteraction
}

var ErrNotFound = errors.New("provider resource not found")

var ErrMetadataTooLarge = errors.New("provider resource metadata exceeds 16 KiB")

const metadataLimit = 16384

type Resource struct {
	ID string `json:"id"`

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
	ContractVersion    *string         `json:"contract_version,omitempty"`
	ParentID           *uuid.UUID      `json:"-"`
	SubmissionID       *string         `json:"-"`
}

func LocalID(kind string, id uuid.UUID) string {
	return kind + "_" + strings.ReplaceAll(id.String(), "-", "")
}

func parseLocal(local string) (uuid.UUID, error) {
	i := strings.LastIndexByte(local, '_')
	if i <= 0 {
		return uuid.Nil, ErrNotFound
	}
	rest := local[i+1:]
	if len(rest) != 32 {
		return uuid.Nil, ErrNotFound
	}
	id, err := uuid.Parse(rest[:8] + "-" + rest[8:12] + "-" + rest[12:16] + "-" + rest[16:20] + "-" + rest[20:])
	if err != nil || LocalID(local[:i], id) != local {
		return uuid.Nil, ErrNotFound
	}
	switch local[:i] {
	case KindFile, KindBatch, KindResponse, KindContinuation, KindStrictResponse, KindInteraction,
		KindStrictFile, KindStrictBatch, KindAnthropicFile, KindAnthropicBatch, KindGeminiUpload,
		KindGeminiFile, KindGeminiBatch, KindCloudBatch:
	default:
		return uuid.Nil, ErrNotFound
	}
	return id, nil
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

const columns = `id,kind,api_key_id,route_slug,provider_id,provider_revision_id,route_revision_id,slot_id,credential_id,upstream_id,state,metadata,expires_at,created_at,updated_at,contract_version,parent_id,submission_id,target_id`

func scan(row pgx.Row) (*Resource, error) {
	var r Resource
	var metadata []byte
	var targetID *string
	err := row.Scan(&r.UUID, &r.Kind, &r.APIKeyID, &r.RouteSlug, &r.ProviderID,
		&r.ProviderRevisionID, &r.RouteRevisionID, &r.SlotID, &r.CredentialID, &r.UpstreamID,
		&r.State, &metadata, &r.ExpiresAt, &r.CreatedAt, &r.UpdatedAt, &r.ContractVersion, &r.ParentID, &r.SubmissionID, &targetID)
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
	if r.TargetID == "" {
		return nil, ErrContract
	}
	if r.Kind == KindContinuation || r.Kind == KindStrictResponse || r.Kind == KindStrictFile || r.Kind == KindStrictBatch {
		return nil, ErrContract
	}
	if len(r.Metadata) == 0 {
		r.Metadata = json.RawMessage(`{}`)
	}
	if len(r.Metadata) > metadataLimit {
		return nil, ErrMetadataTooLarge
	}

	if r.ExpiresAt == nil && ownsPendingUsage(r.Kind) {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(r.Metadata, &fields); err != nil {
			return nil, err
		}
		if pending := fields["pending_usage"]; len(pending) > 0 && string(pending) != "null" {
			expires := time.Now().Add(ContinuationLifetime)
			r.ExpiresAt = &expires
		}
	}
	r.UUID = uuid.Must(uuid.NewV7())
	out, err := scan(s.pool.QueryRow(ctx, `INSERT INTO olp.provider_resources
		(id,kind,api_key_id,route_slug,provider_id,provider_revision_id,route_revision_id,slot_id,credential_id,upstream_id,state,metadata,expires_at,target_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		RETURNING `+columns,
		r.UUID, r.Kind, r.APIKeyID, r.RouteSlug, r.ProviderID, r.ProviderRevisionID,
		r.RouteRevisionID, r.SlotID, r.CredentialID, r.UpstreamID, r.State, r.Metadata, r.ExpiresAt, r.TargetID))
	if err != nil {
		return nil, fmt.Errorf("provider resource put: %w", err)
	}
	return out, nil
}

func (s *Store) Get(ctx context.Context, kind, apiKeyID, localID string) (*Resource, error) {
	id, err := parseLocal(localID)
	if err != nil || LocalID(kind, id) != localID {
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

// NativeIdentity names one resource within the immutable serving source and
// caller that accepted it. CredentialID is the credential-version row identity.
// TargetID identifies the selected model in the exact historical route revision.
type NativeIdentity struct {
	Kind, APIKeyID, RouteSlug, ProviderID, ProviderRevisionID, RouteRevisionID, TargetID, SlotID, UpstreamID string
	CredentialID                                                                                             *string
}

func (r *Resource) NativeIdentity() NativeIdentity {
	return NativeIdentity{Kind: r.Kind, APIKeyID: r.APIKeyID, RouteSlug: r.RouteSlug,
		ProviderID: r.ProviderID, ProviderRevisionID: r.ProviderRevisionID,
		RouteRevisionID: r.RouteRevisionID, TargetID: r.TargetID, SlotID: r.SlotID,
		CredentialID: r.CredentialID, UpstreamID: r.UpstreamID}
}

func (s *Store) GetByUpstream(ctx context.Context, identity NativeIdentity) (*Resource, error) {
	if identity.TargetID == "" {
		return nil, ErrContract
	}
	r, err := scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM olp.provider_resources
		WHERE kind=$1 AND api_key_id=$2 AND provider_id=$3 AND provider_revision_id=$4
		AND route_slug=$5 AND route_revision_id=$6 AND target_id=$7 AND slot_id=$8
		AND credential_id IS NOT DISTINCT FROM $9::uuid AND upstream_id=$10
		AND state<>$11 AND (expires_at IS NULL OR expires_at>now())`,
		identity.Kind, identity.APIKeyID, identity.ProviderID, identity.ProviderRevisionID,
		identity.RouteSlug, identity.RouteRevisionID, identity.TargetID, identity.SlotID,
		identity.CredentialID, identity.UpstreamID, StateDeleted))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("provider resource get by upstream: %w", err)
	}
	return r, nil
}

func (s *Store) List(ctx context.Context, kind, apiKeyID string, limit int, afterID string) ([]*Resource, error) {
	return s.ListKinds(ctx, []string{kind}, apiKeyID, limit, afterID)
}

// ListKinds joins transformed and strict resource identities without changing
// either storage contract. The cursor belongs to this owner and one of the
// requested kinds; an unrelated local ID cannot move the page boundary.
func (s *Store) ListKinds(ctx context.Context, kinds []string, apiKeyID string, limit int, afterID string) ([]*Resource, error) {
	var after uuid.UUID
	if afterID != "" {
		parsed, err := parseLocal(afterID)
		valid := false
		for _, kind := range kinds {
			valid = valid || LocalID(kind, parsed) == afterID
		}
		if err != nil || !valid {
			return nil, ErrNotFound
		}
		after = parsed
	}
	rows, err := s.pool.Query(ctx, `SELECT `+columns+` FROM olp.provider_resources
		WHERE kind=ANY($1) AND api_key_id=$2 AND state<>$3 AND (expires_at IS NULL OR expires_at>now())
		AND ($4::uuid IS NULL OR (created_at,id)<(SELECT created_at,id FROM olp.provider_resources
		WHERE id=$4 AND api_key_id=$2 AND kind=ANY($1) AND state<>$3 AND (expires_at IS NULL OR expires_at>now())))
		ORDER BY created_at DESC, id DESC LIMIT $5`,
		kinds, apiKeyID, StateDeleted, nilUUID(after), limit)
	if err != nil {
		return nil, fmt.Errorf("provider resource list: %w", err)
	}
	defer rows.Close()
	out := []*Resource{}
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("provider resource list scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Update(ctx context.Context, localID, state string, metadata json.RawMessage, expiresAt *time.Time) error {
	if len(metadata) > metadataLimit {
		return ErrMetadataTooLarge
	}
	id, err := parseLocal(localID)
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
	id, err := parseLocal(localID)
	if err != nil {
		return ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var kind string
	var pending []byte
	if err = tx.QueryRow(ctx, `SELECT kind,metadata->'pending_usage' FROM olp.provider_resources WHERE id=$1 AND state<>$2 FOR UPDATE`, id, StateDeleted).Scan(&kind, &pending); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if ownsPendingUsage(kind) {
		if _, err = persistPendingUsage(ctx, tx, pending, usage.AttemptUsage{BillingUncertain: true}, time.Now(), false); err != nil {
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
	if _, err = tx.Exec(ctx, `DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2`, id, secrets.ProviderContinuation); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nilUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}
