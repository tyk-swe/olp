package serve

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/promotion"
)

func plainBindings(values map[string]string) promotion.BindingSet {
	result := promotion.BindingSet{}
	for name, value := range values {
		result[name] = promotion.CredentialBinding{Secret: value}
	}
	return result
}

func (Input *promotionInput) authorizeExternalBindings(p access.Principal) error {
	if len(Input.ExternalBindings) != 0 {
		return p.Authorize(access.Settings)
	}
	return nil
}

func (Input *promotionInput) bindings() (promotion.BindingSet, error) {
	result := plainBindings(Input.SecretBindings)
	for name, reference := range Input.ExternalBindings {
		if _, duplicate := result[name]; duplicate {
			return nil, access.Invalid("external_credential_bindings", "A reference cannot also have a plaintext binding.")
		}
		result[name] = promotion.CredentialBinding{Reference: new(reference)}
	}
	return result, nil
}

func storeBinding(s *promotion.Server, ctx context.Context, tx pgx.Tx, providerID string, binding promotion.CredentialBinding) (string, error) {
	if binding.Reference == nil {
		return s.StoreCredential(ctx, tx, providerID, binding.Secret)
	}
	id := access.NewID()
	data, err := json.Marshal(binding.Reference)
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, "INSERT INTO olp.provider_credentials(id,provider_id,version,external_reference) VALUES($1,$2,(SELECT coalesce(max(version),0)+1 FROM olp.provider_credentials WHERE provider_id=$2),$3)", id, providerID, data)
	return id, err
}
