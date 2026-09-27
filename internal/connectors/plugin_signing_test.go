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
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// recordingSigner answers every signing hook with its headers or failure,
// recording what it was asked to sign.
type recordingSigner struct {
	headers  map[string]string
	err      error
	digests  []string
	requests []abi.SignRequest
	secrets  [][]string
}

func (s *recordingSigner) Sign(_ context.Context, digest string, request abi.SignRequest, secrets []string) (abi.SignResult, error) {
	s.digests, s.requests, s.secrets = append(s.digests, digest), append(s.requests, request), append(s.secrets, secrets)
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
		if !slices.Contains(sensitive, signed) {
			t.Errorf("%q is not redacted: %q", signed, sensitive)
		}
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

func TestPluginProfilesWithoutSigningRunNoPluginCode(t *testing.T) {
	signer := &recordingSigner{}
	req, _ := http.NewRequest(http.MethodPost, "https://api.acme.example/v2/chat/completions", nil)
	if _, err := signingAuth(signer).Apply(context.Background(), req, pluginConfig(t, pluginManifest()), []byte("secret"), nil); err != nil || len(signer.requests) != 0 {
		t.Fatalf("ran a hook the profile does not declare: %v", err)
	}
}

// A signing hook that fails, or returns headers a signature can't add, fails
// authentication, so the request is never sent.
func TestPluginSigningFailuresFailAuthentication(t *testing.T) {
	many := map[string]string{}
	for i := range maxSignedHeaders + 1 {
		many[fmt.Sprintf("X-Signature-%d", i)] = "v"
	}
	failed := errors.New("plugin_timed_out")
	for name, tc := range map[string]struct {
		signer Signer
		cause  error
	}{
		"no signer":         {nil, nil},
		"hook failed":       {&recordingSigner{err: failed}, failed},
		"hop-by-hop header": {&recordingSigner{headers: map[string]string{"Connection": "close"}}, nil},
		"reserved header":   {&recordingSigner{headers: map[string]string{"Content-Type": "text/plain"}}, nil},
		"semantic header":   {&recordingSigner{headers: map[string]string{"OpenAI-Beta": "assistants=v2"}}, nil},
		"declared header":   {&recordingSigner{headers: map[string]string{"x-acme-client": "other"}}, nil},
		"repeated header":   {&recordingSigner{headers: map[string]string{"X-Signature": "a", "x-signature": "b"}}, nil},
		"invalid value":     {&recordingSigner{headers: map[string]string{"X-Signature": "a\r\nX-Injected: 1"}}, nil},
		"too many headers":  {&recordingSigner{headers: many}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, "https://api.acme.example/v2/chat/completions", nil)
			_, err := signingAuth(tc.signer).Apply(context.Background(), req, pluginConfig(t, signingManifest()), []byte("secret"), nil)
			if !errors.Is(err, ErrAuthentication) || errors.Is(err, ErrCredentialRejected) || tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Fatalf("want a transient authentication failure, got %v", err)
			}
		})
	}
}
