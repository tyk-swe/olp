//go:build integration

package integration_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestExportSinkAuthorityRejectsForeignProjectsAndScopedInstallationWrites(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project, other := createProject(h, owner, "Sink owner scope"), createProject(h, owner, "Other sink scope")
	created := h.want(owner, "POST", "/api/v1/sinks", map[string]any{"name": "Scoped facts", "project_id": project, "type": "https", "destination": "http://127.0.0.1:4199/export", "streams": []string{"requests"}, "enabled": false}, idem(uuid.NewString()), 201)
	token := h.want(owner, "POST", "/api/v1/management-tokens", map[string]any{"name": "Other project keys", "scopes": []string{"read", "keys"}, "project_ids": []string{other}, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, idem(uuid.NewString()), 201)
	caller := sweepCaller{token: token["secret"].(string)}
	path := "/api/v1/sinks/" + created["id"].(string)
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		status, _, raw := h.call(caller, method, path, map[string]any{}, map[string]string{"If-Match": `"` + created["etag"].(string) + `"`, "Idempotency-Key": uuid.NewString()})
		if status != http.StatusNotFound {
			t.Fatalf("foreign %s = %d: %s", method, status, raw)
		}
	}
	status, _, raw := h.call(caller, "POST", "/api/v1/sinks", map[string]any{"name": "Unscoped facts", "type": "https", "destination": "http://127.0.0.1:4199/export", "streams": []string{"requests"}, "enabled": false}, idem(uuid.NewString()))
	if status != 403 {
		t.Fatalf("scoped token created installation sink: %d %s", status, raw)
	}
	body := map[string]any{"name": "Scoped facts", "project_id": project, "type": "https", "destination": "http://127.0.0.1:4199/export", "streams": []string{"requests"}, "enabled": false}
	h.want(owner, "POST", "/api/v1/sinks", body, idem(uuid.NewString()), 409)
}
