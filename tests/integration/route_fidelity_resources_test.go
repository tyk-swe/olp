//go:build integration

package integration_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// publishFidelity publishes a new revision of slug with the given mode.
func publishFidelity(t *testing.T, h *accessHarness, owner *browser, slug string, provider any, operations []string, mode string) {
	t.Helper()
	body := fidelityDraft(slug, provider)
	body["operations"] = operations
	body["fidelity"] = map[string]any{"mode": mode}
	draft := h.want(owner, "POST", "/api/v1/route-drafts", body, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	h.refresh()
}

// requireRouteChanged asserts the refusal for a retained resource whose
// contract the route no longer promises.
func requireRouteChanged(t *testing.T, what string, status int, raw []byte, detail string) {
	t.Helper()
	if status != http.StatusConflict || !strings.Contains(string(raw), "provider_resource_unavailable") || !strings.Contains(string(raw), detail) {
		t.Fatalf("%s was not refused after the route changed fidelity: %d %s", what, status, raw)
	}
}

// Lists keep every retained resource, and nothing reaches the provider
// for a strict resource the transformed route no longer serves.

// Its owner can still stop the work and remove the provider-held file.

// A transformed file cannot start strict batch work, but it is still
// listed and retrievable as before.

// The strict batch is served again once its route is strict.
