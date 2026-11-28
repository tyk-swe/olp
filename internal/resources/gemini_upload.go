package resources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/platform/secrets"
)

func (s *Store) PutGeminiUploadSession(ctx context.Context, resource *Resource, uploadURL string) (*Resource, error) {
	now := time.Now()
	if !s.Encrypted() || resource == nil || resource.Kind != KindGeminiUpload ||
		resource.UUID == uuid.Nil || resource.ExpiresAt == nil || !resource.ExpiresAt.After(now) ||
		resource.ExpiresAt.After(now.Add(time.Hour)) || uploadURL == "" {
		return nil, ErrContract
	}
	return withTxValue(ctx, s.pool, func(tx pgx.Tx) (*Resource, error) {
		out, err := s.PutTx(ctx, tx, resource)
		if err != nil {
			return nil, err
		}
		if err := s.keys.Store(ctx, tx, s.installation, out.UUID.String(), secrets.ProviderUploadSession, []byte(uploadURL), out.ExpiresAt); err != nil {
			return nil, err
		}
		return out, nil
	})
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

func lockActiveGeminiUpload(ctx context.Context, tx pgx.Tx, owner string, sessionID uuid.UUID, finalOffset int64) error {
	var state string
	var metadata []byte
	err := tx.QueryRow(ctx, `SELECT state,metadata FROM olp.provider_resources
		WHERE id=$1 AND kind=$2 AND api_key_id=$3 AND state<>$4 AND (expires_at IS NULL OR expires_at>now()) FOR UPDATE`,
		sessionID, KindGeminiUpload, owner, StateDeleted).Scan(&state, &metadata)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if state != "active" {
		return ErrTransition
	}
	var upload struct {
		Expected int64 `json:"expected_length"`
	}
	if json.Unmarshal(metadata, &upload) != nil || upload.Expected <= 0 || finalOffset != upload.Expected {
		return ErrContract
	}
	return nil
}

func closeGeminiUploadTx(ctx context.Context, tx pgx.Tx, owner string, sessionID uuid.UUID, offsetMeta []byte) error {
	tag, err := tx.Exec(ctx, `UPDATE olp.provider_resources SET state=$2,metadata=metadata||$3::jsonb,updated_at=now()
		WHERE id=$1 AND kind=$4 AND api_key_id=$5 AND state=$6`,
		sessionID, StateDeleted, offsetMeta, KindGeminiUpload, owner, "active")
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrTransition
	}
	return secrets.Delete(ctx, tx, secrets.ProviderUploadSession, sessionID.String())
}

func (s *Store) FinalizeGeminiUpload(ctx context.Context, owner string, sessionID uuid.UUID, finalOffset int64, file *Resource) (*Resource, error) {
	if !s.Encrypted() || sessionID == uuid.Nil || file == nil || file.Kind != KindGeminiFile || file.APIKeyID != owner {
		return nil, ErrContract
	}
	offsetMeta, err := json.Marshal(struct {
		Offset int64 `json:"offset"`
	}{Offset: finalOffset})
	if err != nil {
		return nil, err
	}
	return withTxValue(ctx, s.pool, func(tx pgx.Tx) (*Resource, error) {
		if err := lockActiveGeminiUpload(ctx, tx, owner, sessionID, finalOffset); err != nil {
			return nil, err
		}
		created, err := s.PutTx(ctx, tx, file)
		if err != nil {
			return nil, err
		}
		if err := closeGeminiUploadTx(ctx, tx, owner, sessionID, offsetMeta); err != nil {
			return nil, err
		}
		return created, nil
	})
}
