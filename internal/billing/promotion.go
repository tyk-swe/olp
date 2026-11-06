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

type ImportState struct {
	Source       string
	Enabled      bool
	Scope        json.RawMessage
	CredentialID *string
}

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

func ImportCredentialCompatible(ctx context.Context, q access.Queryer, keys *secrets.KeyRing, installation, source string, credentialID *string) (bool, error) {
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
	key, err := decodeCredential(source, current)
	if err != nil {
		return false, nil
	}
	defer clear(key)
	return true, nil
}

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
	encoded, err := encodeCredential(source, credential)
	if err != nil {
		return false, err
	}
	defer clear(encoded)
	canonical, err := decodeCredential(source, encoded)
	if err != nil {
		return false, err
	}
	defer clear(canonical)
	return sha256.Sum256(key) == sha256.Sum256(canonical), nil
}

type ImportConfiguration struct {
	Source       string
	Enabled      bool
	Scope        json.RawMessage
	CredentialID *string
}

func SaveImport(ctx context.Context, tx pgx.Tx, providerID string, desired ImportConfiguration, createdBy string) (string, error) {
	scope := desired.Scope
	if scope == nil {
		scope = json.RawMessage(`{}`)
	}
	canonical, err := ValidateScope(desired.Source, scope)
	if err != nil {
		return "", err
	}
	existing, err := ReadImportState(ctx, tx, providerID)
	if err != nil {
		return "", err
	}
	etag := access.NewID()
	if existing == nil {
		if _, err := tx.Exec(ctx, `INSERT INTO olp.provider_billing_imports
			(provider_id,source,credential_id,scope,enabled,etag,created_by,next_import_at)
			VALUES($1::uuid,$2,$3::uuid,$4::jsonb,$5,$6,$7::uuid,now())`,
			providerID, desired.Source, desired.CredentialID, canonical, desired.Enabled, etag, createdBy); err != nil {
			return "", err
		}
	} else {
		if _, err := tx.Exec(ctx, `UPDATE olp.provider_billing_imports SET
			source=$2,credential_id=$3::uuid,scope=$4::jsonb,enabled=$5,etag=$6,updated_at=now()
			WHERE provider_id=$1::uuid`,
			providerID, desired.Source, desired.CredentialID, canonical, desired.Enabled, etag); err != nil {
			return "", err
		}
		if existing.CredentialID != nil && (desired.CredentialID == nil || *existing.CredentialID != *desired.CredentialID) {
			if _, err := tx.Exec(ctx, `DELETE FROM olp.secrets WHERE id=$1::uuid AND purpose=$2`, *existing.CredentialID, secrets.BillingCredential); err != nil {
				return "", err
			}
		}
	}
	if err := syncImportRevision(ctx, tx, providerID, desired.Source, canonical); err != nil {
		return "", err
	}
	return etag, nil
}

func RemoveImport(ctx context.Context, tx pgx.Tx, providerID string) error {
	existing, err := ReadImportState(ctx, tx, providerID)
	if err != nil {
		return err
	}
	if existing == nil {
		return nil
	}
	if err := rememberImportCoverage(ctx, tx, providerID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM olp.provider_billing_imports WHERE provider_id=$1::uuid`, providerID); err != nil {
		return err
	}
	if existing.CredentialID != nil {
		_, err = tx.Exec(ctx, `DELETE FROM olp.secrets WHERE id=$1::uuid AND purpose=$2`, *existing.CredentialID, secrets.BillingCredential)
	}
	return err
}
