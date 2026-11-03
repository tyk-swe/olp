package connectors

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/oif"
)

func IsServingIdentityHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Openai-Organization", "Openai-Project", "X-Goog-User-Project", "X-Goog-Request-Params", "X-Ms-Region", "X-Ms-Routing-Name":
		return true
	default:
		return false
	}
}

// ServingIdentity identifies the configured model and its pinned environment.
func (c Config) ServingIdentity(providerID, revisionID, model string) oif.ServingIdentity {
	profile, _ := c.Profile()
	binding := c.Bindings[model]
	region := c.CloudRegion
	if binding.Region != "" {
		region = binding.Region
	}
	return oif.ServingIdentity{ProviderID: providerID, RevisionID: revisionID, Model: c.Model(model), ProfileID: profile.ID, ProfileRevision: profile.Revision, PrincipalID: c.ServingPrincipal(model), Snapshot: binding.Snapshot, Region: region, ResourceScope: binding.ResourceScope}
}
