package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

type stubAzureCredential struct {
	calls  atomic.Int32
	token  string
	expiry time.Time
	err    error
}

func (c *stubAzureCredential) GetToken(ctx context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	c.calls.Add(1)
	if len(opts.Scopes) != 1 || opts.Scopes[0] != azureScope {
		return azcore.AccessToken{}, errors.New("unexpected scope")
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
		if e == nil && len(sensitive) == 0 {
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

func TestAzureDeploymentAddressesRequireAConfiguredDeployment(t *testing.T) {
	c := Config{Kind: "azure_openai", Endpoint: "https://resource.example", Deployment: "prod", APIVersion: "2025-01-01-preview",
		Bindings: map[string]Binding{"logical": {Deployment: "bound"}},
		Models:   map[string]json.RawMessage{"declared": json.RawMessage(`{"deployment":"mapped"}`), "facts": json.RawMessage(`{"region":"eastus"}`)}}
	for model, deployment := range map[string]string{"prod": "prod", "logical": "bound", "declared": "mapped", "mapped": "mapped"} {
		if u, err := c.URL(openai.FamilyChat, model, false); err != nil || !strings.Contains(u, "/openai/deployments/"+deployment+"/") {
			t.Errorf("URL %s: %s %v", model, u, err)
		}
		if u, err := c.MediaURL("images/generations", model, nil); err != nil || !strings.Contains(u, "/openai/deployments/"+deployment+"/") {
			t.Errorf("MediaURL %s: %s %v", model, u, err)
		}
	}
	for _, model := range []string{"unknown", "facts"} {
		if u, err := c.URL(openai.FamilyChat, model, false); err == nil {
			t.Errorf("URL %s addressed an unconfigured deployment: %s", model, u)
		}
		if u, err := c.MediaURL("images/generations", model, nil); err == nil {
			t.Errorf("MediaURL %s addressed an unconfigured deployment: %s", model, u)
		}
	}
}
