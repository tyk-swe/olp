package providers

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/secretstore"
)

func validateCredentialInput(plain *string, reference *secretstore.Reference) error {
	if plain != nil && reference != nil {
		return access.Invalid("credential_reference", "Choose a sealed credential or an external reference.")
	}
	if plain != nil {
		return ValidCredential(*plain)
	}
	if reference != nil && reference.Validate() != nil {
		return access.Invalid("credential_reference", "Use an immutable, version-pinned external credential reference.")
	}
	return nil
}

// External references spend the installation's workload authority. An
// installation operator must approve the exact version for this provider;
// project managers may use approved versions but cannot introduce references.
func authorizeCredentialReference(p access.Principal, reference *secretstore.Reference) error {
	if reference != nil {
		return p.Authorize(access.Settings)
	}
	return nil
}

func (s *Server) resolveReference(ctx context.Context, reference secretstore.Reference) ([]byte, error) {
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

func (s *Server) storeCredentialInput(ctx context.Context, tx pgx.Tx, providerID string, plain *string, reference *secretstore.Reference) (string, int, error) {
	if reference == nil {
		return s.StoreCredential(ctx, tx, providerID, *plain)
	}
	id := access.NewID()
	var version int
	data, err := json.Marshal(reference)
	if err != nil {
		return "", 0, err
	}
	err = tx.QueryRow(ctx, "INSERT INTO olp.provider_credentials(id,provider_id,version,external_reference) VALUES($1,$2,(SELECT coalesce(max(version),0)+1 FROM olp.provider_credentials WHERE provider_id=$2),$3) RETURNING version", id, providerID, data).Scan(&version)
	return id, version, err
}

func (s *Server) readCredential(ctx context.Context, q access.Queryer, id string) ([]byte, error) {
	var referenceJSON []byte
	if err := q.QueryRow(ctx, "SELECT external_reference FROM olp.provider_credentials WHERE id=$1", id).Scan(&referenceJSON); err != nil {
		return nil, err
	}
	if len(referenceJSON) > 0 {
		var reference secretstore.Reference
		if json.Unmarshal(referenceJSON, &reference) != nil || reference.Validate() != nil {
			return nil, runtime.ErrCredentialUnavailable
		}
		return s.resolveReference(ctx, reference)
	}
	return s.Access.Keys.Read(ctx, q, s.Access.Installation, id, secrets.ProviderCredential)
}
