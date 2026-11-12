package providers

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/secretstore"
)

type credentialSource interface{ credentialSource() }

type pastedCredential string

type externalCredential secretstore.Reference

func (pastedCredential) credentialSource()   {}
func (externalCredential) credentialSource() {}

func decodeCredential(plain *string, reference *secretstore.Reference) (credentialSource, error) {
	switch {
	case plain != nil && reference != nil:
		return nil, access.Invalid("credential_reference", "Choose a sealed credential or an external reference.")
	case plain != nil:
		if err := ValidCredential(*plain); err != nil {
			return nil, err
		}
		return pastedCredential(*plain), nil
	case reference != nil:
		if reference.Validate() != nil {
			return nil, access.Invalid("credential_reference", "Use an immutable, version-pinned external credential reference.")
		}
		return externalCredential(*reference), nil
	}
	return nil, nil
}

// External references spend the installation's workload authority. An
// installation operator must approve the exact version for this provider;
// project managers may use approved versions but cannot introduce references.
func authorizeCredential(p access.Principal, source credentialSource) error {
	if _, external := source.(externalCredential); external {
		return p.Authorize(access.Settings)
	}
	return nil
}

func (s *Catalogue) resolveReference(ctx context.Context, reference secretstore.Reference) ([]byte, error) {
	if s.ExternalSecrets == nil {
		return nil, access.Fail(422, "external_secret_unavailable", "The external credential store could not resolve the pinned version.")
	}
	value, err := s.ExternalSecrets.Resolve(ctx, reference)
	if err != nil {
		return nil, access.Fail(422, "external_secret_unavailable", "The external credential store could not resolve the pinned version.")
	}
	if err = ValidCredential(string(value)); err != nil {
		clear(value)
		return nil, err
	}
	return value, nil
}

func (s *Catalogue) material(ctx context.Context, source credentialSource) ([]byte, error) {
	if reference, external := source.(externalCredential); external {
		return s.resolveReference(ctx, secretstore.Reference(reference))
	}
	return []byte(source.(pastedCredential)), nil
}

func (s *Catalogue) storeCredential(ctx context.Context, tx pgx.Tx, providerID string, source credentialSource) (string, int, error) {
	if reference, external := source.(externalCredential); external {
		return s.StoreExternalCredential(ctx, tx, providerID, secretstore.Reference(reference))
	}
	return s.StoreCredential(ctx, tx, providerID, string(source.(pastedCredential)))
}

func (s *Catalogue) StoreExternalCredential(ctx context.Context, tx pgx.Tx, providerID string, reference secretstore.Reference) (string, int, error) {
	data, err := json.Marshal(reference)
	if err != nil {
		return "", 0, err
	}
	return insertCredentialVersion(ctx, tx, providerID, data)
}

func insertCredentialVersion(ctx context.Context, tx pgx.Tx, providerID string, externalReference []byte) (string, int, error) {
	id := access.NewID()
	var version int
	err := tx.QueryRow(ctx, "INSERT INTO olp.provider_credentials(id,provider_id,version,external_reference) VALUES($1,$2,(SELECT coalesce(max(version),0)+1 FROM olp.provider_credentials WHERE provider_id=$2),$3) RETURNING version", id, providerID, externalReference).Scan(&version)
	return id, version, err
}

func (s *Catalogue) readCredential(ctx context.Context, q access.Queryer, id string) ([]byte, error) {
	var referenceJSON []byte
	if err := q.QueryRow(ctx, "SELECT external_reference FROM olp.provider_credentials WHERE id=$1", id).Scan(&referenceJSON); err != nil {
		return nil, err
	}
	if len(referenceJSON) > 0 {
		var reference secretstore.Reference
		if json.Unmarshal(referenceJSON, &reference) != nil || reference.Validate() != nil {
			return nil, release.ErrCredentialUnavailable
		}
		return s.resolveReference(ctx, reference)
	}
	return s.Access.Keys.Read(ctx, q, s.Access.Installation, id, secrets.ProviderCredential)
}
