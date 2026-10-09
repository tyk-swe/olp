//go:build integration

package integration_test

import (
	"encoding/json"
	"testing"
	"time"
)

func TestProjectAutomationRequiresOwnerScopeAndObservedETag(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	created := h.want(owner, "POST", "/api/v1/management-tokens", map[string]any{"name": "Project automation", "scopes": []string{"manage_projects"}, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, map[string]string{"Idempotency-Key": "project-automation-token"}, 201)
	caller := sweepCaller{token: created["secret"].(string)}
	want := func(method, path string, input any, headers map[string]string, expected int) map[string]any {
		status, _, body := h.call(caller, method, path, input, headers)
		if status != expected {
			t.Fatalf("%s %s status %d, want %d: %s", method, path, status, expected, body)
		}
		var result map[string]any
		if body != "" {
			if err := json.Unmarshal([]byte(body), &result); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	project := want("POST", "/api/v1/projects", map[string]any{"name": "Automated project"}, map[string]string{"Idempotency-Key": "automated-project"}, 201)
	id := project["id"].(string)
	read := want("GET", "/api/v1/projects/"+id, nil, nil, 200)
	want("PATCH", "/api/v1/projects/"+id, map[string]any{"name": "Renamed project"}, etagHeader(read), 200)
	want("PATCH", "/api/v1/projects/"+id, map[string]any{"name": "Stale rename"}, etagHeader(read), 412)
	want("GET", "/api/v1/providers", nil, nil, 403)
}
