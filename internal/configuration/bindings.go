package configuration

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secretstore"
)

type credentialBinding struct {
	secret    string
	reference *secretstore.Reference
}
type bindingSet map[string]credentialBinding

func plainBindings(values map[string]string) bindingSet {
	result := bindingSet{}
	for name, value := range values {
		result[name] = credentialBinding{secret: value}
	}
	return result
}

func (input *promotionInput) authorizeExternalBindings(p access.Principal) error {
	if len(input.ExternalBindings) != 0 {
		return p.Authorize(access.Settings)
	}
	return nil
}

func (input *promotionInput) bindings() (bindingSet, error) {
	result := plainBindings(input.SecretBindings)
	for name, reference := range input.ExternalBindings {
		if _, duplicate := result[name]; duplicate {
			return nil, access.Invalid("external_credential_bindings", "A reference cannot also have a plaintext binding.")
		}
		result[name] = credentialBinding{reference: new(reference)}
	}
	return result, nil
}

func (s *Server) storeBinding(ctx context.Context, tx pgx.Tx, providerID string, binding credentialBinding) (string, error) {
	if binding.reference == nil {
		return s.StoreCredential(ctx, tx, providerID, binding.secret)
	}
	id := access.NewID()
	data, err := json.Marshal(binding.reference)
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, "INSERT INTO olp.provider_credentials(id,provider_id,version,external_reference) VALUES($1,$2,(SELECT coalesce(max(version),0)+1 FROM olp.provider_credentials WHERE provider_id=$2),$3)", id, providerID, data)
	return id, err
}
