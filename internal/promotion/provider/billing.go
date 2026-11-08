package provider

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/billing"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
)

type Billing struct {
	Enabled bool
	Scope   json.RawMessage
	Source  string
}

func BillingRef(provider string) promotion.Ref { return promotion.Ref(provider + "/billing") }

func parseBilling(entry contract.ConfigurationProviderEntry, kind string, at path) (*Billing, error) {
	if entry.BillingImport == nil {
		return nil, nil
	}
	source, err := billing.SourceFor(kind)
	if err != nil {
		return nil, access.Invalid(at.field("billing_import"), err.Error())
	}
	ref := entry.BillingImport.CredentialRef
	if promotion.Ref(ref) != BillingRef(entry.Name) || len(ref) > MaxRef {
		return nil, access.Invalid(at.field("billing_import.credential_ref"), "Use the provider name followed by /billing.")
	}
	raw, err := promotion.Convert[json.RawMessage](entry.BillingImport.Scope)
	if err != nil {
		return nil, err
	}
	scope, err := billing.ValidateScope(source, raw)
	if err != nil {
		return nil, err
	}
	return &Billing{Enabled: entry.BillingImport.Enabled, Scope: scope, Source: source}, nil
}

func storedBilling(state *billing.ImportState) *Billing {
	if state == nil {
		return nil
	}
	return &Billing{Enabled: state.Enabled, Scope: state.Scope, Source: state.Source}
}

func renderBilling(entry Entry) (*contract.ConfigurationBillingImport, error) {
	if entry.Billing == nil {
		return nil, nil
	}
	scope, err := promotion.Convert[contract.BillingScope](entry.Billing.Scope)
	if err != nil {
		return nil, err
	}
	return &contract.ConfigurationBillingImport{CredentialRef: string(BillingRef(entry.Name)), Enabled: entry.Billing.Enabled, Scope: scope}, nil
}

func billingClaims(entry Entry) []promotion.Claim {
	if entry.Billing == nil {
		return nil
	}
	source := entry.Billing.Source
	return []promotion.Claim{{
		Ref: BillingRef(entry.Name),
		Accept: promotion.SealedOnly("Billing credentials require sealed secret bindings.", func(secret string) error {
			return billing.ValidateCredential(source, secret)
		}),
	}}
}

func importedCredential(existing destination) *string {
	if existing.billing == nil {
		return nil
	}
	return existing.billing.CredentialID
}

func (p planning) billingItems(ctx context.Context, q access.Queryer, entry Entry, existing destination) ([]promotion.Item, error) {
	ref := string(BillingRef(entry.Name))
	current := importedCredential(existing)
	if entry.Billing == nil {
		return when(current != nil, promotion.Item{Kind: contract.ConfigurationPlanItemKindBillingImport, Key: ref, Action: contract.ConfigurationPlanItemActionDelete}), nil
	}
	if _, supplied := p.bindings[BillingRef(entry.Name)]; supplied {
		return []promotion.Item{{Kind: contract.ConfigurationPlanItemKindBillingImport, Key: ref, Action: contract.ConfigurationPlanItemActionBind}}, nil
	}
	if current == nil {
		return []promotion.Item{promotion.Blocker(contract.ConfigurationPlanItemKindBillingImport, ref, "secret_binding_required")}, nil
	}
	compatible, err := billing.ImportCredentialCompatible(ctx, q, p.deps.Keys, p.deps.Installation, entry.Billing.Source, current)
	if err != nil {
		return nil, err
	}
	if !compatible {
		return []promotion.Item{promotion.Blocker(contract.ConfigurationPlanItemKindBillingImport, ref, "secret_binding_required")}, nil
	}
	return []promotion.Item{{Kind: contract.ConfigurationPlanItemKindBillingImport, Key: ref, Action: contract.ConfigurationPlanItemActionReuse}}, nil
}

func (a applying) applyBilling(ctx context.Context, tx pgx.Tx, entry Entry, target destination) error {
	if entry.Billing == nil {
		return billing.RemoveImport(ctx, tx, target.id)
	}
	credentialID, err := a.billingCredential(ctx, tx, entry, target)
	if err != nil {
		return err
	}
	compatible, err := billing.ImportCredentialCompatible(ctx, tx, a.deps.Keys, a.deps.Installation, entry.Billing.Source, credentialID)
	if err != nil {
		return err
	}
	if !compatible {
		return access.Invalid("secret_bindings", "Supply the provider's billing credential binding.")
	}
	_, err = billing.SaveImport(ctx, tx, target.id, billing.ImportConfiguration{Source: entry.Billing.Source, Enabled: entry.Billing.Enabled, Scope: entry.Billing.Scope, CredentialID: credentialID}, a.createdBy)
	return err
}

func (a applying) billingCredential(ctx context.Context, tx pgx.Tx, entry Entry, target destination) (*string, error) {
	current := importedCredential(target)
	sealed, supplied := a.bindings[BillingRef(entry.Name)].(promotion.Sealed)
	if !supplied {
		return current, nil
	}
	same, err := billing.ImportCredentialMatches(ctx, tx, a.deps.Keys, a.deps.Installation, entry.Billing.Source, current, sealed.Secret)
	if err != nil || same {
		return current, err
	}
	id, err := billing.ImportCredential(ctx, tx, a.deps.Keys, a.deps.Installation, entry.Billing.Source, sealed.Secret)
	if err != nil {
		return nil, err
	}
	return &id, nil
}
