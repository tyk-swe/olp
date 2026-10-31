package billing

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
)

// ImportState is the stored billing import configuration promotion compares
// documents against.
type ImportState struct {
	Source       string
	Enabled      bool
	Scope        json.RawMessage
	CredentialID *string
}

// ReadImportState reads one provider's import settings, or nil when it has
// none.
func ReadImportState(ctx context.Context, q access.Queryer, providerID string) (*ImportState, error) {
	var s ImportState
	err := q.QueryRow(ctx, `SELECT source,enabled,scope,credential_id::text
		FROM olp.provider_billing_imports WHERE provider_id=$1::uuid`, providerID).
		Scan(&s.Source, &s.Enabled, &s.Scope, &s.CredentialID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// ImportCredential stores a billing credential under its purpose and returns
// the new secret's id.
func ImportCredential(ctx context.Context, tx pgx.Tx, keys *secrets.KeyRing, installation, source, credential string) (string, error) {
	document, err := encodeCredential(source, credential)
	if err != nil {
		return "", err
	}
	defer clear(document)
	id := access.NewID()
	if err := keys.Store(ctx, tx, installation, id, secrets.BillingCredential, document, nil); err != nil {
		return "", err
	}
	return id, nil
}

// ImportCredentialMatches reports whether the stored credential holds the
// same material as the supplied secret.
func ImportCredentialMatches(ctx context.Context, q access.Queryer, keys *secrets.KeyRing, installation, source string, credentialID *string, credential string) (bool, error) {
	if credentialID == nil {
		return false, nil
	}
	current, err := keys.Read(ctx, q, installation, *credentialID, secrets.BillingCredential)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer clear(current)
	if source == SourceGoogle {
		expected, err := encodeGoogleCredential(credential)
		if err != nil {
			return false, err
		}
		defer clear(expected)
		return sha256.Sum256(current) == sha256.Sum256(expected), nil
	}
	key, err := decodeCredential(source, current)
	if errors.Is(err, errIncompatibleCredential) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer clear(key)
	return sha256.Sum256(key) == sha256.Sum256([]byte(credential)), nil
}

// ApplyImport creates, replaces or deletes a provider's billing import to
// match desired settings. Configuration promotion calls it inside its
// transaction; the API's PUT handler writes the same columns.
func ApplyImport(ctx context.Context, tx pgx.Tx, providerID, source string, enabled bool, scope json.RawMessage, credentialID *string, createdBy string) error {
	if scope == nil {
		scope = json.RawMessage(`{}`)
	}
	canonical, err := ValidateScope(source, scope)
	if err != nil {
		return err
	}
	if existing, err := ReadImportState(ctx, tx, providerID); err != nil {
		return err
	} else if existing == nil {
		_, err = tx.Exec(ctx, `INSERT INTO olp.provider_billing_imports
			(provider_id,source,credential_id,scope,enabled,etag,created_by,next_import_at)
			VALUES($1::uuid,$2,$3::uuid,$4::jsonb,$5,$6,$7::uuid,now())`,
			providerID, source, credentialID, canonical, enabled, access.NewID(), createdBy)
	} else {
		var old *string
		if existing.CredentialID != nil && (credentialID == nil || *existing.CredentialID != *credentialID) {
			old = existing.CredentialID
		}
		_, err = tx.Exec(ctx, `UPDATE olp.provider_billing_imports SET
			source=$2,credential_id=$3::uuid,scope=$4::jsonb,enabled=$5,etag=$6,updated_at=now()
			WHERE provider_id=$1::uuid`,
			providerID, source, credentialID, canonical, enabled, access.NewID())
		if err == nil && old != nil {
			_, err = tx.Exec(ctx, `DELETE FROM olp.secrets WHERE id=$1::uuid AND purpose=$2`, *old, secrets.BillingCredential)
		}
	}
	if err != nil {
		return err
	}
	return syncImportRevision(ctx, tx, providerID, source, canonical)
}

// RemoveImport deletes a provider's billing import and its sealed credential.
// Imported costs are kept.
func RemoveImport(ctx context.Context, tx pgx.Tx, providerID string) error {
	existing, err := ReadImportState(ctx, tx, providerID)
	if err != nil {
		return err
	}
	if existing == nil {
		return nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM olp.provider_billing_imports WHERE provider_id=$1::uuid`, providerID); err != nil {
		return err
	}
	if existing.CredentialID != nil {
		_, err = tx.Exec(ctx, `DELETE FROM olp.secrets WHERE id=$1::uuid AND purpose=$2`, *existing.CredentialID, secrets.BillingCredential)
	}
	return err
}
