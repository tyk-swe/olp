package access

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/openapi"
)

func TestIndependentKeyScopesAndRouteRestrictions(t *testing.T) {
	a := Authority{Policy: KeyPolicy{Scopes: []string{"models_read"}, AllowedRoutes: []string{"private"}}}
	now := time.Now()
	if !a.Allows("models_read", "private", nil, now) || a.Allows("inference", "private", nil, now) || a.Allows("models_read", "other", nil, now) || a.Allows("future_scope", "private", nil, now) {
		t.Fatal("scope or allowlist escaped")
	}
	if a.Allows("models_read", "", nil, now) {
		t.Fatal("a missing route bypassed the allowlist")
	}
	a.Policy.AllowedRoutes = nil
	if !a.Allows("models_read", "", nil, now) {
		t.Fatal("an unrestricted models-read key must permit model discovery")
	}
	expired := now.Add(-time.Second)
	a.ExpiresAt = &expired
	if a.Allows("models_read", "private", nil, now) {
		t.Fatal("accepted expired key")
	}
	a.ExpiresAt = nil
	a.RevokedAt = &now
	if a.Allows("models_read", "private", nil, now) {
		t.Fatal("accepted revoked key")
	}
}
func TestStrongPreconditions(t *testing.T) {
	for _, value := range []string{"", "W/\"etag\"", "*", "etag", "\"other\""} {
		r := httptest.NewRequest("PATCH", "/", nil)
		r.Header.Set("If-Match", value)
		if Match(r, "etag") == nil {
			t.Fatalf("accepted precondition %q", value)
		}
	}
}
func TestReturnPathsAndCookieAmbiguity(t *testing.T) {
	for _, value := range []string{
		"https://evil.test", "//evil.test", "//console.invalid", "///evil.test",
		"/\\evil.test", "/%2f%2fevil.test", "/%2Fevil.test", "/%5cevil.test",
		"/\t/evil.test", "/\nmalformed", "/%09/evil.test", "/%C2%85/evil.test",
		"/bad%encoding", "/invalid-%ff", "/audit?filter=%", "/audit?filter=%GG",
		"/audit?filter=%0d%0aLocation%3A%20https%3A%2F%2Fevil.test", "/audit?filter=%5c",
		"/settings#%00", "/settings#%5c",
	} {
		if safeReturn(value) {
			t.Fatalf("unsafe return path %q", value)
		}
	}
	for control := rune(0); control <= 0x9f; control++ {
		if control >= 0x20 && control < 0x7f {
			continue
		}
		if safeReturn("/" + string(control) + "/evil.test") {
			t.Fatalf("accepted control character U+%04X", control)
		}
	}
	for _, value := range []string{
		"", "/", "/settings/profile?tab=sessions#active", "/access/../settings/profile",
		"/audit?occurred_after=2026-09-13T12%3A00%3A00Z",
		"/search?q=hello%20world&next=%2Fmodels", "/search?q=50%25&url=https%3A%2F%2Fexample.test",
		"/files/a%20b", "/caf%C3%A9", "/settings#section%201",
	} {
		if !safeReturn(value) {
			t.Fatalf("rejected local return path %q", value)
		}
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Cookie", sessionCookie+"=first; "+sessionCookie+"=second")
	if checkCookies(r) == nil {
		t.Fatal("accepted ambiguous authentication")
	}
}

func TestStaleWritesUseTheConsoleConflictProblem(t *testing.T) {
	r := httptest.NewRequest("PATCH", "/", nil)
	r.Header.Set("If-Match", `"stale"`)
	w := httptest.NewRecorder()
	WriteProblem(w, Match(r, "current"))
	var body struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != 412 || body.Status != 412 || body.Type != "https://openllmproxy.dev/problems/etag_mismatch" {
		t.Fatal("stale writes must use the shared typed 412 problem")
	}
}
func TestProductionOIDCEgressCannotReachPrivateOrInsecureEndpoints(t *testing.T) {
	if oidcTestBuild {
		t.Skip("explicit loopback test build")
	}
	for _, raw := range []string{"http://127.0.0.1/discovery", "https://user:password@example.com", "https://example.com/#fragment"} {
		if oidcURL(raw) == nil {
			t.Fatal("accepted unsafe issuer URL")
		}
	}
}

func TestKeyRouteProjectIsolation(t *testing.T) {
	a, b := "project-a", "project-b"
	for _, scope := range []string{"inference", "models_read"} {
		for _, allowlist := range [][]string{nil, {"route"}} {
			for _, keyProject := range []*string{nil, &a, &b} {
				for _, routeProject := range []*string{nil, &a, &b} {
					authority := Authority{ProjectID: keyProject, Policy: KeyPolicy{Scopes: []string{scope}, AllowedRoutes: allowlist}}
					want := keyProject == routeProject
					if got := authority.Allows(scope, "route", routeProject, time.Now()); got != want {
						t.Fatalf("scope=%s allowlist=%v key=%v route=%v: got %v, want %v", scope, allowlist, keyProject, routeProject, got, want)
					}
				}
			}
		}
	}
	// UUID equality is by value, not pointer identity.
	copyA := a
	authority := Authority{ProjectID: &a, Policy: KeyPolicy{Scopes: []string{"inference"}}}
	if !authority.Allows("inference", "route", &copyA, time.Now()) {
		t.Fatal("same project rejected")
	}
}

var update = flag.Bool("update", false, "rewrite golden files from the current behavior")

// golden compares got with testdata/name, or rewrites it under -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s changed; review the difference as a change of policy and rerun with -update:\n%s", path, got)
	}
}

