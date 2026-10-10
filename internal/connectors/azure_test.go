package connectors

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

type stubAzureCredential struct {
	calls     atomic.Int32
	token     string
	expiry    time.Time
	err       error
	wantScope string
}

func (c *stubAzureCredential) GetToken(ctx context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	c.calls.Add(1)
	want := c.wantScope
	if want == "" {
		want = cognitiveServicesScope
	}
	if len(opts.Scopes) != 1 || opts.Scopes[0] != want {
		return azcore.AccessToken{}, errors.New("unexpected scope " + strings.Join(opts.Scopes, ","))
	}
	if c.err != nil {
		return azcore.AccessToken{}, c.err
	}
	return azcore.AccessToken{Token: c.token, ExpiresOn: c.expiry}, nil
}

func TestAzureEntraBearerAndTokenCache(t *testing.T) {
	a := NewAuth(localPolicy())
	credential := &stubAzureCredential{token: "entra-token", expiry: time.Now().Add(10 * time.Second)}
	a.azureFactory = func(mode string, secret []byte) (azcore.TokenCredential, error) {
		if mode != "azure_client_secret" {
			return nil, errors.New("unexpected mode")
		}
		return credential, nil
	}
	cfg := Config{Kind: "azure_openai", AuthMode: "azure_client_secret"}
	secret := []byte(`{"tenant_id":"tenant","client_id":"client","client_secret":"value"}`)
	apply := func() error {
		req, _ := http.NewRequest("POST", "https://resource.example/openai/deployments/prod/responses?api-version=2025-01-01", nil)
		sensitive, e := a.Apply(context.Background(), req, cfg, secret, nil)
		if e == nil && (req.Header.Get("Authorization") != "Bearer entra-token" || req.Header.Get("Api-Key") != "") {
			t.Fatalf("azure apply headers: %v", req.Header)
		}
		if e == nil && !sensitive.Contains("entra-token") {
			t.Fatal("token was not recorded as sensitive material")
		}
		return e
	}
	if err := apply(); err != nil {
		t.Fatal(err)
	}
	if err := apply(); err != nil {
		t.Fatal(err)
	}
	if credential.calls.Load() != 2 {
		t.Fatalf("expiring token was reused past the refresh margin: %d", credential.calls.Load())
	}
	credential.expiry = time.Now().Add(time.Hour)
	if err := apply(); err != nil {
		t.Fatal(err)
	}
	if err := apply(); err != nil {
		t.Fatal(err)
	}
	if credential.calls.Load() != 3 {
		t.Fatalf("cached token was reacquired: %d", credential.calls.Load())
	}
	credential.err = errors.New("authority unavailable")
	rotated := []byte(`{"tenant_id":"other","client_id":"other","client_secret":"value"}`)
	secret = rotated
	if e := apply(); e != ErrAuthentication {
		t.Fatalf("token failure must not leak SDK detail: %v", e)
	}
}

func TestAzureClientSecretValidation(t *testing.T) {
	a := NewAuth(localPolicy())
	for _, secret := range [][]byte{
		[]byte(`{"tenant_id":"t","client_id":"c","client_secret":"s","extra":1}`),
		[]byte(`{"tenant_id":"t","client_id":"c"}`),
		[]byte(`{"tenant_id":"","client_id":"c","client_secret":"s"}`),
		[]byte(`{"tenant_id":"t","client_id":"c","client_secret":""}`),
		[]byte(`not json`),
		[]byte(strings.Repeat("x", 20000)),
	} {
		if _, err := a.newAzureCredential("azure_client_secret", secret); !errors.Is(err, ErrAuthentication) {
			t.Fatalf("accepted malformed secret: %v", err)
		}
	}
	if _, err := a.newAzureCredential("azure_client_secret", []byte(`{"tenant_id":"tenant","client_id":"client","client_secret":"value"}`)); err != nil {
		t.Fatalf("valid client secret rejected: %v", err)
	}
	if _, err := a.newAzureCredential("azure_default", nil); err != nil {
		t.Fatalf("default credential chain rejected: %v", err)
	}
	if SecretRequired("azure_default") {
		t.Fatal("azure_default must not require a stored credential")
	}
	if !SecretRequired("azure_client_secret") {
		t.Fatal("azure_client_secret must require a stored credential")
	}
}

func TestAzureIdentityHostBoundary(t *testing.T) {
	for _, host := range []string{
		"login.microsoftonline.com", "login.microsoftonline.us", "login.chinacloudapi.cn",
		"login.microsoftonline.eaglex.ic.gov", "login.microsoftonline.microsoft.scloud",
		"login.microsoft.com", "169.254.169.254",
	} {
		if !azureIdentityHost(host) {
			t.Fatalf("maintained identity host rejected: %s", host)
		}
	}
	for _, host := range []string{"example.com", "login.microsoftonline.com.evil.example", "", "169.254.169.253"} {
		if azureIdentityHost(host) {
			t.Fatalf("unrelated host admitted: %s", host)
		}
	}
	t.Setenv("IDENTITY_ENDPOINT", "http://identity.internal:8080/token")
	if !azureIdentityHost("identity.internal") {
		t.Fatal("environment-declared identity endpoint rejected")
	}
	if azureIdentityHost("identity.other") {
		t.Fatal("identity host broadened beyond the declared endpoint")
	}
}

func TestAzureStorageScopeAndCacheSegregation(t *testing.T) {
	a := NewAuth(localPolicy())
	storage := &stubAzureCredential{token: "storage-token", expiry: time.Now().Add(time.Hour), wantScope: "https://storage.azure.com/.default"}
	cognitive := &stubAzureCredential{token: "cognitive-token", expiry: time.Now().Add(time.Hour)}
	factories := 0
	a.azureFactory = func(mode string, secret []byte) (azcore.TokenCredential, error) {
		factories++
		if factories == 1 {
			return storage, nil
		}
		return cognitive, nil
	}
	secret := []byte(`{"tenant_id":"tenant","client_id":"client","client_secret":"value"}`)
	req, _ := http.NewRequest("PUT", "https://account.blob.core.windows.net/container/object", nil)
	if _, err := a.ApplyAzureStorage(context.Background(), req, "azure_client_secret", secret); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "Bearer storage-token" {
		t.Fatalf("storage authorization %q", req.Header.Get("Authorization"))
	}
	req2, _ := http.NewRequest("PUT", "https://account.blob.core.windows.net/container/other", nil)
	if _, err := a.ApplyAzureStorage(context.Background(), req2, "azure_client_secret", secret); err != nil {
		t.Fatal(err)
	}
	if factories != 1 || storage.calls.Load() != 1 {
		t.Fatalf("storage token was not cached: factories=%d calls=%d", factories, storage.calls.Load())
	}
	openaiReq, _ := http.NewRequest("POST", "https://resource.example/openai/deployments/prod/responses", nil)
	if _, err := a.Apply(context.Background(), openaiReq, Config{Kind: "azure_openai", AuthMode: "azure_client_secret"}, secret, nil); err != nil {
		t.Fatal(err)
	}
	if openaiReq.Header.Get("Authorization") != "Bearer cognitive-token" || factories != 2 {
		t.Fatalf("storage token leaked into OpenAI scope: %q factories=%d", openaiReq.Header.Get("Authorization"), factories)
	}
}
