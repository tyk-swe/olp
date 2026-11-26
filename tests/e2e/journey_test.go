//go:build e2e

package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/tests/harness"
	"github.com/tyk-swe/olp/tests/harness/vendor"
)

func TestPublishedRouteServesAndSettlesUsage(t *testing.T) {
	t.Parallel()
	s := startService(t)
	owner := s.Owner()
	project := s.Want(owner, "POST", "/api/v1/projects", map[string]any{"name": "journey"}, harness.Idempotent(), http.StatusCreated)["id"].(string)
	provider := s.activeProvider(owner, "journey-provider", newVendor(t).URL, project)
	slug := uniqueName("journey")
	s.publishedRoute(owner, slug, provider, project)
	testSelectorPluginPins(t, s, owner, provider, project)
	key := s.Want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "journey", "scopes": []string{"inference"}, "project_id": project}, harness.Idempotent(), http.StatusCreated)
	consumer := &harness.Caller{Name: "journey key", Token: key["secret"].(string)}
	request := map[string]any{"model": slug, "messages": []any{map[string]any{"role": "user", "content": "hello"}}}

	s.Eventually("the published route serving a request", 20*time.Second, func() (bool, string) {
		status, raw := s.Do(consumer, "POST", "/v1/chat/completions", request, nil)
		if status != http.StatusOK {
			return false, describe(status, raw)
		}
		var completion struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(raw, &completion); err != nil {
			t.Fatalf("invalid completion: %v: %s", err, raw)
		}
		if len(completion.Choices) != 1 || !strings.Contains(completion.Choices[0].Message.Content, vendor.Answer) {
			t.Fatalf("unexpected completion: %s", raw)
		}
		return true, ""
	})

	s.Eventually("usage reaching durable storage", 20*time.Second, func() (bool, string) {
		var attempts, input, output int64
		err := s.Pool.QueryRow(t.Context(), `SELECT count(*),coalesce(sum(input_tokens),0),coalesce(sum(output_tokens),0) FROM olp.attempt_usage_facts`).Scan(&attempts, &input, &output)
		if err != nil {
			t.Fatal(err)
		}
		return attempts == 1 && input == vendor.InputTokens && output == vendor.OutputTokens,
			fmt.Sprintf("attempts=%d input=%d output=%d", attempts, input, output)
	})

	t.Run("simulation_requires_all_predicates", func(t *testing.T) {
		sub := s.withT(t)
		classifier := &routing.ClassifierPredicate{Route: slug, Labels: []string{"complex"}, TimeoutMS: 500}
		plugin := &routing.PluginPredicate{Digest: strings.Repeat("a", 64)}
		draft := sub.Want(owner, "POST", "/api/v1/route-drafts", map[string]any{
			"slug": uniqueName("simulation"), "operations": []string{"generation"}, "overall_timeout_ms": 5000, "max_attempts": 1,
			"fidelity": map[string]any{"mode": "transformed"}, "project_id": project,
			"targets": []any{map[string]any{"provider_id": provider["id"], "provider_model": vendor.Model, "priority": 0, "weight": 1, "timeout_ms": 2000, "tags": []string{"selected"}}},
			"selectors": []routing.Selector{
				{ID: "both", Tags: []string{"selected"}, When: routing.Predicate{Classifier: classifier, Plugin: plugin}},
				{ID: "plugin", Tags: []string{"selected"}, When: routing.Predicate{Plugin: plugin}},
				{ID: "classifier", Tags: []string{"selected"}, When: routing.Predicate{Classifier: classifier}},
			},
		}, harness.Idempotent(), http.StatusCreated)
		result := sub.Want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/simulate", map[string]any{
			"operation": "generation", "surface": "openai", "mode": "unary", "seed": "predicates",
			"classifier_labels": map[string]string{"both": "complex", "classifier": "complex"},
		}, nil, http.StatusOK)
		selectors, ok := result["selectors"].([]any)
		if !ok || len(selectors) != 3 {
			t.Fatalf("simulation selectors = %v, want both plugin predicates skipped before classifier", result["selectors"])
		}
		for i, id := range []string{"both", "plugin", "classifier"} {
			selector := selectors[i].(map[string]any)
			want := "plugin_not_simulated"
			if id == "classifier" {
				want = "matched"
			}
			if selector["id"] != id || selector["matched"] != (id == "classifier") || selector["outcome"] != want {
				t.Fatalf("selector %s = %v, want %s", id, selector, want)
			}
		}
	})
}

