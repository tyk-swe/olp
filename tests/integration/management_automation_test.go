//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/config"
	"github.com/tyk-swe/olp/internal/management"
	"github.com/tyk-swe/olp/internal/process"
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
	created := h.want(owner, "POST", "/api/v3/management-tokens", map[string]any{
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

	h.want(owner, "POST", "/api/v3/management-tokens", map[string]any{
		"name": "missing idempotency", "scopes": []string{"read"},
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}, nil, 400)
	h.want(owner, "POST", "/api/v3/management-tokens", map[string]any{
		"name": "bad scope", "scopes": []string{"inference"},
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}, map[string]string{"Idempotency-Key": "bad-scope"}, 422)
	h.want(owner, "POST", "/api/v3/management-tokens", map[string]any{
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

	h.machineWant(secret, "GET", "/api/v3/routes", nil, nil, 200)
	provider := h.machineWant(secret, "POST", "/api/v3/providers", map[string]any{
		"name":          "Automated vendor",
		"configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": "http://127.0.0.1:9/v1/"},
		"credential":    "vendor-secret",
	}, map[string]string{"Idempotency-Key": "machine-provider"}, 201)
	providerID := provider["id"].(string)
	h.machineWant(secret, "GET", "/api/v3/providers/"+providerID, nil, nil, 200)
	h.machineWant(secret, "GET", "/api/v3/providers/"+providerID+"/models", nil, nil, 200)

	users := h.want(owner, "GET", "/api/v3/users", nil, nil, 200)
	ownerRecord := users["items"].([]any)[0].(map[string]any)
	for _, tc := range []struct {
		base         string
		method, path string
		body         any
		headers      map[string]string
	}{
		{h.HTTP.URL, "POST", "/api/v3/api-keys", map[string]any{"name": "denied"}, map[string]string{"Idempotency-Key": "denied-key"}},
		{h.HTTP.URL, "PATCH", "/api/v3/users/" + ownerRecord["id"].(string), map[string]any{"role": "viewer"}, map[string]string{"If-Match": `"` + ownerRecord["etag"].(string) + `"`}},
		{h.HTTP.URL, "PUT", "/api/v3/settings/retention.usage_days", map[string]any{"value": "30"}, map[string]string{"If-Match": `"00000000-0000-0000-0000-000000000000"`}},
		{usageHTTP.URL, "POST", "/api/v3/pricing/revisions", map[string]any{"effective_at": time.Now().UTC().Format(time.RFC3339)}, map[string]string{"Idempotency-Key": "denied-pricing"}},
		{usageHTTP.URL, "GET", "/api/v3/usage/summary?start=2024-01-01T00:00:00Z&end=2024-01-02T00:00:00Z", nil, nil},
		{h.HTTP.URL, "GET", "/api/v3/management-tokens", nil, nil},
		{h.HTTP.URL, "GET", "/api/v3/profile", nil, nil},
		{h.HTTP.URL, "GET", "/api/v3/sessions/current", nil, nil},
	} {
		if status, _ := h.machineOn(tc.base, secret, tc.method, tc.path, tc.body, tc.headers); status != 403 {
			t.Fatalf("%s %s: a read+configure token must be denied (403), got %d", tc.method, tc.path, status)
		}
	}

	_, allSecret := createToken(h, owner, "everything", []string{"read", "access_read", "access", "settings", "configure", "keys", "playground", "usage"})
	if status, _ := h.machine(allSecret, "GET", "/api/v3/management-tokens", nil, nil); status != 403 {
		t.Fatal("token administration must require an owner session, got", status)
	}
	if status, _ := h.machine(allSecret, "POST", "/api/v3/management-tokens", map[string]any{"name": "nested", "scopes": []string{"read"}, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, map[string]string{"Idempotency-Key": "nested"}); status != 403 {
		t.Fatal("a machine principal must not create management tokens, got", status)
	}
	status, _ := h.machineOn(usageHTTP.URL, allSecret, "GET", "/api/v3/usage/summary?start=2024-01-01T00:00:00Z&end=2024-01-02T00:00:00Z", nil, nil)
	if status != 200 {
		t.Fatal("a usage-scoped token must read usage, got", status)
	}

	listed := h.want(owner, "GET", "/api/v3/management-tokens", nil, nil, 200)
	for _, item := range listed["items"].([]any) {
		record := item.(map[string]any)
		if _, leaked := record["secret"]; leaked {
			t.Fatal("token listing disclosed a secret")
		}
		if _, leaked := record["digest"]; leaked {
			t.Fatal("token listing disclosed a digest")
		}
	}
	detail := h.want(owner, "GET", "/api/v3/management-tokens/"+tokenID, nil, nil, 200)
	if detail["created_by_email"] != "owner@example.com" || detail["revoked_at"] != nil {
		t.Fatal("token detail is incomplete", detail)
	}

	h.want(owner, "POST", "/api/v3/management-tokens/"+tokenID+"/revoke", nil, map[string]string{"Idempotency-Key": "revoke-missing-match"}, 428)
	h.want(owner, "POST", "/api/v3/management-tokens/"+tokenID+"/revoke", nil, map[string]string{"Idempotency-Key": "revoke-stale", "If-Match": `"00000000-0000-0000-0000-000000000000"`}, 412)
	revoked := h.want(owner, "POST", "/api/v3/management-tokens/"+tokenID+"/revoke", nil, withMatch(detail, map[string]string{"Idempotency-Key": "revoke"}), 200)
	if revoked["etag"] == detail["etag"] {
		t.Fatal("revocation must rotate the token etag")
	}
	if status, _ := h.machine(secret, "GET", "/api/v3/routes", nil, nil); status != 401 {
		t.Fatal("a revoked token must fail authentication immediately, got", status)
	}

	events := h.want(owner, "GET", "/api/v3/audit?action=provider.create", nil, nil, 200)
	machineEvent := events["items"].([]any)[0].(map[string]any)
	if machineEvent["actor_type"] != "management_token" || machineEvent["actor_label"] != "deploy" || machineEvent["actor_management_token_id"] != tokenID {
		t.Fatal("machine audit attribution is missing", machineEvent)
	}
	events = h.want(owner, "GET", "/api/v3/audit?action=management_token.create", nil, nil, 200)
	for _, item := range events["items"].([]any) {
		event := item.(map[string]any)
		if event["actor_type"] != "user" || event["actor_label"] != "owner@example.com" {
			t.Fatal("token administration must be attributed to the owner", event)
		}
	}
}

func TestProvisioningReconcilesWithoutLogin(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	_, secret := createToken(h, owner, "provisioner", []string{"access"})
	source := "/api/v3/provisioning/scim-bridge/users/"

	reconciled := h.machineWant(secret, "PUT", source+"ext-001", map[string]any{
		"email": "provisioned@example.com", "display_name": "External One", "role": "developer", "active": true,
	}, nil, 200)
	userID := reconciled["id"].(string)
	if reconciled["etag"] == "" || reconciled["email"] != "provisioned@example.com" {
		t.Fatal("provisioning did not return a safe user record", reconciled)
	}

	updated := h.machineWant(secret, "PUT", source+"ext-001", map[string]any{
		"email": "provisioned@example.com", "display_name": "External One", "role": "operator", "active": true,
	}, nil, 200)
	if updated["role"] != "operator" || updated["etag"] == reconciled["etag"] {
		t.Fatal("reconciliation must apply the external state and rotate the etag")
	}

	if status, out := h.machine(secret, "PUT", source+"ext-999", map[string]any{
		"email": "owner@example.com", "display_name": "Collision", "role": "viewer", "active": true,
	}, nil); status != 409 || problemCode(t, out) != "email_unavailable" {
		t.Fatal("an owned email must not be provisioned to another identity", status, out)
	}
	if status, _ := h.machine(secret, "PUT", "/api/v3/provisioning/bad%2Fsource/users/ext-1", map[string]any{
		"email": "x@example.com", "display_name": "X", "role": "viewer", "active": true,
	}, nil); status == 200 {
		t.Fatal("a source containing a slash must be rejected")
	}

	fresh := h.want(owner, "GET", "/api/v3/users/"+userID, nil, nil, 200)
	h.want(owner, "PATCH", "/api/v3/users/"+userID, map[string]any{"role": "viewer"}, etagHeader(fresh), 200)
	if status, out := h.machine(secret, "PUT", source+"ext-001", map[string]any{
		"email": "provisioned@example.com", "display_name": "External One", "role": "developer", "active": true,
	}, nil); status != 409 || problemCode(t, out) != "provisioning_ownership_changed" {
		t.Fatal("locally managed identities must reject external reconciliation", status, out)
	}
	if status, out := h.machine(secret, "DELETE", source+"ext-001", nil, nil); status != 409 || problemCode(t, out) != "provisioning_ownership_changed" {
		t.Fatal("locally managed identities must reject external deprovisioning", status, out)
	}

	second := h.machineWant(secret, "PUT", source+"ext-002", map[string]any{
		"email": "lifecycle@example.com", "display_name": "External Two", "role": "viewer", "active": true,
	}, nil, 200)
	secondID := second["id"].(string)
	recovered, err := access.RecoverPassword(t.Context(), h.Pool, "lifecycle@example.com", "a temporary recovery passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if recovered["password_recovered"] != true {
		t.Fatal("recovery did not confirm the credential update")
	}
	guest := &browser{}
	h.want(guest, "POST", "/api/v3/sessions", map[string]any{"email": "lifecycle@example.com", "password": "a temporary recovery passphrase"}, nil, 201)
	h.want(guest, "GET", "/api/v3/profile", nil, nil, 200)

	h.machineWant(secret, "PUT", source+"ext-002", map[string]any{
		"email": "lifecycle@example.com", "display_name": "External Two Renamed", "role": "viewer", "active": true,
	}, nil, 200)
	h.want(guest, "GET", "/api/v3/profile", nil, nil, 401)

	h.machineWant(secret, "DELETE", source+"ext-002", nil, nil, 204)
	if status, _ := h.machine(secret, "DELETE", source+"ext-002", nil, nil); status != 204 {
		t.Fatal("deprovisioning must be idempotent for a mapped identity", status)
	}
	if status, _ := h.machine(secret, "DELETE", source+"ext-missing", nil, nil); status != 404 {
		t.Fatal("deprovisioning an unmapped identity must be 404", status)
	}
	if inactive := h.want(owner, "GET", "/api/v3/users/"+secondID, nil, nil, 200); inactive["active"] != false {
		t.Fatal("deprovisioning must reconcile the user to inactive")
	}
	h.want(guest, "POST", "/api/v3/sessions", map[string]any{"email": "lifecycle@example.com", "password": "a temporary recovery passphrase"}, nil, 401)

	events := h.want(owner, "GET", "/api/v3/audit?action=user.provision", nil, nil, 200)
	event := events["items"].([]any)[0].(map[string]any)
	if event["actor_type"] != "management_token" || event["actor_label"] != "provisioner" {
		t.Fatal("provisioning must be attributed to the machine token", event)
	}
}

func TestAccountRecoveryInvalidatesAndAudits(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	dev := h.invite(owner, "dev@example.com", "developer")
	h.want(dev, "GET", "/api/v3/profile", nil, nil, 200)

	recovered, err := access.RecoverPassword(t.Context(), h.Pool, " Dev@Example.com ", "a brand new passphrase")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(recovered)
	if recovered["password_recovered"] != true || recovered["email"] != "dev@example.com" || strings.Contains(string(encoded), "passphrase") {
		t.Fatal("recovery returned an unsafe result", recovered)
	}
	h.want(dev, "GET", "/api/v3/profile", nil, nil, 401)
	h.want(&browser{}, "POST", "/api/v3/sessions", map[string]any{"email": "dev@example.com", "password": accessPassword}, nil, 401)
	fresh := &browser{}
	h.want(fresh, "POST", "/api/v3/sessions", map[string]any{"email": "dev@example.com", "password": "a brand new passphrase"}, nil, 201)
	h.want(fresh, "GET", "/api/v3/profile", nil, nil, 200)

	passwordFile := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(passwordFile, []byte("a second recovery passphrase\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	c := config.Config{DatabaseURL: h.DBURL, DatabaseMaxConnections: 4, RequestTimeout: 5 * time.Second, StartupTimeout: 30 * time.Second}
	if err := process.Maintenance(t.Context(), c, "reset-password", process.MaintenanceOptions{AccountEmail: "dev@example.com", PasswordFile: passwordFile}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"password_recovered":true`) || strings.Contains(output.String(), "passphrase") {
		t.Fatal("recovery output must confirm without disclosing the password", output.String())
	}
	h.want(&browser{}, "POST", "/api/v3/sessions", map[string]any{"email": "dev@example.com", "password": "a second recovery passphrase"}, nil, 201)
	h.want(fresh, "GET", "/api/v3/profile", nil, nil, 401)

	events := h.want(owner, "GET", "/api/v3/audit?action=user.password_recover", nil, nil, 200)
	if len(events["items"].([]any)) != 2 {
		t.Fatal("both recoveries must be audited", events)
	}
	for _, item := range events["items"].([]any) {
		event := item.(map[string]any)
		if event["actor_type"] != "system" || event["actor_user_id"] != nil || event["actor_management_token_id"] != nil {
			t.Fatal("recovery must be attributed to the system, not an actor", event)
		}
	}

	devID := recovered["id"].(string)
	devRecord := h.want(owner, "GET", "/api/v3/users/"+devID, nil, nil, 200)
	h.want(owner, "PATCH", "/api/v3/users/"+devID, map[string]any{"active": false}, etagHeader(devRecord), 200)
	for _, target := range []string{"ghost@example.com", "dev@example.com"} {
		if _, err := access.RecoverPassword(t.Context(), h.Pool, target, "a brand new passphrase"); err == nil || strings.Contains(err.Error(), strings.ToLower(target)) {
			t.Fatal("recovery must fail generically without confirming the account", err)
		}
	}
}
