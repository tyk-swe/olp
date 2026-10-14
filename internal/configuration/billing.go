package configuration

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/billing"
)

// BillingImportEntry is a provider's billing import in a configuration
// document. The credential travels only through secret_bindings under
// credential_ref; it is never part of the document.
type BillingImportEntry struct {
	Enabled       bool            `json:"enabled"`
	Scope         json.RawMessage `json:"scope"`
	CredentialRef string          `json:"credential_ref"`
}

// billingRef names the secret binding a provider's billing credential takes.
func billingRef(name string) string { return name + "/billing" }

// validateBillingImport checks a document's billing entry: the ref names the
// provider's own binding and the scope matches the source the provider's kind
// imports.
func validateBillingImport(prefix string, entry *ProviderEntry) error {
	if entry.BillingImport == nil {
		return nil
	}
	source, err := billing.SourceFor(entry.Configuration.Kind)
	if err != nil {
		return access.Invalid(prefix+".billing_import", err.Error())
	}
	if entry.BillingImport.CredentialRef != billingRef(entry.Name) || len(entry.BillingImport.CredentialRef) > maxCredentialRef {
		return access.Invalid(prefix+".billing_import.credential_ref",
			"Use the provider name followed by /billing.")
	}
	canonical, err := billing.ValidateScope(source, entry.BillingImport.Scope)
	if err != nil {
		return err
	}
	// Canonicalize so an equal document compares equal regardless of the
	// formatting it was supplied in.
	entry.BillingImport.Scope = canonical
	return nil
}

// attachBillingImport fills a provider entry's billing_import from its stored
// row; absent means the provider imports nothing.
func attachBillingImport(ctx context.Context, q access.Queryer, entry *ProviderEntry, providerID string) error {
	state, err := billing.ReadImportState(ctx, q, providerID)
	if err != nil {
		return err
	}
	if state == nil {
		return nil
	}
	entry.BillingImport = &BillingImportEntry{
		Enabled:       state.Enabled,
		Scope:         state.Scope,
		CredentialRef: billingRef(entry.Name),
	}
	return nil
}

// applyBillingImport brings the provider's billing import to the document's:
// created or replaced when declared (reusing a matching stored credential),
// deleted when the document omits it.
func (s *Server) applyBillingImport(ctx context.Context, tx pgx.Tx, providerID string, entry *ProviderEntry, bindings bindingSet, createdBy string) error {
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
		if binding.reference != nil {
			return access.Invalid("secret_bindings", "Billing imports take a plaintext secret binding; external references cannot feed a billing credential.")
		}
		same, err := billing.ImportCredentialMatches(ctx, tx, s.Access.Keys, s.Access.Installation, credentialID, binding.secret)
		if err != nil {
			return err
		}
		if !same {
			id, err := billing.ImportCredential(ctx, tx, s.Access.Keys, s.Access.Installation, binding.secret)
			if err != nil {
				return err
			}
			credentialID = &id
		}
	}
	if credentialID == nil {
		return access.Invalid("secret_bindings", "Supply the provider's billing credential binding.")
	}
	return billing.ApplyImport(ctx, tx, providerID, source, entry.BillingImport.Enabled, entry.BillingImport.Scope, credentialID, createdBy)
}
