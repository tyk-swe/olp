package connectors

import (
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/vendors"
)

func TestAProfilesSurfaceIsItsGenerationFamilysAndServesItsOperations(t *testing.T) {
	t.Parallel()
	modes := []string{"unary", "streaming", "realtime"}
	for _, profile := range Profiles() {
		surface := profile.Surface()
		if family, generates := profile.GenerationFamily(); generates && family.Surface() != surface {
			t.Errorf("profile %s speaks %s, native to the %s surface, and names the %q surface", profile.ID, family, family.Surface(), surface)
		}
		if surface == "" {
			continue
		}
		served := slices.ContainsFunc(vendors.All(), func(vendor vendors.Contract) bool {
			pinned := Config{Kind: profile.Kind, VendorID: vendor.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision}
			return slices.ContainsFunc(profile.Operations, func(operation string) bool {
				return slices.ContainsFunc(modes, func(mode string) bool { return pinned.Supports(operation, surface, mode) })
			})
		})
		if !served {
			t.Errorf("profile %s names the %s surface and serves none of %v on it", profile.ID, surface, profile.Operations)
		}
	}
}
