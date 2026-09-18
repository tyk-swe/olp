package media

import (
	"context"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
)

func testTransport(t *testing.T, handler http.Handler) (*Transport, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	policy := egress.Policy{
		AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		PlainHTTPHosts:  []string{"127.0.0.1"},
	}
	return &Transport{
		Client:           policy.Client(30 * time.Second),
		Auth:             connectors.NewAuth(&policy),
		Egress:           &policy,
		Spool:            testSpool(t, MinCapacityBytes),
		MaxResponseBytes: 1 << 20,
	}, server
}

func testTarget(endpoint string) Target {
	return Target{
		Config: connectors.Config{Kind: "openai", AuthMode: "none", Endpoint: endpoint},
		Model:  "upstream-model",
	}
}

func TestTransportDecodesJSONImageResponse(t *testing.T) {
	transport, server := testTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" {
			t.Errorf("path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"url":"https://cdn.example.com/i.png"}]}`))
	}))
	call := &UpstreamCall{Method: "POST", Path: "images/generations", JSON: []byte(`{"model":"m","prompt":"p"}`), Kind: ResponseImages}
	result, failure := transport.Do(context.Background(), testTarget(server.URL+"/v1"), call, nil)
	if failure != nil {
		t.Fatal(failure)
	}
	if result.Images == nil || len(result.Images.Images) != 1 {
		t.Fatalf("images: %+v", result.Images)
	}
}

func TestTransportStagesBinaryResponse(t *testing.T) {
	transport, server := testTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write([]byte("audio-bytes-payload"))
	}))
	call := &UpstreamCall{Method: "POST", Path: "audio/speech", JSON: []byte(`{"model":"m","input":"i","voice":"v"}`), Kind: ResponseBinary}
	result, failure := transport.Do(context.Background(), testTarget(server.URL+"/v1"), call, nil)
	if failure != nil {
		t.Fatal(failure)
	}
	if result.Artifact == nil {
		t.Fatal("binary response not staged")
	}
	opened, err := transport.Spool.Open(result.Artifact.Handle)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(opened.File)
	opened.File.Close()
	if string(data) != "audio-bytes-payload" {
		t.Fatalf("payload: %q", data)
	}
	transport.Spool.Remove(result.Artifact.Handle)
}

func TestTransportBoundsOversizedResponse(t *testing.T) {
	transport, server := testTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"b64_json":"` + strings.Repeat("e", 3<<20) + `"}]}`))
	}))
	call := &UpstreamCall{Method: "POST", Path: "images/generations", JSON: []byte(`{}`), Kind: ResponseImages}
	if _, failure := transport.Do(context.Background(), testTarget(server.URL+"/v1"), call, nil); failure == nil {
		t.Fatal("oversized response admitted")
	}
}

func TestTransportCleansImagesAfterPartialDecodeFailure(t *testing.T) {
	for _, second := range []string{`{"b64_json":"###"}`, `{"url":"https://example.com/i.png","b64_json":"AQID"}`} {
		t.Run(second, func(t *testing.T) {
			transport, server := testTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"data":[{"b64_json":"AQID"},`+second+`]}`)
			}))
			call := &UpstreamCall{Method: "POST", Path: "images/generations", JSON: []byte(`{}`), Kind: ResponseImages, Ambiguous: true}
			result, failure := transport.Do(t.Context(), testTarget(server.URL+"/v1"), call, nil)
			if result != nil || failure == nil || !failure.Ambiguous {
				t.Fatalf("malformed image result: %+v %+v", result, failure)
			}
			if transport.Spool.UsedBytes() != 0 {
				t.Fatal("failed image decode leaked staged artifacts")
			}
		})
	}
}

func TestMultipartFileHeadersPreserveNames(t *testing.T) {
	filename := `image with "quotes"; version 1.png`
	header := filePartHeader("image[]", filename, "image/png")
	kind, params, err := mime.ParseMediaType(header.Get("Content-Disposition"))
	if err != nil || kind != "form-data" || params["name"] != "image[]" || params["filename"] != filename {
		t.Fatalf("multipart header lost field or filename: %v %v", header, err)
	}
}

func TestTransportClassifiesUpstreamFailures(t *testing.T) {
	for _, tc := range []struct {
		status int
		class  FailureClass
	}{
		{401, ClassCredential},
		{403, ClassCredential},
		{429, ClassRateLimit},
		{500, ClassUpstreamServer},
		{400, ClassUpstreamClient},
	} {
		transport, server := testTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(tc.status)
			w.Write([]byte(`{"error":{"message":"upstream rejected"}}`))
		}))
		call := &UpstreamCall{Method: "POST", Path: "images/generations", JSON: []byte(`{}`), Kind: ResponseImages}
		_, failure := transport.Do(context.Background(), testTarget(server.URL+"/v1"), call, nil)
		if failure == nil || failure.Class != tc.class {
			t.Fatalf("status %d: %v; want %s", tc.status, failure, tc.class)
		}
		if failure.Status != tc.status {
			t.Fatalf("status %d: got %d", tc.status, failure.Status)
		}
	}
}

func TestTransportRejectsUnsafeEndpoint(t *testing.T) {
	transport, _ := testTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	call := &UpstreamCall{Method: "POST", Path: "images/generations", JSON: []byte(`{}`), Kind: ResponseImages}
	if _, failure := transport.Do(context.Background(), testTarget("http://10.0.0.1.internal/v1"), call, nil); failure == nil {
		t.Fatal("unsafe endpoint dispatched")
	}
}

func TestTransportVideoCreateIsAmbiguousOnDispatchFailure(t *testing.T) {
	// A server that accepts the request then stalls past the client deadline.
	release := make(chan struct{})
	transport, server := testTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	call := &UpstreamCall{Method: "POST", Path: "videos", JSON: []byte(`{"model":"m","prompt":"p"}`), Kind: ResponseVideoJob, Ambiguous: true}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, failure := transport.Do(ctx, testTarget(server.URL+"/v1"), call, nil)
	close(release)
	if failure == nil || !failure.Ambiguous {
		t.Fatalf("expected ambiguous failure, got %+v", failure)
	}
}

func TestTransportVideoListQuery(t *testing.T) {
	transport, server := testTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/videos" || r.Method != "GET" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		if got := r.URL.Query().Get("limit"); got != "20" {
			t.Errorf("limit: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"id":"v1","object":"video","status":"completed"}],"has_more":false}`))
	}))
	query := url.Values{"limit": {"20"}}
	call := &UpstreamCall{Method: "GET", Path: "videos", Query: query, Kind: ResponseVideoList}
	result, failure := transport.Do(context.Background(), testTarget(server.URL+"/v1"), call, nil)
	if failure != nil {
		t.Fatal(failure)
	}
	if result.List == nil || len(result.List.Jobs) != 1 {
		t.Fatalf("list: %+v", result.List)
	}
}
