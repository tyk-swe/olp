//go:build integration

package integration_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/testutil"
)

// The old binary is pinned to the pre-fidelity release and built separately by
// integration.sh. This checks a live cached reader, rather than projecting an
// old struct through current code and assuming that it behaves like a process.
func TestMixedVersionStrictRouteUsesNewIdentity(t *testing.T) {
	oldBinary := required(t, "OLP_TEST_OLD_BINARY")
	newBinary := required(t, "OLP_TEST_BINARY")
	pool, dbURL := accessDatabase(t)
	assets := t.TempDir()
	if err := os.WriteFile(assets+"/index.html", []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	processEnv := map[string]string{
		"OLP_DATABASE_URL": dbURL, "OLP_VALKEY_URL": required(t, "OLP_TEST_VALKEY_URL"),
		"OLP_AUTH_HMAC_KEY_FILE":   required(t, "OLP_AUTH_HMAC_KEY_FILE"),
		"OLP_MASTER_KEY_FILE":      required(t, "OLP_MASTER_KEY_FILE"),
		"OLP_BOOTSTRAP_TOKEN_FILE": required(t, "OLP_BOOTSTRAP_TOKEN_FILE"),
		"OLP_PUBLIC_ORIGIN":        "https://console.test", "OLP_LISTEN_ADDR": "127.0.0.1:0",
		"OLP_OBSERVABILITY_LISTEN_ADDR": "127.0.0.1:0", "OLP_CONSOLE_DIR": assets,
		"OLP_PROVIDER_EGRESS_ALLOW_CIDRS":      "127.0.0.0/8",
		"OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS": "127.0.0.1",
		"OLP_SHUTDOWN_TIMEOUT":                 "1s",
	}
	migrate := exec.CommandContext(t.Context(), oldBinary, "migrate")
	migrate.Env = testutil.Environment(processEnv)
	if output, err := migrate.CombinedOutput(); err != nil {
		t.Fatalf("old migrate: %v: %s", err, output)
	}
	old := testutil.StartProcess(t, oldBinary, "all", processEnv)
	bootstrap, err := os.ReadFile(processEnv["OLP_BOOTSTRAP_TOKEN_FILE"])
	if err != nil {
		t.Fatal(err)
	}
	h := &accessHarness{t: t, Pool: pool, DBURL: dbURL, Bootstrap: strings.TrimSpace(string(bootstrap)),
		HTTP: &httptest.Server{URL: old.PublicOrigin}, Server: &access.Server{Origin: "https://console.test"}}
	var dispatches atomic.Int64
	fixture := newOpenAIFixture(t, "")
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Api-Key") != vendorSecret {
			http.Error(w, "unauthorized fixture", 401)
			return
		}
		if r.Method == "GET" && (r.URL.Path == "/v1/models" || r.URL.Path == "/openai/models") {
			writeJSON(w, map[string]any{"data": []any{map[string]any{"id": vendorModel}}})
			return
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/chat/completions") {
			dispatches.Add(1)
		}
		request, err := http.NewRequestWithContext(r.Context(), r.Method, fixture.URL+r.URL.RequestURI(), r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		request.Header = r.Header.Clone()
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Error(err)
			return
		}
		defer response.Body.Close()
		for name, values := range response.Header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		io.Copy(w, response.Body)
	}))
	t.Cleanup(vendor.Close)
	owner := h.owner()
	provider := h.want(owner, "POST", "/api/v3/providers", map[string]any{
		"name": "mixed version provider", "model": vendorModel, "credential": vendorSecret,
		"configuration": map[string]any{"kind": "azure_openai", "auth_mode": "api_key", "endpoint": vendor.URL, "deployment": vendorModel, "api_version": "2024-10-21"}}, idem(uuid.NewString()), 201)
	providerPath := "/api/v3/providers/" + provider["id"].(string)
	h.want(owner, "POST", providerPath+"/probe", nil, etagHeader(provider), 200)
	model := h.want(owner, "GET", providerPath+"/models", nil, nil, 200)["items"].([]any)[0].(map[string]any)
	provider = h.want(owner, "PATCH", providerPath+"/models/"+model["id"].(string), map[string]any{"enabled": true, "capabilities": []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}}, etagHeader(provider), 200)
	certified := h.want(owner, "POST", providerPath+"/models/"+model["id"].(string)+"/certify", nil, etagHeader(provider), 200)
	if certified["status"] != "certified" {
		t.Fatalf("old provider certification: %v", certified)
	}
	provider = h.want(owner, "GET", providerPath, nil, nil, 200)
	h.want(owner, "POST", providerPath+"/activate", nil, withMatch(provider, idem(uuid.NewString())), 200)
	slug := "mixed-" + uuid.NewString()[:8]
	draft := h.want(owner, "POST", "/api/v3/route-drafts", fidelityDraft(slug, provider["id"]), idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v3/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	oldKey := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "old route", "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, idem(uuid.NewString()), 201)
	oldSecret := oldKey["secret"].(string)
	otherOldKey := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "retained old route", "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, idem(uuid.NewString()), 201)
	otherOldSecret := otherOldKey["secret"].(string)
	call := func(origin, secret, route string) int {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"model": route, "messages": []any{map[string]any{"role": "user", "content": "legacy or strict"}}})
		req, _ := http.NewRequestWithContext(t.Context(), "POST", origin+"/v1/chat/completions", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+secret)
		client := &http.Client{Timeout: 5 * time.Second}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		io.Copy(io.Discard, response.Body)
		return response.StatusCode
	}
	awaitRoute := func(origin, secret, slug string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			got := call(origin, secret, slug)
			if got == 200 {
				return
			}
			if got != 401 && got != 404 && got != 503 || time.Now().After(deadline) {
				t.Fatalf("reader failed its route %s: %d", slug, got)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	awaitRoute(old.PublicOrigin, oldSecret, slug)
	oldDispatches := dispatches.Load()
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	newer := testutil.StartProcess(t, newBinary, "all", processEnv)
	h.HTTP.URL = newer.PublicOrigin
	provider = h.want(owner, "GET", providerPath, nil, nil, 200)
	configuration := provider["configuration"].(map[string]any)
	configuration["profile_id"], configuration["profile_revision"] = "azure-legacy-chat", "1"
	h.want(owner, "PATCH", providerPath, map[string]any{"name": provider["name"], "configuration": configuration}, etagHeader(provider), 200)
	certifyProfileNetworkProvider(t, h, owner, provider["id"].(string))
	route := h.want(owner, "GET", "/api/v3/routes", nil, nil, 200)["items"].([]any)[0].(map[string]any)
	newSlug := slug + "-strict"
	migrated := h.want(owner, "POST", "/api/v3/routes/"+route["id"].(string)+"/migration-draft", map[string]any{"slug": newSlug}, withMatch(route, idem(uuid.NewString())), 201)
	path := "/api/v3/route-drafts/" + migrated["id"].(string)
	migrated = h.want(owner, "POST", path+"/validate", nil, etagHeader(migrated), 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(migrated, idem(uuid.NewString())), 200)
	newSecret := stateKey(t, h, owner, newSlug, false)
	awaitRoute(newer.PublicOrigin, newSecret, newSlug)
	before := dispatches.Load()
	// Wait until the old manager actually observes and rejects the incompatible
	// release. A quick read before its poll would not prove the cutover property.
	deadline := time.Now().Add(12 * time.Second)
	for !strings.Contains(old.Log(), "snapshot digest mismatch") {
		if time.Now().After(deadline) {
			t.Fatal("old reader did not observe the incompatible strict release")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got := call(old.PublicOrigin, oldSecret, slug); got != 200 {
		t.Fatalf("old reader lost the retained legacy route: %d", got)
	}
	if got := call(old.PublicOrigin, newSecret, newSlug); got == 200 {
		t.Fatal("old reader served the new strict identity")
	}
	if dispatches.Load() != before+1 || before < oldDispatches+1 {
		t.Fatal("mixed-version route dispatch counts changed")
	}
	oldMigrate := exec.CommandContext(t.Context(), oldBinary, "migrate")
	oldMigrate.Env = testutil.Environment(processEnv)
	if output, err := oldMigrate.CombinedOutput(); err == nil || !strings.Contains(string(output), "newer Go binary") {
		t.Fatal("old binary restarted against a newer schema", err)
	}
	keyRecord := h.want(owner, "GET", "/api/v3/api-keys/"+oldKey["id"].(string), nil, nil, 200)
	h.want(owner, "POST", "/api/v3/api-keys/"+oldKey["id"].(string)+"/revoke", nil, withMatch(keyRecord, idem(uuid.NewString())), 200)
	deadline = time.Now().Add(12 * time.Second)
	for {
		got := call(old.PublicOrigin, oldSecret, slug)
		if got == 401 {
			break
		}
		if got != 200 || time.Now().After(deadline) {
			t.Fatalf("old reader did not enforce current key revocation: %d", got)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got := call(old.PublicOrigin, otherOldSecret, slug); got != 200 {
		t.Fatalf("independent retained key stopped serving: %d", got)
	}
	credentials := h.want(owner, "GET", providerPath+"/credentials", nil, nil, 200)["items"].([]any)
	credentialID := credentials[0].(map[string]any)["id"].(string)
	provider = h.want(owner, "GET", providerPath, nil, nil, 200)
	h.want(owner, "POST", providerPath+"/credentials/"+credentialID+"/revoke", nil, withMatch(provider, idem(uuid.NewString())), 200)
	deadline = time.Now().Add(12 * time.Second)
	for {
		got := call(old.PublicOrigin, otherOldSecret, slug)
		if got == 503 {
			break
		}
		if got != 200 || time.Now().After(deadline) {
			t.Fatalf("old reader did not enforce current credential revocation: %d", got)
		}
		time.Sleep(100 * time.Millisecond)
	}
	priorRejection := dispatches.Load()
	if got := call(old.PublicOrigin, otherOldSecret, slug); got != 503 || dispatches.Load() != priorRejection {
		t.Fatal("revoked credential reached the provider after current authority was observed")
	}
}
