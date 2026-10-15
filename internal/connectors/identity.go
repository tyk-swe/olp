package connectors

import "github.com/tyk-swe/olp/internal/oif"

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
