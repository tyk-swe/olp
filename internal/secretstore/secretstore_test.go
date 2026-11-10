package secretstore

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
)

const (
	vaultJWT   = "workload.jwt.value"
	vaultToken = "s.client-token"
	vaultRole  = "olp"
	liveSecret = `{"data":{"data":{"api_key":"sk-pinned","count":3},"metadata":{"version":REQUESTED,"destroyed":false,"deletion_time":""}}}`
)

type answers struct {
	status int
	secret string
	plain  string
}

type vault struct {
	server   *httptest.Server
	logins   atomic.Int32
	requests atomic.Int32
}

func newVault(t *testing.T, a answers) *vault {
	t.Helper()
	v := &vault{}
	authorized := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("X-Vault-Token") == vaultToken {
			return true
		}
		http.Error(w, "permission denied for "+vaultJWT, http.StatusForbidden)
		return false
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/jwt/login", func(w http.ResponseWriter, r *http.Request) {
		v.logins.Add(1)
		var login struct{ Role, JWT string }
		if json.NewDecoder(r.Body).Decode(&login) != nil || login.Role != vaultRole || login.JWT != vaultJWT {
			http.Error(w, "permission denied for "+vaultJWT, http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"auth":{"client_token":"` + vaultToken + `"}}`))
	})
	mux.HandleFunc("GET /v1/secret/data/provider", func(w http.ResponseWriter, r *http.Request) {
		if authorized(w, r) {
			w.WriteHeader(cmp.Or(a.status, http.StatusOK))
			_, _ = w.Write([]byte(strings.ReplaceAll(a.secret, "REQUESTED", r.URL.Query().Get("version"))))
		}
	})
	mux.HandleFunc("POST /v1/transit/decrypt/master", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Ciphertext string }
		if authorized(w, r) && json.NewDecoder(r.Body).Decode(&body) == nil && body.Ciphertext == "vault:v1:sealed" {
			_, _ = w.Write([]byte(`{"data":{"plaintext":"` + a.plain + `"}}`))
		}
	})
	v.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.requests.Add(1)
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(v.server.Close)
	return v
}

func (v *vault) reference(field string) Reference {
	return Reference{Store: "vault", SecretID: v.server.URL + "/v1/secret/data/provider", Version: "3", Field: field}
}

func jwtFile(t *testing.T, jwt string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jwt")
	if err := os.WriteFile(path, []byte(jwt+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func resolver(origin, jwt string) *Resolver {
	policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	environment := map[string]string{"OLP_VAULT_ADDR": origin, "OLP_VAULT_ROLE": vaultRole, "OLP_VAULT_JWT_FILE": jwt}
	return New(policy, func(name string) string { return environment[name] })
}

func TestResolveReturnsOnlyThePinnedLiveVaultVersion(t *testing.T) {
	resolve := func(a answers, field string) ([]byte, error) {
		v := newVault(t, a)
		return resolver(v.server.URL, jwtFile(t, vaultJWT, 0600)).Resolve(t.Context(), v.reference(field))
	}
	if secret, err := resolve(answers{secret: liveSecret}, "api_key"); err != nil || string(secret) != "sk-pinned" {
		t.Fatalf("got %q, %v", secret, err)
	}
	for name, c := range map[string]struct {
		answers
		field string
	}{
		"another version answered": {answers{secret: strings.ReplaceAll(liveSecret, "REQUESTED", "4")}, "api_key"},
		"a destroyed version":      {answers{secret: strings.ReplaceAll(liveSecret, `"destroyed":false`, `"destroyed":true`)}, "api_key"},
		"a deleted version":        {answers{secret: strings.ReplaceAll(liveSecret, `"deletion_time":""`, `"deletion_time":"2026-01-01T00:00:00Z"`)}, "api_key"},
		"an error status":          {answers{secret: liveSecret, status: http.StatusNotFound}, "api_key"},
		"a missing field":          {answers{secret: liveSecret}, "token"},
		"a field that is not text": {answers{secret: liveSecret}, "count"},
		"an empty secret":          {answers{secret: strings.ReplaceAll(liveSecret, "sk-pinned", "")}, "api_key"},
		"an oversized secret":      {answers{secret: strings.ReplaceAll(liveSecret, "sk-pinned", strings.Repeat("s", maxSecret+1))}, "api_key"},
	} {
		if secret, err := resolve(c.answers, c.field); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: got %q, %v", name, secret, err)
		}
	}
}

func TestVaultIdentityGoesOnlyToTheOperatorsVault(t *testing.T) {
	trusted, other := newVault(t, answers{secret: liveSecret}), newVault(t, answers{secret: liveSecret})
	secret, err := resolver(trusted.server.URL, jwtFile(t, vaultJWT, 0600)).Resolve(t.Context(), other.reference("api_key"))
	if !errors.Is(err, ErrUnavailable) || other.requests.Load() != 0 || trusted.requests.Load() != 0 {
		t.Errorf("got %q, %v after %d requests to the referenced vault and %d to the operator's", secret, err, other.requests.Load(), trusted.requests.Load())
	}
	for name, mode := range map[string]os.FileMode{"world readable": 0644, "world writable": 0602, "executable": 0700} {
		if secret, err := resolver(trusted.server.URL, jwtFile(t, vaultJWT, mode)).Resolve(t.Context(), trusted.reference("api_key")); !errors.Is(err, ErrUnavailable) {
			t.Errorf("a %s identity file: got %q, %v", name, secret, err)
		}
	}
	if trusted.logins.Load() != 0 {
		t.Errorf("an unsafe identity file was sent to the vault %d times", trusted.logins.Load())
	}
}

func TestVaultRefusalsCarryNoServiceResponse(t *testing.T) {
	v := newVault(t, answers{secret: liveSecret})
	_, err := resolver(v.server.URL, jwtFile(t, "another.jwt", 0600)).Resolve(t.Context(), v.reference("api_key"))
	if err != ErrUnavailable || v.logins.Load() != 1 {
		t.Errorf("got %v after %d logins, want only %v", err, v.logins.Load(), ErrUnavailable)
	}
}

func TestUnwrapAcceptsOnlyA256BitKey(t *testing.T) {
	unwrap := func(size int) ([]byte, error) {
		v := newVault(t, answers{plain: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", size)))})
		key := WrappedKey{Store: "vault", KeyID: v.server.URL + "/v1/transit/keys/master", Ciphertext: "vault:v1:sealed"}
		return resolver(v.server.URL, jwtFile(t, vaultJWT, 0400)).Unwrap(t.Context(), key)
	}
	if plain, err := unwrap(32); err != nil || string(plain) != strings.Repeat("k", 32) {
		t.Fatalf("got %d bytes, %v", len(plain), err)
	}
	for _, size := range []int{0, 16, 31, 33, 64} {
		if plain, err := unwrap(size); !errors.Is(err, ErrUnavailable) {
			t.Errorf("a %d byte key: got %d bytes, %v", size, len(plain), err)
		}
	}
}

func TestReferenceValidateRequiresAnImmutableVersion(t *testing.T) {
	const awsVersion = "0123456789abcdef0123456789abcdef"
	references := map[string]Reference{
		"aws":   {Store: "aws", SecretID: "arn:aws:secretsmanager:us-east-1:111122223333:secret:provider", Version: awsVersion, Region: "us-east-1"},
		"gcp":   {Store: "gcp", SecretID: "projects/olp/secrets/provider", Version: "7"},
		"azure": {Store: "azure", SecretID: "https://olp.vault.azure.net/secrets/provider", Version: awsVersion},
		"vault": {Store: "vault", SecretID: "https://vault.internal/v1/secret/data/provider", Version: "3", Field: "api_key"},
	}
	for name, reference := range references {
		if err := reference.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		for _, alias := range []string{"", "latest", "AWSCURRENT", "0", "-1"} {
			moving := reference
			moving.Version = alias
			if moving.Validate() == nil {
				t.Errorf("%s accepted the moving version %q", name, alias)
			}
		}
	}
	with := func(store string, change func(*Reference)) Reference {
		reference := references[store]
		change(&reference)
		return reference
	}
	for name, reference := range map[string]Reference{
		"an unknown store":                    with("vault", func(r *Reference) { r.Store = "onepassword" }),
		"aws without a region":                with("aws", func(r *Reference) { r.Region = "" }),
		"aws with a field":                    with("aws", func(r *Reference) { r.Field = "api_key" }),
		"gcp outside projects":                with("gcp", func(r *Reference) { r.SecretID = "folders/olp/secrets/provider" }),
		"gcp with a version in the name":      with("gcp", func(r *Reference) { r.SecretID += "/versions/latest" }),
		"gcp with a traversal":                with("gcp", func(r *Reference) { r.SecretID = "projects/../secrets/provider" }),
		"azure on another host":               with("azure", func(r *Reference) { r.SecretID = "https://olp.vault.azure.net.example.com/secrets/provider" }),
		"azure over plain HTTP":               with("azure", func(r *Reference) { r.SecretID = "http://olp.vault.azure.net/secrets/provider" }),
		"azure with credentials":              with("azure", func(r *Reference) { r.SecretID = "https://user:pass@olp.vault.azure.net/secrets/provider" }),
		"azure naming a key":                  with("azure", func(r *Reference) { r.SecretID = "https://olp.vault.azure.net/keys/provider" }),
		"azure with a version in the name":    with("azure", func(r *Reference) { r.SecretID += "/" + awsVersion }),
		"vault outside the API":               with("vault", func(r *Reference) { r.SecretID = "https://vault.internal/ui/secret/data/provider" }),
		"vault with a query":                  with("vault", func(r *Reference) { r.SecretID += "?version=1" }),
		"vault with a traversal":              with("vault", func(r *Reference) { r.SecretID = "https://vault.internal/v1/secret/data/../../sys/seal" }),
		"vault with an encoded separator":     with("vault", func(r *Reference) { r.SecretID = "https://vault.internal/v1/secret/data/a%2Fb" }),
		"vault with credentials":              with("vault", func(r *Reference) { r.SecretID = "https://token@vault.internal/v1/secret/data/provider" }),
		"vault without a field":               with("vault", func(r *Reference) { r.Field = "" }),
		"vault with a region":                 with("vault", func(r *Reference) { r.Region = "us-east-1" }),
		"a secret name with a line break":     with("aws", func(r *Reference) { r.SecretID += "\nX-Injected: 1" }),
		"a secret name with outer whitespace": with("aws", func(r *Reference) { r.SecretID = " " + r.SecretID }),
	} {
		if reference.Validate() == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestWrappedKeyValidateRefusesKeysOutsideTheirService(t *testing.T) {
	keys := map[string]WrappedKey{
		"aws":   {Store: "aws", KeyID: "alias/olp", Ciphertext: "c2VhbGVk", Region: "eu-west-1", Context: map[string]string{"installation": "olp"}},
		"gcp":   {Store: "gcp", KeyID: "projects/olp/locations/global/keyRings/olp/cryptoKeys/master", Ciphertext: "c2VhbGVk"},
		"azure": {Store: "azure", KeyID: "https://olp.vault.azure.net/keys/master/0123456789abcdef0123456789abcdef", Ciphertext: "c2VhbGVk"},
		"vault": {Store: "vault", KeyID: "https://vault.internal/v1/transit/keys/master", Ciphertext: "vault:v1:sealed"},
	}
	for name, key := range keys {
		if err := key.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	with := func(store string, change func(*WrappedKey)) WrappedKey {
		key := keys[store]
		change(&key)
		return key
	}
	for name, key := range map[string]WrappedKey{
		"an unknown service":         with("vault", func(k *WrappedKey) { k.Store = "local" }),
		"no ciphertext":              with("aws", func(k *WrappedKey) { k.Ciphertext = "" }),
		"aws without a region":       with("aws", func(k *WrappedKey) { k.Region = "" }),
		"an empty context value":     with("aws", func(k *WrappedKey) { k.Context = map[string]string{"installation": ""} }),
		"a context outside aws":      with("gcp", func(k *WrappedKey) { k.Context = map[string]string{"installation": "olp"} }),
		"a gcp key version":          with("gcp", func(k *WrappedKey) { k.KeyID += "/cryptoKeyVersions/1" }),
		"a gcp key with a traversal": with("gcp", func(k *WrappedKey) { k.KeyID = "projects/olp/locations/../keyRings/olp/cryptoKeys/master" }),
		"an unversioned azure key":   with("azure", func(k *WrappedKey) { k.KeyID = "https://olp.vault.azure.net/keys/master" }),
		"an azure secret":            with("azure", func(k *WrappedKey) { k.KeyID = strings.Replace(k.KeyID, "/keys/", "/secrets/", 1) }),
		"an azure key on another host": with("azure", func(k *WrappedKey) {
			k.KeyID = strings.Replace(k.KeyID, "olp.vault.azure.net", "vault.azure.net.example.com", 1)
		}),
		"a vault path that is not a key":      with("vault", func(k *WrappedKey) { k.KeyID = "https://vault.internal/v1/transit/decrypt/master" }),
		"a vault ciphertext without a prefix": with("vault", func(k *WrappedKey) { k.Ciphertext = "c2VhbGVk" }),
	} {
		if key.Validate() == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
