package connectors

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"

	"github.com/tyk-swe/olp/internal/egress"
)

type environment map[string]string

func (e environment) lookup(name string) string { return e[name] }

func unset(string) string { return "" }

const credential = "sk-fixture-credential-0123456789"

func upstreamRequest(t *testing.T, target string) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodPost, target, strings.NewReader(`{"input":"hello"}`))
}

func sent(r *http.Request) string {
	var out strings.Builder
	out.WriteString(r.URL.String())
	if err := r.Header.Write(&out); err != nil {
		panic(err)
	}
	return out.String()
}

func TestApplySendsTheCredentialInOneHeaderAndReportsItForRedaction(t *testing.T) {
	auth := NewAuth(&egress.Policy{}, unset)
	cases := map[string]struct {
		config Config
		secret string
		hidden []string
	}{
		"an API key":           {Config{Kind: "openai", AuthMode: "api_key"}, credential, []string{credential}},
		"an Anthropic API key": {Config{Kind: "anthropic", AuthMode: "api_key"}, credential, []string{credential}},
		"a Gemini API key":     {Config{Kind: "gemini", AuthMode: "api_key"}, credential, []string{credential}},
		"an Azure API key":     {Config{Kind: "azure_openai", AuthMode: "api_key"}, credential, []string{credential}},
		"a caller's API key":   {Config{Kind: "openai", AuthMode: "api_key", CredentialSource: "caller"}, credential, []string{credential}},
		"declared headers":     {Config{Kind: "openai", AuthMode: "headers", CredentialHeaders: []string{"X-Tenant-Key", "X-Tenant"}}, `{"X-Tenant-Key":"` + credential + `","X-Tenant":"tenant-7f3a"}`, []string{credential, "tenant-7f3a"}},
		"static AWS keys":      {Config{Kind: "bedrock", AuthMode: "static", CloudRegion: "us-east-1"}, `{"access_key_id":"AKIDFIXTURE000000001","secret_access_key":"` + credential + `"}`, []string{credential, "AKIDFIXTURE000000001", "Signature="}},
		"temporary AWS keys":   {Config{Kind: "bedrock", AuthMode: "static", CloudRegion: "us-east-1"}, `{"access_key_id":"AKIDFIXTURE000000001","secret_access_key":"` + credential + `","session_token":"session-token-fixture-value"}`, []string{credential, "session-token-fixture-value"}},
		"no credential at all": {Config{Kind: "openai", AuthMode: "none"}, "", nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			request := upstreamRequest(t, "https://upstream.example/v1/responses?mode=fast")
			sensitive, err := auth.Apply(t.Context(), request, c.config, []byte(c.secret), []byte(`{"input":"hello"}`))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(request.URL.String(), credential) {
				t.Fatalf("the credential was put in the URL: %s", request.URL)
			}
			carriers := 0
			for _, values := range request.Header {
				if strings.Contains(strings.Join(values, "\n"), credential) {
					carriers++
				}
			}
			if secretSent := strings.Contains(c.secret, credential) && c.config.AuthMode != "static"; secretSent != (carriers == 1) {
				t.Fatalf("the credential is in %d headers: %v", carriers, request.Header)
			}
			if c.config.AuthMode != "none" && request.Header.Get("Authorization") == "" && carriers == 0 {
				t.Fatalf("the request carries no authorization: %v", request.Header)
			}
			if _, signature, signed := strings.Cut(request.Header.Get("Authorization"), "Signature="); signed && strings.Contains(sensitive.Redact("upstream refused "+signature), signature) {
				t.Fatalf("the request signature survives redaction on its own: %s", signature)
			}
			redacted := sensitive.Redact(sent(request))
			for _, hidden := range c.hidden {
				if strings.Contains(redacted, hidden) {
					t.Fatalf("%q survives redaction of what was sent: %s", hidden, redacted)
				}
			}
		})
	}
}

