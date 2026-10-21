//go:build bench

package loadgen

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// captured is a request as the server saw it.
type captured struct {
	method, path, rawQuery string
	header                 http.Header
	body                   []byte
	contentLength          int64
}

// capture serves 200 and keeps every request.
func capture() (http.Handler, func() []captured) {
	var mu sync.Mutex
	var got []captured
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, captured{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), body, r.ContentLength})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{}`)
	})
	return h, func() []captured { mu.Lock(); defer mu.Unlock(); return append([]captured(nil), got...) }
}

func TestRequestShapePerDialect(t *testing.T) {
	for _, tc := range []struct {
		dialect     Dialect
		path, query string
		authHeader  string
		authValue   string
		streamPath  string
		streamQuery string
	}{
		{OpenAI, "/v1/chat/completions", "", "Authorization", "Bearer secret", "/v1/chat/completions", ""},
		{Anthropic, "/v1/messages", "", "X-Api-Key", "secret", "/v1/messages", ""},
		{Gemini, "/v1beta/models/the-model:generateContent", "", "X-Goog-Api-Key", "secret", "/v1beta/models/the-model:streamGenerateContent", "alt=sse"},
	} {
		t.Run(string(tc.dialect), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h, got := capture()
				cfg := base(h)
				cfg.Dialect, cfg.APIKey, cfg.Model = tc.dialect, "secret", "the-model"
				cfg.Rate, cfg.Duration, cfg.StreamShare, cfg.PromptTokens, cfg.MaxTokens = 40, time.Second, 0.5, []int{0, 300}, 16
				cfg.Headers = map[string]string{"X-Extra": "1"}
				if report := run(t, cfg); report.Requests.Sent != 40 {
					t.Fatalf("%+v", report.Requests)
				}
				requests := got()
				if len(requests) != 40 {
					t.Fatalf("%d requests", len(requests))
				}
				streams, long, ids := 0, 0, map[string]bool{}
				for _, r := range requests {
					var doc map[string]json.RawMessage
					if err := json.Unmarshal(r.body, &doc); err != nil {
						t.Fatalf("the body is not JSON: %v\n%.200s", err, r.body)
					}
					if int64(len(r.body)) != r.contentLength {
						t.Fatalf("content length %d, body %d bytes", r.contentLength, len(r.body))
					}
					streaming := false
					switch tc.dialect {
					case OpenAI, Anthropic:
						streaming = string(doc["stream"]) == "true"
						if string(doc["model"]) != `"the-model"` || string(doc["max_tokens"]) != "16" {
							t.Fatalf("body %.200s", r.body)
						}
					case Gemini:
						streaming = strings.Contains(r.path, "streamGenerateContent")
						if !strings.Contains(string(doc["generationConfig"]), `"maxOutputTokens":16`) {
							t.Fatalf("body %.200s", r.body)
						}
					}
					wantPath, wantQuery := tc.path, tc.query
					if streaming {
						wantPath, wantQuery = tc.streamPath, tc.streamQuery
					}
					if r.method != http.MethodPost || r.path != wantPath || r.rawQuery != wantQuery {
						t.Fatalf("%s %s?%s, want POST %s?%s", r.method, r.path, r.rawQuery, wantPath, wantQuery)
					}
					if r.header.Get(tc.authHeader) != tc.authValue || r.header.Get("Content-Type") != "application/json" || r.header.Get("X-Extra") != "1" {
						t.Fatalf("headers %v", r.header)
					}
					if tc.dialect == Anthropic && r.header.Get("Anthropic-Version") == "" {
						t.Fatal("Anthropic requires anthropic-version")
					}
					if streaming {
						streams++
						if tc.dialect == OpenAI && !strings.Contains(string(r.body), `"stream_options":{"include_usage":true}`) {
							t.Fatalf("a streaming OpenAI request asks for usage: %.300s", r.body)
						}
					}
					if len(r.body) > 1000 {
						long++
					}
					// Each request is named, so none repeats another byte for byte.
					id := string(r.body[:min(len(r.body), 300)])
					ids[id[strings.Index(id, "request"):strings.Index(id, ": ")]] = true
				}
				if streams != 20 || long != 20 || len(ids) != 40 {
					t.Fatalf("%d streaming, %d long, %d distinct of 40", streams, long, len(ids))
				}
			})
		})
	}
}
