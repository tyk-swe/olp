//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/secrets"
)

func TestMountedDefaultsServeWithoutMasterKeyAndKeepPublishedOwnership(t *testing.T) {
	fixture := glSeed(t, nil, limits.FailClosed)
	h := fixture.h
	_, key := fixture.key("mounted-key", nil)
	upstream := newVendor(t)
	upstream.mu.Lock()
	upstream.secrets = map[string]bool{"file-mounted-secret": true}
	upstream.mu.Unlock()
	directory := t.TempDir()
	secretPath := filepath.Join(directory, "credential")
	if err := os.WriteFile(secretPath, []byte("file-mounted-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	document := map[string]any{"providers": []any{map[string]any{"provider_id": fixture.provider, "credential_file": secretPath, "configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": upstream.URL + "/v1", "options": map[string]any{"limits": map[string]any{"requests_per_minute": 999}, "models": map[string]any{vendorModel: map[string]any{"region": "unpublished-region", "deployment": vendorModel}}}}}}}
	data, _ := json.Marshal(document)
	configPath := filepath.Join(directory, "connectors.json")
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	mounted, err := providers.LoadMounted(configPath, &policy)
	if err != nil {
		t.Fatal(err)
	}
	installation, err := database.Installation(t.Context(), h.Pool)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := secrets.DecodeKey(h.AuthHex)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := runtime.NewManager(h.Pool, installation, secrets.NewAuthKey(auth, installation), nil, log)
	manager.Mounted = mounted
	if err = manager.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	published := manager.Release().Snapshot.Providers[fixture.provider]
	if published.Limits != nil {
		t.Fatalf("mounted limits replaced published quota ownership: %+v", published.Limits)
	}
	var metadata runtime.ModelMetadata
	if err = json.Unmarshal(published.Models[vendorModel], &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Region != nil || metadata.Deployment == nil || *metadata.Deployment != vendorModel {
		t.Fatalf("mounted facts replaced published policy evidence: %+v", metadata)
	}
	serving := gateway.New(manager, &policy, gateway.Config{MaxInFlight: 4, MaxBodyBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxEventBytes: 65536}, log)
	serving.Admission = h.Gateway.Admission
	mux := http.NewServeMux()
	serving.Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	path, body, _ := parityRequest(openai.FamilyChat, false)
	payload, _ := json.Marshal(body)
	request, _ := http.NewRequestWithContext(t.Context(), "POST", server.URL+path, bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+key)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	content, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || upstream.chats.Load() != 1 {
		t.Fatalf("mounted dispatch: %d %s", response.StatusCode, content)
	}
	// Draft readiness follows the usable pool, independently of a revoked default.
	slots := h.want(fixture.owner, "GET", fixture.path+"/credential-slots", nil, nil, 200)
	h.want(fixture.owner, "PUT", fixture.path+"/credential-slots/"+access.NewID(), map[string]any{"slot": map[string]any{"name": "standby"}, "credential": vendorSecret}, withMatch(slots, map[string]string{"Idempotency-Key": "standby"}), 200)
	current := h.want(fixture.owner, "GET", fixture.path, nil, nil, 200)
	h.want(fixture.owner, "POST", fixture.path+"/credentials/"+current["draft_credential_id"].(string)+"/revoke", nil, withMatch(current, map[string]string{"Idempotency-Key": "revoke-default"}), 200)
	current = h.want(fixture.owner, "GET", fixture.path, nil, nil, 200)
	if current["connector_ready"] != true {
		t.Fatal("revoked default hid an enabled named credential from draft readiness")
	}
	if err = os.Chmod(secretPath, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = providers.LoadMounted(configPath, &policy); err == nil {
		t.Fatal("publicly readable mounted credential accepted")
	}
}
