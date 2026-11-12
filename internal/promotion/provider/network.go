package provider

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/providers/connection"
)

func NetworkRef(provider string) promotion.Ref { return promotion.Ref(provider + "/network") }

func portableConfiguration(input contract.ProviderConfiguration, at path) (connection.Configuration, error) {
	converted, err := promotion.Convert[connection.Configuration](input)
	if err != nil {
		return connection.Configuration{}, err
	}
	configuration := converted.Normalized()
	if configuration.Options.Network != nil && configuration.Options.Network.CredentialID != "" {
		return connection.Configuration{}, access.Invalid(at.field("configuration.options.network.credential_id"), "Configuration artifacts use network_credential_ref instead of environment-specific credential IDs.")
	}
	return configuration, nil
}

func parseNetwork(entry contract.ConfigurationProviderEntry, configuration connection.Configuration, at path) (bool, error) {
	if entry.NetworkCredentialRef == nil {
		return false, nil
	}
	ref := *entry.NetworkCredentialRef
	if configuration.Options.Network == nil || promotion.Ref(ref) != NetworkRef(entry.Name) || len(ref) > maxRef {
		return false, access.Invalid(at.field("network_credential_ref"), "Use the provider name followed by /network and configure network options.")
	}
	return true, nil
}

func renderNetwork(entry Entry) *string {
	return promotion.PointerIf(string(NetworkRef(entry.Name)), entry.Network)
}

func storedNetwork(configuration connection.Configuration) bool {
	return configuration.Options.Network != nil && configuration.Options.Network.CredentialID != ""
}

func networkClaims(entry Entry) []promotion.Claim {
	ref := NetworkRef(entry.Name)
	return promotion.When(entry.Network, promotion.Claim{Ref: ref, Accept: func(credential promotion.Credential) error {
		switch bound := credential.(type) {
		case promotion.External:
			if err := promotion.ValidateExternal(bound.Reference); err != nil {
				return err
			}
			return access.Invalid("external_credential_bindings", "Network identities require sealed secret bindings.")
		case promotion.Sealed:
			if err := egress.ValidateConnectionSecret([]byte(bound.Secret)); err != nil {
				return access.Invalid("secret_bindings."+string(ref), err.Error())
			}
			return validSecret(ref, bound.Secret)
		}
		return nil
	}})
}

func networkItems(entry Entry, existing destination, bindings promotion.Bindings) []promotion.Item {
	if !entry.Network {
		return nil
	}
	ref := string(NetworkRef(entry.Name))
	if _, supplied := bindings[NetworkRef(entry.Name)]; supplied {
		return []promotion.Item{{Kind: contract.ConfigurationPlanItemKindNetworkCredential, Key: ref, Action: contract.ConfigurationPlanItemActionBind}}
	}
	if existing.network != nil {
		return []promotion.Item{{Kind: contract.ConfigurationPlanItemKindNetworkCredential, Key: ref, Action: contract.ConfigurationPlanItemActionReuse}}
	}
	return []promotion.Item{promotion.Blocker(contract.ConfigurationPlanItemKindNetworkCredential, ref, "secret_binding_required")}
}

func (a applying) bindNetwork(ctx context.Context, tx pgx.Tx, entry Entry, prepared connection.Prepared, target destination, same bool) error {
	configured, rotated, err := a.networkConfiguration(ctx, tx, entry, prepared, target)
	if err != nil || (same && !rotated) {
		return err
	}
	return a.deps.Drafts.StoreDraftConfiguration(ctx, tx, target.id, entry.Name, configured)
}

func (a applying) networkConfiguration(ctx context.Context, tx pgx.Tx, entry Entry, prepared connection.Prepared, target destination) (connection.Prepared, bool, error) {
	if !entry.Network {
		return prepared, false, nil
	}
	credentialID, rotated, err := a.networkCredential(ctx, tx, entry, target)
	if err != nil {
		return connection.Prepared{}, false, err
	}
	if credentialID == nil {
		return connection.Prepared{}, false, access.Invalid("secret_bindings", "Supply the provider's network credential binding.")
	}
	return prepared.WithNetworkCredential(*credentialID), rotated, nil
}

func (a applying) networkCredential(ctx context.Context, tx pgx.Tx, entry Entry, target destination) (*string, bool, error) {
	sealed, supplied := a.bindings[NetworkRef(entry.Name)].(promotion.Sealed)
	if !supplied {
		return target.network, false, nil
	}
	same, err := a.deps.matches(ctx, tx, target.network, sealed)
	if err != nil || same {
		return target.network, false, err
	}
	id, err := a.deps.Drafts.StoreNetworkCredential(ctx, tx, target.id, sealed.Secret)
	if err != nil {
		return nil, false, err
	}
	return &id, true, nil
}
