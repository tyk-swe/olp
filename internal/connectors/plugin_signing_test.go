package connectors

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
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
