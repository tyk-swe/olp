package secretstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	cloudauth "cloud.google.com/go/auth"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/tyk-swe/olp/internal/egress"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type googleIdentity struct{}

func (googleIdentity) Token(context.Context) (*cloudauth.Token, error) {
	return &cloudauth.Token{Value: "google-workload-token", Expiry: time.Now().Add(time.Hour)}, nil
}

type azureIdentity struct{}

func (azureIdentity) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "azure-workload-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func response(body any) *http.Response {
	data, _ := json.Marshal(body)
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(data)))}
}

func resolverFixture(t *testing.T, transport roundTripFunc) *Resolver {
	t.Helper()
	r := New(egress.Policy{})
	r.client.Transport = transport
	r.azure = azureIdentity{}
	r.google = make(chan credentialResult, 1)
	r.google <- credentialResult{credential: cloudauth.NewCredentials(&cloudauth.CredentialsOptions{TokenProvider: googleIdentity{}})}
	r.aws["us-east-1"] = aws.Config{Region: "us-east-1", Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider("fixture-identity", "fixture-signing-key", "fixture-session")), HTTPClient: r.client, RetryMaxAttempts: 1}
	jwtFile := filepath.Join(t.TempDir(), "workload.jwt")
	if err := os.WriteFile(jwtFile, []byte("fixture.workload.jwt"), 0600); err != nil {
		t.Fatal(err)
	}
	r.getenv = func(name string) string {
		return map[string]string{"OLP_VAULT_ROLE": "operator", "OLP_VAULT_JWT_FILE": jwtFile}[name]
	}
	return r
}

func checksum(value []byte) string {
	return strconv.FormatUint(uint64(crc32.Checksum(value, crc32.MakeTable(crc32.Castagnoli))), 10)
}

func TestEveryWrappedKeyServiceUsesWorkloadAuthorityAndChecksTheResult(t *testing.T) {
	plaintext := []byte(strings.Repeat("k", 32))
	encoded := base64.StdEncoding.EncodeToString(plaintext)
	keys := []WrappedKey{
		{Store: "aws", Region: "us-east-1", KeyID: "arn:aws:kms:us-east-1:123456789012:key/test", Ciphertext: base64.StdEncoding.EncodeToString([]byte("wrapped")), Context: map[string]string{"installation": "fixture"}},
		{Store: "gcp", KeyID: "projects/project/locations/global/keyRings/ring/cryptoKeys/key", Ciphertext: base64.StdEncoding.EncodeToString([]byte("wrapped"))},
		{Store: "azure", KeyID: "https://fixture.vault.azure.net/keys/key/0123456789abcdef0123456789abcdef", Ciphertext: base64.RawURLEncoding.EncodeToString([]byte{0xfb, 0xff})},
		{Store: "vault", KeyID: "https://vault.example/v1/transit/keys/key", Ciphertext: "vault:v1:wrapped"},
	}
	for _, key := range keys {
		t.Run(key.Store, func(t *testing.T) {
			calls := 0
			r := resolverFixture(t, func(request *http.Request) (*http.Response, error) {
				calls++
				var input map[string]any
				if request.Body != nil {
					_ = json.NewDecoder(request.Body).Decode(&input)
				}
				if strings.Contains(request.URL.Path, "/auth/jwt/login") {
					if input["jwt"] != "fixture.workload.jwt" || input["role"] != "operator" {
						t.Error("Vault did not exchange workload identity")
					}
					return response(map[string]any{"auth": map[string]string{"client_token": "vault-workload-token"}}), nil
				}
				switch key.Store {
				case "aws":
					if !strings.HasPrefix(request.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") || input["KeyId"] != key.KeyID || request.Header.Get("X-Amz-Security-Token") != "fixture-session" {
						t.Error("AWS identity or key binding absent")
					}
					return response(map[string]string{"Plaintext": encoded, "KeyId": key.KeyID}), nil
				case "gcp":
					if request.Header.Get("Authorization") != "Bearer google-workload-token" || !strings.HasSuffix(request.URL.Path, ":decrypt") || input["ciphertextCrc32c"] == nil {
						t.Error("Google authority/integrity binding absent")
					}
					return response(map[string]any{"plaintext": encoded, "plaintextCrc32c": checksum(plaintext), "verifiedCiphertextCrc32c": true}), nil
				case "azure":
					if request.Header.Get("Authorization") != "Bearer azure-workload-token" || input["alg"] != "RSA-OAEP-256" || input["value"] != key.Ciphertext {
						t.Error("Azure authority/algorithm absent")
					}
					return response(map[string]string{"value": base64.RawURLEncoding.EncodeToString(plaintext), "kid": key.KeyID}), nil
				case "vault":
					if request.Header.Get("X-Vault-Token") != "vault-workload-token" || request.URL.Path != "/v1/transit/decrypt/key" {
						t.Error("Vault authority/transit binding absent")
					}
					return response(map[string]any{"data": map[string]string{"plaintext": encoded}}), nil
				}
				return nil, errors.New("unexpected service")
			})
			got, err := r.Unwrap(t.Context(), key)
			if err != nil || string(got) != string(plaintext) || calls == 0 {
				t.Fatalf("unwrap: %v, calls %d", err, calls)
			}
		})
	}
}

