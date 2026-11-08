package code

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/provider"
	"github.com/tyk-swe/olp/internal/providers"
)

type Connection struct {
	Configuration providers.Configuration
	Network       bool
}

type pin struct {
	provider      string
	configuration providers.Configuration
}

func parseConnection(name string, e contract.ConfigurationCodeConnection) (Connection, error) {
	configuration, err := promotion.Convert[providers.Configuration](e.Configuration)
	if err != nil {
		return Connection{}, err
	}
	network := configuration.Options.Network
	if network != nil && network.CredentialID != "" {
		return Connection{}, access.Invalid("code.connections", "Use a network credential reference instead of an installation credential ID.")
	}
	if e.NetworkCredentialRef != nil && (*e.NetworkCredentialRef != string(provider.NetworkRef(name)) || network == nil) {
		return Connection{}, access.Invalid("code.connections", "Use the named provider network credential reference.")
	}
	return Connection{Configuration: configuration, Network: e.NetworkCredentialRef != nil}, nil
}

func renderConnection(name string, c Connection) (contract.ConfigurationCodeConnection, error) {
	configuration, err := promotion.Convert[contract.ProviderConfiguration](c.Configuration)
	if err != nil || !c.Network {
		return contract.ConfigurationCodeConnection{Configuration: configuration}, err
	}
	return contract.ConfigurationCodeConnection{Configuration: configuration, NetworkCredentialRef: new(string(provider.NetworkRef(name)))}, nil
}

func portableConnection(configuration providers.Configuration) Connection {
	stored := configuration.Options.Network != nil && configuration.Options.Network.CredentialID != ""
	return Connection{Configuration: configuration.WithNetworkCredential(""), Network: stored}
}

func (c Connection) resolve(ctx context.Context, q access.Queryer, name, project, pool string, in scope) (pin, error) {
	prepared, err := providers.PrepareConfiguration(ctx, q, in.deps.Unconfined, in.deps.Egress, c.Configuration)
	if _, refused := provider.Refusal(err); refused {
		return pin{}, access.Invalid("code.connections", "Install and approve the pinned frozen provider profile before promotion.")
	}
	if err != nil {
		return pin{}, err
	}
	configuration := prepared.Configuration()
	var id string
	var current providers.Configuration
	switch err := q.QueryRow(ctx, `SELECT id::text,configuration FROM olp.providers WHERE lower(name)=lower($1) AND project_id=$2`, name, project).Scan(&id, &current); {
	case errors.Is(err, pgx.ErrNoRows):
		return pin{}, access.Invalid("code.routes.published.connections", "Create the destination provider before freezing its connection.")
	case err != nil:
		return pin{}, err
	}
	if configuration.Kind != current.Kind || configuration.AuthMode != current.AuthMode || configuration.ProfileID != current.ProfileID {
		return pin{}, access.Invalid("code.routes.published.connections", "Frozen connections must retain the destination provider's kind, authentication and profile.")
	}
	if err := enrolled(ctx, q, id, configuration, pool, in); err != nil {
		return pin{}, err
	}
	bound, err := networked(configuration, current, c.Network)
	return pin{provider: id, configuration: bound}, err
}

func enrolled(ctx context.Context, q access.Queryer, providerID string, configuration providers.Configuration, pool string, in scope) error {
	at := slices.IndexFunc(in.pools, func(p resolved[access.CodePoolInput]) bool { return p.ref == pool })
	if at < 0 {
		return access.Invalid("code.routes.published", "Resolve the published pool before its credential bindings.")
	}
	members := in.pools[at].input.AccountIDs
	for _, account := range in.accounts {
		if account.input.ProviderID != providerID || !slices.Contains(members, account.to.id) {
			continue
		}
		matches, err := exists(ctx, q, `SELECT coalesce(plugin_digest=$2 AND profile_id=$3,false) FROM olp.provider_credentials WHERE id=$1`, account.input.CredentialID, configuration.ProfileRevision, configuration.ProfileID)
		if err != nil {
			return err
		}
		if !matches {
			return access.Invalid("code.routes.published.connections", "Bind destination account credentials enrolled for the frozen provider profile and revision.")
		}
	}
	return nil
}

func networked(configuration, current providers.Configuration, network bool) (providers.Configuration, error) {
	if !network {
		return configuration, nil
	}
	if configuration.Options.Network == nil || current.Options.Network == nil || current.Options.Network.CredentialID == "" {
		return providers.Configuration{}, access.Invalid("code.connections.network_credential_ref", "Bind the named destination provider network credential first.")
	}
	return configuration.WithNetworkCredential(current.Options.Network.CredentialID), nil
}
