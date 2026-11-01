package configuration

import (
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/providers"
)

func codeNetworkConfiguration(configuration, current providers.Configuration, ref *string, name string) (providers.Configuration, error) {
	if ref == nil {
		return configuration, nil
	}
	if *ref != networkRef(name) || configuration.Options.Network == nil || current.Options.Network == nil || current.Options.Network.CredentialID == "" {
		return providers.Configuration{}, access.Invalid("code.connections.network_credential_ref", "Bind the named destination provider network credential first.")
	}
	network := *configuration.Options.Network
	network.CredentialID = current.Options.Network.CredentialID
	bound := configuration
	bound.Options.Network = &network
	return bound, nil
}
