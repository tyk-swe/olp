//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
)

func policyRulesEqual(t *testing.T, stored any, want []map[string]any) {
	t.Helper()
	policy, ok := stored.(map[string]any)
	if !ok {
		t.Fatalf("stored policy is not an object: %v", stored)
	}
	rules, _ := policy["rules"].([]any)
	if len(rules) != len(want) {
		t.Fatalf("stored rules %v, want %d", rules, len(want))
	}
	for i, raw := range rules {
		rule, _ := raw.(map[string]any)
		for key, value := range want[i] {
			if rule[key] != value {
				t.Fatalf("rule %d field %s = %v, want %v (rule %v)", i, key, rule[key], value, rule)
			}
		}
	}
}

func TestContentPolicy(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	sink := &captureSink{}
	h.Gateway.Sink = sink

	rules := []map[string]any{
		{"id": "mask-in", "phase": "input", "action": "redact", "pattern": "policy-marker-[0-9]+", "replacement": "[MASKED]"},
		{"id": "deny-in", "phase": "input", "action": "block", "pattern": "POLICY-DENY"},
		{"id": "mask-out", "phase": "output", "action": "redact", "pattern": "OK", "replacement": "clean"},
	}
	owner, detail, slug, secret := provisionOpenAIWith(t, h, fixture.URL,
		[]any{
			map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"},
			map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"},
			map[string]any{"operation": "batch", "surface": "openai", "mode": "unary"},
		},
		[]string{"generation", "batch"},
		map[string]any{"content_policy": map[string]any{"rules": rules}})

	bad := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": "policy-bad", "operations": []string{"generation"},
		"overall_timeout_ms": 10000, "max_attempts": 1,
		"targets":        []any{map[string]any{"provider_id": detail["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}},
		"content_policy": map[string]any{"rules": []any{map[string]any{"id": "bad", "phase": "input", "action": "block", "pattern": "("}}},
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 422)
	if problemCode(t, bad) != "validation_failed" {
		t.Fatalf("uncompilable policy: %v", bad)
	}

	routes := h.want(owner, "GET", "/api/v1/routes", nil, nil, 200)
	var routeID string
	for _, item := range routes["items"].([]any) {
		if item.(map[string]any)["slug"] == slug {
			routeID = item.(map[string]any)["id"].(string)
		}
	}
	if routeID == "" {
		t.Fatalf("route %s not listed: %v", slug, routes)
	}
	revisions := h.want(owner, "GET", "/api/v1/routes/"+routeID+"/revisions", nil, nil, 200)
	rev := revisions["items"].([]any)[0].(map[string]any)
	policyRulesEqual(t, rev["content_policy"], rules)

	status, body, _ := h.gateway("POST", "/v1/chat/completions", secret,
		map[string]any{"model": slug, "messages": []any{map[string]any{"role": "user", "content": "tell me policy-marker-9"}}})
	if status != http.StatusOK {
		t.Fatalf("redacted request = %d %v", status, body)
	}
	upstream, _ := fixture.lastReq.Load().(map[string]any)
	encodedUpstream, _ := json.Marshal(upstream)
	if !strings.Contains(string(encodedUpstream), "[MASKED]") || strings.Contains(string(encodedUpstream), "policy-marker-9") {
		t.Fatalf("upstream body not transformed: %s", encodedUpstream)
	}
	choices, _ := body["choices"].([]any)
	message, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "clean" {
		t.Fatalf("output not rewritten: %v", message)
	}
	event := sink.last()
	if len(event.PolicyDecisions) != 2 ||
		event.PolicyDecisions[0].RuleID != "mask-in" || event.PolicyDecisions[0].Outcome != "redacted" ||
		event.PolicyDecisions[1].RuleID != "mask-out" || event.PolicyDecisions[1].Outcome != "redacted" {
		t.Fatalf("envelope decisions %+v", event.PolicyDecisions)
	}
	if encoded, _ := json.Marshal(event); strings.Contains(string(encoded), "policy-marker-9") {
		t.Fatalf("envelope retained matched text: %s", encoded)
	}

	beforePath := fixture.lastPath.Load()
	status, blocked, _ := h.gateway("POST", "/v1/chat/completions", secret,
		map[string]any{"model": slug, "messages": []any{map[string]any{"role": "user", "content": "say POLICY-DENY now"}}})
	if status != http.StatusBadRequest || h.gatewayCode(status, blocked) != "content_policy_blocked" {
		t.Fatalf("blocked request = %d %v", status, blocked)
	}
	if fixture.lastPath.Load() != beforePath {
		t.Fatal("provider was called for a blocked request")
	}
	if last := sink.last().PolicyDecisions; len(last) != 1 || last[0].RuleID != "deny-in" || last[0].Outcome != "blocked" {
		t.Fatalf("block decisions %+v", last)
	}

	beforePath = fixture.lastPath.Load()
	status, raw, _ := h.gatewayRaw("POST", "/v1/chat/completions", secret,
		strings.NewReader(`{"model":"`+slug+`","messages":[{"role":"user","content":"hi"}],"stream":true}`),
		map[string]string{"Content-Type": "application/json"})
	if status != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "content_policy_streaming_requires_unary") {
		t.Fatalf("streaming preflight = %d %s", status, raw)
	}
	if fixture.lastPath.Load() != beforePath {
		t.Fatal("provider was called for a streaming preflight rejection")
	}

	beforeDials := fixture.dials.Load()
	status, upload := h.uploadTestFile(slug, secret, `{"custom_id":"1","body":{}}`+"\n")
	if status != http.StatusUnprocessableEntity || !strings.Contains(fmt.Sprint(upload), "content_policy_surface_unavailable") {
		t.Fatalf("upload surface gate = %d %v", status, upload)
	}
	if fixture.dials.Load() != beforeDials {
		t.Fatal("provider was called for a gated upload")
	}

	restored := h.want(owner, "POST",
		"/api/v1/routes/"+routeID+"/revisions/"+rev["id"].(string)+"/restore-as-draft",
		nil, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	policyRulesEqual(t, restored["content_policy"], rules)

	exported := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	var exportedRoute map[string]any
	for _, item := range exported["document"].(map[string]any)["routes"].([]any) {
		if item.(map[string]any)["slug"] == slug {
			exportedRoute = item.(map[string]any)
		}
	}
	if exportedRoute == nil {
		t.Fatalf("export missing route %s: %v", slug, exported)
	}
	policyRulesEqual(t, exportedRoute["content_policy"], rules)

	requestID := access.NewID()
	now := time.Now().UTC()
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.usage_request_anchors (request_id, request_started_at) VALUES ($1, $2) ON CONFLICT DO NOTHING`, requestID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.requests (id, runtime_generation_id, api_key_id, route_slug, operation,
	        surface, started_at, completed_at, status_code, attempt_count, policy_decisions)
	    VALUES ($1, $2, (SELECT id FROM olp.api_keys ORDER BY created_at LIMIT 1), $3, 'generation',
	        'openai', $4, $4, 200, 1, $5::jsonb)`,
		requestID, access.NewID(), slug, now,
		`[{"rule_id":"mask-in","phase":"input","action":"redact","outcome":"redacted"}]`); err != nil {
		t.Fatal(err)
	}
	detail2 := h.want(owner, "GET", "/api/v1/requests/"+requestID, nil, nil, 200)
	decisions, _ := detail2["policy_decisions"].([]any)
	if len(decisions) != 1 {
		t.Fatalf("history decisions %v", detail2)
	}
	decision, _ := decisions[0].(map[string]any)
	if len(decision) != 4 || decision["rule_id"] != "mask-in" || decision["phase"] != "input" ||
		decision["action"] != "redact" || decision["outcome"] != "redacted" {
		t.Fatalf("decision carries more than metadata: %v", decision)
	}

	for _, query := range []string{
		`SELECT coalesce(string_agg(policy_decisions::text,''),'') FROM olp.requests`,
		`SELECT coalesce(string_agg(routing::text,''),'') FROM olp.attempts`,
	} {
		var text string
		if err := h.Pool.QueryRow(t.Context(), query).Scan(&text); err != nil {
			t.Fatal(err)
		}
		for _, marker := range []string{"policy-marker-9", "POLICY-DENY", "clean"} {
			if strings.Contains(text, marker) {
				t.Fatalf("persisted record contains marker %q: %s", marker, text)
			}
		}
	}
	for _, env := range sink.all() {
		encoded, _ := json.Marshal(env)
		for _, marker := range []string{"policy-marker-9", "POLICY-DENY"} {
			if strings.Contains(string(encoded), marker) {
				t.Fatalf("terminal envelope contains marker %q: %s", marker, encoded)
			}
		}
	}
}
