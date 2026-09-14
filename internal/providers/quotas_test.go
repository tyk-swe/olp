package providers

import (
	"context"
	"errors"
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

func TestSlotQuotasReportLiveCountersUnderTheSharedNames(t *testing.T) {
	slots, published, health := quotaFixture()
	source := &stubQuotas{
		usage: map[string]limits.Usage{
			limits.ConnectionLookup(quotaProvider): {RequestsThisMinute: 7, TokensThisMinute: 900, ConcurrentRequests: 2},
			limits.SlotLookup(quotaSlot):           {RequestsThisMinute: 3, TokensThisMinute: 400, ConcurrentRequests: 1},
		},
		cooling: map[string]bool{limits.SlotScope(quotaSlot): true},
	}
	server := &Server{Quotas: source, Log: slog.New(slog.DiscardHandler)}
	connection := server.quotas(context.Background(), quotaProvider, slots, published, health)

	want := map[string]any{"requests_this_minute": int64(7), "tokens_this_minute": int64(900), "concurrent_requests": int64(2)}
	usage, ok := connection.(map[string]any)
	if !ok || usage["requests_this_minute"] != want["requests_this_minute"] ||
		usage["tokens_this_minute"] != want["tokens_this_minute"] ||
		usage["concurrent_requests"] != want["concurrent_requests"] {
		t.Fatalf("connection usage %v, want %v", connection, want)
	}
	entry := health[quotaSlot].(map[string]any)
	slotUsage, ok := entry["usage"].(map[string]any)
	if !ok || slotUsage["requests_this_minute"] != int64(3) || slotUsage["tokens_this_minute"] != int64(400) ||
		slotUsage["concurrent_requests"] != int64(1) {
		t.Fatalf("slot usage %v", entry["usage"])
	}
	if entry["cooling_down"] != true {
		t.Fatalf("cooling_down %v, want true", entry["cooling_down"])
	}
	// A slot cools down under the credential version it dispatches with as
	// well as under its own identity, and the connection is counted apart.
	if len(source.lookups) != 2 || source.lookups[0] != limits.ConnectionLookup(quotaProvider) ||
		source.lookups[1] != limits.SlotLookup(quotaSlot) {
		t.Fatalf("usage lookups %v", source.lookups)
	}
	credential := quotaCredential
	if len(source.scopes) != 1 || len(source.scopes[0]) != 2 ||
		source.scopes[0][0] != limits.CredentialScope(quotaProvider, &credential) ||
		source.scopes[0][1] != limits.SlotScope(quotaSlot) {
		t.Fatalf("cooldown scopes %v", source.scopes)
	}
}

func TestSlotQuotasStayUnknownWithoutAReadableSource(t *testing.T) {
	for name, source := range map[string]QuotaSource{
		"unconfigured": nil,
		"unreachable":  &stubQuotas{err: errors.New("valkey is unreachable")},
	} {
		t.Run(name, func(t *testing.T) {
			slots, published, health := quotaFixture()
			server := &Server{Log: slog.New(slog.DiscardHandler)}
			if source != nil {
				server.Quotas = source
			}
			if connection := server.quotas(context.Background(), quotaProvider, slots, published, health); connection != nil {
				t.Fatalf("connection usage %v, want unknown", connection)
			}
			entry := health[quotaSlot].(map[string]any)
			if entry["usage"] != nil || entry["cooling_down"] != nil {
				t.Fatalf("slot health %v, want unknown counters", entry)
			}
			if entry["revoked"] != false {
				t.Fatalf("slot health lost its stored fields: %v", entry)
			}
		})
	}
}