func TestEverySecretStorePinsItsVersionAndRejectsChangedVersions(t *testing.T) {
	secret := "private-upstream-key"
	references := []Reference{
		{Store: "aws", Region: "us-east-1", SecretID: "provider-key", Version: "0123456789abcdef0123456789abcdef"},
		{Store: "gcp", SecretID: "projects/project/secrets/provider-key", Version: "7"},
		{Store: "azure", SecretID: "https://fixture.vault.azure.net/secrets/provider-key", Version: "0123456789abcdef0123456789abcdef"},
		{Store: "vault", SecretID: "https://vault.example/v1/secret/data/provider-key", Version: "7", Field: "api_key"},
	}
	for _, reference := range references {
		t.Run(reference.Store, func(t *testing.T) {
			mismatch := false
			r := resolverFixture(t, func(request *http.Request) (*http.Response, error) {
				if strings.Contains(request.URL.Path, "/auth/jwt/login") {
					return response(map[string]any{"auth": map[string]string{"client_token": "vault-workload-token"}}), nil
				}
				version := reference.Version
				if mismatch {
					version = "different"
				}
				switch reference.Store {
				case "aws":
					var input map[string]string
					_ = json.NewDecoder(request.Body).Decode(&input)
					if input["VersionId"] != reference.Version || input["SecretId"] != reference.SecretID {
						t.Error("AWS version not pinned")
					}
					return response(map[string]string{"SecretString": secret, "VersionId": version}), nil
				case "gcp":
					if request.URL.Path != "/v1/"+reference.SecretID+"/versions/7:access" {
						t.Error("Google version not pinned")
					}
					return response(map[string]any{"name": reference.SecretID + "/versions/" + version, "payload": map[string]string{"data": base64.StdEncoding.EncodeToString([]byte(secret)), "dataCrc32c": checksum([]byte(secret))}}), nil
				case "azure":
					if !strings.HasSuffix(request.URL.Path, "/"+reference.Version) {
						t.Error("Azure version not pinned")
					}
					return response(map[string]string{"id": reference.SecretID + "/" + version, "value": secret}), nil
				case "vault":
					if request.URL.Query().Get("version") != "7" {
						t.Error("Vault version not pinned")
					}
					v := 7
					if mismatch {
						v = 8
					}
					return response(map[string]any{"data": map[string]any{"data": map[string]string{"api_key": secret}, "metadata": map[string]any{"version": v, "destroyed": false, "deletion_time": ""}}}), nil
				}
				return nil, ErrUnavailable
			})
			got, err := r.Resolve(t.Context(), reference)
			if err != nil || string(got) != secret {
				t.Fatalf("resolve: %v", err)
			}
			mismatch = true
			got, err = r.Resolve(t.Context(), reference)
			if !errors.Is(err, ErrUnavailable) || got != nil || strings.Contains(err.Error(), secret) {
				t.Fatal("changed version was accepted or disclosed")
			}
		})
	}
}

func TestReferencesRefuseAliasesAndUnsafeResourceShapes(t *testing.T) {
	for _, reference := range []Reference{
		{Store: "gcp", SecretID: "projects/p/secrets/s", Version: "latest"},
		{Store: "vault", SecretID: "https://vault.example/v1/secret/data/key", Version: "0", Field: "key"},
		{Store: "azure", SecretID: "https://attacker.example/secrets/key", Version: strings.Repeat("a", 32)},
		{Store: "aws", SecretID: "secret", Region: "us-east-1", Version: "AWSCURRENT"},
	} {
		if reference.Validate() == nil {
			t.Errorf("unsafe reference accepted: %s", reference.Store)
		}
	}
}
