package connectors

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// recordingSigner answers every signing hook with its headers or failure,
// recording what it was asked to sign.
type recordingSigner struct {
	headers   map[string]string
	err       error
	digests   []string
	providers []abi.Provider
	requests  []abi.SignRequest
	secrets   [][]string
}

func (s *recordingSigner) Sign(_ context.Context, digest string, provider abi.Provider, request abi.SignRequest, secrets []string) (abi.SignResult, error) {
	s.digests, s.providers, s.requests, s.secrets = append(s.digests, digest), append(s.providers, provider), append(s.requests, request), append(s.secrets, secrets)
	return abi.SignResult{Headers: s.headers}, s.err
}

func signingManifest() abi.Manifest {
	manifest := pluginManifest()
	manifest.Profiles[0].Signing = true
	return manifest
}

func signingAuth(signer Signer) *Auth {
	auth := NewAuth(&egress.Policy{})
	auth.Signer = signer
	return auth
}

// The signing hook runs over the request hosting placed, with its final body,
// and the headers it returns are sent and redacted like the credential.
func TestPluginSigningHookSignsTheFinishedRequest(t *testing.T) {
	signer := &recordingSigner{headers: map[string]string{"X-Acme-Signature": "hmac-of-request", "x-acme-timestamp": "1759000000"}}
	req, _ := http.NewRequest(http.MethodPost, "https://api.acme.example/v2/chat/completions", nil)
	req.Header.Set("Content-Type", "application/json")
	body := []byte(`{"model":"acme-large"}`)
	sensitive, err := signingAuth(signer).Apply(context.Background(), req, pluginConfig(t, signingManifest()), []byte("secret"), body)
	if err != nil {
		t.Fatal(err)
	}
	want := abi.SignRequest{
		Profile: "acme-chat", Method: http.MethodPost, URL: "https://api.acme.example/v2/chat/completions?key=secret",
		Header: map[string][]string{"Authorization": {"Token secret"}, "Content-Type": {"application/json"}, "X-Acme-Client": {"olp"}, "X-Acme-Key": {"secret"}},
		Body:   body, Credential: "secret",
	}
	if len(signer.requests) != 1 || signer.digests[0] != pluginDigest || !reflect.DeepEqual(signer.requests[0], want) {
		t.Fatalf("signed %v %+v", signer.digests, signer.requests)
	}
	if !slices.Contains(signer.secrets[0], "secret") || !slices.Contains(signer.secrets[0], "Token secret") {
		t.Fatalf("the hook's output may reveal secrets: %q", signer.secrets[0])
	}
	if req.Header.Get("X-Acme-Signature") != "hmac-of-request" || req.Header.Get("X-Acme-Timestamp") != "1759000000" {
		t.Fatalf("signed headers %v", req.Header)
	}
	for _, signed := range []string{"hmac-of-request", "1759000000"} {
		if !slices.Contains(sensitive.Values(), signed) {
			t.Errorf("%q is not redacted: %q", signed, sensitive)
		}
	}
}

// The signing hook signs for the provider the request is for: the plugin
// receives its profile and option values as the call's provider.
func TestPluginSigningHookReceivesTheProvidersOptions(t *testing.T) {
	manifest := optionsManifest()
	manifest.Profiles[0].Signing = true
	c := pluginConfig(t, manifest)
	c.PluginOptions = map[string]string{"account": "acme", "region": "eu", "team": "search"}
	c.Endpoint = c.Plugin.Address(c.PluginOptions)
	endpoint, err := c.URL(openai.FamilyChat, "acme-large", false)
	if err != nil {
		t.Fatal(err)
	}
	signer := &recordingSigner{}
	req, _ := http.NewRequest(http.MethodPost, endpoint, nil)
	if _, err = signingAuth(signer).Apply(context.Background(), req, c, []byte("secret"), nil); err != nil {
		t.Fatal(err)
	}
	want := abi.Provider{Profile: "acme-chat", Options: map[string]string{"account": "acme", "region": "eu", "team": "search"}}
	if len(signer.providers) != 1 || !reflect.DeepEqual(signer.providers[0], want) {
		t.Fatalf("signed for %+v", signer.providers)
	}
}

func TestASigningProfileNeedNotPlaceTheCredential(t *testing.T) {
	manifest := signingManifest()
	manifest.Profiles[0].Hosting = abi.Hosting{Address: "https://api.acme.example/v2", Headers: map[string]string{"X-Acme-Client": "olp"}}
	if err := ValidatePluginProfile(manifest.Profiles[0]); err != nil {
		t.Fatal(err)
	}
	signer := &recordingSigner{headers: map[string]string{"X-Acme-Signature": "hmac-of-request"}}
	req, _ := http.NewRequest(http.MethodPost, "https://api.acme.example/v2/chat/completions", nil)
	if _, err := signingAuth(signer).Apply(context.Background(), req, pluginConfig(t, manifest), []byte("secret"), nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprint(req.Header, req.URL), "secret") || req.Header.Get("X-Acme-Signature") != "hmac-of-request" {
		t.Fatalf("sent %v %v", req.URL, req.Header)
	}
	if _, err := signingAuth(signer).Apply(context.Background(), req, pluginConfig(t, manifest), []byte("line\nbreak"), nil); !errors.Is(err, ErrCredentialRejected) || len(signer.requests) != 1 {
		t.Fatalf("signed with a credential OLP refuses: %v", err)
	}
}