func testSelectorPluginPins(t *testing.T, s *service, owner *harness.Caller, provider map[string]any, project string) {
	t.Helper()
	const digest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := s.Pool.Exec(t.Context(), `INSERT INTO olp.plugins(digest, abi_version, manifest, module, size_bytes, etag, installed_by, approved_by, approved_at)
 SELECT $1, 1, '{"name":"selector-fixture","version":"0.1.0","origins":[],"profiles":[]}', '\x0061736d01000000'::bytea, 8, gen_random_uuid(), id, id, now() FROM olp.users WHERE email='owner@example.com'`, digest); err != nil {
		t.Fatal(err)
	}
	pluginPath := "/api/v1/plugins/" + digest
	installed := s.Want(owner, "GET", pluginPath, nil, nil, http.StatusOK)
	draft := s.Want(owner, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": uniqueName("selector-pin"), "project_id": project, "operations": []string{"generation"}, "overall_timeout_ms": 5000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"},
		"targets":   []any{map[string]any{"provider_id": provider["id"], "provider_model": vendor.Model, "priority": 0, "weight": 1, "timeout_ms": 2000, "tags": []string{"selected"}}},
		"selectors": []routing.Selector{{ID: "module", Tags: []string{"selected"}, When: routing.Predicate{Plugin: &routing.PluginPredicate{Digest: digest}}}},
	}, harness.Idempotent(), http.StatusCreated)
	requirePinned := func() {
		t.Helper()
		problem := s.Want(owner, "DELETE", pluginPath, nil, harness.Idempotent(harness.ETag(installed)), http.StatusConflict)
		if !strings.HasSuffix(problem["type"].(string), "/plugin_pinned") {
			t.Fatalf("selector pin uninstall = %v", problem)
		}
	}
	requirePinned()
	draftPath := "/api/v1/route-drafts/" + draft["id"].(string)
	activated := s.Want(owner, "POST", draftPath+"/activate", nil, harness.Idempotent(harness.ETag(draft)), http.StatusOK)
	current := s.Want(owner, "GET", draftPath, nil, nil, http.StatusOK)
	s.Want(owner, "DELETE", draftPath, nil, harness.Idempotent(harness.ETag(current)), http.StatusNoContent)
	requirePinned()
	routePath := "/api/v1/routes/" + activated["route_id"].(string)
	route := s.Want(owner, "GET", routePath, nil, nil, http.StatusOK)
	s.Want(owner, "POST", routePath+"/retire", nil, harness.Idempotent(harness.ETag(route)), http.StatusOK)
	requirePinned()
}

func TestFirstOwnerSetupWaitsForAMountedBootstrapToken(t *testing.T) {
	t.Parallel()
	pool, databaseURL := harness.Database(t)
	env := harness.NewEnvironment(t, databaseURL, "Setup test")
	tokenFile := env.Values["OLP_BOOTSTRAP_TOKEN_FILE"]
	if err := os.Remove(tokenFile); err != nil {
		t.Fatal(err)
	}
	s := harness.StartProcess(t, pool, env)
	status := func() any {
		return s.Want(nil, "GET", "/api/v1/setup/status", nil, nil, http.StatusOK)["bootstrap_token_configured"]
	}
	if configured := status(); configured != false {
		t.Fatalf("bootstrap_token_configured=%v without a token file", configured)
	}
	s.Want(nil, "POST", "/api/v1/setup", map[string]any{"email": "owner@example.com", "display_name": "Owner", "password": "a long service test password", "installation_name": "Setup test"}, map[string]string{"X-OLP-Setup-Token": env.Bootstrap}, http.StatusForbidden)
	if err := os.WriteFile(tokenFile, []byte(env.Bootstrap), 0o600); err != nil {
		t.Fatal(err)
	}
	if configured := status(); configured != true {
		t.Fatalf("bootstrap_token_configured=%v after mounting the token", configured)
	}
	s.Owner()
}

