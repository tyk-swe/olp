package access

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
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
func TestRoleMatrixAndStrongPreconditions(t *testing.T) {
	for _, role := range []string{"owner", "operator", "developer", "viewer", "unknown"} {
		if Permission(role, "access") != (role == "owner") {
			t.Fatal("membership permission", role)
		}
		if Permission(role, "keys") != (role == "owner" || role == "operator" || role == "developer") {
			t.Fatal("key permission", role)
		}
	}
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
