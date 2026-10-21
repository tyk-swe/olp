package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/coderesearch"
)

func TestResearchOriginBoundary(t *testing.T) {
	for _, origin := range []string{"", "https://olp.example", "https://evil.example", "null", "https://olp.example/path", "https://user@olp.example"} {
		r := httptest.NewRequest("POST", "https://olp.example/code/research/mcp/search", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if got, want := researchOriginAllowed(r), origin == "" || origin == "https://olp.example"; got != want {
			t.Errorf("origin %q: %v; want %v", origin, got, want)
		}
	}
}

func TestResearchScopeAndDisabledServices(t *testing.T) {
	_, _, server := newCodeForwardHarness(t)
	for _, test := range []struct {
		key    string
		status int
	}{{fullKey, 404}, {readKey, 403}, {"invalid-key", 401}} {
		request, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/code/coding/mcp/search", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+test.key)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != test.status {
			t.Errorf("key scope: %d; want %d", response.StatusCode, test.status)
		}
	}
}

func TestResearchSSEHeadersFlushWhileIdle(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reader, writer := io.Pipe()
				defer reader.Close()
				go func() {
					io.WriteString(writer, ": heartbeat\n\n")
					<-r.Context().Done()
					writer.CloseWithError(r.Context().Err())
				}()
				response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader}
				(&Server{}).copyResearchResponse(w, r, response, researchResponse{service: "search"})
			}))
			defer server.Close()
			transport := &http.Transport{ResponseHeaderTimeout: time.Second}
			defer transport.CloseIdleConnections()
			request, err := http.NewRequestWithContext(t.Context(), method, server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := (&http.Client{Transport: transport}).Do(request)
			if err != nil {
				t.Fatalf("idle SSE headers were not flushed: %v", err)
			}
			response.Body.Close()
			if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/event-stream" {
				t.Fatalf("unexpected SSE headers: %v", response)
			}
		})
	}
}

const researchInitialized = `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","serverInfo":{"name":"fixture","version":"1"},"capabilities":{"tools":{}}}}`

func TestResearchInitializationMustReachClient(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, test := range []struct {
			name, body string
			valid      bool
		}{
			{"success", researchInitialized, true},
			{"error", `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"unavailable"}}`, false},
			{"wrong ID", strings.Replace(researchInitialized, `"id":1`, `"id":2`, 1), false},
			{"missing fields", `{"jsonrpc":"2.0","id":1,"result":{}}`, false},
			{"malformed", `{"jsonrpc":`, false},
			{"empty", "", false},
		} {
			t.Run(test.name+map[bool]string{false: "/json", true: "/sse"}[streaming], func(t *testing.T) {
				media, body := "application/json", test.body
				if streaming {
					media, body = "text/event-stream", "data: "+body+"\n\n"
				}
				response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {media}}, Body: io.NopCloser(strings.NewReader(body))}
				forwarding := researchResponse{method: "initialize", service: "search", requestID: []byte(`1`)}
				got := (&Server{}).copyResearchResponse(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil), response, forwarding)
				if got != test.valid {
					t.Fatalf("initialization accepted: %v; want %v", got, test.valid)
				}
				// Even a valid result cannot retain a session if delivery fails.
				response.Body = io.NopCloser(strings.NewReader(body))
				if (&Server{}).copyResearchResponse(&researchFailedWriter{httptest.NewRecorder()}, httptest.NewRequest("POST", "/", nil), response, forwarding) {
					t.Fatal("failed delivery established a session")
				}
			})
		}
	}
}

type researchFailedWriter struct{ *httptest.ResponseRecorder }

func (*researchFailedWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestResearchResponseBoundsAndNativeErrors(t *testing.T) {
	s := &Server{}
	for _, test := range []struct {
		name, media, encoding, body string
		upstream, want              int
	}{
		{"oversized", "application/json", "", strings.Repeat("x", coderesearch.MaxResponse+1), 200, 502},
		{"encoded", "application/json", "gzip", "opaque", 200, 502},
		{"invalid JSON", "application/json", "", `{"result":{}}`, 200, 502},
		{"vendor error", "application/json", "", `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"service unavailable"}}`, 429, 429},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{StatusCode: test.upstream, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(test.body))}
			response.Header.Set("Content-Type", test.media)
			response.Header.Set("Content-Encoding", test.encoding)
			w := httptest.NewRecorder()
			s.copyResearchResponse(w, httptest.NewRequest("POST", "/code/research/mcp/search", nil), response, researchResponse{method: "tools/call", service: "search", resumeKey: "test-bearer"})
			if w.Code != test.want || test.want == 429 && w.Body.String() != test.body {
				t.Fatalf("response %d %s", w.Code, w.Body.String())
			}
		})
	}
	// A malformed or overlarge SSE event is dropped before it reaches clients.
	response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + strings.Repeat("x", coderesearch.MaxEvent) + "\n\n"))}
	w := httptest.NewRecorder()
	s.copyResearchResponse(w, httptest.NewRequest("POST", "/code/research/mcp/search", nil), response, researchResponse{method: "tools/call", service: "search", resumeKey: "test-bearer"})
	if w.Body.Len() != 0 {
		t.Fatal("overlarge SSE event was forwarded")
	}
}
