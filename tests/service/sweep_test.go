//go:build service

package service_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
)

// readinessRoute reports 503 by design until a process has a fresh snapshot.
const readinessRoute = "GET /api/v1/health/ready"

var routeParameter = regexp.MustCompile(`\{[^}]+\}`)

func sweepPath(pattern string, known map[string]string) string {
	_, path, _ := strings.Cut(pattern, " ")
	resolved := routeParameter.ReplaceAllStringFunc(path, func(parameter string) string {
		name := strings.Trim(parameter, "{}")
		if value, ok := known[name]; ok {
			return value
		}
		switch name {
		case "scope":
			return "route-draft"
		case "key":
			return "sweep.unknown"
		case "source", "external_id":
			return "sweep"
		case "provider_kind":
			return "openai_compatible"
		case "plugin_digest":
			return strings.Repeat("0", 64)
		}
		return uuid.NewString()
	})
	now := time.Now().UTC()
	switch {
	case path == "/api/v1/usage/reconciliation", strings.HasSuffix(path, "/invoiced-costs"):
		return resolved + "?" + url.Values{
			"start": {now.Add(-24 * time.Hour).Format(time.DateOnly)},
			"end":   {now.Format(time.DateOnly)},
		}.Encode()
	case strings.HasPrefix(path, "/api/v1/usage/"):
		return resolved + "?" + url.Values{
			"start":     {now.Add(-time.Hour).Format(time.RFC3339)},
			"end":       {now.Format(time.RFC3339)},
			"dimension": {"route"},
		}.Encode()
	case strings.HasSuffix(path, "/revisions/diff"):
		return resolved + "?" + url.Values{"from": {uuid.NewString()}, "to": {uuid.NewString()}}.Encode()
	case path == "/api/v1/media-jobs/{job_id}/content":
		return resolved + "?variant=video"
	case path == "/api/v1/code/routes/{id}/client-config":
		return resolved + "?" + url.Values{"gateway_url": {"https://gateway.example.com"}}.Encode()
	case path == "/api/v1/delegation-options":
		return resolved + "?" + url.Values{"api_key_id": {known["api_key_id"]}}.Encode()
	default:
		return resolved
	}
}

type canaryFixture struct {
	ids     map[string]string
	project string
}

// seedCanary creates a project whose resource names contain "canary", so any
// response mentioning that word has crossed a project boundary.
func (s *service) seedCanary(owner *caller, vendorURL string) canaryFixture {
	s.t.Helper()
	organization := s.want(owner, "POST", "/api/v1/organizations", map[string]any{"name": "canary-organization"}, idempotent(), http.StatusCreated)["id"].(string)
	project := s.want(owner, "POST", "/api/v1/organizations/"+organization+"/projects", map[string]any{"name": "canary-project"}, idempotent(), http.StatusCreated)["id"].(string)
	provider := s.activeProvider(owner, "canary-provider", vendorURL, project)
	draft, activated := s.publishedRoute(owner, "canary-route", provider, project)
	key := s.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "canary-key", "scopes": []string{"inference"}, "project_id": project}, idempotent(), http.StatusCreated)
	group := s.want(owner, "POST", "/api/v1/budget-groups", map[string]any{"name": "canary-budget", "monthly_cost_limit": "5.00", "project_id": project}, idempotent(), http.StatusCreated)
	increase := s.want(owner, "POST", "/api/v1/budget-increases", map[string]any{"target": map[string]any{"kind": "budget_group", "id": group["id"]}, "window": "month", "amount": "1", "reason": "canary increase"}, idempotent(), http.StatusCreated)
	policy := s.want(owner, "POST", "/api/v1/guardrail-policies", map[string]any{"name": "canary-policy", "project_id": project, "entries": []any{}}, idempotent(), http.StatusCreated)
	s.want(owner, "POST", "/api/v1/guardrail-policies/"+policy["policy_id"].(string)+"/activate", map[string]any{"revision_id": policy["revision_id"]}, idempotent(etag(policy)), http.StatusNoContent)
	attachment := s.want(owner, "POST", "/api/v1/guardrail-attachments", map[string]any{"scope": "installation", "scope_id": "installation", "policy_revision_id": policy["revision_id"], "order": 0}, idempotent(), http.StatusCreated)
	s.eventually("the canary runtime snapshot being ready", startupLimit, func() (bool, string) {
		status, raw := s.do(owner, "GET", "/api/v1/health/ready", nil, nil)
		return status == http.StatusOK, describe(status, raw)
	})
	return canaryFixture{
		project: project,
		ids: map[string]string{
			"organization_id": organization,
			"project_id":      project,
			"provider_id":     provider["id"].(string),
			"draft_id":        draft["id"].(string),
			"route_id":        activated["route_id"].(string),
			"api_key_id":      key["id"].(string),
			"budget_group_id": group["id"].(string),
			"increase_id":     increase["id"].(string),
			"policy_id":       policy["policy_id"].(string),
			"revision_id":     policy["revision_id"].(string),
			"attachment_id":   attachment["id"].(string),
		},
	}
}

