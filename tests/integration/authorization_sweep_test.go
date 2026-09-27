//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
)

// routeGolden is internal/access/testdata/authorization.golden.json: the
// callers every management route admits, derived from the contract and the
// policy.
type routeGolden struct {
	Archetypes []struct {
		Name        string   `json:"name"`
		Kind        string   `json:"kind"`
		Role        string   `json:"role"`
		CreatorRole string   `json:"creator_role"`
		Global      bool     `json:"global"`
		Scopes      []string `json:"scopes"`
	} `json:"archetypes"`
	Routes map[string]string `json:"routes"`
}

// sweepCaller is a live principal matching one golden archetype.
type sweepCaller struct {
	name    string
	browser *browser
	token   string
}

// call performs one management request as c and returns its status, problem
// code, and body.
func (h *accessHarness) call(c sweepCaller, method, path string, body any, headers map[string]string) (int, string, string) {
	h.t.Helper()
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	if c.token == "" {
		response, raw := h.do(c.browser, method, path, body, headers)
		return response.StatusCode, codeOf(raw), string(raw)
	}
	r, err := http.NewRequest(method, h.HTTP.URL+path, bytes.NewReader(data))
	if err != nil {
		h.t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+c.token)
	r.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(r)
	if err != nil {
		h.t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	return response.StatusCode, codeOf(raw), string(raw)
}

func codeOf(raw []byte) string {
	var problem struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &problem) != nil || problem.Type == "" {
		return ""
	}
	return problem.Type[strings.LastIndex(problem.Type, "/")+1:]
}

// sweepPath fills a route pattern's parameters, preferring the identifiers in
// known and otherwise inventing ones that exist nowhere.
func sweepPath(pattern string, known map[string]string) (string, string) {
	method, path, _ := strings.Cut(pattern, " ")
	path = regexp.MustCompile(`\{[^}]+\}`).ReplaceAllStringFunc(path, func(parameter string) string {
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
		}
		return uuid.NewString()
	})
	return method, path
}

// sweepCallers creates one live caller per golden archetype: members with the
// archetype's role and access scope, assigned members in project A, and
// management tokens whose creators have the archetype's current authority.
func sweepCallers(h *accessHarness, owner *browser, golden routeGolden, projectA string) map[string]sweepCaller {
	h.t.Helper()
	callers := map[string]sweepCaller{"owner global": {name: "owner global", browser: owner}}
	userID := func(b *browser) (string, map[string]any) {
		profile := h.want(b, "GET", "/api/v1/profile", nil, nil, 200)
		return profile["id"].(string), profile
	}
	for i, archetype := range golden.Archetypes {
		switch {
		case archetype.Name == "owner global":
		case archetype.Kind == "user":
			email := strings.ReplaceAll(archetype.Name, " ", "-") + "@example.com"
			member := h.invite(owner, email, archetype.Role)
			if !archetype.Global {
				id, profile := userID(member)
				h.want(owner, "PATCH", "/api/v1/users/"+id, map[string]any{"access_scope": "assigned"}, etagHeader(profile), 200)
				addMember(h, owner, projectA, id, "manager")
				member = login(h, email)
			}
			callers[archetype.Name] = sweepCaller{name: archetype.Name, browser: member}
		default:
			// A token takes its creator's current authority, so it is issued
			// by an owner who then takes the archetype's role and scope.
			creator := owner
			if archetype.CreatorRole != access.RoleOwner || !archetype.Global {
				email := "token-creator-" + string(rune('a'+i)) + "@example.com"
				creator = h.invite(owner, email, access.RoleOwner)
			}
			_, secret := createToken(h, creator, strings.ReplaceAll(archetype.Name, " ", "-"), archetype.Scopes)
			if creator != owner {
				id, profile := userID(creator)
				change := map[string]any{"role": archetype.CreatorRole}
				if !archetype.Global {
					change["access_scope"] = "assigned"
				}
				h.want(owner, "PATCH", "/api/v1/users/"+id, change, etagHeader(profile), 200)
			}
			callers[archetype.Name] = sweepCaller{name: archetype.Name, token: secret}
		}
	}
	return callers
}

// A handler may demand more than its route's entry requirement. A routing
// policy write needs the operation its scope implies, and the sweep writes a
// route draft's policy, which a developer's keys operation does not cover.
var sweepRefinements = map[string]bool{
	"PUT /api/v1/routing-policies/{scope}/{id} as developer global":   true,
	"PUT /api/v1/routing-policies/{scope}/{id} as developer assigned": true,
}

// sweepBody is the request body for pattern: resources are placed in the
// swept project, because the unassigned boundary is installation-wide and
// would refine assigned callers the route itself admits.
func sweepBody(pattern, projectID string) any {
	switch pattern {
	case "POST /api/v1/route-drafts", "POST /api/v1/notifications/destinations", "POST /api/v1/notifications/rules":
		return map[string]any{"project_id": projectID}
	}
	if strings.HasPrefix(pattern, "GET ") {
		return nil
	}
	return map[string]any{}
}

