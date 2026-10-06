package providers

import (
	"crypto/sha1"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/vendors"
)

type mediaProbeTransport func(*http.Request) (*http.Response, error)

func (f mediaProbeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type probeSocket struct{}

func (probeSocket) Read(p []byte) (int, error)  { return 0, io.EOF }
func (probeSocket) Write(p []byte) (int, error) { return len(p), nil }
func (probeSocket) Close() error                { return nil }

func nativeMediaProbeServer(t *testing.T) *Server {
	t.Helper()
	s := New(nil, &egress.Policy{}, nil)
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
	if mediaTuples != 17 {
		t.Fatalf("media capabilities=%d, want 17", mediaTuples)
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
	} {
		t.Run(cfg.Kind+"/"+cfg.AuthMode+"/"+value(cfg.Endpoint), func(t *testing.T) {
			cfg.Normalize()
			s := New(nil, &egress.Policy{}, nil)
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
		for _, tuple := range capabilitiesFor(kind, vendors.DefaultFor(kind)) {
			switch tuple.Operation {
			case "generation", "token_count", "embeddings", "moderation":
			case "batch", "realtime":
				if kind != KindAzure {
					t.Errorf("%s advertises un-certifiable tuple %v", kind, tuple)
				}
			case "image_generation", "speech", "transcription":
				// An Azure deployment certifies these by its own minimal call.
				if kind != KindAzure || tuple.Mode != ModeUnary {
					t.Errorf("%s advertises un-certifiable tuple %v", kind, tuple)
				}
			default:
				t.Errorf("%s advertises un-certifiable tuple %v", kind, tuple)
			}
		}
	}
}

// TestAzureMediaCertifiesThroughTheDeployment covers Azure OpenAI's media
// certification: the smallest real call of each operation to the
// deployment, and no media through the v1 API, which serves none.
func TestAzureMediaCertifiesThroughTheDeployment(t *testing.T) {
	cfg := Configuration{Kind: KindAzure, AuthMode: AuthAPIKey, Endpoint: new("https://example.openai.azure.com"), Deployment: new("media"), APIVersion: new("2025-04-01-preview")}
	cfg.Normalize()
	s := New(nil, &egress.Policy{}, nil)
	seen := map[string]string{}
	s.client.Transport = mediaProbeTransport(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		seen[r.URL.Path] = string(body)
		if r.Header.Get("Api-Key") != "test-credential" || r.URL.Query().Get("api-version") != "2025-04-01-preview" {
			t.Errorf("probe %s lacks the key or API version", r.URL)
		}
		reply := `{"created":1,"data":[{"b64_json":"aW1hZ2U="}]}`
		contentType := "application/json"
		switch {
		case strings.HasSuffix(r.URL.Path, "/audio/speech"):
			reply, contentType = "audio", "audio/mpeg"
		case strings.HasSuffix(r.URL.Path, "/audio/transcriptions"):
			reply = `{"text":""}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(reply)), Request: r}, nil
	})
	for _, operation := range []string{"image_generation", "speech", "transcription"} {
		if err := s.certifyTuple(t.Context(), &cfg, []byte("test-credential"), "media", CapabilityInput{operation, "openai", "unary"}, 4096); err != nil {
			t.Fatalf("certify %s: %v", operation, err)
		}
	}
	if image := seen["/openai/deployments/media/images/generations"]; !strings.Contains(image, `"quality":"low"`) || !strings.Contains(image, `"n":1`) {
		t.Fatalf("image probe = %s", image)
	}
	if speech := seen["/openai/deployments/media/audio/speech"]; !strings.Contains(speech, `"input":"OK"`) {
		t.Fatalf("speech probe = %s", speech)
	}
	if transcription := seen["/openai/deployments/media/audio/transcriptions"]; !strings.Contains(transcription, "RIFF") || !strings.Contains(transcription, `name="model"`) {
		t.Fatalf("transcription probe = %q", transcription)
	}
	v1 := Configuration{Kind: KindAzure, AuthMode: AuthAPIKey, ProfileID: "azure-v1-chat", ProfileRevision: "1", Endpoint: new("https://example.openai.azure.com")}
	v1.Normalize()
	if err := s.certifyMediaCall(t.Context(), &v1, []byte("test-credential"), "media", "speech"); err == nil {
		t.Fatal("Azure v1 certified speech")
	}
}
