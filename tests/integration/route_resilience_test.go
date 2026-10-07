//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/vendors"
)

// resilienceFields are a route's declared fallbacks, selectors, retry policy,
// affinity and spend cap, as the management API serves them.
var resilienceFields = []string{"fallbacks", "selectors", "retry", "affinity", "budget"}

// sameJSON compares two decoded JSON values after a round trip through JSON,
// so that a number decoded from either side compares equal.
func sameJSON(t *testing.T, got, want any) bool {
	t.Helper()
	normal := func(value any) any {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var decoded any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	return reflect.DeepEqual(normal(got), normal(want))
}

// Fallbacks, selectors, retry policy, affinity, spend caps, target tags and
// shadow targets round-trip through drafts, revisions, restore and
// configuration export, plan and apply.
func TestRouteResilienceRoundTripsThroughDraftsRevisionsAndConfiguration(t *testing.T) {
	h := newAccessHarness(t)
	fixture := newOpenAIFixture(t, "")
	generation := []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}
	owner, provider, backup, _ := provisionOpenAIWith(t, h, fixture.URL, generation, []string{"generation"}, nil)
	// A shadow target mirrors another connection, since a provider model
	// appears once per route.
	candidate := activeAzureProvider(t, h, owner, "Shadow candidate", fixture.URL, generation)

	behavior := map[string]any{
		"fallbacks": []any{map[string]any{"route": backup, "on": []any{"exhausted", "context_window"}}},
		"selectors": []any{map[string]any{"id": "short", "when": map[string]any{"max_input_tokens": 1000, "tools": false}, "tags": []any{"fast"}}},
		"retry":     map[string]any{"rate_limit": map[string]any{"max_retries": 2, "base_backoff_ms": 100, "max_backoff_ms": 1000, "respect_retry_after": true}},
		"affinity":  map[string]any{"source": "cache_key"},
		"budget":    map[string]any{"daily_cost_limit": "5", "monthly_cost_limit": "100"},
	}
	slug := "resilient-" + uuid.NewString()[:8]
	body := map[string]any{"slug": slug, "operations": []string{"generation"}, "overall_timeout_ms": 10000, "max_attempts": 2,
		"fidelity": map[string]any{"mode": "transformed"}, "targets": []any{
			map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000, "tags": []any{"fast"}},
			map[string]any{"provider_id": candidate["id"], "provider_model": vendorModel, "priority": 1, "weight": 1, "timeout_ms": 5000, "shadow": map[string]any{"sample_rate": 0.25}},
		}}
	for key, value := range behavior {
		body[key] = value
	}
	requireDeclared := func(label string, resource map[string]any) {
		t.Helper()
		for _, field := range resilienceFields {
			if !sameJSON(t, resource[field], behavior[field]) {
				t.Fatalf("%s %s = %v, want %v", label, field, resource[field], behavior[field])
			}
		}
		targets := resource["targets"].([]any)
		first, second := targets[0].(map[string]any), targets[1].(map[string]any)
		if !sameJSON(t, first["tags"], []any{"fast"}) || first["shadow"] != nil || !sameJSON(t, second["shadow"], map[string]any{"sample_rate": 0.25}) {
			t.Fatalf("%s targets %v", label, targets)
		}
	}

	for _, zero := range []string{"0", "0.0", "00.000000000000"} {
		for _, limit := range []string{"daily_cost_limit", "monthly_cost_limit"} {
			body["budget"] = map[string]any{limit: zero}
			h.want(owner, "POST", "/api/v1/route-drafts", body, idem(uuid.NewString()), 422)
		}
	}
	body["budget"] = behavior["budget"]
	draft := h.want(owner, "POST", "/api/v1/route-drafts", body, idem(uuid.NewString()), 201)
	requireDeclared("draft", h.want(owner, "GET", "/api/v1/route-drafts/"+draft["id"].(string), nil, nil, 200))

	// A fallback to a route that is not active, and a cycle, never publish.
	refused := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{"slug": "dangling-" + uuid.NewString()[:8], "operations": []string{"generation"},
		"overall_timeout_ms": 10000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"},
		"targets":   []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}},
		"fallbacks": []any{map[string]any{"route": "nowhere-route", "on": []any{"exhausted"}}}}, idem(uuid.NewString()), 201)
	if code := problemCode(t, h.want(owner, "POST", "/api/v1/route-drafts/"+refused["id"].(string)+"/activate", nil, withMatch(refused, idem(uuid.NewString())), 422)); code != "route_reference_unknown" {
		t.Fatalf("a dangling fallback published: %s", code)
	}

	activated := h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	routePath := "/api/v1/routes/" + activated["route_id"].(string)
	first := activated["revision_id"].(string)
	requireDeclared("revision", h.want(owner, "GET", routePath+"/revisions/"+first, nil, nil, 200))
	h.refresh()
	if route := h.Runtime.Release().Snapshot.Routes[slug]; len(route.Fallbacks) != 1 || route.Budget == nil || len(route.Targets) != 2 || route.Targets[1].Shadow == nil {
		t.Fatalf("published route %+v", route)
	}

	cycle := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{"slug": backup, "operations": []string{"generation"},
		"overall_timeout_ms": 10000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"},
		"targets":   []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}},
		"fallbacks": []any{map[string]any{"route": slug, "on": []any{"exhausted"}}}}, idem(uuid.NewString()), 201)
	if code := problemCode(t, h.want(owner, "POST", "/api/v1/route-drafts/"+cycle["id"].(string)+"/activate", nil, withMatch(cycle, idem(uuid.NewString())), 422)); code != "route_graph_cycle" {
		t.Fatalf("a fallback cycle published: %s", code)
	}

	// A second revision without the behavior differs from the first, and
	// restoring the first brings every declaration back.
	plain := map[string]any{}
	for key, value := range body {
		plain[key] = value
	}
	for _, field := range resilienceFields {
		delete(plain, field)
	}
	second := h.want(owner, "POST", "/api/v1/route-drafts", plain, idem(uuid.NewString()), 201)
	activated = h.want(owner, "POST", "/api/v1/route-drafts/"+second["id"].(string)+"/activate", nil, withMatch(second, idem(uuid.NewString())), 200)
	diff := h.want(owner, "GET", routePath+"/revisions/diff?from="+first+"&to="+activated["revision_id"].(string), nil, nil, 200)
	if diff["behavior_changed"] != true {
		t.Fatalf("revision diff %v", diff)
	}
	restored := h.want(owner, "POST", routePath+"/revisions/"+first+"/restore-as-draft", nil, idem(uuid.NewString()), 201)
	requireDeclared("restored draft", h.want(owner, "GET", "/api/v1/route-drafts/"+restored["id"].(string), nil, nil, 200))
	h.want(owner, "POST", "/api/v1/route-drafts/"+restored["id"].(string)+"/activate", nil, withMatch(restored, idem(uuid.NewString())), 200)

	// Export carries the declarations, and plans as a no-op; an edited
	// document stages them in a draft.
	export := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	document := export["document"].(map[string]any)
	var exported map[string]any
	for _, entry := range document["routes"].([]any) {
		if route := entry.(map[string]any); route["slug"] == slug {
			exported = route
		}
	}
	for _, field := range resilienceFields {
		if !sameJSON(t, exported[field], behavior[field]) {
			t.Fatalf("exported %s = %v", field, exported[field])
		}
	}
	planned := h.want(owner, "POST", "/api/v1/configuration/plan", map[string]any{"document": document}, nil, 200)
	if len(planned["conflicts"].([]any)) != 0 || len(planned["blockers"].([]any)) != 0 {
		t.Fatalf("exported document did not round-trip: %v", planned)
	}

	exported["retry"] = map[string]any{"timeout": map[string]any{"max_retries": 1, "base_backoff_ms": 50, "max_backoff_ms": 500, "respect_retry_after": false}}
	behavior["retry"] = exported["retry"]
	h.want(owner, "POST", "/api/v1/configuration/apply", map[string]any{"document": document}, idem(uuid.NewString()), 200)
	for _, item := range h.want(owner, "GET", "/api/v1/route-drafts", nil, nil, 200)["items"].([]any) {
		if staged := item.(map[string]any); staged["slug"] == slug && staged["state"] == "draft" {
			requireDeclared("applied draft", h.want(owner, "GET", "/api/v1/route-drafts/"+staged["id"].(string), nil, nil, 200))
			return
		}
	}
	t.Fatal("apply staged no draft for the edited route")
}

