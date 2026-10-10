//go:build integration

package integration_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/testutil"
)

func TestProviderCredentialDestinationIsImmutableEvenAfterSlotDetach(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	for _, kind := range []string{"openai", "openai_compatible", "vertex_ai"} {
		t.Run(kind, func(t *testing.T) {
			cfg := map[string]any{"kind": kind, "auth_mode": "api_key", "endpoint": "https://api.openai.com/v1"}
			if kind == "vertex_ai" {
				cfg = map[string]any{"kind": kind, "auth_mode": "service_account", "cloud_region": "global", "cloud_project": "project", "endpoint": "https://aiplatform.googleapis.com/v1/projects/project/locations/global/publishers/google"}
			}
			created := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": kind, "configuration": cfg, "credential": vendorSecret}, idem(uuid.NewString()), 201)
			path := "/api/v1/providers/" + created["id"].(string)
			// Historical versions remain available to explicit slot selection. An
			// empty current pool must not authorize rebinding those write-only secrets.
			if _, err := h.Pool.Exec(t.Context(), "UPDATE olp.provider_slots SET credential_id=NULL WHERE provider_id=$1", created["id"]); err != nil {
				t.Fatal(err)
			}
			cfg["endpoint"] = "https://attacker.example/v1"
			if kind == "vertex_ai" {
				cfg["cloud_project"] = "another-project"
				cfg["endpoint"] = "https://aiplatform.googleapis.com/v1/projects/another-project/locations/global/publishers/google"
			}
			refused := h.want(owner, "PATCH", path, map[string]any{"name": kind, "configuration": cfg}, etagHeader(created), 422)
			if problemCode(t, refused) != "credential_destination_changed" {
				t.Fatal(refused)
			}
			current := h.want(owner, "GET", path, nil, nil, 200)
			if current["etag"] != created["etag"] {
				t.Fatal("rejected update changed provider")
			}
			if kind == "openai" {
				cfg["kind"] = "openai_compatible"
				h.want(owner, "PATCH", path, map[string]any{"name": kind, "configuration": cfg}, etagHeader(created), 422)
			}
		})
	}
}

func TestGrantVersionRejectsAnotherProfileWithinItsBuild(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	authority := testutil.NewOAuthServer(t)
	upstream := newGrantUpstream(t, authority)
	digest := installGrantPlugin(t, h, owner, upstream, "0.1.0")
	path := grantProvider(t, h, owner, digest, nil)
	credential := enrollGrant(t, h, owner, path)
	var profile string
	if err := h.Pool.QueryRow(t.Context(), "SELECT profile_id FROM olp.provider_credentials WHERE id=$1", credential).Scan(&profile); err != nil || profile != "reference-grant-chat" {
		t.Fatalf("enrollment binding %q: %v", profile, err)
	}
	// Switch to a sibling profile in the same installed and approved build.
	detail := h.want(owner, "GET", path, nil, nil, 200)
	cfg := detail["configuration"].(map[string]any)
	cfg["profile_id"] = "reference-device-chat"
	h.want(owner, "PATCH", path, map[string]any{"name": detail["name"], "configuration": cfg}, etagHeader(detail), 200)
	detail = h.want(owner, "GET", path, nil, nil, 200)
	refused := h.want(owner, "POST", path+"/probe", nil, etagHeader(detail), 422)
	if problemCode(t, refused) != "credential_mismatch" {
		t.Fatal(refused)
	}
}

func TestConfigurationImportCannotMoveStoredCredentials(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Bound provider", "configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": "https://api.openai.com/v1"}, "credential": vendorSecret}, idem(uuid.NewString()), 201)
	exported := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	document := exported["document"].(map[string]any)
	provider := document["providers"].([]any)[0].(map[string]any)
	provider["configuration"].(map[string]any)["endpoint"] = "https://attacker.example/v1"
	refused := h.want(owner, "POST", "/api/v1/configuration/apply", map[string]any{"document": document}, idem(uuid.NewString()), 422)
	if problemCode(t, refused) != "credential_destination_changed" {
		t.Fatal(refused)
	}
}

