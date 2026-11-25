package connection

import (
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/vendors"
)

func TestEveryVocabularyCapabilityIsOfferedByAConnector(t *testing.T) {
	t.Parallel()
	profiles := connectors.Profiles()
	contracts := vendors.All()
	for _, capability := range vocabulary {
		offered := slices.ContainsFunc(profiles, func(profile connectors.Profile) bool {
			return slices.ContainsFunc(contracts, func(vendor vendors.Contract) bool {
				pinned := connectors.Config{Kind: profile.Kind, VendorID: vendor.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision}
				return connectors.Supports(profile.Kind, vendor.ID, capability.Operation, capability.Surface, capability.Mode) ||
					pinned.Supports(capability.Operation, capability.Surface, capability.Mode)
			})
		})
		if !offered {
			t.Errorf("the vocabulary lists %v, which no connector kind or reviewed profile offers", capability)
		}
	}
}
