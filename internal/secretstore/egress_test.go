package secretstore

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
)

func TestVaultStoreDestinationsUseProviderEgressAndRefuseRedirects(t *testing.T) {
	var calls, redirected atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, target.URL+"/collect", http.StatusTemporaryRedirect)
	}))
	defer store.Close()
	file := filepath.Join(t.TempDir(), "identity.jwt")
	if err := os.WriteFile(file, []byte("fixture.identity.jwt"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OLP_VAULT_ROLE", "operator")
	t.Setenv("OLP_VAULT_JWT_FILE", file)
	t.Setenv("OLP_VAULT_ADDR", store.URL)
	reference := Reference{Store: "vault", SecretID: store.URL + "/v1/secret/data/provider", Version: "1", Field: "api_key"}
	if _, err := New(egress.Policy{}).Resolve(t.Context(), reference); err == nil || calls.Load() != 0 {
		t.Fatal("store bypassed private-address/plaintext egress policy")
	}
	policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	if _, err := New(policy).Resolve(t.Context(), reference); err == nil || calls.Load() != 1 || redirected.Load() != 0 {
		t.Fatal("workload identity followed a store redirect")
	}
}