// TestAuthorizationMatrix pins who may perform every management operation.
func TestAuthorizationMatrix(t *testing.T) {
	member := func(role string, global bool) Principal {
		return Principal{User: User{Role: role}, Kind: "user", AllProjects: global}
	}
	token := func(creatorRole string, global bool, scopes ...Operation) Principal {
		p := Principal{Kind: "machine", AllProjects: global, creatorRole: creatorRole}
		for _, op := range scopes {
			p.scopes |= 1 << op
		}
		return p
	}
	principals := []struct {
		name string
		p    Principal
	}{}
	for _, role := range []string{RoleOwner, RoleOperator, RoleDeveloper, RoleViewer} {
		principals = append(principals,
			struct {
				name string
				p    Principal
			}{role + " global", member(role, true)},
			struct {
				name string
				p    Principal
			}{role + " assigned", member(role, false)})
	}
	for _, tc := range []struct {
		name string
		p    Principal
	}{
		{"token every scope, owner global", token(RoleOwner, true, Operations()...)},
		{"token every scope, owner assigned", token(RoleOwner, false, Operations()...)},
		{"token every scope, operator global", token(RoleOperator, true, Operations()...)},
		{"token read+configure, owner global", token(RoleOwner, true, Read, Configure)},
		{"unknown role", member("unknown", true)},
	} {
		principals = append(principals, tc)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "%-36s", "principal")
	for _, op := range Operations() {
		fmt.Fprintf(&out, " %s", op)
	}
	out.WriteString("\n")
	for _, principal := range principals {
		fmt.Fprintf(&out, "%-36s", principal.name)
		for _, op := range Operations() {
			mark := "-"
			if principal.p.Authorize(op) == nil {
				mark = "Y"
			}
			fmt.Fprintf(&out, " %*s", len(op.String()), mark)
		}
		out.WriteString("\n")
	}
	golden(t, "operations.golden", []byte(out.String()))
}

func TestOperationNamesRoundTrip(t *testing.T) {
	for _, op := range Operations() {
		if parsed, ok := ParseOperation(op.String()); !ok || parsed != op {
			t.Fatalf("%s does not round-trip", op)
		}
	}
	if _, ok := ParseOperation("unknown"); ok {
		t.Fatal("parsed an unknown operation")
	}
	if (Principal{Kind: "user", User: User{Role: RoleOwner}, AllProjects: true}).Authorize(0) == nil {
		t.Fatal("an unknown operation must never be authorized")
	}
}

func TestTokenScopesMatchTheContract(t *testing.T) {
	var document struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Items struct {
						Enum []string `json:"enum"`
					} `json:"items"`
				} `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(openapi.Document, &document); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, op := range TokenScopes() {
		names = append(names, op.String())
	}
	enum := document.Components.Schemas["CreateManagementTokenRequest"].Properties["scopes"].Items.Enum
	if !slices.Equal(slices.Sorted(slices.Values(enum)), slices.Sorted(slices.Values(names))) {
		t.Fatalf("the contract offers token scopes %v, the policy delegates %v", enum, names)
	}
}

// Authorization decisions live in the policy: comparing a principal's role or
// kind elsewhere bypasses it, as the owner-only session checks once did.
func TestPrincipalRolesAreDecidedOnlyByThePolicy(t *testing.T) {
	comparison := regexp.MustCompile(`\b(p|principal)\.(Role\s*[!=]=\s*"|Kind\s*[!=]=\s*"(user|machine)")`)
	sources, err := filepath.Glob("../*/*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range sources {
		base := filepath.Base(name)
		if strings.HasSuffix(name, "_test.go") || filepath.Base(filepath.Dir(name)) == "access" && (base == "policy.go" || base == "principal.go") {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if match := comparison.Find(source); match != nil {
			t.Errorf("%s compares %s; authorize an operation instead", name, match)
		}
	}
}
