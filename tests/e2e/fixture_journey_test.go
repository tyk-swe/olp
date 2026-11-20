//go:build e2e

package e2e_test

import (
	"bytes"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/tests/harness"
	"github.com/tyk-swe/olp/tests/harness/vendor"
)

func TestRouteFixtureComparisonKeepsPlainTextAndFlagsStructuredOutput(t *testing.T) {
	t.Parallel()
	s := startService(t)
	owner := s.Owner()
	project := s.Want(owner, "POST", "/api/v1/projects", map[string]any{"name": "route fixtures"}, harness.Idempotent(), http.StatusCreated)["id"].(string)
	var calls atomic.Int64
	handler := vendor.Handler()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	large := s.activeProviderWithModelFacts(owner, "fixture-large", upstream.URL, project, map[string]any{"context_length": 2048})
	small := s.activeProviderWithModelFacts(owner, "fixture-small", upstream.URL, project, map[string]any{"context_length": 64})
	slug := uniqueName("fixture-route")
	content := map[string]any{
		"slug": slug, "project_id": project, "operations": []string{"generation"}, "overall_timeout_ms": 5000, "max_attempts": 1,
		"fidelity": map[string]any{"mode": "transformed"},
		"targets": []any{
			map[string]any{"provider_id": large["id"], "provider_model": vendor.Model, "priority": 0, "weight": 1, "timeout_ms": 2000, "tags": []string{"large"}},
			map[string]any{"provider_id": small["id"], "provider_model": vendor.Model, "priority": 1, "weight": 1, "timeout_ms": 2000, "tags": []string{"small"}},
		},
	}
	draft := s.Want(owner, "POST", "/api/v1/route-drafts", content, harness.Idempotent(), http.StatusCreated)
	path := "/api/v1/route-drafts/" + draft["id"].(string)
	body := fixtureComparisonFile(slug)
	s.Want(owner, "POST", path+"/check-fixtures", body, nil, http.StatusConflict)
	first := s.Want(owner, "POST", path+"/activate", nil, harness.Idempotent(harness.ETag(draft)), http.StatusOK)
	newer := s.Want(owner, "POST", "/api/v1/route-drafts", content, harness.Idempotent(), http.StatusCreated)
	published := s.Want(owner, "POST", "/api/v1/route-drafts/"+newer["id"].(string)+"/activate", nil, harness.Idempotent(harness.ETag(newer)), http.StatusOK)
	content["selectors"] = []routing.Selector{
		{ID: "narrow-structured", Tags: []string{"small"}, When: routing.Predicate{StructuredOutput: new(true)}},
	}
	draft = s.Want(owner, "PUT", path, content, harness.Idempotent(harness.ETag(map[string]any{"etag": first["draft_etag"]})), http.StatusOK)

	readToken := s.Want(owner, "POST", "/api/v1/management-tokens", map[string]any{
		"name": "fixtures in CI", "scopes": []string{"read"}, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}, harness.Idempotent(), http.StatusCreated)
	reader := &harness.Caller{Name: "read-only automation", Token: readToken["secret"].(string)}
	beforeCalls, beforeAudit := calls.Load(), fixtureAuditCount(t, s)
	result := s.Want(reader, "POST", path+"/check-fixtures", body, nil, http.StatusOK)
	if result["passed"] != false || result["published_revision_id"] != published["revision_id"] || result["draft_etag"] != draft["etag"] || result["live_supply"] != "not_simulated" {
		t.Fatalf("incorrect comparison metadata: %v", result)
	}
	results := result["results"].([]any)
	structured, plain := results[0].(map[string]any), results[1].(map[string]any)
	if structured["passed"] != false || structured["published"].(map[string]any)["winner"] == nil || structured["draft"].(map[string]any)["winner"] != nil {
		t.Fatalf("structured fixture did not catch the regression: %v", structured)
	}
	for _, change := range []string{"winner", "eligibility", "selector", "reason"} {
		if !slices.Contains(structured["changes"].([]any), any(change)) {
			t.Errorf("structured fixture did not report %s: %v", change, structured["changes"])
		}
	}
	if plain["passed"] != true || len(plain["changes"].([]any)) != 0 || plain["draft"].(map[string]any)["winner"] == nil {
		t.Fatalf("plain fixture should still pass unchanged: %v", plain)
	}
	for _, variant := range []string{"published", "draft"} {
		targets := structured[variant].(map[string]any)["targets"].([]any)
		if !slices.ContainsFunc(targets, func(row any) bool {
			target := row.(map[string]any)
			window, ok := target["context_window"].(map[string]any)
			return ok && window["input_breakdown"] != nil
		}) {
			t.Errorf("%s omitted the input token breakdown: %v", variant, targets)
		}
	}
	status, raw := s.Do(reader, "POST", path+"/check-fixtures", body, nil)
	if status != http.StatusOK || bytes.Contains(raw, []byte("Synthetic green square")) || bytes.Contains(raw, []byte("response_format")) {
		t.Fatalf("comparison echoed fixture request content or returned an error: %s", describe(status, raw))
	}
	if calls.Load() != beforeCalls || fixtureAuditCount(t, s) != beforeAudit {
		t.Fatal("fixture comparison dispatched upstream or wrote an audit row")
	}
	if saved := s.Want(owner, "GET", path, nil, nil, http.StatusOK); saved["etag"] != draft["etag"] {
		t.Fatal("fixture check changed the draft")
	}
	body["synthetic"] = false
	s.Want(reader, "POST", path+"/check-fixtures", body, nil, http.StatusUnprocessableEntity)
	body["synthetic"] = true
	body["fixtures"] = slices.Repeat(body["fixtures"].([]any), 9)
	s.Want(reader, "POST", path+"/check-fixtures", body, nil, http.StatusUnprocessableEntity)
	s.Want(reader, "POST", path+"/check-fixtures", map[string]any{"synthetic": true, "fixtures": []any{strings.Repeat("x", 256<<10)}}, nil, http.StatusBadRequest)
}

func fixtureComparisonFile(slug string) map[string]any {
	const sample = "Synthetic green square"
	request := map[string]any{
		"model": slug, "messages": []any{map[string]string{"role": "user", "content": sample}}, "max_completion_tokens": 128,
	}
	structured := maps.Clone(request)
	structured["response_format"] = map[string]string{"type": "json_object"}
	fixture := func(name string, sample map[string]any) map[string]any {
		return map[string]any{"name": name, "operation": "generation", "surface": "openai", "mode": "unary", "seed": "same-seed", "expect_eligible": true, "request": sample}
	}
	return map[string]any{"synthetic": true, "fixtures": []any{fixture("Structured output", structured), fixture("Plain text", request)}}
}

func fixtureAuditCount(t *testing.T, s *service) int {
	t.Helper()
	var count int
	if err := s.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.audit").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
