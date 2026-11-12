//go:build e2e

package e2e_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/tests/harness/vendor"
)

const legacyRouteSlug = "legacy-content-policy-for-a-route-whose-slug-is-long-enough-to-hash-its-name"

func testPromotionRoundTrip(t *testing.T, parent *service, owner *caller, fixture canaryFixture) {
	s := &service{t: t, pool: parent.pool, origin: parent.origin, env: parent.env}
	operator := s.seedPromotion(owner, fixture)

	exported := s.want(operator, "GET", "/api/v1/configuration/export", nil, nil, http.StatusOK)
	document := exported["document"].(map[string]any)
	assertCanonicalDocument(t, document)
	assertExportedBillingImport(t, document)
	request := map[string]any{"document": document, "expected_digest": exported["digest"]}

	planned := s.want(operator, "POST", "/api/v1/configuration/plan", request, nil, http.StatusOK)
	assertSettledPlan(t, planned)
	if planned["digest"] != exported["digest"] {
		t.Errorf("plan digest %v differs from export digest %v", planned["digest"], exported["digest"])
	}

	s.want(operator, "POST", "/api/v1/configuration/apply", request, idempotent(), http.StatusOK)

	reexported := s.want(operator, "GET", "/api/v1/configuration/export", nil, nil, http.StatusOK)
	if reexported["digest"] != exported["digest"] {
		t.Errorf("applying an unchanged export moved the digest from %v to %v", exported["digest"], reexported["digest"])
	}
	replanned := s.want(operator, "POST", "/api/v1/configuration/plan", request, nil, http.StatusOK)
	if !reflect.DeepEqual(replanned, planned) {
		t.Errorf("re-plan after apply differs:\nbefore %v\nafter  %v", planned, replanned)
	}

	legacy := s.want(operator, "POST", "/api/v1/configuration/plan", map[string]any{"document": legacyDocument(t, document)}, nil, http.StatusOK)
	policy := "route.legacy-content-policy-for-a-route-whose-slug-is-long-enoug-25c7b87b3a3e376a"
	want := []map[string]any{
		{"kind": "guardrail_attachment", "key": "route/" + legacyRouteSlug + "/" + policy, "action": "create"},
		{"kind": "guardrail_policy", "key": "canary-project/" + policy, "action": "create"},
	}
	got := []map[string]any{}
	for _, action := range items(legacy["actions"]) {
		if action["kind"] == "guardrail_attachment" || action["kind"] == "guardrail_policy" {
			got = append(got, map[string]any{"kind": action["kind"], "key": action["key"], "action": action["action"]})
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("v1 content policy converted to %v, want %v", got, want)
	}

	s.want(operator, "POST", "/api/v1/configuration/apply", map[string]any{"document": installationPolicyDocument(document)}, idempotent(), http.StatusOK)
	policies := items(s.want(operator, "GET", "/api/v1/guardrail-policies", nil, nil, http.StatusOK)["items"])
	index := slices.IndexFunc(policies, func(p map[string]any) bool {
		return p["name"] == "installation-wide" && p["project_id"] == "installation"
	})
	if index < 0 {
		t.Fatalf("installation policy is not listed: %v", policies)
	}
	installed := policies[index]
	s.want(operator, "POST", "/api/v1/guardrail-attachments", map[string]any{
		"scope": "installation", "scope_id": "installation", "policy_revision_id": installed["revision_id"], "order": 1,
	}, idempotent(), http.StatusCreated)
	testPromotionEmptyAttributionPolicyConverges(t, s, operator)
}

func installationPolicyDocument(exported map[string]any) map[string]any {
	guardrails, _ := exported["guardrails"].([]any)
	policies, _ := exported["guardrail_policies"].([]any)
	document := maps.Clone(exported)
	document["guardrails"] = slices.Concat([]any{map[string]any{
		"project": "", "name": "installation-keywords", "type": "builtin.keywords", "configuration": map[string]any{"words": []any{"forbidden"}},
	}}, guardrails)
	document["guardrail_policies"] = slices.Concat([]any{map[string]any{
		"project": "", "name": "installation-wide", "entries": []any{map[string]any{"guardrail": "installation-keywords", "phase": "input", "action": "block"}},
	}}, policies)
	return document
}

func (s *service) installationToken(owner *caller, name string) *caller {
	s.t.Helper()
	scopes := make([]string, 0, len(access.TokenScopes()))
	for _, operation := range access.TokenScopes() {
		scopes = append(scopes, operation.String())
	}
	token := s.want(owner, "POST", "/api/v1/management-tokens", map[string]any{
		"name": name, "scopes": scopes, "expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	}, idempotent(), http.StatusCreated)
	return &caller{name: name + " token", token: token["secret"].(string)}
}

func (s *service) seedPromotion(owner *caller, fixture canaryFixture) *caller {
	s.t.Helper()
	operator := s.installationToken(owner, "promotion")

	zone := s.want(operator, "GET", "/api/v1/settings/budgets.time_zone", nil, nil, http.StatusOK)
	s.want(operator, "PUT", "/api/v1/settings/budgets.time_zone", map[string]any{"value": "Europe/Berlin"}, etag(zone), http.StatusOK)
	budget := s.want(operator, "GET", "/api/v1/budgets/installation", nil, nil, http.StatusOK)
	s.want(operator, "PUT", "/api/v1/budgets/installation", map[string]any{"policy": map[string]any{"monthly_cost_limit": "500.00"}}, etag(budget), http.StatusOK)
	organization := fixture.ids["organization_id"]
	second := s.want(operator, "POST", "/api/v1/organizations/"+organization+"/projects", map[string]any{"name": "Promotion-Second"}, idempotent(), http.StatusCreated)["id"].(string)
	s.want(operator, "POST", "/api/v1/route-templates", map[string]any{
		"name": "promotion-template", "provider_selector": "provider:" + fixture.ids["provider_id"], "model_filter": "*",
		"slug_pattern": "templated-{model}", "overall_timeout_ms": 5000, "max_attempts": 1, "project_id": fixture.project,
	}, idempotent(), http.StatusCreated)
	price := func(model, input string) map[string]any {
		return map[string]any{"provider_kind": "openai_compatible", "provider_id": fixture.ids["provider_id"], "model": model, "operation": "generation", "currency": "USD", "input_per_million": input, "output_per_million": "2.00"}
	}
	s.want(operator, "POST", "/api/v1/pricing/revisions", map[string]any{
		"effective_at": time.Now().UTC().Format(time.RFC3339), "prices": []any{price(vendor.Model, "1.00"), price("a-cheaper-model", "0.50")},
	}, idempotent(), http.StatusCreated)
	s.want(operator, "POST", "/api/v1/pricing/discounts", map[string]any{"provider_kind": "openai_compatible", "provider_id": fixture.ids["provider_id"], "multiplier": "0.9"}, idempotent(), http.StatusCreated)
	card := s.want(operator, "POST", "/api/v1/rate-cards", map[string]any{"owner": map[string]any{"kind": "organization", "id": organization}}, idempotent(), http.StatusCreated)
	s.want(operator, "POST", "/api/v1/rate-cards/"+card["id"].(string)+"/revisions", map[string]any{"markup": "1.25"}, idempotent(), http.StatusCreated)
	group := func(name string, projects ...string) {
		grants := make([]any, 0, len(projects))
		for _, project := range projects {
			grants = append(grants, map[string]any{"value": project, "role": "viewer"})
		}
		s.want(operator, "POST", "/scim/v2/Groups", map[string]any{
			"schemas":     []string{"urn:ietf:params:scim:schemas:core:2.0:Group"},
			"displayName": name,
			"urn:openllmproxy:params:scim:schemas:extension:2.0:Group": map[string]any{"role": "viewer", "accessScope": "assigned", "projects": grants},
		}, nil, http.StatusCreated)
	}
	bedrock := s.want(operator, "POST", "/api/v1/providers", map[string]any{
		"name": "promotion-bedrock", "project_id": fixture.project,
		"configuration": map[string]any{"kind": "bedrock", "auth_mode": "default_chain", "cloud_region": "us-east-1"},
	}, idempotent(), http.StatusCreated)
	s.want(operator, "PUT", "/api/v1/providers/"+bedrock["id"].(string)+"/billing-import", map[string]any{
		"credential": map[string]any{"auth_mode": "workload"},
		"scope": map[string]any{
			"payer_account_id": "123456789012", "bucket": "olp-cur", "report_prefix": "cur/olp", "region": "us-east-1",
			"linked_account_ids": []string{"210987654321", "123456789012"},
		},
	}, idempotent(etag(bedrock)), http.StatusCreated)
	group("zeta", second, fixture.project)
	group("alpha", fixture.project)
	mfa := s.want(operator, "GET", "/api/v1/settings/auth.mfa_required", nil, nil, http.StatusOK)
	s.want(operator, "PUT", "/api/v1/settings/auth.mfa_required", map[string]any{"value": "true"}, etag(mfa), http.StatusOK)
	return operator
}

func assertSettledPlan(t *testing.T, plan map[string]any) {
	t.Helper()
	for _, list := range []string{"conflicts", "blockers"} {
		if found := items(plan[list]); len(found) > 0 {
			t.Errorf("plan of an unchanged export has %s: %v", list, found)
		}
	}
	actions := items(plan["actions"])
	for _, action := range actions {
		if action["action"] != "noop" && action["action"] != "reuse" {
			t.Errorf("plan of an unchanged export wants %v %v %v", action["action"], action["kind"], action["key"])
		}
	}
	assertSorted(t, "actions", actions, func(a map[string]any) []string {
		return []string{text(a, "kind"), text(a, "key"), text(a, "action"), text(a, "detail")}
	})
}

func assertCanonicalDocument(t *testing.T, document map[string]any) {
	t.Helper()
	byName := func(entry map[string]any) []string {
		return []string{strings.ToLower(text(entry, "name")), text(entry, "name")}
	}
	byProjectName := func(entry map[string]any) []string {
		return []string{strings.ToLower(text(entry, "project")), strings.ToLower(text(entry, "name"))}
	}
	for _, list := range []string{"projects", "providers", "organizations"} {
		assertSorted(t, list, items(document[list]), byName)
	}
	assertSorted(t, "routes", items(document["routes"]), func(r map[string]any) []string { return []string{text(r, "slug")} })
	assertSorted(t, "templates", items(document["templates"]), func(r map[string]any) []string { return []string{text(r, "name")} })
	assertSorted(t, "guardrails", items(document["guardrails"]), byProjectName)
	assertSorted(t, "guardrail_policies", items(document["guardrail_policies"]), byProjectName)
	mappings := items(document["scim_group_mappings"])
	assertSorted(t, "scim_group_mappings", mappings, func(m map[string]any) []string { return []string{strings.ToLower(text(m, "name"))} })
	for _, mapping := range mappings {
		assertSorted(t, "scim_group_mappings "+text(mapping, "name")+" projects", items(mapping["projects"]), func(p map[string]any) []string {
			return []string{strings.ToLower(text(p, "project"))}
		})
	}
	pricing, _ := document["pricing"].(map[string]any)
	assertSorted(t, "pricing prices", items(pricing["prices"]), func(p map[string]any) []string {
		return []string{text(p, "provider_kind"), strings.ToLower(text(p, "provider")), strings.ToLower(text(p, "vendor_id")), text(p, "model"), text(p, "operation")}
	})
}

func assertExportedBillingImport(t *testing.T, document map[string]any) {
	t.Helper()
	providers := items(document["providers"])
	index := slices.IndexFunc(providers, func(p map[string]any) bool { return p["name"] == "promotion-bedrock" })
	if index < 0 {
		t.Fatalf("export has no promotion-bedrock provider: %v", providers)
	}
	billing, _ := providers[index]["billing_import"].(map[string]any)
	scope, _ := billing["scope"].(map[string]any)
	if scope["payer_account_id"] != "123456789012" {
		t.Errorf("exported promotion-bedrock billing import is %v", providers[index]["billing_import"])
	}
}

func assertSorted(t *testing.T, label string, entries []map[string]any, key func(map[string]any) []string) {
	t.Helper()
	if !slices.IsSortedFunc(entries, func(a, b map[string]any) int { return slices.Compare(key(a), key(b)) }) {
		t.Errorf("%s are not in canonical order: %v", label, entries)
	}
}

func legacyDocument(t *testing.T, exported map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(exported)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err = json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	routes := items(document["routes"])
	canary := routes[slices.IndexFunc(routes, func(r map[string]any) bool { return r["slug"] == "canary-route" })]
	legacy := map[string]any{}
	for field, value := range canary {
		legacy[field] = value
	}
	legacy["slug"] = legacyRouteSlug
	legacy["content_policy"] = map[string]any{"rules": []any{map[string]any{"id": "secret", "phase": "input", "pattern": "sk-[a-z]+", "action": "block"}}}
	document["api_version"] = "openllmproxy.dev/config/v1"
	document["routes"] = append(document["routes"].([]any), legacy)
	for _, field := range []string{"guardrails", "guardrail_policies", "guardrail_attachments"} {
		delete(document, field)
	}
	return document
}

func items(value any) []map[string]any {
	list, _ := value.([]any)
	out := make([]map[string]any, 0, len(list))
	for _, entry := range list {
		out = append(out, entry.(map[string]any))
	}
	return out
}

func text(entry map[string]any, field string) string {
	value, _ := entry[field].(string)
	return value
}

func testPromotionEmptyAttributionPolicyConverges(t *testing.T, s *service, owner *caller) {
	t.Helper()
	policies := map[string]map[string]any{
		uniqueName("empty-required"): {"required_attribution_keys": []any{}},
		uniqueName("empty-defaults"): {"attribution_defaults": map[string]any{}},
		uniqueName("empty-both"):     {"required_attribution_keys": []any{}, "attribution_defaults": map[string]any{}},
	}
	for name := range policies {
		s.want(owner, "POST", "/api/v1/projects", map[string]any{"name": name}, idempotent(), http.StatusCreated)
	}
	exported := s.want(owner, "GET", "/api/v1/configuration/export", nil, nil, http.StatusOK)
	document := exported["document"].(map[string]any)
	for _, project := range items(document["projects"]) {
		if policy, ok := policies[text(project, "name")]; ok {
			project["attribution_policy"] = policy
		}
	}
	request := map[string]any{"document": document}
	s.want(owner, "POST", "/api/v1/configuration/apply", request, idempotent(), http.StatusOK)
	assertSettledPlan(t, s.want(owner, "POST", "/api/v1/configuration/plan", request, nil, http.StatusOK))
	for _, desired := range []any{map[string]any{"attribution_defaults": map[string]any{"team": "fixture"}}, nil} {
		for _, project := range items(document["projects"]) {
			if _, ok := policies[text(project, "name")]; ok {
				project["attribution_policy"] = desired
			}
		}
		plan := s.want(owner, "POST", "/api/v1/configuration/plan", request, nil, http.StatusOK)
		actions := items(plan["actions"])
		for name := range policies {
			index := slices.IndexFunc(actions, func(action map[string]any) bool {
				return action["kind"] == "project" && action["key"] == name
			})
			if index < 0 || actions[index]["action"] != "replace" {
				t.Fatalf("changed attribution policy for %s was suppressed: %v", name, actions)
			}
		}
		s.want(owner, "POST", "/api/v1/configuration/apply", request, idempotent(), http.StatusOK)
		assertSettledPlan(t, s.want(owner, "POST", "/api/v1/configuration/plan", request, nil, http.StatusOK))
	}
}
