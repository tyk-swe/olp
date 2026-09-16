package providers

import (
	"context"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
)

// A successful live proof clears the shared rejection for the exact version
// and logical slot. Serving processes consult this authority without a stale
// local shadow when distributed coordination is available.
func (s *Server) clearValidatedCooldowns(ctx context.Context, providerID string, slot slotRow) {
	clearer, ok := s.Quotas.(interface {
		Cooldown(context.Context, string, time.Duration) error
	})
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	scopes := []string{limits.SlotScope(slot.ID), limits.CredentialScope(providerID, slot.CredentialID)}
	for _, scope := range scopes {
		if err := clearer.Cooldown(ctx, scope, 0); err != nil && s.Log != nil {
			s.Log.Warn("validated credential cooldown could not be cleared", "provider_id", providerID)
		}
	}
}
