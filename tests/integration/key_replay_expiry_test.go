//go:build integration

package integration_test

import (
	"reflect"
	"testing"
	"time"
)

func TestExpiredKeyCreationStillReplaysItsOriginalResult(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	expires := time.Now().Add(3 * time.Second)
	input := map[string]any{"name": "short-lived key", "expires_at": expires.Format(time.RFC3339Nano)}
	headers := map[string]string{"Idempotency-Key": "expiring-create"}
	status, created, originalHeaders := h.request(owner, "POST", "/api/v3/api-keys", input, headers)
	if status != 201 {
		t.Fatal("failed to create the short-lived key", status)
	}
	select {
	case <-time.After(time.Until(expires.Add(time.Millisecond))):
	case <-t.Context().Done():
		t.Fatal("test cancelled before key expiry")
	}
	status, replayed, replayHeaders := h.request(owner, "POST", "/api/v3/api-keys", input, headers)
	if status != 201 || !reflect.DeepEqual(created, replayed) || originalHeaders.Get("ETag") != replayHeaders.Get("ETag") || originalHeaders.Get("Location") != replayHeaders.Get("Location") {
		t.Fatal("expiry changed the original creation replay", status)
	}
	authority, err := h.Server.LookupAuthority(t.Context(), created["secret"].(string))
	if err != nil || authority.Allows("inference", "", nil, time.Now()) {
		t.Fatal("replay must not extend the key's authority")
	}
	h.want(owner, "POST", "/api/v3/api-keys", input, map[string]string{"Idempotency-Key": "new-expired-create"}, 422)
	input["name"] = "different request"
	h.want(owner, "POST", "/api/v3/api-keys", input, headers, 409)
	var count int
	if err = h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp_go.audit WHERE action='api_key.create'").Scan(&count); err != nil || count != 1 {
		t.Fatal("replay or rejected requests duplicated the creation audit")
	}
}
