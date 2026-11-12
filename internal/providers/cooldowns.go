package providers

import (
	"context"
	"time"

	"github.com/tyk-swe/olp/internal/limits/fleet"
)

// A successful live proof clears the shared rejection for the exact version
// and logical slot. Serving processes consult this authority without a stale
// local shadow when distributed coordination is available.
func (s *Server) clearValidatedCooldowns(ctx context.Context, providerID string, slot slotRow) {
	if s.quota == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	scopes := []string{fleet.SlotScope(slot.ID), fleet.CredentialScope(providerID, slot.CredentialID, slot.GrantGeneration)}
	for _, scope := range scopes {
		if err := s.quota.Cooldown(ctx, scope, 0); err != nil {
			s.log.Warn("validated credential cooldown could not be cleared", "provider_id", providerID)
		}
	}
}