func TestApplyRefusesCredentialsItCannotSendSafely(t *testing.T) {
	auth := NewAuth(&egress.Policy{}, unset)
	aws := Config{Kind: "bedrock", AuthMode: "static", CloudRegion: "us-east-1"}
	cases := map[string]struct {
		config Config
		secret string
	}{
		"an empty API key":                      {Config{Kind: "openai", AuthMode: "api_key"}, ""},
		"an API key that ends its header":       {Config{Kind: "openai", AuthMode: "api_key"}, credential + "\r\nX-Forged: 1"},
		"an API key with a NUL":                 {Config{Kind: "anthropic", AuthMode: "api_key"}, credential + "\x00"},
		"an auth mode nobody implements":        {Config{Kind: "openai", AuthMode: "telepathy"}, credential},
		"no auth mode":                          {Config{Kind: "openai"}, credential},
		"headers missing a declared name":       {Config{Kind: "openai", AuthMode: "headers", CredentialHeaders: []string{"X-Tenant-Key"}}, `{"X-Other":"x"}`},
		"AWS keys with an unknown field":        {aws, `{"access_key_id":"AKIDFIXTURE000000001","secret_access_key":"` + credential + `","role":"admin"}`},
		"AWS keys followed by more data":        {aws, `{"access_key_id":"AKIDFIXTURE000000001","secret_access_key":"` + credential + `"}{}`},
		"an AWS key too short to be one":        {aws, `{"access_key_id":"AKID","secret_access_key":"` + credential + `"}`},
		"an AWS key with a space in it":         {aws, `{"access_key_id":"AKIDFIXTURE 00000001","secret_access_key":"` + credential + `"}`},
		"a caller using the gateway's AWS role": {Config{Kind: "bedrock", AuthMode: "default_chain", CloudRegion: "us-east-1", CredentialSource: "caller"}, ""},
		"a caller using the gateway's identity": {Config{Kind: "azure_openai", AuthMode: "azure_default", CredentialSource: "caller"}, ""},
		"a caller credential on a plugin":       {Config{Kind: KindPlugin, AuthMode: "api_key", CredentialSource: "caller"}, credential},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			request := upstreamRequest(t, "https://upstream.example/v1/responses")
			_, err := auth.Apply(t.Context(), request, c.config, []byte(c.secret), nil)
			if !errors.Is(err, ErrCredentialRejected) || !errors.Is(err, ErrAuthentication) {
				t.Fatalf("got %v, want a rejected credential", err)
			}
			if strings.Contains(sent(request), credential) || request.Header.Get("Authorization") != "" || request.Header.Get("X-Forged") != "" {
				t.Fatalf("the refused request still carries a credential: %v", request.Header)
			}
		})
	}
}

func TestApplyKeepsVertexCredentialsOnTheConfiguredGoogleEndpoint(t *testing.T) {
	auth := NewAuth(&egress.Policy{}, unset)
	config := Config{Kind: "vertex_ai", AuthMode: "service_account", CloudRegion: "us-central1", CloudProject: "fixture-project"}
	google, err := url.Parse(DefaultEndpoint(config.Kind, config.CloudRegion, config.CloudProject))
	if err != nil {
		t.Fatal(err)
	}
	pinned := config
	pinned.Network = &egress.ConnectionOptions{TrustRootsPEM: "-----BEGIN CERTIFICATE-----"}
	elsewhere := config
	elsewhere.CloudRegion = "europe-west4"
	cases := map[string]struct {
		config  Config
		target  string
		refused bool
	}{
		"the configured endpoint":          {config, "https://" + google.Host + "/v1/projects/fixture-project", false},
		"the endpoint on its default port": {config, "https://" + google.Host + ":443/v1/projects/fixture-project", false},
		"another host":                     {config, "https://upstream.example/v1/projects/fixture-project", true},
		"a host that only starts like it":  {config, "https://" + google.Host + ".upstream.example/v1", true},
		"another location's endpoint":      {elsewhere, "https://" + google.Host + "/v1", true},
		"plain HTTP":                       {config, "http://" + google.Host + "/v1", true},
		"another port":                     {config, "https://" + google.Host + ":8443/v1", true},
		"user information in the URL":      {config, "https://user@" + google.Host + "/v1", true},
		"operator trust roots":             {pinned, "https://" + google.Host + "/v1", true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			request := upstreamRequest(t, c.target)
			_, err := auth.Apply(t.Context(), request, c.config, []byte(`not a service account`), nil)
			if err == nil {
				t.Fatal("a malformed service account was accepted")
			}
			if refused := !errors.Is(err, ErrCredentialRejected); refused != c.refused {
				t.Fatalf("got %v, destination refused %t, want %t", err, refused, c.refused)
			}
		})
	}
}

const issuedToken = "issued-access-token-fixture"

type tokenAuthority struct {
	*httptest.Server
	exchanges *atomic.Int32
}

