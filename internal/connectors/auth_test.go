package connectors

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
)

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
	auth := NewAuth(&egress.Policy{})
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
	auth := NewAuth(&egress.Policy{})
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
	auth := NewAuth(&egress.Policy{})
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