func (f canaryFixture) named(path string) bool {
	for _, id := range f.ids {
		if strings.Contains(path, id) {
			return true
		}
	}
	return false
}

func secureReads(t *testing.T) []string {
	t.Helper()
	requirements, err := access.ContractRequirements()
	if err != nil {
		t.Fatal(err)
	}
	patterns := make([]string, 0, len(requirements))
	for pattern, requirement := range requirements {
		if !requirement.Public && strings.HasPrefix(pattern, "GET ") {
			patterns = append(patterns, pattern)
		}
	}
	sort.Strings(patterns)
	return patterns
}

// TestManagementReadsAnswerWithoutServerErrors reads every secured management
// route against seeded data. A query that no longer matches the schema answers
// 5xx here, because nothing else runs this SQL before merge.
func TestManagementReadsAnswerWithoutServerErrors(t *testing.T) {
	s := startService(t)
	owner := s.owner()
	fixture := s.seedCanary(owner, newVendor(t).URL)
	for _, pattern := range secureReads(t) {
		status, raw := s.do(owner, "GET", sweepPath(pattern, fixture.ids), nil, nil)
		if pattern == readinessRoute && status == http.StatusServiceUnavailable {
			continue
		}
		if status == http.StatusBadRequest || status == http.StatusUnprocessableEntity || status >= http.StatusInternalServerError {
			t.Errorf("%s: %s", pattern, describe(status, raw))
		}
	}
}

