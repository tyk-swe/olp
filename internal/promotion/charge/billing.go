package charge

import (
	"context"
	"github.com/tyk-swe/olp/internal/promotion"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/billing"
)

// promotion.BillingRef names the secret binding a provider's billing credential takes.
// ValidateBillingImport checks a document's billing entry: the ref names the
// provider's own binding and the scope matches the source the provider's kind
// imports.
func ValidateBillingImport(prefix string, entry *promotion.ProviderEntry) error {
	if entry.BillingImport == nil {
		return nil
	}
	source, err := billing.SourceFor(entry.Configuration.Kind)
	if err != nil {
		return access.Invalid(prefix+".billing_import", err.Error())
	}
	if entry.BillingImport.CredentialRef != promotion.BillingRef(entry.Name) || len(entry.BillingImport.CredentialRef) > promotion.MaxCredentialRef {
		return access.Invalid(prefix+".billing_import.credential_ref",
			"Use the provider name followed by /billing.")
	}
	canonical, err := billing.ValidateScope(source, entry.BillingImport.Scope)
	if err != nil {
		return err
	}
	// promotion.Canonicalize so an equal document compares equal regardless of the
	// formatting it was supplied in.
	entry.BillingImport.Scope = canonical
	return nil
}

// AttachBillingImport fills a provider entry's billing_import from its stored
// row; absent means the provider imports nothing.
func AttachBillingImport(ctx context.Context, q access.Queryer, entry *promotion.ProviderEntry, providerID string) error {
	state, err := billing.ReadImportState(ctx, q, providerID)
	if err != nil {
		return err
	}
	if state == nil {
		return nil
	}
	entry.BillingImport = &promotion.BillingImportEntry{
		Enabled:       state.Enabled,
		Scope:         state.Scope,
		CredentialRef: promotion.BillingRef(entry.Name),
	}
	return nil
}

// ApplyBillingImport brings the provider's billing import to the document's:
// created or replaced when declared (reusing a matching stored credential),
// deleted when the document omits it.
func ApplyBillingImport(s *promotion.Server, ctx context.Context, tx pgx.Tx, providerID string, entry *promotion.ProviderEntry, bindings promotion.BindingSet, createdBy string) error {
	if entry.BillingImport == nil {
		return billing.RemoveImport(ctx, tx, providerID)
	}
	source, err := billing.SourceFor(entry.Configuration.Kind)
	if err != nil {
		return access.Invalid("billing_import", err.Error())
	}
	var credentialID *string
	if existing, err := billing.ReadImportState(ctx, tx, providerID); err != nil {
		return err
	} else if existing != nil {
		credentialID = existing.CredentialID
	}
	if binding, supplied := bindings[entry.BillingImport.CredentialRef]; supplied {
		if binding.Reference != nil {
			return access.Invalid("secret_bindings", "Billing imports take a plaintext secret binding; external references cannot feed a billing credential.")
		}
		same, err := billing.ImportCredentialMatches(ctx, tx, s.Access.Keys, s.Access.Installation, source, credentialID, binding.Secret)
		if err != nil {
			return err
		}
		if !same {
			id, err := billing.ImportCredential(ctx, tx, s.Access.Keys, s.Access.Installation, source, binding.Secret)
			if err != nil {
				return err
			}
			credentialID = &id
		}
	}
	compatible, err := billing.ImportCredentialCompatible(ctx, tx, s.Access.Keys, s.Access.Installation, source, credentialID)
	if err != nil {
		return err
	}
	if !compatible {
		return access.Invalid("secret_bindings", "Supply the provider's billing credential binding.")
	}
	return billing.ApplyImport(ctx, tx, providerID, source, entry.BillingImport.Enabled, entry.BillingImport.Scope, credentialID, createdBy)
}
