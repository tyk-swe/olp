package access

import (
	"testing"
)

func TestContractRequirementsMustReadTruthfully(t *testing.T) {
	operation := func(path, method, security string) string {
		return `{"paths":{"` + path + `":{"` + method + `":{"security":` + security + `}}}}`
	}
	for name, document := range map[string]string{
		"missing security":         `{"paths":{"/api/v1/x":{"get":{}}}}`,
		"unknown scheme":           operation("/api/v1/x", "get", `[{"bearerToken":[]}]`),
		"unknown operation":        operation("/api/v1/x", "get", `[{"sessionCookie":["root"]}]`),
		"safe method with proof":   operation("/api/v1/x", "get", `[{"sessionCookie":["read"],"csrfToken":[]}]`),
		"unsafe method no proof":   operation("/api/v1/x", "post", `[{"sessionCookie":["configure"]}]`),
		"undelegable token scope":  operation("/api/v1/x", "get", `[{"managementToken":["self"]}]`),
		"hidden installation":      operation("/api/v1/x", "get", `[{"sessionCookie":["settings"]}]`),
		"no operation":             operation("/api/v1/x", "get", `[{"sessionCookie":["installation"]}]`),
		"combined token":           operation("/api/v1/x", "get", `[{"managementToken":["read"],"sessionCookie":["read"]}]`),
		"consumer management read": operation("/api/v1/users", "get", `[{"apiKeyBearer":["models_read"]}]`),
		"consumer write":           operation("/api/v1/catalog", "put", `[{"apiKeyBearer":["models_read"]}]`),
		"consumer broader scope":   operation("/api/v1/catalog", "get", `[{"apiKeyBearer":["inference"]}]`),
	} {
		if _, err := parseRequirements([]byte(document)); err == nil {
			t.Errorf("%s: an untruthful contract loaded", name)
		}
	}
	valid := operation("/api/v1/x", "post", `[{"sessionCookie":["settings","installation"],"csrfToken":[]},{"managementToken":["settings","installation"]}]`)
	requirements, err := parseRequirements([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	req := requirements["POST /api/v1/x"]
	global := Principal{Kind: "user", User: User{Role: RoleOperator}, AllProjects: true}
	assigned := Principal{Kind: "user", User: User{Role: RoleOperator}}
	if req.Admits(global) != nil || req.Admits(assigned) == nil {
		t.Fatal("an installation requirement admitted the wrong members")
	}
	if public, err := parseRequirements([]byte(operation("/api/v1/x", "post", `[]`))); err != nil || !public["POST /api/v1/x"].Public {
		t.Fatal("an empty requirement must be public", err)
	}
}

// archetype is a caller the route-level golden evaluates, described so the
// console's own authorization can evaluate the same callers.
type archetype struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Role        string   `json:"role,omitempty"`
	CreatorRole string   `json:"creator_role,omitempty"`
	Global      bool     `json:"global"`
	Scopes      []string `json:"scopes,omitempty"`
	// Operations is what the principal holds, as a session reports it.
	Operations []string `json:"operations"`
}

func (a archetype) principal() Principal {
	p := Principal{Kind: a.Kind, User: User{Role: a.Role}, AllProjects: a.Global, creatorRole: a.CreatorRole}
	for _, name := range a.Scopes {
		op, _ := ParseOperation(name)
		p.scopes |= 1 << op
	}
	return p
}

func archetypes() []archetype {
	var all []archetype
	for _, role := range []string{RoleOwner, RoleOperator, RoleDeveloper, RoleViewer} {
		all = append(all,
			archetype{Name: role + " global", Kind: "user", Role: role, Global: true},
			archetype{Name: role + " assigned", Kind: "user", Role: role})
	}
	var every []string
	for _, op := range TokenScopes() {
		every = append(every, op.String())
	}
	return append(all,
		archetype{Name: "token every scope, owner global", Kind: "machine", CreatorRole: RoleOwner, Global: true, Scopes: every},
		archetype{Name: "token every scope, owner assigned", Kind: "machine", CreatorRole: RoleOwner, Scopes: every},
		archetype{Name: "token every scope, operator global", Kind: "machine", CreatorRole: RoleOperator, Global: true, Scopes: every},
		archetype{Name: "token read+configure, owner global", Kind: "machine", CreatorRole: RoleOwner, Global: true, Scopes: []string{"read", "configure"}},
	)
}