func newTokenAuthority(t *testing.T, status int) tokenAuthority {
	t.Helper()
	exchanges := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		exchanges.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"access_token":"` + issuedToken + `","token_type":"Bearer","expires_in":3600}`))
		}
	}))
	t.Cleanup(server.Close)
	return tokenAuthority{Server: server, exchanges: exchanges}
}

var serviceAccountKey = sync.OnceValue(func() string {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
})

func serviceAccount(t *testing.T, tokenURI string) []byte {
	t.Helper()
	account, err := json.Marshal(map[string]string{
		"type": "service_account", "project_id": vertexProject, "private_key_id": "fixture-key", "private_key": serviceAccountKey(),
		"client_email": "fixture@" + vertexProject + ".iam.gserviceaccount.com", "client_id": "1", "token_uri": tokenURI,
	})
	if err != nil {
		t.Fatal(err)
	}
	return account
}

type countedCredential struct{ exchanges *atomic.Int32 }

func (c countedCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	c.exchanges.Add(1)
	return azcore.AccessToken{Token: issuedToken, ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func TestApplyExchangesAStoredCredentialForOneTokenItReuses(t *testing.T) {
	t.Parallel()
	watsonx := validConfigs()["watsonx"]
	google := changed(vertex("vertex-gemini"), func(c *Config) { c.AuthMode = AuthServiceAccount })
	microsoft := changed(azure("azure-legacy-chat"), func(c *Config) { c.AuthMode = AuthAzureClientSecret })
	cases := map[string]struct {
		config Config
		target string
		secret func(authority tokenAuthority) []byte
		wire   func(auth *Auth, authority tokenAuthority)
	}{
		"an IBM Cloud API key": {
			watsonx, watsonx.Endpoint + "/ml/v1/text/chat",
			func(tokenAuthority) []byte { return []byte(credential) },
			func(auth *Auth, authority tokenAuthority) {
				auth.iamEndpoint, auth.publicClient = authority.URL, authority.Client()
			},
		},
		"a Google service account": {
			google, vertexLocation + "/publishers/google/models/gemini:generateContent",
			func(authority tokenAuthority) []byte { return serviceAccount(t, authority.URL) },
			func(auth *Auth, authority tokenAuthority) { auth.publicClient = authority.Client() },
		},
		"a Microsoft Entra client secret": {
			microsoft, microsoft.Endpoint + "/openai/deployments/dep-1/chat/completions",
			func(tokenAuthority) []byte { return []byte(credential) },
			func(auth *Auth, authority tokenAuthority) {
				auth.azureFactory = func(string, []byte) (azcore.TokenCredential, error) {
					return countedCredential{authority.exchanges}, nil
				}
			},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			authority := newTokenAuthority(t, http.StatusOK)
			auth := NewAuth(&egress.Policy{}, unset)
			c.wire(auth, authority)
			secret := c.secret(authority)
			for range 2 {
				request := upstreamRequest(t, c.target)
				sensitive, err := auth.Apply(t.Context(), request, c.config, secret, nil)
				if err != nil {
					t.Fatal(err)
				}
				if got := request.Header.Get("Authorization"); got != "Bearer "+issuedToken {
					t.Fatalf("Authorization %q, want the issued token", got)
				}
				if !sensitive.Contains(issuedToken) {
					t.Fatal("the issued token is not reported for redaction")
				}
			}
			if got := authority.exchanges.Load(); got != 1 {
				t.Fatalf("%d token exchanges for two requests, want 1", got)
			}
		})
	}
}

func TestApplyTellsARefusedCredentialFromAnAuthorityThatMayRecover(t *testing.T) {
	t.Parallel()
	watsonx := validConfigs()["watsonx"]
	cases := map[string]struct {
		status   int
		rejected bool
	}{
		"the authority refuses the key": {http.StatusUnauthorized, true},
		"the authority is failing":      {http.StatusServiceUnavailable, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			authority := newTokenAuthority(t, c.status)
			auth := NewAuth(&egress.Policy{}, unset)
			auth.iamEndpoint, auth.publicClient = authority.URL, authority.Client()
			request := upstreamRequest(t, watsonx.Endpoint+"/ml/v1/text/chat")
			_, err := auth.Apply(t.Context(), request, watsonx, []byte(credential), nil)
			if !errors.Is(err, ErrAuthentication) || errors.Is(err, ErrCredentialRejected) != c.rejected {
				t.Fatalf("got %v, credential rejected %t, want %t", err, errors.Is(err, ErrCredentialRejected), c.rejected)
			}
			if request.Header.Get("Authorization") != "" {
				t.Fatalf("the refused request still carries a credential: %v", request.Header)
			}
		})
	}
}

func TestApplicationDefaultCredentialsComeFromTheFileTheEnvironmentNames(t *testing.T) {
	t.Parallel()
	authority := newTokenAuthority(t, http.StatusOK)
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, serviceAccount(t, authority.URL), 0o600); err != nil {
		t.Fatal(err)
	}
	auth := NewAuth(&egress.Policy{}, environment{"GOOGLE_APPLICATION_CREDENTIALS": path}.lookup)
	auth.publicClient = authority.Client()
	request := upstreamRequest(t, vertexLocation+"/publishers/google/models/gemini:generateContent")
	if _, err := auth.Apply(t.Context(), request, vertex("vertex-gemini"), nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer "+issuedToken {
		t.Fatalf("Authorization %q, want the token issued for the named file", got)
	}
}

func TestIdentityRequestsReachOnlyTheEndpointsTheEnvironmentNames(t *testing.T) {
	t.Parallel()
	loopback := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	identity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	t.Cleanup(identity.Close)
	clients := map[string]struct {
		variable string
		client   func(getenv func(string) string) *http.Client
	}{
		"a Microsoft managed identity endpoint": {"IDENTITY_ENDPOINT", func(getenv func(string) string) *http.Client {
			return NewAuth(loopback, getenv).azureClient
		}},
		"an AWS container credentials endpoint": {"AWS_CONTAINER_CREDENTIALS_FULL_URI", func(getenv func(string) string) *http.Client {
			return NewBillingAuth(loopback, &http.Client{Transport: http.DefaultTransport}, getenv).client
		}},
	}
	for name, c := range clients {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if response, err := c.client(unset).Get(identity.URL); err == nil {
				response.Body.Close()
				t.Fatal("an endpoint the environment does not name was reached")
			}
			response, err := c.client(environment{c.variable: identity.URL}.lookup).Get(identity.URL)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusNoContent {
				t.Fatalf("status %d from the named endpoint", response.StatusCode)
			}
		})
	}
}
