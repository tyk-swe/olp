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
	current := want("GET", "/api/v1/projects/"+id, nil, nil, 200)
	stale := etagHeader(read)
	stale["Idempotency-Key"] = "stale-project-delete"
	want("DELETE", "/api/v1/projects/"+id, nil, stale, 412)
	headers := etagHeader(current)
	headers["Idempotency-Key"] = "project-delete"
	want("DELETE", "/api/v1/projects/"+id, nil, headers, 204)
	want("DELETE", "/api/v1/projects/"+id, nil, headers, 204)
	want("GET", "/api/v1/projects/"+id, nil, nil, 404)
	retained := createProject(h, owner, "Retained project")
	h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Retained key", "project_id": retained}, map[string]string{"Idempotency-Key": "retained-project-key"}, 201)
	retainedRead := want("GET", "/api/v1/projects/"+retained, nil, nil, 200)
	retainedHeaders := etagHeader(retainedRead)
	retainedHeaders["Idempotency-Key"] = "retain-project-boundary"
	want("DELETE", "/api/v1/projects/"+retained, nil, retainedHeaders, 409)
	want("GET", "/api/v1/projects/"+retained, nil, nil, 200)
}
