package resources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/secrets"
)

func (s *Store) PutGeminiUploadSession(ctx context.Context, resource *Resource, uploadURL string) (*Resource, error) {
	if !s.Encrypted() || resource == nil || resource.Kind != KindGeminiUpload ||
		resource.UUID == uuid.Nil || resource.ExpiresAt == nil || !resource.ExpiresAt.After(time.Now()) ||
		resource.ExpiresAt.After(time.Now().Add(time.Hour)) || uploadURL == "" {
		return nil, ErrContract
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	out, err := s.PutTx(ctx, tx, resource)
	if err != nil {
		return nil, err
	}
	if err := s.keys.Store(ctx, tx, s.installation, out.UUID.String(), secrets.ProviderUploadSession, []byte(uploadURL), out.ExpiresAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) ReadGeminiUploadURL(ctx context.Context, id uuid.UUID) (string, error) {
	if !s.Encrypted() || id == uuid.Nil {
		return "", ErrContract
	}
	plain, err := s.keys.Read(ctx, s.pool, s.installation, id.String(), secrets.ProviderUploadSession)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("Gemini upload session secret: %w", err)
	}
	return string(plain), nil
}

func (s *Store) FinalizeGeminiUpload(ctx context.Context, owner string, sessionID uuid.UUID, finalOffset int64, file *Resource) (*Resource, error) {
	if !s.Encrypted() || sessionID == uuid.Nil || file == nil || file.Kind != KindGeminiFile || file.APIKeyID != owner {
		return nil, ErrContract
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var state string
	var metadata []byte
	if err := tx.QueryRow(ctx, `SELECT state,metadata FROM olp.provider_resources
		WHERE id=$1 AND kind=$2 AND api_key_id=$3 AND state<>$4 AND (expires_at IS NULL OR expires_at>now()) FOR UPDATE`,
		sessionID, KindGeminiUpload, owner, StateDeleted).Scan(&state, &metadata); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if state != "active" {
		return nil, ErrTransition
	}
	var upload struct {
		Expected int64 `json:"expected_length"`
	}
	if json.Unmarshal(metadata, &upload) != nil || upload.Expected <= 0 || finalOffset != upload.Expected {
		return nil, ErrContract
	}
	created, err := s.PutTx(ctx, tx, file)
	if err != nil {
		return nil, err
	}
	tag, err := tx.Exec(ctx, `UPDATE olp.provider_resources SET state=$2,metadata=metadata||$3::jsonb,updated_at=now()
		WHERE id=$1 AND kind=$4 AND api_key_id=$5 AND state=$6`, sessionID, StateDeleted,
		[]byte(fmt.Sprintf(`{"offset":%d}`, finalOffset)), KindGeminiUpload, owner, "active")
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() != 1 {
		return nil, ErrTransition
	}
	if _, err := tx.Exec(ctx, "DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2", sessionID, secrets.ProviderUploadSession); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return created, nil
}