func TestManagementTransportDeniesForeignOriginsExceptOnProtocolEndpoints(t *testing.T) {
	t.Parallel()
	s := startService(t)
	owner := s.Owner()
	foreign := map[string]string{"Origin": "https://attacker.example"}
	crossSite := map[string]string{"Sec-Fetch-Site": "cross-site"}
	both := map[string]string{"Origin": "https://attacker.example", "Sec-Fetch-Site": "cross-site"}
	denied := func(what string, status int, raw []byte) {
		t.Helper()
		if status != http.StatusForbidden || !strings.Contains(string(raw), "origin_denied") {
			t.Fatalf("%s: %s", what, describe(status, raw))
		}
	}
	admitted := func(what string, status int, raw []byte) {
		t.Helper()
		if strings.Contains(string(raw), "origin_denied") {
			t.Fatalf("%s: %s", what, describe(status, raw))
		}
	}

	status, raw := s.Do(owner, "POST", "/api/v1/projects", map[string]any{"name": "foreign"}, harness.Idempotent(foreign))
	denied("cookie write from a foreign origin", status, raw)
	status, raw = s.Do(owner, "GET", "/api/v1/projects", nil, crossSite)
	denied("cross-site cookie read", status, raw)
	if status, raw := s.Do(owner, "GET", "/api/v1/projects", nil, foreign); status != http.StatusOK {
		t.Fatalf("same-site read with a foreign Origin header: %s", describe(status, raw))
	}

	token := s.Want(owner, "POST", "/api/v1/management-tokens", map[string]any{
		"name": "automation", "scopes": []string{"keys"}, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}, harness.Idempotent(), http.StatusCreated)
	machine := &harness.Caller{Name: "automation token", Token: token["secret"].(string)}
	if status, raw := s.Do(machine, "POST", "/api/v1/api-keys", map[string]any{"name": "automated", "scopes": []string{"inference"}}, harness.Idempotent(both)); status != http.StatusCreated {
		t.Fatalf("bearer write from a foreign origin: %s", describe(status, raw))
	}

	status, raw = s.Do(owner, "GET", "/api/v1/oidc/callback", nil, crossSite)
	admitted("cross-site OIDC callback", status, raw)
	status, raw = s.Do(owner, "GET", "/api/v1/saml/complete", nil, crossSite)
	admitted("cross-site SAML completion", status, raw)
	status, raw = s.Do(owner, "POST", "/api/v1/saml/acs", nil, both)
	admitted("SAML response posted by the identity provider", status, raw)
	status, raw = s.Do(owner, "POST", "/api/v1/saml/login", nil, foreign)
	denied("SAML login from a foreign origin", status, raw)

	if status, raw := s.Do(owner, "GET", "/scim/v2/Users", nil, both); status != http.StatusForbidden || strings.Contains(string(raw), "origin_denied") {
		t.Fatalf("SCIM with a console session: %s", describe(status, raw))
	}

	conflicting := map[string]string{"Cookie": "__Host-olp_session=one; __Host-olp_session=two"}
	if status, raw := s.Do(nil, "GET", "/api/v1/projects", nil, conflicting); status != http.StatusBadRequest || !strings.Contains(string(raw), "conflicting_cookie_values") {
		t.Fatalf("conflicting cookies: %s", describe(status, raw))
	}
	if status, raw := s.Do(nil, "GET", "/scim/v2/Users", nil, conflicting); status != http.StatusUnauthorized {
		t.Fatalf("SCIM ignores cookies: %s", describe(status, raw))
	}
}

