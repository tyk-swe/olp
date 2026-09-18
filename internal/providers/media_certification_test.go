package providers

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
)

type mediaProbeTransport func(*http.Request) (*http.Response, error)

func (f mediaProbeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func nativeMediaProbeServer(t *testing.T) *Server {
	t.Helper()
	s := New(nil, &egress.Policy{})
	s.client.Transport = mediaProbeTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.String() != DefaultOpenAIEndpoint+"/models" || r.Header.Get("Authorization") != "Bearer test-credential" {
			t.Errorf("unexpected certification request: %s %s", r.Method, r.URL)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"object":"list","data":[{"id":"media-model","object":"model"}]}`)), Request: r}, nil
	})
	return s
}

func TestNativeMediaCapabilitiesCanBeDeclaredAndCertified(t *testing.T) {
	s := nativeMediaProbeServer(t)
	cfg := Configuration{Kind: KindOpenAI, AuthMode: AuthAPIKey}
	cfg.normalize()
	mediaTuples := 0
	for _, tuple := range capabilitiesFor(KindOpenAI, KindOpenAI) {
		switch tuple.Operation {
		case "generation", "token_count", "embeddings", "moderation":
			continue
		}
		mediaTuples++
		if _, err := validCapabilities([]capabilityInput{tuple}); err != nil {
			t.Fatalf("advertised media tuple rejected: %v", err)
		}
		if err := s.certifyTuple(t.Context(), &cfg, []byte("test-credential"), "media-model", tuple, 4096); err != nil {
			t.Fatalf("certify %v: %v", tuple, err)
		}
	}
	if mediaTuples != 14 {
		t.Fatalf("media capabilities=%d, want 14", mediaTuples)
	}
	if err := s.certifyTuple(t.Context(), &cfg, []byte("test-credential"), "missing-model", capabilityInput{"speech", "openai", "unary"}, 4096); err == nil {
		t.Fatal("inaccessible media model certified")
	}
}

func TestMediaCertificationDoesNotBorrowChatEvidence(t *testing.T) {
	for _, cfg := range []Configuration{
		{Kind: KindOpenAI, AuthMode: AuthAPIKey, Endpoint: new("https://custom.example/v1")},
		{Kind: KindOpenAI, AuthMode: AuthNone},
		{Kind: KindOpenAI, AuthMode: AuthAPIKey, Endpoint: new("https://api.openai.com/custom")},
		{Kind: KindOpenAICompatible, AuthMode: AuthAPIKey, Endpoint: new(DefaultOpenAIEndpoint)},
		{Kind: KindAzure, AuthMode: AuthAPIKey, Endpoint: new("https://example.openai.azure.com")},
	} {
		t.Run(cfg.Kind+"/"+cfg.AuthMode+"/"+value(cfg.Endpoint), func(t *testing.T) {
			cfg.normalize()
			s := New(nil, &egress.Policy{})
			s.client.Transport = mediaProbeTransport(func(r *http.Request) (*http.Response, error) {
				t.Errorf("unsupported media certification sent %s", r.URL)
				return nil, io.ErrUnexpectedEOF
			})
			if err := s.certifyTuple(t.Context(), &cfg, []byte("test-credential"), "media-model", capabilityInput{"image_generation", "openai", "unary"}, 4096); err == nil {
				t.Fatal("media certified without native evidence")
			}
		})
	}
	for _, kind := range []string{KindOpenAICompatible, KindAzure} {
		for _, tuple := range capabilitiesFor(kind, defaultVendor(kind)) {
			switch tuple.Operation {
			case "generation", "token_count", "embeddings", "moderation":
			default:
				t.Errorf("%s advertises un-certifiable tuple %v", kind, tuple)
			}
		}
	}
}