func TestGrantProfileMigrationCannotAlsoMoveStoredCredentials(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	authority := testutil.NewOAuthServer(t)
	first := installGrantPlugin(t, h, owner, newGrantUpstream(t, authority), "0.1.0")
	second := installGrantPlugin(t, h, owner, newGrantUpstream(t, authority), "0.2.0")
	path := grantProvider(t, h, owner, first, nil)
	enrollGrant(t, h, owner, path)
	detail := h.want(owner, "GET", path, nil, nil, 200)
	cfg := detail["configuration"].(map[string]any)
	cfg["profile_revision"] = second
	// This build also changes the endpoint. Letting the profile exception
	// carry other boundary changes would allow a later return to the first
	// profile to reuse its grant under a different destination or options.
	refused := h.want(owner, "PATCH", path, map[string]any{"name": detail["name"], "configuration": cfg}, etagHeader(detail), 422)
	if problemCode(t, refused) != "credential_destination_changed" {
		t.Fatal(refused)
	}
	revokePluginCredentials(t, h, owner, path)
	detail = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "PATCH", path, map[string]any{"name": detail["name"], "configuration": cfg}, etagHeader(detail), 200)
}

func TestProviderRestoreCannotRebindCurrentCredentials(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	first, second := newVendor(t), newVendor(t)
	created := h.want(owner, "POST", "/api/v1/providers", map[string]any{
		"name": "Restored destination", "model": vendorModel, "credential": vendorSecret,
		"configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": first.URL + "/v1"},
	}, idem(uuid.NewString()), 201)
	path := "/api/v1/providers/" + created["id"].(string)
	certifyPluginProvider(t, h, owner, path)
	revokePluginCredentials(t, h, owner, path)
	detail := h.want(owner, "GET", path, nil, nil, 200)
	cfg := detail["configuration"].(map[string]any)
	cfg["endpoint"] = second.URL + "/v1"
	detail = h.want(owner, "PATCH", path, map[string]any{"name": "Restored destination", "configuration": cfg}, etagHeader(detail), 200)
	h.want(owner, "POST", path+"/credentials", map[string]any{"credential": vendorSecret}, withMatch(detail, idem(uuid.NewString())), 201)
	for _, restore := range []string{"/restore-as-draft", "/revisions/1/restore-as-draft"} {
		detail = h.want(owner, "GET", path, nil, nil, 200)
		refused := h.want(owner, "POST", path+restore, nil, withMatch(detail, idem(uuid.NewString())), 422)
		if problemCode(t, refused) != "credential_destination_changed" {
			t.Fatal(refused)
		}
		current := h.want(owner, "GET", path, nil, nil, 200)
		if current["etag"] != detail["etag"] || current["configuration"].(map[string]any)["endpoint"] != second.URL+"/v1" {
			t.Fatal("refused restoration changed the credential's destination")
		}
	}
	revokePluginCredentials(t, h, owner, path)
	detail = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/restore-as-draft", nil, withMatch(detail, idem(uuid.NewString())), 200)
}

func TestDetachedNetworkCredentialKeepsItsDestination(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	cfg := map[string]any{"kind": "openai_compatible", "auth_mode": "none", "endpoint": "https://provider.example/v1",
		"options": map[string]any{"network": map[string]any{"proxy_url": "https://proxy.example"}}}
	created := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Network binding", "configuration": cfg}, idem(uuid.NewString()), 201)
	path := "/api/v1/providers/" + created["id"].(string)
	secret := `{"proxy_username":"fixture-user","proxy_password":"fixture-password"}`
	h.want(owner, "POST", path+"/network-credentials", map[string]any{"credential": secret}, withMatch(created, idem(uuid.NewString())), 201)
	detail := h.want(owner, "GET", path, nil, nil, 200)
	cfg["options"].(map[string]any)["network"].(map[string]any)["proxy_url"] = "https://other-proxy.example"
	refused := h.want(owner, "PATCH", path, map[string]any{"name": "Network binding", "configuration": cfg}, etagHeader(detail), 422)
	if problemCode(t, refused) != "credential_destination_changed" || strings.Contains(refused["detail"].(string), "fixture-password") {
		t.Fatal(refused)
	}
}