// TestManagementIsolationSweep reads every secured management route as an
// assigned member and a project-scoped token of another project. No response
// may mention the canary, and naming a canary resource must answer as if it
// did not exist.
func TestManagementIsolationSweep(t *testing.T) {
	s := startService(t)
	owner := s.owner()
	fixture := s.seedCanary(owner, newVendor(t).URL)
	project := s.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "isolated"}, idempotent(), http.StatusCreated)

	member := s.invite(owner, "isolated-operator@example.com", access.RoleOperator)
	profile := s.want(member, "GET", "/api/v1/profile", nil, nil, http.StatusOK)
	userID := profile["id"].(string)
	s.want(owner, "PATCH", "/api/v1/users/"+userID, map[string]any{"access_scope": "assigned"}, etag(profile), http.StatusOK)
	s.want(owner, "PUT", "/api/v1/projects/"+project["id"].(string)+"/members/"+userID, map[string]any{"role": "manager"}, etag(project), http.StatusOK)

	scopes := make([]string, 0, len(access.TokenScopes()))
	for _, operation := range access.TokenScopes() {
		scopes = append(scopes, operation.String())
	}
	token := s.want(owner, "POST", "/api/v1/management-tokens", map[string]any{
		"name": "isolated", "scopes": scopes, "project_ids": []string{project["id"].(string)},
		"expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	}, idempotent(), http.StatusCreated)

	callers := []*caller{
		s.login("isolated-operator@example.com"),
		{name: "project token", token: token["secret"].(string)},
	}
	for _, pattern := range secureReads(t) {
		path := sweepPath(pattern, fixture.ids)
		for _, c := range callers {
			status, raw := s.do(c, "GET", path, nil, nil)
			if status == http.StatusBadRequest || status == http.StatusUnprocessableEntity {
				t.Errorf("%s as %s: %s", pattern, c.name, describe(status, raw))
			}
			if strings.Contains(string(raw), "canary") || strings.Contains(string(raw), fixture.ids["attachment_id"]) {
				t.Errorf("%s as %s disclosed another project (%d)", pattern, c.name, status)
			}
			if fixture.named(path) && status/100 == 2 {
				t.Errorf("%s as %s: %s for another project's resource", pattern, c.name, describe(status, raw))
			}
		}
	}

	t.Run("project_policy_promotion_preserves_local_state_and_rolls_back", func(t *testing.T) {
		other := s.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "batch-policy"}, idempotent(), http.StatusCreated)
		projectID, otherID := project["id"].(string), other["id"].(string)
		path := "/api/v1/projects/" + projectID
		templates := s.want(owner, "GET", path+"/limit-templates", nil, nil, http.StatusOK)
		s.want(owner, "PUT", path+"/limit-templates", map[string]any{
			"templates": map[string]any{"retained": map[string]any{"requests_per_minute": 5}},
		}, etag(templates), http.StatusOK)
		s.want(owner, "POST", "/api/v1/api-keys", map[string]any{
			"name": "template-member", "scopes": []string{"inference"}, "project_id": projectID, "limit_template": "retained",
		}, idempotent(), http.StatusCreated)
		for index, id := range []string{projectID, otherID} {
			policyPath := "/api/v1/projects/" + id + "/end-user-policy"
			current := s.want(owner, "GET", policyPath, nil, nil, http.StatusOK)
			s.want(owner, "PUT", policyPath, map[string]any{"policy": map[string]any{
				"defaults":  map[string]any{"requests_per_minute": 11 - index},
				"overrides": map[string]any{strings.Repeat("a", 64): map[string]any{"requests_per_minute": 3}},
				"blocked":   []string{strings.Repeat("b", 64)},
			}}, etag(current), http.StatusOK)
		}
		live := s.want(owner, "GET", path+"/end-user-policy", nil, nil, http.StatusOK)
		document := map[string]any{"api_version": "openllmproxy.dev/config/v2", "projects": []map[string]any{
			{"name": project["name"], "end_user_defaults": map[string]any{"requests_per_minute": 11}},
			{"name": other["name"], "end_user_defaults": map[string]any{"requests_per_minute": 11}},
		}}
		s.want(owner, "POST", "/api/v1/configuration/apply", map[string]any{"document": document}, idempotent(), http.StatusOK)
		liveAfter := s.want(owner, "GET", path+"/end-user-policy", nil, nil, http.StatusOK)
		promoted := s.want(owner, "GET", "/api/v1/projects/"+otherID+"/end-user-policy", nil, nil, http.StatusOK)
		livePolicy, err := json.Marshal(live["policy"])
		if err != nil {
			t.Fatal(err)
		}
		promotedPolicy, err := json.Marshal(promoted["policy"])
		if err != nil {
			t.Fatal(err)
		}
		if string(livePolicy) != string(promotedPolicy) {
			t.Fatalf("promoted policy = %s, live policy = %s", promotedPolicy, livePolicy)
		}
		invalid := map[string]any{"api_version": "openllmproxy.dev/config/v2", "projects": []map[string]any{
			{"name": other["name"], "end_user_defaults": map[string]any{"requests_per_minute": 17}},
			{"name": project["name"], "end_user_defaults": map[string]any{"requests_per_minute": 17}, "limit_templates": map[string]any{}},
		}}
		problem := s.want(owner, "POST", "/api/v1/configuration/apply", map[string]any{"document": invalid}, idempotent(), http.StatusConflict)
		if problem["type"] != "https://openllmproxy.dev/problems/template_in_use" {
			t.Fatalf("promotion did not reach the referenced-template check: %v", problem)
		}
		for index, id := range []string{projectID, otherID} {
			current := s.want(owner, "GET", "/api/v1/projects/"+id+"/end-user-policy", nil, nil, http.StatusOK)
			currentPolicy, err := json.Marshal(current["policy"])
			if err != nil {
				t.Fatal(err)
			}
			before := []map[string]any{liveAfter, promoted}[index]
			if string(currentPolicy) != string(livePolicy) || current["etag"] != before["etag"] {
				t.Fatalf("failed promotion changed project %s policy or version: %v", id, current)
			}
		}
	})

	t.Run("queued_mutation_rechecks_project_membership", func(t *testing.T) {
		projectID := project["id"].(string)
		path := "/api/v1/projects/" + projectID + "/end-user-policy"
		current := s.want(callers[0], "GET", path, nil, nil, http.StatusOK)
		gate, err := s.pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer gate.Rollback(t.Context())
		var gatePID int32
		if err := gate.QueryRow(t.Context(), "SELECT pg_backend_pid() FROM olp.installation WHERE singleton FOR UPDATE").Scan(&gatePID); err != nil {
			t.Fatal(err)
		}
		type response struct {
			status int
			raw    []byte
		}
		result := make(chan response, 1)
		go func() {
			status, raw := s.do(callers[0], "PUT", path, map[string]any{"policy": nil}, etag(current))
			result <- response{status, raw}
		}()
		s.eventually("the policy mutation waiting for the concurrent membership change", 5*time.Second, func() (bool, string) {
			var blocked bool
			err := s.pool.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))", gatePID).Scan(&blocked)
			if err != nil {
				t.Fatal(err)
			}
			return blocked, "the mutation has not reached the membership transaction"
		})
		if _, err := gate.Exec(t.Context(), "DELETE FROM olp.project_members WHERE project_id=$1 AND user_id=$2", projectID, userID); err != nil {
			t.Fatal(err)
		}
		if err := gate.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		select {
		case reply := <-result:
			if reply.status != http.StatusNotFound {
				t.Fatalf("queued mutation retained revoked project authority: %s", describe(reply.status, reply.raw))
			}
		case <-time.After(5 * time.Second):
			t.Fatal("queued mutation did not finish after the membership change")
		}
		after := s.want(owner, "GET", path, nil, nil, http.StatusOK)
		beforeJSON, err := json.Marshal(current)
		if err != nil {
			t.Fatal(err)
		}
		afterJSON, err := json.Marshal(after)
		if err != nil {
			t.Fatal(err)
		}
		if string(beforeJSON) != string(afterJSON) {
			t.Fatalf("denied mutation changed the project policy: before %s, after %s", beforeJSON, afterJSON)
		}
	})
	t.Run("guardrail_journey", func(t *testing.T) {
		testGuardrailJourney(t, s, owner)
	})
	t.Run("promotion_round_trip", func(t *testing.T) {
		testPromotionRoundTrip(t, s, owner, fixture)
	})
}
