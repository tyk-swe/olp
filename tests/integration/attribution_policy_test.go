//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestAttributionPoliciesEnforceBothBoundariesAndPersistResolvedLabels(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Attribution"}, idem("project"), 201)
	projectID := project["id"].(string)
	path := "/api/v1/projects/" + projectID + "/attribution-policy"
	initial := h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "PUT", path, map[string]any{}, etagHeader(initial), 422)
	for _, policy := range []any{
		map[string]any{"required_attribution_keys": []string{"team", "team"}},
		map[string]any{"attribution_defaults": map[string]string{"team": "raw text"}},
		map[string]any{"required_attribution_keys": []string{"a", "b", "c", "d", "e"}},
	} {
		h.want(owner, "PUT", path, map[string]any{"policy": policy}, etagHeader(initial), 422)
	}
	h.want(owner, "PUT", path, map[string]any{"policy": map[string]any{"required_attribution_keys": []string{"team"}, "attribution_defaults": map[string]string{"team": "core"}}}, etagHeader(initial), 200)
	fixture := newOpenAIFixture(t, "")
	provider := activeAzureProviderInProject(t, h, owner, "Scoped provider", fixture.URL, &projectID, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}})
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{"project_id": projectID, "slug": "attributed", "overall_timeout_ms": 10000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"}, "targets": []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}}}, idem("draft"), 201)
	draftPath := "/api/v1/route-drafts/" + draft["id"].(string)
	validated := h.want(owner, "POST", draftPath+"/validate", nil, etagHeader(draft), 200)
	h.want(owner, "POST", draftPath+"/activate", nil, withMatch(validated, idem("activate")), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Attributed key", "project_id": projectID, "scopes": []string{"inference"}, "allowed_routes": []string{"attributed"}, "allowed_attribution_keys": []string{"task"}, "required_attribution_keys": []string{"task"}, "attribution_defaults": map[string]string{"env": "prod"}}, idem("key"), 201)
	replica := newAccessHarnessOn(t, h.Pool, h.DBURL)
	replica.refresh()
	persisted := make(chan error, 16)
	replica.Gateway.Sink = &gateway.PersistingSink{Persist: func(ctx context.Context, ev *usage.Event, raw []byte) error {
		_, err := usage.PersistEvent(ctx, h.Pool, ev, raw)
		persisted <- err
		return err
	}}
	secret := key["secret"].(string)
	for _, tc := range []struct {
		header string
		status int
		code   string
	}{
		{"", 400, "missing_attribution"},
		{`{"task":"build","team":"other"}`, 400, "pinned_attribution_override"},
		{`{"task":"build","env":"dev"}`, 400, "pinned_attribution_override"},
		{`{"task":"build"}`, 200, ""},
	} {
		var headers []string
		if tc.header != "" {
			headers = []string{tc.header}
		}
		status, raw := attributionCall(t, replica, "attributed", secret, headers)
		if status != tc.status || tc.code != "" && !strings.Contains(string(raw), tc.code) {
			t.Fatalf("%d %s", status, raw)
		}
	}
	if err := <-persisted; err != nil {
		t.Fatal(err)
	}
	var labels map[string]string
	if err := h.Pool.QueryRow(t.Context(), "SELECT attribution FROM olp.requests WHERE api_key_id=$1 AND status_code=200", key["id"]).Scan(&labels); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(labels, map[string]string{"task": "build", "team": "core", "env": "prod"}) {
		t.Fatalf("stored labels: %v", labels)
	}
	current := h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "PUT", path, map[string]any{"policy": map[string]any{"required_attribution_keys": []string{"team"}, "attribution_defaults": map[string]string{"team": "next"}}}, etagHeader(current), 200)
	replica.refresh()
	if status, raw := attributionCall(t, replica, "attributed", secret, []string{`{"task":"build","team":"core"}`}); status != 400 {
		t.Fatalf("stale project policy: %d %s", status, raw)
	}
	current = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "PUT", path, map[string]any{"policy": nil}, etagHeader(current), 200)
	keyPath := "/api/v1/api-keys/" + key["id"].(string)
	detail := h.want(owner, "GET", keyPath, nil, nil, 200)
	h.want(owner, "PATCH", keyPath, map[string]any{"required_attribution_keys": nil, "attribution_defaults": nil}, etagHeader(detail), 200)
	replica.refresh()
	if status, raw := attributionCall(t, replica, "attributed", secret, nil); status != 200 {
		t.Fatalf("cleared policies: %d %s", status, raw)
	}
	if err := <-persisted; err != nil {
		t.Fatal(err)
	}
}

func TestProjectAttributionPromotionRequiresKeysAndRoundTrips(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Portable attribution"}, idem("project"), 201)
	path := "/api/v1/projects/" + project["id"].(string) + "/attribution-policy"
	policy := map[string]any{"required_attribution_keys": []string{"team"}, "attribution_defaults": map[string]string{"team": "core"}}
	initial := h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "PUT", path, map[string]any{"policy": policy}, etagHeader(initial), 200)
	doc := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)["document"].(map[string]any)
	entry := doc["projects"].([]any)[0].(map[string]any)
	encoded, _ := json.Marshal(entry["attribution_policy"])
	if !strings.Contains(string(encoded), "core") {
		t.Fatalf("missing portable policy: %s", encoded)
	}
	entry["attribution_policy"].(map[string]any)["attribution_defaults"].(map[string]any)["team"] = "platform"
	body := map[string]any{"document": doc}
	token := h.want(owner, "POST", "/api/v1/management-tokens", map[string]any{"name": "Configure", "scopes": []string{"configure"}, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, idem("token"), 201)["secret"].(string)
	h.machineWant(token, "POST", "/api/v1/configuration/apply", body, idem("denied"), 403)
	h.want(owner, "POST", "/api/v1/configuration/plan", body, nil, 200)
	h.want(owner, "POST", "/api/v1/configuration/apply", body, idem("apply"), 200)
	h.machineWant(token, "POST", "/api/v1/configuration/apply", body, idem("unchanged"), 200)
	saved := h.want(owner, "GET", path, nil, nil, 200)["policy"].(map[string]any)
	if saved["attribution_defaults"].(map[string]any)["team"] != "platform" {
		t.Fatal("policy not applied")
	}
}