// A profile that authenticates with a grant signs with the grant's access
// token, never the credential version's secret that holds it.
func TestAGrantProfileSignsWithTheAccessToken(t *testing.T) {
	manifest := grantManifest()
	manifest.Profiles[0].Signing = true
	c := pluginConfig(t, manifest)
	c.AuthMode, c.PluginOptions = AuthGrant, map[string]string{"region": "eu"}
	signer := &recordingSigner{headers: map[string]string{"X-Acme-Signature": "hmac-of-request"}}
	req, _ := http.NewRequest(http.MethodPost, "https://api.acme.example/v2/chat/completions", nil)
	if _, err := signingAuth(signer).Apply(context.Background(), req, c, grantCredential(t, "at-123", map[string]string{"account": "a", "project": "p"}), nil); err != nil {
		t.Fatal(err)
	}
	if len(signer.requests) != 1 || signer.requests[0].Credential != "at-123" || !slices.Contains(signer.secrets[0], "at-123") {
		t.Fatalf("signed %+v redacting %q", signer.requests, signer.secrets)
	}
}

func TestPluginProfilesWithoutSigningRunNoPluginCode(t *testing.T) {
	signer := &recordingSigner{}
	req, _ := http.NewRequest(http.MethodPost, "https://api.acme.example/v2/chat/completions", nil)
	if _, err := signingAuth(signer).Apply(context.Background(), req, pluginConfig(t, pluginManifest()), []byte("secret"), nil); err != nil || len(signer.requests) != 0 {
		t.Fatalf("ran a hook the profile does not declare: %v", err)
	}
}

// A signing hook that fails, or returns headers a signature can't add, fails
// the request before it is sent. Only a failure the plugin reports blames the
// credential; OLP's limits, a crashed plugin or a refused header don't.
func TestPluginSigningFailuresFailTheRequest(t *testing.T) {
	many := map[string]string{}
	for i := range maxSignedHeaders + 1 {
		many[fmt.Sprintf("X-Signature-%d", i)] = "v"
	}
	limited := errors.New("plugin_timed_out")
	reported := &abi.Error{Code: "credential_expired", Message: "The key expired."}
	crashed := &abi.Error{Code: abi.CodeInternal, Message: "The plugin panicked."}
	for name, tc := range map[string]struct {
		signer Signer
		want   error
	}{
		"no signer":         {nil, ErrSigningUnavailable},
		"hook past limits":  {&recordingSigner{err: limited}, ErrSigningUnavailable},
		"hook crashed":      {&recordingSigner{err: crashed}, ErrSigningUnavailable},
		"hook reported":     {&recordingSigner{err: reported}, ErrAuthentication},
		"hop-by-hop header": {&recordingSigner{headers: map[string]string{"Connection": "close"}}, ErrSigningUnavailable},
		"reserved header":   {&recordingSigner{headers: map[string]string{"Content-Type": "text/plain"}}, ErrSigningUnavailable},
		"semantic header":   {&recordingSigner{headers: map[string]string{"OpenAI-Beta": "assistants=v2"}}, ErrSigningUnavailable},
		"declared header":   {&recordingSigner{headers: map[string]string{"x-acme-client": "other"}}, ErrSigningUnavailable},
		"repeated header":   {&recordingSigner{headers: map[string]string{"X-Signature": "a", "x-signature": "b"}}, ErrSigningUnavailable},
		"invalid value":     {&recordingSigner{headers: map[string]string{"X-Signature": "a\r\nX-Injected: 1"}}, ErrSigningUnavailable},
		"too many headers":  {&recordingSigner{headers: many}, ErrSigningUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, "https://api.acme.example/v2/chat/completions", nil)
			_, err := signingAuth(tc.signer).Apply(context.Background(), req, pluginConfig(t, signingManifest()), []byte("secret"), nil)
			blamed := errors.Is(err, ErrAuthentication)
			if !errors.Is(err, tc.want) || blamed != (tc.want == ErrAuthentication) || errors.Is(err, ErrCredentialRejected) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			if rs, ok := tc.signer.(*recordingSigner); ok && rs.err != nil && !errors.Is(err, rs.err) {
				t.Fatalf("the hook's failure %v is not in %v", rs.err, err)
			}
		})
	}
}
