//go:build service

package service_test

import (
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
			if strings.Contains(string(raw), "canary") {
				t.Errorf("%s as %s disclosed another project (%d)", pattern, c.name, status)
			}
			if fixture.named(path) && status/100 == 2 {
				t.Errorf("%s as %s: %s for another project's resource", pattern, c.name, describe(status, raw))
			}
		}
	}
}