// TestManagementAuthorizationSweep calls every secured management operation
// as every golden archetype and holds each outcome to the route
// authorization golden file.
func TestManagementAuthorizationSweep(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	raw, err := os.ReadFile("../../internal/access/testdata/authorization.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden routeGolden
	if err = json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	requirements, err := access.ContractRequirements()
	if err != nil {
		t.Fatal(err)
	}
	projectA := createProject(h, owner, "Sweep")
	callers := sweepCallers(h, owner, golden, projectA)
	patterns := slices.Sorted(func(yield func(string) bool) {
		for pattern := range golden.Routes {
			if !yield(pattern) {
				return
			}
		}
	})
	// Signing out ends the caller's session, so it runs after every other
	// operation.
	signOut := "DELETE /api/v1/sessions/current"
	patterns = append(slices.DeleteFunc(patterns, func(p string) bool { return p == signOut }), signOut)
	headers := func() map[string]string {
		return map[string]string{"Idempotency-Key": uuid.NewString(), "If-Match": `"` + uuid.NewString() + `"`}
	}
	for _, pattern := range patterns {
		if requirements[pattern].Public {
			continue
		}
		method, path := sweepPath(pattern, nil)
		body := sweepBody(pattern, projectA)
		if status, code, _ := h.call(sweepCaller{browser: &browser{}}, method, path, body, headers()); status != 401 {
			t.Errorf("%s without credentials: %d %s, want 401", pattern, status, code)
		}
		for i, archetype := range golden.Archetypes {
			admitted := golden.Routes[pattern][i] == 'Y'
			status, code, _ := h.call(callers[archetype.Name], method, path, body, headers())
			refused := status == 401 || status == 403 && code == "permission_denied"
			switch {
			case status == 403 && code != "permission_denied":
				t.Errorf("%s as %s: unexpected refusal %s", pattern, archetype.Name, code)
			case !admitted && !(status == 403 && code == "permission_denied"):
				t.Errorf("%s as %s: %d %s, want the route to refuse", pattern, archetype.Name, status, code)
			case admitted && refused && !sweepRefinements[pattern+" as "+archetype.Name]:
				t.Errorf("%s as %s: %d %s, want the route to admit", pattern, archetype.Name, status, code)
			}
		}
	}
}

// TestManagementIsolationSweep seeds a canary project and reads every secured
// management operation as an assigned member and a project-scoped token of
// another project: no response may mention the canary, and naming a canary
// resource must answer as if it did not exist.
func TestManagementIsolationSweep(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	up := newVendor(t)
	projectA := createProject(h, owner, "Isolated")
	projectB := createProject(h, owner, "canary-project")
	providerB := createScopedProvider(h, owner, "canary-provider", up.URL+"/v1", projectB, 201)
	activateScopedProvider(h, owner, providerB)
	draftB := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": "canary-route", "operations": []string{"generation"}, "overall_timeout_ms": 5000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"},
		"targets":    []any{map[string]any{"provider_id": providerB["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 2000}},
		"project_id": projectB,
	}, idem("draft-canary"), 201)
	activated := h.want(owner, "POST", "/api/v1/route-drafts/"+draftB["id"].(string)+"/activate", nil, withMatch(draftB, idem("activate-canary")), 200)
	keyB := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "canary-key", "scopes": []string{"inference"}, "project_id": projectB}, idem("key-canary"), 201)
	groupB := h.want(owner, "POST", "/api/v1/budget-groups", map[string]any{"name": "canary-budget", "monthly_cost_limit": "5.00", "project_id": projectB}, idem("group-canary"), 201)
	canary := map[string]string{
		"project_id": projectB, "provider_id": providerB["id"].(string), "draft_id": draftB["id"].(string),
		"route_id": activated["route_id"].(string), "api_key_id": keyB["id"].(string), "budget_group_id": groupB["id"].(string),
	}

	member := h.invite(owner, "isolated-operator@example.com", access.RoleOperator)
	profile := h.want(member, "GET", "/api/v1/profile", nil, nil, 200)
	h.want(owner, "PATCH", "/api/v1/users/"+profile["id"].(string), map[string]any{"access_scope": "assigned"}, etagHeader(profile), 200)
	addMember(h, owner, projectA, profile["id"].(string), "manager")
	var scopes []string
	for _, op := range access.TokenScopes() {
		scopes = append(scopes, op.String())
	}
	token := h.want(owner, "POST", "/api/v1/management-tokens", map[string]any{
		"name": "isolated", "scopes": scopes, "project_ids": []string{projectA},
		"expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	}, idem("token-isolated"), 201)
	callers := []sweepCaller{
		{name: "assigned operator", browser: login(h, "isolated-operator@example.com")},
		{name: "project token", token: token["secret"].(string)},
	}

	requirements, err := access.ContractRequirements()
	if err != nil {
		t.Fatal(err)
	}
	for pattern, req := range requirements {
		if req.Public || !strings.HasPrefix(pattern, "GET ") {
			continue
		}
		_, path := sweepPath(pattern, canary)
		namesCanary := false
		for _, id := range canary {
			namesCanary = namesCanary || strings.Contains(path, id)
		}
		for _, caller := range callers {
			status, code, body := h.call(caller, "GET", path, nil, nil)
			if strings.Contains(body, "canary") {
				t.Errorf("%s as %s disclosed another project (%d)", pattern, caller.name, status)
			}
			if namesCanary && status/100 == 2 {
				t.Errorf("%s as %s: %d %s for another project's resource, want 404", pattern, caller.name, status, code)
			}
		}
	}
}
