//go:build integration

package integration_test

import (
	"github.com/google/uuid"
	"net/http"
	"testing"
)

func TestCredentialSlotMutationsExposeOnlyTheirAtomicParentTransition(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	provider := createScopedProvider(h, owner, "Tracked draft", "https://api.openai.com/v1", nil, 201)
	providerPath := "/api/v1/providers/" + provider["id"].(string)
	pool := h.want(owner, "GET", providerPath+"/credential-slots", nil, nil, 200)
	slotPath := providerPath + "/credential-slots/" + uuid.NewString()
	check := func(response *http.Response, previous, current string) {
		t.Helper()
		if response.Header.Get("OLP-Previous-Parent-ETag") != `"`+previous+`"` || response.Header.Get("OLP-Parent-ETag") != `"`+current+`"` || previous == current {
			t.Fatalf("slot parent transition: got %q -> %q, expected %q -> %q", response.Header.Get("OLP-Previous-Parent-ETag"), response.Header.Get("OLP-Parent-ETag"), previous, current)
		}
	}
	response, _ := h.do(owner, "PUT", slotPath, map[string]any{"slot": map[string]any{"name": "Tracked slot", "priority": 0, "weight": 1}}, withMatch(pool, idem(uuid.NewString())))
	if response.StatusCode != 200 {
		t.Fatalf("slot create: %d", response.StatusCode)
	}
	current := h.want(owner, "GET", providerPath, nil, nil, 200)
	check(response, provider["etag"].(string), current["etag"].(string))
	slot := h.want(owner, "GET", slotPath, nil, nil, 200)
	headers := withMatch(slot, idem(uuid.NewString()))
	response, _ = h.do(owner, "DELETE", slotPath, nil, headers)
	if response.StatusCode != 204 {
		t.Fatalf("slot delete: %d", response.StatusCode)
	}
	after := h.want(owner, "GET", providerPath, nil, nil, 200)
	check(response, current["etag"].(string), after["etag"].(string))
	// A replay retains the original transition, even after another actor edits
	// the provider. It cannot claim that unrelated ETag as its own result.
	h.want(owner, "PATCH", providerPath, map[string]any{"name": "External edit", "configuration": after["configuration"]}, etagHeader(after), 200)
	replay, _ := h.do(owner, "DELETE", slotPath, nil, headers)
	if replay.StatusCode != 204 {
		t.Fatalf("replayed delete: %d", replay.StatusCode)
	}
	check(replay, current["etag"].(string), after["etag"].(string))
}
