package providers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/secretstore"
)

// BindDraftCredential stages an already stored destination credential. A new
// identity clears slot evidence and advances both mutation preconditions; a
// repeated binding is a no-op. The caller owns authorization and transaction.
func (s *Catalogue) BindDraftCredential(ctx context.Context, tx pgx.Tx, providerID, slotID, credentialID string) error {
	tag, err := tx.Exec(ctx, `UPDATE olp.provider_slots SET credential_id=$3,validated_at=NULL,validated_fingerprint=NULL
  WHERE provider_id=$1 AND id=$2 AND credential_id IS DISTINCT FROM $3::uuid`, providerID, slotID, credentialID)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	_, err = touchSlots(ctx, tx, providerID)
	return err
}

func (s *Catalogue) CredentialEquals(ctx context.Context, q access.Queryer, credentialID, secret string) (bool, error) {
	current, err := s.Access.Keys.Read(ctx, q, s.Access.Installation, credentialID, secrets.ProviderCredential)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer clear(current)
	return sha256.Sum256(current) == sha256.Sum256([]byte(secret)), nil
}

func (s *Catalogue) ExternalCredentialEquals(ctx context.Context, q access.Queryer, credentialID string, reference secretstore.Reference) (bool, error) {
	var data []byte
	err := q.QueryRow(ctx, "SELECT external_reference FROM olp.provider_credentials WHERE id=$1 AND revoked_at IS NULL", credentialID).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var current secretstore.Reference
	return len(data) > 0 && json.Unmarshal(data, &current) == nil && current == reference, nil
}
