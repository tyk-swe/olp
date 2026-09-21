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
)

const (
	KindFile     = "file"
	KindBatch    = "batch"
	KindResponse = "response"
)

const StateDeleted = "deleted"

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
	SlotID             string          `json:"slot_id"`
	CredentialID       *string         `json:"credential_id,omitempty"`
	UpstreamID         string          `json:"upstream_id"`
	State              string          `json:"state"`
	Metadata           json.RawMessage `json:"metadata"`
	ExpiresAt          *time.Time      `json:"expires_at,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

func LocalID(kind string, id uuid.UUID) string {
	return kind + "_" + strings.ReplaceAll(id.String(), "-", "")
}

func parseLocal(local string) (uuid.UUID, error) {
	i := strings.IndexByte(local, '_')
	if i <= 0 {
		return uuid.Nil, ErrNotFound
	}
	rest := local[i+1:]
	if len(rest) != 32 {
		return uuid.Nil, ErrNotFound
	}
	id, err := uuid.Parse(rest[:8] + "-" + rest[8:12] + "-" + rest[12:16] + "-" + rest[16:20] + "-" + rest[20:])
	if err != nil {
		return uuid.Nil, ErrNotFound
	}
	return id, nil
}

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Begin(ctx context.Context) (pgx.Tx, error) {
	return s.pool.Begin(ctx)
}

const columns = `id,kind,api_key_id,route_slug,provider_id,provider_revision_id,route_revision_id,slot_id,credential_id,upstream_id,state,metadata,expires_at,created_at,updated_at`

func scan(row pgx.Row) (*Resource, error) {
	var r Resource
	var metadata []byte
	err := row.Scan(&r.UUID, &r.Kind, &r.APIKeyID, &r.RouteSlug, &r.ProviderID,
		&r.ProviderRevisionID, &r.RouteRevisionID, &r.SlotID, &r.CredentialID, &r.UpstreamID,
		&r.State, &metadata, &r.ExpiresAt, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	r.ID = LocalID(r.Kind, r.UUID)
	r.Metadata = metadata
	return &r, nil
}

func (s *Store) Put(ctx context.Context, r *Resource) (*Resource, error) {
	if len(r.Metadata) == 0 {
		r.Metadata = json.RawMessage(`{}`)
	}
	if len(r.Metadata) > metadataLimit {
		return nil, ErrMetadataTooLarge
	}
	r.UUID = uuid.Must(uuid.NewV7())
	out, err := scan(s.pool.QueryRow(ctx, `INSERT INTO olp_go.provider_resources
		(id,kind,api_key_id,route_slug,provider_id,provider_revision_id,route_revision_id,slot_id,credential_id,upstream_id,state,metadata,expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING `+columns,
		r.UUID, r.Kind, r.APIKeyID, r.RouteSlug, r.ProviderID, r.ProviderRevisionID,
		r.RouteRevisionID, r.SlotID, r.CredentialID, r.UpstreamID, r.State, r.Metadata, r.ExpiresAt))
	if err != nil {
		return nil, fmt.Errorf("provider resource put: %w", err)
	}
	return out, nil
}

func (s *Store) Get(ctx context.Context, kind, apiKeyID, localID string) (*Resource, error) {
	id, err := parseLocal(localID)
	if err != nil || !strings.HasPrefix(localID, kind+"_") {
		return nil, ErrNotFound
	}
	r, err := scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM olp_go.provider_resources
		WHERE kind=$1 AND id=$2 AND api_key_id=$3 AND state<>$4`,
		kind, id, apiKeyID, StateDeleted))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("provider resource get: %w", err)
	}
	return r, nil
}

func (s *Store) GetByUpstream(ctx context.Context, kind, apiKeyID, providerID, upstreamID string) (*Resource, error) {
	r, err := scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM olp_go.provider_resources
		WHERE kind=$1 AND api_key_id=$2 AND provider_id=$3 AND upstream_id=$4 AND state<>$5`,
		kind, apiKeyID, providerID, upstreamID, StateDeleted))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("provider resource get by upstream: %w", err)
	}
	return r, nil
}

func (s *Store) List(ctx context.Context, kind, apiKeyID string, limit int, afterID string) ([]*Resource, error) {
	var after uuid.UUID
	if afterID != "" {
		parsed, err := parseLocal(afterID)
		if err != nil {
			return nil, ErrNotFound
		}
		after = parsed
	}
	rows, err := s.pool.Query(ctx, `SELECT `+columns+` FROM olp_go.provider_resources
		WHERE kind=$1 AND api_key_id=$2 AND state<>$3
		AND ($4::uuid IS NULL OR (created_at,id)<(SELECT created_at,id FROM olp_go.provider_resources WHERE id=$4))
		ORDER BY created_at DESC, id DESC LIMIT $5`,
		kind, apiKeyID, StateDeleted, nilUUID(after), limit)
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
	tag, err := s.pool.Exec(ctx, `UPDATE olp_go.provider_resources
		SET state=COALESCE($2,state), metadata=metadata||COALESCE($3,'{}'::jsonb),
			expires_at=COALESCE($4,expires_at), updated_at=now()
		WHERE id=$1 AND state<>$5`,
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
	tag, err := s.pool.Exec(ctx, `UPDATE olp_go.provider_resources
		SET state=$2, updated_at=now() WHERE id=$1 AND state<>$2`, id, StateDeleted)
	if err != nil {
		return fmt.Errorf("provider resource tombstone: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CleanupExpired(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM olp_go.provider_resources WHERE expires_at IS NOT NULL AND expires_at<$1`, now)
	if err != nil {
		return 0, fmt.Errorf("provider resource cleanup: %w", err)
	}
	return tag.RowsAffected(), nil
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
