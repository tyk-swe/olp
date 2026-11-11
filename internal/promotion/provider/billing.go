package provider

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/billing/imports"
	"github.com/tyk-swe/olp/internal/billing/source"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
)

type Billing struct {
	Enabled bool
	Scope   source.Scope
}

func billingRef(provider string) promotion.Ref { return promotion.Ref(provider + "/billing") }

func parseBilling(entry contract.ConfigurationProviderEntry, kind string, at path) (*Billing, error) {
	if entry.BillingImport == nil {
		return nil, nil
	}
	src, err := source.For(kind)
	if err != nil {
		return nil, access.Invalid(at.field("billing_import"), err.Error())
	}
	ref := entry.BillingImport.CredentialRef
	if promotion.Ref(ref) != billingRef(entry.Name) || len(ref) > maxRef {
		return nil, access.Invalid(at.field("billing_import.credential_ref"), "Use the provider name followed by /billing.")
	}
	raw, err := promotion.Convert[json.RawMessage](entry.BillingImport.Scope)
	if err != nil {
		return nil, err
	}
	scope, err := src.Scope(raw)
	if err != nil {
		return nil, err
	}
	return &Billing{Enabled: entry.BillingImport.Enabled, Scope: scope}, nil
}

func storedBilling(config *imports.Config) *Billing {
	if config == nil {
		return nil
	}
	return &Billing{Enabled: config.Enabled, Scope: config.Scope}
}

func renderBilling(entry Entry) (*contract.ConfigurationBillingImport, error) {
	if entry.Billing == nil {
		return nil, nil
	}
	scope, err := promotion.Convert[contract.BillingScope](entry.Billing.Scope.JSON())
	if err != nil {
		return nil, err
	}
	return &contract.ConfigurationBillingImport{CredentialRef: string(billingRef(entry.Name)), Enabled: entry.Billing.Enabled, Scope: scope}, nil
}

func billingClaims(entry Entry) []promotion.Claim {
	if entry.Billing == nil {
		return nil
	}
	src := entry.Billing.Scope.Source()
	return []promotion.Claim{{
		Ref: billingRef(entry.Name),
		Accept: promotion.SealedOnly("Billing credentials require sealed secret bindings.", func(secret string) error {
			_, err := src.Secret(secret)
			return err
		}),
	}}
}

func importedCredential(existing destination) *string {
	if existing.billing == nil {
		return nil
	}
	return existing.billing.CredentialID
}

func vault(deps Deps) imports.Vault {
	return imports.Vault{Keys: deps.Keys, Installation: deps.Installation}
}

func (p planning) billingItems(ctx context.Context, q access.Queryer, entry Entry, existing destination) ([]promotion.Item, error) {
	ref := string(billingRef(entry.Name))
	current := importedCredential(existing)
	if entry.Billing == nil {
		return promotion.When(current != nil, promotion.Item{Kind: contract.ConfigurationPlanItemKindBillingImport, Key: ref, Action: contract.ConfigurationPlanItemActionDelete}), nil
	}
	if _, supplied := p.bindings[billingRef(entry.Name)]; supplied {
		return []promotion.Item{{Kind: contract.ConfigurationPlanItemKindBillingImport, Key: ref, Action: contract.ConfigurationPlanItemActionBind}}, nil
	}
	fits, err := vault(p.deps).Fits(ctx, q, entry.Billing.Scope.Source(), current)
	if err != nil {
		return nil, err
	}
	if !fits {
		return []promotion.Item{promotion.Blocker(contract.ConfigurationPlanItemKindBillingImport, ref, "secret_binding_required")}, nil
	}
	return []promotion.Item{{Kind: contract.ConfigurationPlanItemKindBillingImport, Key: ref, Action: contract.ConfigurationPlanItemActionReuse}}, nil
}

func (a applying) applyBilling(ctx context.Context, tx pgx.Tx, entry Entry, target destination) error {
	if entry.Billing == nil {
		return imports.Remove(ctx, tx, target.id)
	}
	credential, err := a.billingCredential(entry)
	if err != nil {
		return err
	}
	desired := imports.Desired{Enabled: entry.Billing.Enabled, Scope: entry.Billing.Scope, Credential: credential}
	_, err = imports.Save(ctx, tx, vault(a.deps), target.id, desired, a.createdBy)
	if errors.Is(err, imports.ErrCredentialRequired) || errors.Is(err, imports.ErrCredentialIncompatible) {
		return access.Invalid("secret_bindings", "Supply the provider's billing credential binding.")
	}
	return err
}

func (a applying) billingCredential(entry Entry) (*source.Credential, error) {
	sealed, supplied := a.bindings[billingRef(entry.Name)].(promotion.Sealed)
	if !supplied {
		return nil, nil
	}
	credential, err := entry.Billing.Scope.Source().Secret(sealed.Secret)
	if err != nil {
		return nil, err
	}
	return &credential, nil
}
