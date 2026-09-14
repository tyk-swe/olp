package gateway

import (
	"net/http"
	"testing"
)

func TestMalformedHeaderCredentialFailsOverWithoutDispatch(t *testing.T) {
	h := newHarness(t, Config{})
	for id, provider := range h.rt.release.Snapshot.Providers {
		if provider.Slots[0].ID == h.slotA {
			provider.AuthMode = "headers"
			provider.CredentialHeaders = []string{"X-Api-Key"}
			h.rt.release.Snapshot.Providers[id] = provider
		}
	}
	resp, result := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusOK || result["model"] != routeSlug || h.mock.count("a") != 0 || h.mock.count("b") != 1 {
		t.Fatalf("malformed credential was dispatched or blocked failover: %d %v", resp.StatusCode, result)
	}
	env := h.sink.last(t)
	if len(env.Attempts) != 2 || env.Attempts[0].Class != classCredential || env.Attempts[0].Status != 0 || env.Attempts[1].Class != classSuccess {
		t.Fatalf("credential failure accounting: %+v", env)
	}
}
