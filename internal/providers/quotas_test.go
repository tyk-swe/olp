package providers

import (
	"context"
	"log/slog"
	"testing"

	"github.com/tyk-swe/olp/internal/limits"
)

// stubQuotas answers the reads a slot list makes and records what it was asked,
// so the test holds the console to the same shared names the gateway writes.
type stubQuotas struct {
	usage   map[string]limits.Usage
	cooling map[string]bool
	err     error
	lookups []string
	scopes  [][]string
}

func (s *stubQuotas) ProviderUsage(_ context.Context, lookup string) (limits.Usage, error) {
	s.lookups = append(s.lookups, lookup)
	if s.err != nil {
		return limits.Usage{}, s.err
	}
	return s.usage[lookup], nil
}

func (s *stubQuotas) Cooling(_ context.Context, scopes ...string) (bool, error) {
	s.scopes = append(s.scopes, scopes)
	if s.err != nil {
		return false, s.err
	}
	for _, scope := range scopes {
		if s.cooling[scope] {
			return true, nil
		}
	}
	return false, nil
}

const (
	quotaProvider   = "3f1b2c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	quotaSlot       = "9a8b7c6d-5e4f-4a3b-2c1d-0e9f8a7b6c5d"
	quotaCredential = "1c2d3e4f-5a6b-4c7d-8e9f-0a1b2c3d4e5f"
)

// quotaFixture is one slot exactly as the list builds it before the live
// counters are asked for.
func quotaFixture() ([]slotRow, map[string]*string, map[string]any) {
	credential := quotaCredential
	slots := []slotRow{{ID: quotaSlot, CredentialID: &credential}}
	published := map[string]*string{quotaSlot: &credential}
	health := map[string]any{quotaSlot: map[string]any{"revoked": false, "cooling_down": nil, "usage": nil}}
	return slots, published, health
}

// A slot cools down under the credential version it dispatches with as
// well as under its own identity, and the connection is counted apart.

func TestSlotQuotasReadThePublishedGrantsGeneration(t *testing.T) {
	slots, published, health := quotaFixture()
	credential := quotaCredential
	old := limits.CredentialScope(quotaProvider, &credential, 1)
	current := limits.CredentialScope(quotaProvider, &credential, 2)
	source := &stubQuotas{cooling: map[string]bool{old: true}}
	server := &Server{Quotas: source, Log: slog.New(slog.DiscardHandler)}
	server.quotas(t.Context(), quotaProvider, slots, published, map[string]int64{credential: 2}, health)
	if health[quotaSlot].(map[string]any)["cooling_down"] != false || source.scopes[0][0] != current {
		t.Fatal("the management health read a superseded grant's cooldown")
	}
	source.cooling[current] = true
	server.quotas(t.Context(), quotaProvider, slots, published, map[string]int64{credential: 2}, health)
	if health[quotaSlot].(map[string]any)["cooling_down"] != true {
		t.Fatal("the management health missed the current grant's cooldown")
	}
}
