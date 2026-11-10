package providers

import (
	"context"
	"time"

	"github.com/tyk-swe/olp/internal/limits/fleet"
)

// A successful live proof clears the shared rejection for the exact version
// and logical slot. Serving processes consult this authority without a stale
// local shadow when distributed coordination is available.
func (s *Catalogue) clearValidatedCooldowns(ctx context.Context, providerID string, slot slotRow) {
	if s.Quotas == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	scopes := []string{fleet.SlotScope(slot.ID), fleet.CredentialScope(providerID, slot.CredentialID, slot.GrantGeneration)}
	for _, scope := range scopes {
		if err := s.Quotas.Cooldown(ctx, scope, 0); err != nil && s.Log != nil {
			s.Log.Warn("validated credential cooldown could not be cleared", "provider_id", providerID)
		}
	}
}
