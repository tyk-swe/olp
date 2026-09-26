//go:build integration

package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/secrets"
)

// Exercise the public setup, declaration, certification, and publication APIs.
// Only OpenAI's network transport is replaced; no provider or route is seeded.
func TestMediaManagementSetupPublishesUsableRoutes(t *testing.T) {
	adminURL := os.Getenv("OLP_TEST_DATABASE_ADMIN_URL")
	if adminURL == "" {
		t.Fatal("OLP_TEST_DATABASE_ADMIN_URL is required; run make integration")
	}
	cfg, err := pgxpool.ParseConfig(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	name := "olp_media_setup_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(t.Context(), "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Log(err)
		}
		admin.Close()
	})
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	installation, err := database.Installation(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.ParseRing([]byte(`{"active_version":1,"keys":[{"version":1,"key":"` + strings.Repeat("ab", 32) + `"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	auth, _ := secrets.DecodeKey(strings.Repeat("cd", 32))
	bootstrap := secrets.Token()
	a, err := access.New(t.Context(), pool, installation, "https://console.test", secrets.NewAuthKey(auth, installation), ring, bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	provider := nativeMediaProbeServer(t)
	provider.Access = a
	mux := http.NewServeMux()
	a.Register(mux)
	provider.Register(mux)
	routes.New(a).Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	cookies := map[string]*http.Cookie{}
	csrf := ""
	call := func(method, path string, body any, etag string, want int) map[string]any {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", a.Origin)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-OLP-Setup-Token", bootstrap)
		req.Header.Set("X-CSRF-Token", csrf)
		req.Header.Set("Idempotency-Key", uuid.NewString())
		if etag != "" {
			req.Header.Set("If-Match", `"`+etag+`"`)
		}
		for _, c := range cookies {
			req.AddCookie(c)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != want {
			t.Fatalf("%s %s: status=%d want=%d body=%s err=%v", method, path, resp.StatusCode, want, raw, err)
		}
		var result map[string]any
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		for _, c := range resp.Cookies() {
			if c.MaxAge < 0 {
				delete(cookies, c.Name)
			} else {
				cookies[c.Name] = c
			}
		}
		if token, ok := result["csrf_token"].(string); ok {
			csrf = token
		}
		if token := resp.Header.Get("X-CSRF-Token"); token != "" {
			csrf = token
		}
		return result
	}
	call("POST", "/api/v1/setup", map[string]any{"email": "owner@example.com", "display_name": "Owner", "password": "correct horse battery staple", "installation_name": "Media setup"}, "", 201)
	created := call("POST", "/api/v1/providers", map[string]any{"name": "Native media", "model": "media-model", "credential": "test-credential", "configuration": map[string]any{"kind": "openai", "auth_mode": "api_key"}}, "", 201)
	providerPath := "/api/v1/providers/" + created["id"].(string)
	models := call("GET", providerPath+"/models", nil, "", 200)
	modelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
	var tuples []CapabilityInput
	for _, tuple := range capabilitiesFor(KindOpenAI, KindOpenAI) {
		switch tuple.Operation {
		case "generation", "token_count", "embeddings", "moderation":
		default:
			tuples = append(tuples, tuple)
		}
	}
	current := call("GET", providerPath, nil, "", 200)
	call("PATCH", providerPath+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": tuples}, current["etag"].(string), 200)
	current = call("GET", providerPath, nil, "", 200)
	certified := call("POST", providerPath+"/models/"+modelID+"/certify", nil, current["etag"].(string), 200)
	if certified["status"] != "certified" {
		t.Fatalf("media certification: %v", certified)
	}
	current = call("GET", providerPath, nil, "", 200)
	call("POST", providerPath+"/activate", nil, current["etag"].(string), 200)
	groups := [][]string{{"image_generation", "image_edit", "image_variation"}, {"speech"}, {"transcription"}, {"video_create", "video_list", "video_get", "video_content", "video_delete"}}
	for _, ops := range groups {
		slug := strings.ReplaceAll(ops[0], "_", "-")
		draft := call("POST", "/api/v1/route-drafts", map[string]any{"slug": slug, "operations": ops, "overall_timeout_ms": 5000, "max_attempts": 1, "targets": []any{map[string]any{"provider_model_id": modelID, "priority": 0, "weight": 1, "timeout_ms": 2000}}}, "", 201)
		path := "/api/v1/route-drafts/" + draft["id"].(string)
		draft = call("POST", path+"/validate", nil, draft["etag"].(string), 200)
		mode := "unary"
		if ops[0] == "video_create" {
			mode = "async"
		}
		simulated := call("POST", path+"/simulate", map[string]any{"operation": ops[0], "surface": "openai", "mode": mode, "seed": "media-setup"}, "", 200)
		if len(simulated["targets"].([]any)) != 1 || simulated["targets"].([]any)[0].(map[string]any)["eligible"] != true {
			t.Fatalf("media route has no eligible target: %v", simulated)
		}
		call("POST", path+"/activate", nil, draft["etag"].(string), 200)
	}
	var encoded []byte
	if err := pool.QueryRow(t.Context(), "SELECT snapshot FROM olp_go.runtime_releases ORDER BY sequence DESC LIMIT 1").Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	var snapshot runtime.Snapshot
	if err := json.Unmarshal(encoded, &snapshot); err != nil {
		t.Fatal(err)
	}
	for _, ops := range groups {
		slug := strings.ReplaceAll(ops[0], "_", "-")
		for _, op := range ops {
			for _, tuple := range tuples {
				if tuple.Operation != op {
					continue
				}
				plan, err := runtime.PlanRequest(&snapshot, slug, op, "openai", tuple.Mode, []byte("media-setup"), runtime.SelectionOptions{CheckSlots: true})
				if err != nil || len(plan.Attempts) != 1 {
					t.Fatalf("published %s/%s/%s unusable: %+v %v", slug, op, tuple.Mode, plan, err)
				}
			}
		}
	}
}
