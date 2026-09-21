package providers

import (
	"crypto/sha1"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
)

type mediaProbeTransport func(*http.Request) (*http.Response, error)

func (f mediaProbeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type probeSocket struct{}

func (probeSocket) Read(p []byte) (int, error)  { return 0, io.EOF }
func (probeSocket) Write(p []byte) (int, error) { return len(p), nil }
func (probeSocket) Close() error                { return nil }

func nativeMediaProbeServer(t *testing.T) *Server {
	t.Helper()
	s := New(nil, &egress.Policy{})
	s.client.Transport = mediaProbeTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer test-credential" {
			t.Errorf("unexpected certification request: %s %s", r.Method, r.URL)
		}
		if r.Method == "GET" && r.Header.Get("Upgrade") == "websocket" {
			sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
			return &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: http.Header{
				"Connection":           []string{"Upgrade"},
				"Upgrade":              []string{"websocket"},
				"Sec-Websocket-Accept": []string{base64.StdEncoding.EncodeToString(sum[:])},
			}, Body: probeSocket{}, Request: r}, nil
		}
		if r.Method != "GET" || !strings.HasPrefix(r.URL.String(), DefaultOpenAIEndpoint+"/") {
			t.Errorf("unexpected certification request: %s %s", r.Method, r.URL)
		}
		switch r.URL.Path {
		case "/v1/files", "/v1/batches":
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"object":"list","data":[]}`)), Request: r}, nil
		case "/v1/models":
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"object":"list","data":[{"id":"media-model","object":"model"}]}`)), Request: r}, nil
		}
		t.Errorf("unexpected certification request: %s %s", r.Method, r.URL)
		return &http.Response{StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
	})
	return s
}

func TestNativeMediaCapabilitiesCanBeDeclaredAndCertified(t *testing.T) {
	s := nativeMediaProbeServer(t)
	cfg := Configuration{Kind: KindOpenAI, AuthMode: AuthAPIKey}
	cfg.Normalize()
	mediaTuples := 0
	for _, tuple := range capabilitiesFor(KindOpenAI, KindOpenAI) {
		switch tuple.Operation {
		case "generation", "token_count", "embeddings", "moderation":
			continue
		}
		mediaTuples++
		if _, err := ValidCapabilities([]CapabilityInput{tuple}); err != nil {
			t.Fatalf("advertised media tuple rejected: %v", err)
		}
		if err := s.certifyTuple(t.Context(), &cfg, []byte("test-credential"), "media-model", tuple, 4096); err != nil {
			t.Fatalf("certify %v: %v", tuple, err)
		}
	}
	if mediaTuples != 16 {
		t.Fatalf("media capabilities=%d, want 16", mediaTuples)
	}
	if err := s.certifyTuple(t.Context(), &cfg, []byte("test-credential"), "missing-model", CapabilityInput{"speech", "openai", "unary"}, 4096); err == nil {
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
			cfg.Normalize()
			s := New(nil, &egress.Policy{})
			s.client.Transport = mediaProbeTransport(func(r *http.Request) (*http.Response, error) {
				t.Errorf("unsupported media certification sent %s", r.URL)
				return nil, io.ErrUnexpectedEOF
			})
			if err := s.certifyTuple(t.Context(), &cfg, []byte("test-credential"), "media-model", CapabilityInput{"image_generation", "openai", "unary"}, 4096); err == nil {
				t.Fatal("media certified without native evidence")
			}
		})
	}
	for _, kind := range []string{KindOpenAICompatible, KindAzure} {
		for _, tuple := range capabilitiesFor(kind, defaultVendor(kind)) {
			switch tuple.Operation {
			case "generation", "token_count", "embeddings", "moderation":
			case "batch", "realtime":
				if kind != KindAzure {
					t.Errorf("%s advertises un-certifiable tuple %v", kind, tuple)
				}
			default:
				t.Errorf("%s advertises un-certifiable tuple %v", kind, tuple)
			}
		}
	}
}
