//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/management"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/usage"
)

func (h *accessHarness) machineOn(base, secret, method, path string, body any, headers map[string]string) (int, map[string]any) {
	h.t.Helper()
	var data []byte
	if body != nil {
		var err error
		if data, err = json.Marshal(body); err != nil {
			h.t.Fatal(err)
		}
	}
	r, err := http.NewRequest(method, base+path, bytes.NewReader(data))
	if err != nil {
		h.t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+secret)
	r.Header.Set("User-Agent", "automation-test")
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(r)
	if err != nil {
		h.t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			h.t.Fatal("invalid JSON response", err)
		}
		validateManagementResponse(h.t, method, r.URL.Path, response.StatusCode, out)
	}
	return response.StatusCode, out
}

func (h *accessHarness) machine(secret, method, path string, body any, headers map[string]string) (int, map[string]any) {
	h.t.Helper()
	return h.machineOn(h.HTTP.URL, secret, method, path, body, headers)
}

func (h *accessHarness) machineWant(secret, method, path string, body any, headers map[string]string, want int) map[string]any {
	h.t.Helper()
	status, out := h.machine(secret, method, path, body, headers)
	if status != want {
		h.t.Fatalf("%s %s: status %d, want %d; problem: %v", method, path, status, want, out["detail"])
	}
	return out
}

func createToken(h *accessHarness, owner *browser, name string, scopes []string) (map[string]any, string) {
	h.t.Helper()
	created := h.want(owner, "POST", "/api/v1/management-tokens", map[string]any{
		"name":       name,
		"scopes":     scopes,
		"expires_at": time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339),
	}, map[string]string{"Idempotency-Key": "token-" + name}, 201)
	secret, _ := created["secret"].(string)
	if !strings.HasPrefix(secret, "olpm_") || len(strings.Split(secret, "_")) != 3 {
		h.t.Fatal("management token secret has an unexpected shape")
	}
	return created, secret
}

