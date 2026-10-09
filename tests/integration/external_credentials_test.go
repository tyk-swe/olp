//go:build integration

package integration_test

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/secretstore"
	"github.com/tyk-swe/olp/internal/testutil"
)

func TestExternalCredentialAndWrappedMasterKeyRotationKeepPublishedVersionsPinned(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	var unavailable atomic.Bool
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/auth/jwt/login" {
			var input map[string]string
			_ = json.NewDecoder(r.Body).Decode(&input)
			if input["role"] != "olp" || input["jwt"] != "fixture.workload.jwt" {
				w.WriteHeader(403)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]string{"client_token": "fixture-vault-token"}})
			return
		}
		if r.Header.Get("X-Vault-Token") != "fixture-vault-token" || unavailable.Load() {
			w.WriteHeader(503)
			return
		}
		if r.URL.Path == "/v1/transit/decrypt/old" || r.URL.Path == "/v1/transit/decrypt/new" {
			key := strings.Repeat("ab", 32)
			if strings.HasSuffix(r.URL.Path, "/new") {
				key = strings.Repeat("ef", 32)
			}
			decoded, _ := hex.DecodeString(key)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"plaintext": base64.StdEncoding.EncodeToString(decoded)}})
			return
		}
		version := r.URL.Query().Get("version")
		value, n := vendorSecret, 1
		if version == "2" {
			value, n = vendorRotated, 2
		} else if version != "1" {
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]string{"api_key": value}, "metadata": map[string]any{"version": n, "destroyed": false, "deletion_time": ""}}})
	}))
	t.Cleanup(vault.Close)
	jwtFile := filepath.Join(t.TempDir(), "identity.jwt")
	if err := os.WriteFile(jwtFile, []byte("fixture.workload.jwt"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OLP_VAULT_ROLE", "olp")
	t.Setenv("OLP_VAULT_JWT_FILE", jwtFile)
	resolver := secretstore.New(*h.Server.Egress)
	h.Runtime.ExternalSecrets = resolver
	upstream := newVendor(t)
	upstream.accept(vendorRotated)
	reference := secretstore.Reference{Store: "vault", SecretID: vault.URL + "/v1/secret/data/provider", Version: "1", Field: "api_key"}
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "External credential provider", "configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": upstream.URL + "/v1"}, "credential_reference": reference, "model": vendorModel}, map[string]string{"Idempotency-Key": "external-provider"}, 201)
	providerID := provider["id"].(string)
	activateScopedProvider(h, owner, provider)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	published := h.Runtime.Release()
	var oldID string
	for _, slot := range published.Snapshot.Providers[providerID].Slots {
		if slot.CredentialID != nil {
			oldID = *slot.CredentialID
		}
	}
	value, _, err := h.Runtime.Secret(t.Context(), published, oldID)
	if err != nil || string(value) != vendorSecret {
		t.Fatal("initial pinned reference was not served")
	}
	current := h.want(owner, "GET", "/api/v1/providers/"+providerID, nil, nil, 200)
	headers := etagHeader(current)
	headers["Idempotency-Key"] = "rotate-external-credential"
	reference.Version = "2"
	rotated := h.want(owner, "POST", "/api/v1/providers/"+providerID+"/credentials", map[string]any{"credential_reference": reference}, headers, 201)
	if err = h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	value, _, err = h.Runtime.Secret(t.Context(), published, oldID)
	if err != nil || string(value) != vendorSecret || h.Runtime.Release().ID != published.ID {
		t.Fatal("rotation changed a published revision's pinned version")
	}
	var stored int
	if err = h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.secrets WHERE id IN (SELECT id FROM olp.provider_credentials WHERE provider_id=$1)", providerID).Scan(&stored); err != nil || stored != 0 {
		t.Fatal("reference stored upstream values in the installation")
	}
	document := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	destination := newAccessHarness(t)
	destinationOwner := destination.owner()
	destination.Runtime.ExternalSecrets = resolver
	exportedDocument := document["document"].(map[string]any)
	bindings := map[string]secretstore.Reference{}
	for _, entry := range exportedDocument["providers"].([]any) {
		for _, slot := range entry.(map[string]any)["slots"].([]any) {
			if ref, ok := slot.(map[string]any)["credential_ref"].(string); ok {
				bindings[ref] = reference
			}
		}
	}
	if len(bindings) == 0 {
		t.Fatal("export omitted logical reference")
	}
	promotion := map[string]any{"document": exportedDocument, "external_credential_bindings": bindings}
	destination.want(destinationOwner, "POST", "/api/v1/configuration/plan", promotion, nil, 200)
	destination.want(destinationOwner, "POST", "/api/v1/configuration/apply", promotion, map[string]string{"Idempotency-Key": "external-promotion"}, 200)
	var pinned []byte
	if err = destination.Pool.QueryRow(t.Context(), "SELECT external_reference FROM olp.provider_credentials").Scan(&pinned); err != nil {
		t.Fatal(err)
	}
	var imported secretstore.Reference
	if json.Unmarshal(pinned, &imported) != nil || imported != reference {
		t.Fatal("promotion changed pinned store metadata")
	}
	if err = destination.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.secrets WHERE id IN (SELECT id FROM olp.provider_credentials)").Scan(&stored); err != nil || stored != 0 {
		t.Fatal("promotion copied resolved secret values")
	}
	current = h.want(owner, "GET", "/api/v1/providers/"+providerID, nil, nil, 200)
	activateScopedProvider(h, owner, current)
	if err = h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	value, _, err = h.Runtime.Secret(t.Context(), h.Runtime.Release(), rotated["credential_id"].(string))
	if err != nil || string(value) != vendorRotated {
		t.Fatal("activation did not serve validated new version")
	}
	exported, _ := json.Marshal(document)
	if strings.Contains(string(exported), vendorSecret) || strings.Contains(string(exported), vendorRotated) {
		t.Fatal("export disclosed resolved credentials")
	}
	ringDocument := map[string]any{"active_version": 2, "keys": []any{
		map[string]any{"version": 1, "wrapped": secretstore.WrappedKey{Store: "vault", KeyID: vault.URL + "/v1/transit/keys/old", Ciphertext: "vault:v1:old"}},
		map[string]any{"version": 2, "wrapped": secretstore.WrappedKey{Store: "vault", KeyID: vault.URL + "/v1/transit/keys/new", Ciphertext: "vault:v1:new"}},
	}}
	ringFile := filepath.Join(t.TempDir(), "master.json")
	encoded, _ := json.Marshal(ringDocument)
	if err = os.WriteFile(ringFile, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.LoadRingWithUnwrapper(t.Context(), ringFile, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ring.Rotate(t.Context(), h.Pool, h.Server.Installation); err != nil {
		t.Fatal(err)
	}
	versions, err := ring.VerifyAll(t.Context(), h.Pool, h.Server.Installation)
	if err != nil || versions[1] != 0 || versions[2] == 0 {
		t.Fatalf("wrapped-key rotation verification: %v, %v", versions, err)
	}
	h.Server.Keys = ring
	authFile := filepath.Join(t.TempDir(), "auth.key")
	if err = os.WriteFile(authFile, []byte(h.AuthHex), 0600); err != nil {
		t.Fatal(err)
	}
	started := testutil.StartProcess(t, required(t, "OLP_TEST_BINARY"), "gateway", map[string]string{
		"OLP_DATABASE_URL": h.DBURL, "OLP_VALKEY_URL": required(t, "OLP_TEST_VALKEY_URL"), "OLP_AUTH_HMAC_KEY_FILE": authFile, "OLP_MASTER_KEY_FILE": ringFile,
		"OLP_LISTEN_ADDR": "127.0.0.1:0", "OLP_OBSERVABILITY_LISTEN_ADDR": "127.0.0.1:0", "OLP_PUBLIC_ORIGIN": "http://127.0.0.1:8080",
		"OLP_PROVIDER_EGRESS_ALLOW_CIDRS": "127.0.0.0/8", "OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS": "127.0.0.1", "OLP_VAULT_ROLE": "olp", "OLP_VAULT_JWT_FILE": jwtFile,
	})
	if started == nil {
		t.Fatal("wrapped ring did not start the gateway")
	}
	value, _, err = h.Runtime.Secret(t.Context(), published, oldID)
	if err != nil || string(value) != vendorSecret {
		t.Fatal("master-key rotation changed a pinned external version")
	}
	if rotated["credential_id"] == oldID {
		t.Fatal("rotation reused the old credential ID")
	}
	unavailable.Store(true)
	current = h.want(owner, "GET", "/api/v1/providers/"+providerID, nil, nil, 200)
	headers = etagHeader(current)
	headers["Idempotency-Key"] = "unavailable-reference"
	h.want(owner, "POST", "/api/v1/providers/"+providerID+"/credentials", map[string]any{"credential_reference": reference}, headers, 422)
}