// A route template turns certified models into ordinary routes: provider
// activation generates them, the apply operation catches up on connections
// activated before the template existed, and an uncertified model never
// reaches a caller.
func TestRouteTemplatesPublishCertifiedModelsAsOrdinaryRoutes(t *testing.T) {
	h := newAccessHarness(t)
	fixture := newOpenAIFixture(t, "")
	generation := []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}
	owner := h.owner()
	earlier := activeAzureProvider(t, h, owner, "Earlier connection", fixture.URL, generation)
	vendor := vendors.DefaultFor("azure_openai")
	template := map[string]any{
		"name": "published-models", "provider_selector": "vendor:" + vendor, "model_filter": "*",
		"slug_pattern": "auto-{model}", "overall_timeout_ms": 10000, "max_attempts": 2,
		"fidelity": map[string]any{"mode": "transformed"}, "auto_publish": true,
	}
	created := h.want(owner, "POST", "/api/v1/route-templates", template, idem(uuid.NewString()), 201)
	path := "/api/v1/route-templates/" + created["id"].(string)

	applied := h.want(owner, "POST", path+"/apply", nil, idem(uuid.NewString()), 200)
	results, _ := applied["results"].([]any)
	if len(results) != 1 || applied["runtime_generation"] == nil {
		t.Fatalf("apply results %v", applied)
	}
	first := results[0].(map[string]any)
	slug, _ := first["route_slug"].(string)
	if first["outcome"] != "published" || first["provider_id"] != earlier["id"] || !strings.HasPrefix(slug, "auto-") {
		t.Fatalf("apply result %v", first)
	}
	h.refresh()
	route, ok := h.Runtime.Release().Snapshot.Routes[slug]
	if !ok || len(route.Targets) != 1 || route.Targets[0].ProviderID != earlier["id"] || route.Fidelity.Strict() {
		t.Fatalf("published route %+v", route)
	}
	if again := h.want(owner, "POST", path+"/apply", nil, idem(uuid.NewString()), 200); len(again["results"].([]any)) != 0 {
		t.Fatalf("a second apply regenerated %v", again)
	}

	// Activating another connection to the same model joins the generated
	// route as a target, in the activation's own transaction.
	later := activeAzureProvider(t, h, owner, "Later connection", fixture.URL, generation)
	h.refresh()
	route = h.Runtime.Release().Snapshot.Routes[slug]
	if len(route.Targets) != 2 || route.Targets[1].ProviderID != later["id"] {
		t.Fatalf("the activated connection did not join %+v", route.Targets)
	}
	detail := h.want(owner, "GET", path, nil, nil, 200)
	if members, _ := detail["routes"].([]any); len(members) != 2 {
		t.Fatalf("template members %v", detail["routes"])
	}
	listed := h.want(owner, "GET", "/api/v1/route-templates", nil, nil, 200)
	if items, _ := listed["items"].([]any); len(items) != 1 {
		t.Fatalf("templates %v", listed)
	}

	// Templates round-trip through configuration export and plan.
	export := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	document := export["document"].(map[string]any)
	templates, _ := document["templates"].([]any)
	if len(templates) != 1 || templates[0].(map[string]any)["slug_pattern"] != "auto-{model}" {
		t.Fatalf("exported templates %v", document["templates"])
	}
	planned := h.want(owner, "POST", "/api/v1/configuration/plan", map[string]any{"document": document}, nil, 200)
	for _, item := range planned["actions"].([]any) {
		if item := item.(map[string]any); item["kind"] == "route_template" && item["action"] != "noop" {
			t.Fatalf("an unchanged template planned %v", item)
		}
	}

	// A later operator revision makes the generated draft stale. Further
	// template joins stay drafts rather than overwriting the published limits.
	edited := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": slug, "operations": []string{"generation"}, "overall_timeout_ms": 20000, "max_attempts": 4,
		"fidelity": map[string]any{"mode": "transformed"}, "budget": map[string]any{"daily_cost_limit": "5"},
		"targets": []any{
			map[string]any{"provider_id": earlier["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000},
			map[string]any{"provider_id": later["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000},
		},
	}, idem(uuid.NewString()), 201)
	activated := h.want(owner, "POST", "/api/v1/route-drafts/"+edited["id"].(string)+"/activate", nil, withMatch(edited, idem(uuid.NewString())), 200)
	third := activeAzureProvider(t, h, owner, "Third connection", fixture.URL, generation)
	h.refresh()
	route = h.Runtime.Release().Snapshot.Routes[slug]
	if route.RevisionID != activated["revision_id"] || route.OverallTimeout != 20000 || route.MaxAttempts != 4 ||
		route.Budget == nil || route.Budget.DailyCostLimit == nil || *route.Budget.DailyCostLimit != "5" || len(route.Targets) != 2 {
		t.Fatalf("template join overwrote the operator revision: %+v", route)
	}
	detail = h.want(owner, "GET", path, nil, nil, 200)
	var staged bool
	for _, member := range detail["routes"].([]any) {
		member := member.(map[string]any)
		if member["provider_id"] == third["id"] {
			staged = member["outcome"] == "draft"
			joined := h.want(owner, "GET", "/api/v1/route-drafts/"+member["draft_id"].(string), nil, nil, 200)
			if len(joined["targets"].([]any)) != 3 {
				t.Fatalf("staged template join lost a target: %v", joined)
			}
		}
	}
	if !staged {
		t.Fatalf("stale template join was not staged: %v", detail["routes"])
	}
	h.want(owner, "DELETE", path, nil, withMatch(detail, nil), 204)
}

func TestRouteTemplateJoinsLeaveOperatorEditsPending(t *testing.T) {
	for _, edit := range []string{"validated draft", "routing policy"} {
		t.Run(edit, func(t *testing.T) {
			h := newAccessHarness(t)
			fixture := newOpenAIFixture(t, "")
			generation := []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}
			owner := h.owner()
			earlier := activeAzureProvider(t, h, owner, "Earlier connection", fixture.URL, generation)
			created := h.want(owner, "POST", "/api/v1/route-templates", map[string]any{
				"name": "published-models", "provider_selector": "vendor:" + vendors.DefaultFor("azure_openai"), "model_filter": "*",
				"slug_pattern": "auto-{model}", "overall_timeout_ms": 10000, "max_attempts": 2,
				"fidelity": map[string]any{"mode": "transformed"}, "auto_publish": true,
			}, idem(uuid.NewString()), 201)
			templatePath := "/api/v1/route-templates/" + created["id"].(string)
			applied := h.want(owner, "POST", templatePath+"/apply", nil, idem(uuid.NewString()), 200)
			first := applied["results"].([]any)[0].(map[string]any)
			slug := first["route_slug"].(string)
			draftPath := "/api/v1/route-drafts/" + first["draft_id"].(string)
			draft := h.want(owner, "GET", draftPath, nil, nil, 200)
			h.refresh()
			published := h.Runtime.Release().Snapshot.Routes[slug]
			if edit == "validated draft" {
				draft = h.want(owner, "PUT", draftPath, map[string]any{
					"slug": slug, "operations": []string{"generation"}, "overall_timeout_ms": 20000, "max_attempts": 4,
					"fidelity": map[string]any{"mode": "transformed"},
					"targets":  []any{map[string]any{"provider_id": earlier["id"], "provider_model": vendorModel, "weight": 1, "timeout_ms": 10000}},
				}, etagHeader(draft), 200)
				h.want(owner, "POST", draftPath+"/validate", nil, etagHeader(draft), 200)
			} else {
				policyPath := "/api/v1/routing-policies/route-draft/" + first["draft_id"].(string)
				policy := h.want(owner, "GET", policyPath, nil, nil, 200)
				h.want(owner, "PUT", policyPath, map[string]any{"defaults": map[string]any{"strategy": "price"}}, withMatch(policy, idem(uuid.NewString())), 200)
			}
			// Repeated joins must keep both the edits and every new target staged.
			for index := range 2 {
				activeAzureProvider(t, h, owner, fmt.Sprintf("Later connection %d", index), fixture.URL, generation)
				h.refresh()
				live := h.Runtime.Release().Snapshot.Routes[slug]
				if live.RevisionID != published.RevisionID || live.OverallTimeout != 10000 || live.MaxAttempts != 2 || len(live.Targets) != 1 {
					t.Fatalf("pending operator edits became live: %+v", live)
				}
				if live.Policy != nil && live.Policy.Defaults.Strategy != nil {
					t.Fatalf("pending routing policy became live: %+v", live.Policy)
				}
				draft = h.want(owner, "GET", draftPath, nil, nil, 200)
				if len(draft["targets"].([]any)) != index+2 || draft["based_on_revision_id"] != published.RevisionID {
					t.Fatalf("joined draft %v", draft)
				}
			}
			h.want(owner, "POST", draftPath+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
			h.refresh()
			live := h.Runtime.Release().Snapshot.Routes[slug]
			if len(live.Targets) != 3 || edit == "validated draft" && (live.OverallTimeout != 20000 || live.MaxAttempts != 4) {
				t.Fatalf("explicit activation lost pending edits: %+v", live)
			}
			if edit == "routing policy" && (live.Policy == nil || live.Policy.Defaults.Strategy == nil || *live.Policy.Defaults.Strategy != "price") {
				t.Fatalf("explicit activation lost routing policy: %+v", live.Policy)
			}
			// Explicit activation restores automatic joins to the approved draft.
			activeAzureProvider(t, h, owner, "Approved connection", fixture.URL, generation)
			h.refresh()
			if live := h.Runtime.Release().Snapshot.Routes[slug]; len(live.Targets) != 4 {
				t.Fatalf("approved draft stopped accepting automatic joins: %+v", live)
			}
		})
	}
}

// flakyUpstream serves the OpenAI fixture until broken, after which every chat
// completion streams one chunk and drops the connection, committing the
// caller's stream before failing.
func flakyUpstream(t *testing.T) (string, *atomic.Bool) {
	t.Helper()
	fixture := newOpenAIFixture(t, "")
	target, err := url.Parse(fixture.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	broken := new(atomic.Bool)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !broken.Load() || !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			proxy.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"`+vendorModel+`","choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":null}]}`+"\n\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(server.Close)
	return server.URL, broken
}

// Through the management API and the public gateway: a request cannot raise
// its priority above its key's ceiling, a fallback route serves a request
// whose route is exhausted before commitment, and no fallback follows a
// committed stream.
func TestRouteFallbacksAndPriorityCeilingsThroughTheGateway(t *testing.T) {
	h := newAccessHarness(t)
	healthy := newOpenAIFixture(t, "")
	generation := []any{
		map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"},
		map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"},
	}
	owner, _, backup, _ := provisionOpenAIWith(t, h, healthy.URL, generation, []string{"generation"}, nil)
	endpoint, broken := flakyUpstream(t)
	flaky := activeAzureProvider(t, h, owner, "Flaky connection", endpoint, generation)
	primary := "primary-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": primary, "operations": []string{"generation"}, "overall_timeout_ms": 10000, "max_attempts": 3,
		"fidelity":  map[string]any{"mode": "transformed"},
		"targets":   []any{map[string]any{"provider_id": flaky["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}},
		"fallbacks": []any{map[string]any{"route": backup, "on": []string{"exhausted"}}},
	}, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{
		"name": "resilient caller", "scopes": []string{"inference"}, "allowed_routes": []string{primary, backup},
		"priority": "low", "max_priority": "normal",
	}, idem(uuid.NewString()), 201)
	secret := key["secret"].(string)
	h.refresh()
	broken.Store(true)

	chat := func(stream bool, priority string) (int, []byte) {
		body := fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":"hi"}]}`, primary, stream)
		headers := map[string]string{"Content-Type": "application/json"}
		if priority != "" {
			headers["X-OLP-Routing"] = `{"priority":"` + priority + `"}`
		}
		status, raw, _ := h.gatewayRaw("POST", "/v1/chat/completions", secret, strings.NewReader(body), headers)
		return status, raw
	}

	if status, raw := chat(false, "high"); status != http.StatusBadRequest || !strings.Contains(string(raw), "priority_increase_forbidden") {
		t.Fatalf("a priority above the ceiling answered %d %s", status, raw)
	}
	if status, raw := chat(false, "normal"); status != http.StatusOK || !strings.Contains(string(raw), `"model":"`+primary+`"`) {
		t.Fatalf("the fallback route did not serve the exhausted route: %d %s", status, raw)
	}
	status, raw := chat(true, "")
	if status != http.StatusOK || !strings.Contains(string(raw), "partial") || strings.Contains(string(raw), `"content":"OK"`) {
		t.Fatalf("a committed stream continued on the fallback route: %d %s", status, raw)
	}
	// The backup's last request is still the unary fallback: the committed
	// stream never reached it.
	if last, _ := healthy.lastReq.Load().(map[string]any); last == nil || last["stream"] == true {
		t.Fatalf("the backup route served the committed stream: %v", last)
	}
}
