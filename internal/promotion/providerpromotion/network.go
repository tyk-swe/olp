package providerpromotion

import (
	"context"
	"github.com/tyk-swe/olp/internal/promotion"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/providers"
)

func PortableNetwork(entry *promotion.ProviderEntry) {
	if network := entry.Configuration.Options.Network; network != nil && network.CredentialID != "" {
		copy := *network
		copy.CredentialID = ""
		entry.Configuration.Options.Network = &copy
		ref := promotion.NetworkRef(entry.Name)
		entry.NetworkCredentialRef = &ref
	}
}

// NetworkConfiguration binds portable network references without mutating the
// canonical import document or exposing a destination environment's secret IDs.
func NetworkConfiguration(s *promotion.Server, ctx context.Context, tx pgx.Tx, providerID string, entry *promotion.ProviderEntry, existing *promotion.ExistingProvider, bindings promotion.BindingSet) (providers.Configuration, error) {
	cfg := entry.Configuration
	if cfg.Options.Network == nil {
		return cfg, nil
	}
	network := *cfg.Options.Network
	cfg.Options.Network = &network
	if entry.NetworkCredentialRef == nil {
		return cfg, nil
	}
	var current *string
	if existing != nil {
		current = existing.NetworkCredentialID
	}
	if secret, provided := bindings[*entry.NetworkCredentialRef]; provided {
		same, err := s.BindingMatches(ctx, tx, current, secret)
		if err != nil {
			return cfg, err
		}
		if !same {
			if s.StoreNetworkCredential == nil {
				return cfg, access.Invalid("secret_bindings", "Network credential storage is unavailable.")
			}
			id, err := s.StoreNetworkCredential(ctx, tx, providerID, secret.Secret)
			if err != nil {
				return cfg, err
			}
			current = &id
		}
	}
	if current == nil {
		return cfg, access.Invalid("secret_bindings", "Supply the provider's network credential binding.")
	}
	network.CredentialID = *current
	return cfg, nil
}

func BindNetwork(s *promotion.Server, ctx context.Context, tx pgx.Tx, providerID string, entry *promotion.ProviderEntry, existing *promotion.ExistingProvider, bindings promotion.BindingSet) error {
	cfg, err := NetworkConfiguration(s, ctx, tx, providerID, entry, existing, bindings)
	if err != nil {
		return err
	}
	return providers.StoreDraftConfiguration(ctx, tx, providerID, entry.Name, &cfg, s.Egress)
}