func TestManagementTokenLifecycleAndScopes(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()

	h.want(owner, "POST", "/api/v1/management-tokens", map[string]any{
		"name": "missing idempotency", "scopes": []string{"read"},
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}, nil, 400)
	h.want(owner, "POST", "/api/v1/management-tokens", map[string]any{
		"name": "bad scope", "scopes": []string{"inference"},
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}, map[string]string{"Idempotency-Key": "bad-scope"}, 422)
	h.want(owner, "POST", "/api/v1/management-tokens", map[string]any{
		"name": "distant", "scopes": []string{"read"},
		"expires_at": time.Now().Add(400 * 24 * time.Hour).UTC().Format(time.RFC3339),
	}, map[string]string{"Idempotency-Key": "distant"}, 422)

	usageMux := http.NewServeMux()
	management.Register(usageMux)
	h.Server.Register(usageMux)
	(&usage.Server{Access: h.Server, VendorKind: providers.VendorKind}).Register(usageMux)
	usageHTTP := httptest.NewServer(usageMux)
	t.Cleanup(usageHTTP.Close)

	created, secret := createToken(h, owner, "deploy", []string{"read", "configure"})
	if _, leaked := created["digest"]; leaked {
		t.Fatal("token creation disclosed the stored digest")
	}
	tokenID := created["id"].(string)

	h.machineWant(secret, "GET", "/api/v1/routes", nil, nil, 200)
	provider := h.machineWant(secret, "POST", "/api/v1/providers", map[string]any{
		"name":          "Automated vendor",
		"configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": "http://127.0.0.1:9/v1/"},
		"credential":    "vendor-secret",
	}, map[string]string{"Idempotency-Key": "machine-provider"}, 201)
	providerID := provider["id"].(string)
	h.machineWant(secret, "GET", "/api/v1/providers/"+providerID, nil, nil, 200)
	h.machineWant(secret, "GET", "/api/v1/providers/"+providerID+"/models", nil, nil, 200)

	users := h.want(owner, "GET", "/api/v1/users", nil, nil, 200)
	ownerRecord := users["items"].([]any)[0].(map[string]any)
	for _, tc := range []struct {
		base         string
		method, path string
		body         any
		headers      map[string]string
	}{
		{h.HTTP.URL, "POST", "/api/v1/api-keys", map[string]any{"name": "denied"}, map[string]string{"Idempotency-Key": "denied-key"}},
		{h.HTTP.URL, "PATCH", "/api/v1/users/" + ownerRecord["id"].(string), map[string]any{"role": "viewer"}, map[string]string{"If-Match": `"` + ownerRecord["etag"].(string) + `"`}},
		{h.HTTP.URL, "PUT", "/api/v1/settings/retention.usage_days", map[string]any{"value": "30"}, map[string]string{"If-Match": `"00000000-0000-0000-0000-000000000000"`}},
		{usageHTTP.URL, "POST", "/api/v1/pricing/revisions", map[string]any{"effective_at": time.Now().UTC().Format(time.RFC3339)}, map[string]string{"Idempotency-Key": "denied-pricing"}},
		{usageHTTP.URL, "GET", "/api/v1/usage/summary?start=2024-01-01T00:00:00Z&end=2024-01-02T00:00:00Z", nil, nil},
		{h.HTTP.URL, "GET", "/api/v1/management-tokens", nil, nil},
		{h.HTTP.URL, "GET", "/api/v1/profile", nil, nil},
		{h.HTTP.URL, "GET", "/api/v1/sessions/current", nil, nil},
	} {
		if status, _ := h.machineOn(tc.base, secret, tc.method, tc.path, tc.body, tc.headers); status != 403 {
			t.Fatalf("%s %s: a read+configure token must be denied (403), got %d", tc.method, tc.path, status)
		}
	}

	_, allSecret := createToken(h, owner, "everything", []string{"read", "access_read", "access", "settings", "configure", "keys", "playground", "usage"})
	if status, _ := h.machine(allSecret, "GET", "/api/v1/management-tokens", nil, nil); status != 403 {
		t.Fatal("token administration must require an owner session, got", status)
	}
	if status, _ := h.machine(allSecret, "POST", "/api/v1/management-tokens", map[string]any{"name": "nested", "scopes": []string{"read"}, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, map[string]string{"Idempotency-Key": "nested"}); status != 403 {
		t.Fatal("a machine principal must not create management tokens, got", status)
	}
	status, _ := h.machineOn(usageHTTP.URL, allSecret, "GET", "/api/v1/usage/summary?start=2024-01-01T00:00:00Z&end=2024-01-02T00:00:00Z", nil, nil)
	if status != 200 {
		t.Fatal("a usage-scoped token must read usage, got", status)
	}

	listed := h.want(owner, "GET", "/api/v1/management-tokens", nil, nil, 200)
	for _, item := range listed["items"].([]any) {
		record := item.(map[string]any)
		if _, leaked := record["secret"]; leaked {
			t.Fatal("token listing disclosed a secret")
		}
		if _, leaked := record["digest"]; leaked {
			t.Fatal("token listing disclosed a digest")
		}
	}
	detail := h.want(owner, "GET", "/api/v1/management-tokens/"+tokenID, nil, nil, 200)
	if detail["created_by_email"] != "owner@example.com" || detail["revoked_at"] != nil {
		t.Fatal("token detail is incomplete", detail)
	}

	h.want(owner, "POST", "/api/v1/management-tokens/"+tokenID+"/revoke", nil, map[string]string{"Idempotency-Key": "revoke-missing-match"}, 428)
	h.want(owner, "POST", "/api/v1/management-tokens/"+tokenID+"/revoke", nil, map[string]string{"Idempotency-Key": "revoke-stale", "If-Match": `"00000000-0000-0000-0000-000000000000"`}, 412)
	revoked := h.want(owner, "POST", "/api/v1/management-tokens/"+tokenID+"/revoke", nil, withMatch(detail, map[string]string{"Idempotency-Key": "revoke"}), 200)
	if revoked["etag"] == detail["etag"] {
		t.Fatal("revocation must rotate the token etag")
	}
	if status, _ := h.machine(secret, "GET", "/api/v1/routes", nil, nil); status != 401 {
		t.Fatal("a revoked token must fail authentication immediately, got", status)
	}

	events := h.want(owner, "GET", "/api/v1/audit?action=provider.create", nil, nil, 200)
	machineEvent := events["items"].([]any)[0].(map[string]any)
	if machineEvent["actor_type"] != "management_token" || machineEvent["actor_label"] != "deploy" || machineEvent["actor_management_token_id"] != tokenID {
		t.Fatal("machine audit attribution is missing", machineEvent)
	}
	events = h.want(owner, "GET", "/api/v1/audit?action=management_token.create", nil, nil, 200)
	for _, item := range events["items"].([]any) {
		event := item.(map[string]any)
		if event["actor_type"] != "user" || event["actor_label"] != "owner@example.com" {
			t.Fatal("token administration must be attributed to the owner", event)
		}
	}
}

// A demoted creator's token must not keep the owner authority it was
// issued under, such as promoting another member to owner.

// Authority is evaluated against the creator on every request, so a
// reactivated creator's token resumes with the creator's current role.
