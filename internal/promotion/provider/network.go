package provider

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/providers"
)

func NetworkRef(provider string) promotion.Ref { return promotion.Ref(provider + "/network") }

func portableConfiguration(input contract.ProviderConfiguration, at path) (providers.Configuration, error) {
	configuration, err := promotion.Convert[providers.Configuration](input)
	if err != nil {
		return providers.Configuration{}, err
	}
	configuration.Normalize()
	if configuration.Options.Network != nil && configuration.Options.Network.CredentialID != "" {
		return providers.Configuration{}, access.Invalid(at.field("configuration.options.network.credential_id"), "Configuration artifacts use network_credential_ref instead of environment-specific credential IDs.")
	}
	return configuration, nil
}

func parseNetwork(entry contract.ConfigurationProviderEntry, configuration providers.Configuration, at path) (bool, error) {
	if entry.NetworkCredentialRef == nil {
		return false, nil
	}
	ref := *entry.NetworkCredentialRef
	if configuration.Options.Network == nil || promotion.Ref(ref) != NetworkRef(entry.Name) || len(ref) > MaxRef {
		return false, access.Invalid(at.field("network_credential_ref"), "Use the provider name followed by /network and configure network options.")
	}
	return true, nil
}

func renderNetwork(entry Entry) *string {
	return optional(string(NetworkRef(entry.Name)), entry.Network)
}

func storedNetwork(configuration providers.Configuration) bool {
	return configuration.Options.Network != nil && configuration.Options.Network.CredentialID != ""
}

func withNetworkCredential(configuration providers.Configuration, credentialID string) providers.Configuration {
	if configuration.Options.Network == nil {
		return configuration
	}
	network := *configuration.Options.Network
	network.CredentialID = credentialID
	configured := configuration
	configured.Options.Network = &network
	return configured
}

func networkClaims(entry Entry) []promotion.Claim {
	ref := NetworkRef(entry.Name)
	return when(entry.Network, promotion.Claim{Ref: ref, Accept: func(credential promotion.Credential) error {
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

func (a applying) bindNetwork(ctx context.Context, tx pgx.Tx, entry Entry, target destination) error {
	configuration, err := a.networkConfiguration(ctx, tx, entry, target)
	if err != nil {
		return err
	}
	prepared, err := providers.PrepareConfiguration(ctx, tx, a.deps.Unconfined, a.deps.Egress, configuration)
	if err != nil {
		return err
	}
	return providers.StoreDraftConfiguration(ctx, tx, target.id, entry.Name, prepared)
}

func (a applying) networkConfiguration(ctx context.Context, tx pgx.Tx, entry Entry, target destination) (providers.Configuration, error) {
	if !entry.Network {
		return entry.Configuration, nil
	}
	credentialID, err := a.networkCredential(ctx, tx, entry, target)
	if err != nil {
		return providers.Configuration{}, err
	}
	if credentialID == nil {
		return providers.Configuration{}, access.Invalid("secret_bindings", "Supply the provider's network credential binding.")
	}
	return withNetworkCredential(entry.Configuration, *credentialID), nil
}

func (a applying) networkCredential(ctx context.Context, tx pgx.Tx, entry Entry, target destination) (*string, error) {
	sealed, supplied := a.bindings[NetworkRef(entry.Name)].(promotion.Sealed)
	if !supplied {
		return target.network, nil
	}
	same, err := a.deps.matches(ctx, tx, target.network, sealed)
	if err != nil || same {
		return target.network, err
	}
	id, err := a.deps.Catalogue.StoreNetworkCredential(ctx, tx, target.id, sealed.Secret)
	if err != nil {
		return nil, err
	}
	return &id, nil
}