func TestPlaygroundRunsARouteForItsProjectMembersStreamsItAndAccountsItToTheInstallation(t *testing.T) {
	t.Parallel()
	s := startService(t)
	owner := s.Owner()
	project := s.Want(owner, "POST", "/api/v1/projects", map[string]any{"name": "playground"}, harness.Idempotent(), http.StatusCreated)["id"].(string)
	provider := s.activeProvider(owner, "playground-provider", newVendor(t).URL, project)
	slug := uniqueName("playground")
	s.publishedRoute(owner, slug, provider, project)
	typed := map[string]any{"model": slug, "input": "hello"}
	streamed := map[string]any{"model": slug, "input": "hello", "stream": true}

	answered := func(what string, result map[string]any) {
		t.Helper()
		attempts, _ := result["routing"].([]any)
		usage, _ := result["usage"].(map[string]any)
		if result["model"] != slug || len(attempts) != 1 || usage["input_tokens"] != float64(vendor.InputTokens) || usage["output_tokens"] != float64(vendor.OutputTokens) {
			t.Fatalf("%s = %v, want one attempt on %s with the provider's usage", what, result, slug)
		}
		attempt := attempts[0].(map[string]any)
		if attempt["provider_id"] != provider["id"] || attempt["upstream_model"] != vendor.Model {
			t.Fatalf("%s attempt = %v, want provider %v model %s", what, attempt, provider["id"], vendor.Model)
		}
	}
	s.Eventually("the playground reaching the published route", 20*time.Second, func() (bool, string) {
		status, raw := s.Do(owner, "POST", "/api/v1/playground", typed, nil)
		return status == http.StatusOK, describe(status, raw)
	})
	document := map[string]any{"model": slug, "request": map[string]any{"model": slug, "messages": []any{map[string]any{"role": "user", "content": "hello"}}}}
	for what, body := range map[string]map[string]any{"typed input": typed, "request document": document} {
		result := s.Want(owner, "POST", "/api/v1/playground", body, nil, http.StatusOK)
		if result["output_text"] != vendor.Answer || result["provider_model"] != vendor.Model {
			t.Fatalf("%s = %v, want the provider's answer", what, result)
		}
		answered(what, result)
	}

	status, raw := s.Do(owner, "POST", "/api/v1/playground/stream", streamed, nil)
	events := playgroundEvents(t, raw)
	if status != http.StatusOK || len(events) < 2 || events[len(events)-1].name != "done" {
		t.Fatalf("stream = %s, want provider frames then done", describe(status, raw))
	}
	text := ""
	for _, event := range events[:len(events)-1] {
		frame, _ := event.data["data"].(string)
		if event.name != "frame" || frame == "" {
			t.Fatalf("stream event = %v, want a provider frame", event)
		}
		text += frame
	}
	if !strings.Contains(text, vendor.Answer) {
		t.Fatalf("stream frames = %q, want the provider's answer", text)
	}
	answered("stream summary", events[len(events)-1].data)

	for what, refused := range map[string]struct {
		path string
		body map[string]any
	}{
		"a stream on the whole reply endpoint":  {"/api/v1/playground", streamed},
		"a whole reply on the stream endpoint":  {"/api/v1/playground/stream", typed},
		"embeddings without a request document": {"/api/v1/playground", map[string]any{"model": slug, "operation": "embeddings", "input": "hello"}},
		"a request document beside typed input": {"/api/v1/playground", map[string]any{"model": slug, "input": "hello", "request": document["request"]}},
	} {
		if status, raw := s.Do(owner, "POST", refused.path, refused.body, nil); status != http.StatusUnprocessableEntity {
			t.Errorf("%s: %s, want 422", what, describe(status, raw))
		}
	}

	outsider, _ := s.assignProjectMember(owner, "outsider@example.com", authz.RoleDeveloper, "", "")
	missing := map[string]any{"model": "missing", "input": "hello"}
	for path, body := range map[string]map[string]any{"/api/v1/playground": typed, "/api/v1/playground/stream": streamed} {
		hidden := s.Want(outsider, "POST", path, body, nil, http.StatusNotFound)
		absent := s.Want(owner, "POST", "/api/v1/playground", missing, nil, http.StatusNotFound)
		if hidden["type"] != absent["type"] || hidden["detail"] != strings.ReplaceAll(absent["detail"].(string), "missing", slug) {
			t.Errorf("%s outside the project = %v, want the reply of a missing route %v", path, hidden, absent)
		}
	}
	viewer := s.Invite(owner, "viewer@example.com", authz.RoleViewer)
	if status, raw := s.Do(viewer, "POST", "/api/v1/playground", typed, nil); status != http.StatusForbidden {
		t.Errorf("viewer: %s, want 403", describe(status, raw))
	}

	s.Eventually("playground usage reaching durable storage", 20*time.Second, func() (bool, string) {
		var attempts, unkeyed int64
		err := s.Pool.QueryRow(t.Context(), `SELECT count(*),count(*) FILTER (WHERE r.origin='playground' AND r.api_key_id IS NULL AND f.api_key_id IS NULL)
 FROM olp.attempt_usage_facts f JOIN olp.requests r ON r.id=f.request_id`).Scan(&attempts, &unkeyed)
		if err != nil {
			t.Fatal(err)
		}
		return attempts == 4 && unkeyed == 4, fmt.Sprintf("attempts=%d playground attempts without a key=%d", attempts, unkeyed)
	})
}

type playgroundEvent struct {
	name string
	data map[string]any
}

func playgroundEvents(t *testing.T, raw []byte) []playgroundEvent {
	t.Helper()
	var events []playgroundEvent
	for block := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n\n") {
		name, payload, found := strings.Cut(block, "\ndata: ")
		event := playgroundEvent{name: strings.TrimPrefix(name, "event: ")}
		if !found || json.Unmarshal([]byte(payload), &event.data) != nil {
			t.Fatalf("playground stream block %q is not an event with JSON data", block)
		}
		events = append(events, event)
	}
	return events
}
