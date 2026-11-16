package registration

import (
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/routing"
)

func validateOAuth(config *routing.MCPOAuth, policy *egress.Policy) error {
	if config == nil {
		return nil
	}
	if !config.Valid() {
		return httpkit.Invalid("oauth", "Supply a client, audience, bounded OAuth scopes and declared authorization and token endpoints.")
	}
	for _, endpoint := range []string{config.AuthorizationEndpoint, config.TokenEndpoint, config.DeviceAuthorizationEndpoint} {
		if endpoint == "" {
			continue
		}
		if _, err := policy.ValidateEndpoint(endpoint); err != nil {
			return httpkit.Invalid("oauth", "Every OAuth destination must satisfy provider egress policy.")
		}
	}
	return nil
}
