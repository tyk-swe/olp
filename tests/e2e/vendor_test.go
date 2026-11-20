//go:build e2e

package e2e_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/tests/harness"
	"github.com/tyk-swe/olp/tests/harness/vendor"
)

func newVendor(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(vendor.Handler())
	t.Cleanup(server.Close)
	return server
}

// activeProvider creates a provider against the fixture vendor and takes it
// through probe, certification and activation.
func (s *service) activeProvider(owner *harness.Caller, name, endpoint string, projectID string) map[string]any {
	s.T.Helper()
	provider := s.Want(owner, "POST", "/api/v1/providers", map[string]any{
		"name":          name,
		"configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": endpoint + "/v1"},
		"credential":    vendor.Secret,
		"model":         vendor.Model,
		"project_id":    projectID,
	}, harness.Idempotent(), http.StatusCreated)
	path := "/api/v1/providers/" + provider["id"].(string)
	probe := s.Want(owner, "POST", path+"/probe", nil, harness.ETag(provider), http.StatusOK)
	if probe["succeeded"] != true {
		s.T.Fatalf("probe failed: %v", probe)
	}
	models := s.Want(owner, "GET", path+"/models", nil, nil, http.StatusOK)["items"].([]any)
	modelID := ""
	for _, item := range models {
		model := item.(map[string]any)
		if model["upstream_model"] == vendor.Model {
			modelID = model["id"].(string)
		}
	}
	if modelID == "" {
		s.T.Fatalf("probe did not discover %s: %v", vendor.Model, models)
	}
	detail := s.Want(owner, "GET", path, nil, nil, http.StatusOK)
	detail = s.Want(owner, "PATCH", path+"/models/"+modelID, map[string]any{
		"enabled": true,
		"capabilities": []any{
			map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"},
			map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"},
		},
	}, harness.ETag(detail), http.StatusOK)
	certified := s.Want(owner, "POST", path+"/models/"+modelID+"/certify", nil, harness.ETag(detail), http.StatusOK)
	if certified["status"] != "certified" {
		s.T.Fatalf("certification failed: %v", certified)
	}
	detail = s.Want(owner, "GET", path, nil, nil, http.StatusOK)
	s.Want(owner, "POST", path+"/activate", nil, harness.Idempotent(harness.ETag(detail)), http.StatusOK)
	return provider
}

// publishedRoute drafts and activates a route onto provider, returning the
// activation response.
func (s *service) publishedRoute(owner *harness.Caller, slug string, provider map[string]any, projectID string) (map[string]any, map[string]any) {
	s.T.Helper()
	draft := s.Want(owner, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": slug, "operations": []string{"generation"}, "overall_timeout_ms": 5000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"},
		"targets":    []any{map[string]any{"provider_id": provider["id"], "provider_model": vendor.Model, "priority": 0, "weight": 1, "timeout_ms": 2000}},
		"project_id": projectID,
	}, harness.Idempotent(), http.StatusCreated)
	activated := s.Want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, harness.Idempotent(harness.ETag(draft)), http.StatusOK)
	return draft, activated
}

func uniqueName(prefix string) string { return prefix + "-" + uuid.NewString()[:8] }
