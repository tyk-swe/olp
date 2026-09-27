package connectors

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"
	"github.com/aws/aws-sdk-go-v2/service/sso"
)

func TestAuthenticationFailuresTellARejectedCredentialFromATransientOne(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	// authority answers every token or credential request with status.
	authority := func(t *testing.T, status int) string {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			fmt.Fprint(w, `{"error":"fixture_refusal","message":"fixture refusal"}`)
		}))
		t.Cleanup(server.Close)
		return server.URL
	}
	serviceAccount := func(t *testing.T, status int) []byte {
		secret, _ := json.Marshal(map[string]string{"type": "service_account", "client_email": "fixture@project.iam.gserviceaccount.com", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "token_uri": authority(t, status)})
		return secret
	}
	entra := func(a *Auth, err error) {
		a.azureFactory = func(string, []byte) (azcore.TokenCredential, error) { return &stubAzureCredential{err: err}, nil }
	}
	awsChain := func(t *testing.T, a *Auth, c Config, status int) {
		dir := t.TempDir()
		tokenFile := filepath.Join(dir, "sso-token.json")
		token, _ := json.Marshal(map[string]string{"accessToken": "fixture-sso-token", "expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
		if err := os.WriteFile(tokenFile, token, 0600); err != nil {
			t.Fatal(err)
		}
		client := sso.NewFromConfig(aws.Config{Region: "us-east-1", HTTPClient: a.client, Retryer: func() aws.Retryer { return aws.NopRetryer{} }}, func(o *sso.Options) { o.BaseEndpoint = aws.String(authority(t, status)) })
		provider := ssocreds.New(client, "123456789012", "fixture-role", "https://fixture.awsapps.com/start", func(o *ssocreds.Options) { o.CachedTokenFilepath = tokenFile })
		a.aws[cacheKey(c, nil)] = aws.NewCredentialsCache(provider)
	}
	vertex := Config{Kind: "vertex_ai", AuthMode: "service_account", CloudRegion: "global"}
	azure := Config{Kind: "azure_openai", AuthMode: "azure_client_secret"}
	entraSecret := []byte(`{"tenant_id":"tenant","client_id":"client","client_secret":"value"}`)
	chain := Config{Kind: "bedrock", AuthMode: "default_chain", CloudRegion: "us-east-1"}
	for _, test := range []struct {
		name     string
		rejected bool
		prepare  func(t *testing.T, a *Auth) (Config, []byte)
	}{
		{"unregistered auth mode", true, func(*testing.T, *Auth) (Config, []byte) {
			return Config{Kind: "openai", AuthMode: "unregistered"}, []byte("fixture-secret")
		}},
		{"empty API key", true, func(*testing.T, *Auth) (Config, []byte) {
			return Config{Kind: "openai", AuthMode: "api_key"}, nil
		}},
		{"malformed credential headers", true, func(*testing.T, *Auth) (Config, []byte) {
			return Config{Kind: "openai", AuthMode: "headers", CredentialHeaders: []string{"X-Custom"}}, []byte("not json")
		}},
		{"malformed service-account key", true, func(*testing.T, *Auth) (Config, []byte) {
			return vertex, []byte("not json")
		}},
		{"service-account key refused by its authority", true, func(t *testing.T, _ *Auth) (Config, []byte) {
			return vertex, serviceAccount(t, http.StatusBadRequest)
		}},
		{"Google token endpoint unavailable", false, func(t *testing.T, _ *Auth) (Config, []byte) {
			return vertex, serviceAccount(t, http.StatusServiceUnavailable)
		}},
		{"Google token endpoint rate limiting", false, func(t *testing.T, _ *Auth) (Config, []byte) {
			return vertex, serviceAccount(t, http.StatusTooManyRequests)
		}},
		{"malformed Entra client secret", true, func(*testing.T, *Auth) (Config, []byte) {
			return azure, []byte(`{"tenant_id":"tenant"}`)
		}},
		{"Entra client secret refused by its authority", true, func(_ *testing.T, a *Auth) (Config, []byte) {
			entra(a, &azidentity.AuthenticationFailedError{RawResponse: &http.Response{StatusCode: http.StatusUnauthorized}})
			return azure, entraSecret
		}},
		{"Entra authority unavailable", false, func(_ *testing.T, a *Auth) (Config, []byte) {
			entra(a, errors.New("authority unavailable"))
			return azure, entraSecret
		}},
		{"malformed AWS credential JSON", true, func(*testing.T, *Auth) (Config, []byte) {
			return Config{Kind: "bedrock", AuthMode: "static", CloudRegion: "us-east-1"}, []byte("not json")
		}},
		{"AWS credential chain refused by its authority", true, func(t *testing.T, a *Auth) (Config, []byte) {
			awsChain(t, a, chain, http.StatusForbidden)
			return chain, nil
		}},
		{"AWS credential chain unavailable", false, func(t *testing.T, a *Auth) (Config, []byte) {
			awsChain(t, a, chain, http.StatusServiceUnavailable)
			return chain, nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := NewAuth(localPolicy())
			// Test-only token endpoints: production keeps Google's public-only client.
			a.googleClient = a.client
			c, secret := test.prepare(t, a)
			req, _ := http.NewRequest("POST", "https://provider.example", nil)
			_, err := a.Apply(t.Context(), req, c, secret, nil)
			if !errors.Is(err, ErrAuthentication) || errors.Is(err, ErrCredentialRejected) != test.rejected {
				t.Fatalf("authentication failure %v: rejected=%t, want %t", err, errors.Is(err, ErrCredentialRejected), test.rejected)
			}
			if err.Error() != ErrAuthentication.Error() {
				t.Fatalf("authentication failure changed its detail: %q", err)
			}
			if req.Header.Get("Authorization") != "" || req.Header.Get("X-Custom") != "" {
				t.Fatalf("failed authentication authorized the request: %v", req.Header)
			}
		})
	}
}

func TestOnlyStoredCredentialModesRequireASecret(t *testing.T) {
	for mode, required := range map[string]bool{
		"none": false, "adc": false, "azure_default": false, "default_chain": false,
		"api_key": true, "headers": true, "service_account": true, "azure_client_secret": true, "static": true,
		"unregistered": true,
	} {
		if SecretRequired(mode) != required {
			t.Errorf("%s: secret required=%t, want %t", mode, !required, required)
		}
	}
}
