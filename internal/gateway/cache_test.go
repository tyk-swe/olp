package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/tests/fixtures"
)

type cacheMemory struct {
	mu       sync.Mutex
	values   map[string]string
	fail     bool
	commands int
}

func (m *cacheMemory) Do(_ context.Context, a ...string) (any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commands++
	if m.fail {
		return nil, errors.New("offline")
	}
	switch a[0] {
	case "SET":
		m.values[a[1]] = a[2]
		return "OK", nil
	case "HGET":
		v, ok := m.values[a[1]]
		if !ok {
			return nil, nil
		}
		return v, nil
	case "EVAL":
		if a[2] == "2" {
			v := []any{}
			for i := range 2 {
				if m.values[a[3+i]] == "" {
					m.values[a[3+i]] = a[5+i]
				}
				v = append(v, m.values[a[3+i]])
			}
			key := a[7] + ":" + v[0].(string) + ":" + v[1].(string)
			bucket := a[8] + ":" + v[0].(string) + ":" + v[1].(string)
			value := ""
			if a[9] == "1" {
				value = m.values[key]
			}
			return []any{key, bucket, value}, nil
		}
		m.values[a[3]] = a[4]
		return int64(1), nil
	}
	return nil, errors.New("unsupported")
}
func cacheHarness(t *testing.T) (*harness, *cacheMemory) {
	t.Helper()
	return enableTestCache(t, newHarness(t, Config{}))
}
func enableTestCache(t *testing.T, h *harness) (*harness, *cacheMemory) {
	t.Helper()
	m := &cacheMemory{values: map[string]string{}}
	keys, err := secrets.ParseRing([]byte(`{"active_version":1,"keys":[{"version":1,"key":"` + strings.Repeat("1", 64) + `"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	h.gateway.Cache = &cache.Store{Client: m, Keys: keys, Auth: secrets.NewAuthKey(bytes.Repeat([]byte{2}, 32), "test"), Installation: "test"}
	h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
		r := s.Routes[routeSlug]
		r.Cache = &cache.Policy{Mode: "exact"}
		s.Routes[routeSlug] = r
	})
	return h, m
}
func TestResponseCacheUnaryReplayControlsAndOutage(t *testing.T) {
	h, m := cacheHarness(t)
	h.rt.mu.Lock()
	key := h.rt.keys[fullKey]
	key.Policy.ResponseMetadata = true
	h.rt.keys[fullKey] = key
	h.rt.mu.Unlock()
	body := []byte(`{"model":"` + routeSlug + `","messages":[{"role":"user","content":"hello"}]}`)
	request := func(headers map[string]string) (string, string) {
		r := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, body, headers)
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		if r.StatusCode != 200 {
			t.Fatalf("%d %s", r.StatusCode, b)
		}
		if r.Header.Get("X-OLP-Cache") == "hit" && (r.Header.Get("X-OLP-Cost") != "0" || r.Header.Get("X-OLP-Attempts") != "0") {
			t.Fatalf("incorrect cache metadata: %v", r.Header)
		}
		return r.Header.Get("X-OLP-Cache"), string(b)
	}
	result, first := request(nil)
	if result != "miss" {
		t.Fatal(result)
	}
	count := h.mock.count("a") + h.mock.count("b")
	result, second := request(nil)
	if result != "hit" || first != second || h.mock.count("a")+h.mock.count("b") != count {
		t.Fatalf("replay result=%s first=%s second=%s", result, first, second)
	}
	ev := h.sink.last(t)
	if len(ev.Attempts) != 1 || ev.Attempts[0].Class != "cache_hit" || ev.Attempts[0].Cache == nil {
		t.Fatalf("cache accounting %#v", ev.Attempts)
	}
	if r, _ := request(map[string]string{"X-OLP-Cache": "no-cache"}); r != "miss" {
		t.Fatal(r)
	}
	if r, _ := request(map[string]string{"X-OLP-Cache": "no-store"}); r != "bypass" {
		t.Fatal(r)
	}
	m.mu.Lock()
	m.fail = true
	m.mu.Unlock()
	if r, _ := request(nil); r != "bypass" {
		t.Fatal(r)
	}
}
func TestResponseCacheStreamReplaysNativeFrames(t *testing.T) {
	h, _ := cacheHarness(t)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"id":"c","object":"chat.completion.chunk","model":"model-a","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`+"\n\ndata: [DONE]\n\n")
	})
	body := []byte(`{"model":"` + routeSlug + `","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	read := func() (string, string) {
		r := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, body, nil)
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		if r.StatusCode != 200 {
			t.Fatalf("%d %s", r.StatusCode, b)
		}
		return r.Header.Get("X-OLP-Cache"), string(b)
	}
	first, a := read()
	second, b := read()
	if first != "miss" || second != "hit" || a != b {
		t.Fatalf("stream replay %s/%s\n%s\n%s", first, second, a, b)
	}
	env := h.sink.nth(t, 2)
	if !env.Committed || !env.Attempts[0].Committed {
		t.Fatal("cache replay lost stream commitment")
	}
}
func TestResponseCacheRevisionAndKeyPolicy(t *testing.T) {
	h, _ := cacheHarness(t)
	body := []byte(`{"model":"` + routeSlug + `","messages":[{"role":"user","content":"hello"}]}`)
	do := func() string {
		r := h.do(t.Context(), "POST", "/v1/chat/completions", fullKey, body, nil)
		defer r.Body.Close()
		_, _ = io.Copy(io.Discard, r.Body)
		return r.Header.Get("X-OLP-Cache")
	}
	if do() != "miss" || do() != "hit" {
		t.Fatal("cache did not warm")
	}
	h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
		r := s.Routes[routeSlug]
		r.RevisionID = "new-revision"
		s.Routes[routeSlug] = r
	})
	if do() != "miss" {
		t.Fatal("served old revision")
	}
	h.rt.mu.Lock()
	key := h.rt.keys[fullKey]
	key.Policy.ForbidResponseCache = true
	h.rt.keys[fullKey] = key
	h.rt.mu.Unlock()
	if do() != "bypass" {
		t.Fatal("ignored key cache policy")
	}
}

func TestResponseCacheClientSurfaces(t *testing.T) {
	for _, tc := range []struct {
		name, kind, profile, surface, path, body, reply, stream string
	}{
		{"chat", "openai", "openai-chat", "openai", "/v1/chat/completions", `{"model":"team-chat","messages":[{"role":"user","content":"hello"}]}`, benchChat, "openai-chat"},
		{"responses", "openai", "openai-responses", "openai", "/v1/responses", `{"model":"team-chat","input":"hello","store":false}`, `{"id":"resp_1","object":"response","model":"model-a","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`, "responses"},
		{"anthropic", "anthropic", "anthropic-messages", "anthropic", "/anthropic/v1/messages", `{"model":"team-chat","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, `{"id":"msg_1","type":"message","role":"assistant","model":"model-a","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}`, "anthropic-messages"},
		{"gemini", "gemini", "gemini-generation", "gemini", "/gemini/v1beta/models/team-chat:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`, `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}`, "gemini-generate-content"},
		{"bedrock", "bedrock", "bedrock-converse", "bedrock", "/bedrock/model/team-chat/converse", `{"messages":[{"role":"user","content":[{"text":"hello"}]}],"inferenceConfig":{"maxTokens":16}}`, `{"output":{"message":{"role":"assistant","content":[{"text":"hello"}]}},"stopReason":"end_turn","usage":{"inputTokens":2,"outputTokens":3,"totalTokens":5},"metrics":{"latencyMs":1}}`, "bedrock"},
		{"bedrock-transformed", "bedrock", "bedrock-converse", "bedrock", "/bedrock/model/team-chat/converse", `{"messages":[{"role":"user","content":[{"text":"hello"}]}],"inferenceConfig":{"maxTokens":16}}`, `{"output":{"message":{"role":"assistant","content":[{"text":"hello"}]}},"stopReason":"end_turn","usage":{"inputTokens":2,"outputTokens":3,"totalTokens":5},"metrics":{"latencyMs":1}}`, "bedrock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"unary", "streaming"} {
				t.Run(mode, func(t *testing.T) {
					h, _ := enableTestCache(t, strictHarness(t, func(snapshot *routing.Snapshot) {
						for id, provider := range snapshot.Providers {
							provider.Kind, provider.ProfileID = tc.kind, tc.profile
							provider.ProfileRevision = connectors.ProfileRevision
							provider.VendorID = ""
							if tc.kind == "bedrock" {
								provider.CloudRegion = "us-east-1"
								provider.AuthMode = "static"
							}
							provider.Capabilities = []routing.Capability{{Model: provider.Capabilities[0].Model, Operation: "generation", Surface: tc.surface, Mode: mode}}
							snapshot.Providers[id] = provider
						}
						if tc.name == "bedrock-transformed" {
							route := snapshot.Routes[routeSlug]
							route.Fidelity.Mode = routing.FidelityTransformed
							snapshot.Routes[routeSlug] = route
						}
					}))
					path, body := tc.path, tc.body
					if tc.kind == "bedrock" {
						credentials := map[string][]byte{}
						for _, provider := range h.rt.release.Snapshot.Providers {
							credentials[*provider.ActiveCredential] = []byte(`{"access_key_id":"TESTBEDROCKKEY123456","secret_access_key":"test-bedrock-secret-for-cache"}`)
						}
						var err error
						h.rt.release, err = runtime.NewRelease(h.rt.release.ID, h.rt.release.Sequence, h.rt.release.Snapshot, credentials)
						if err != nil {
							t.Fatal(err)
						}
						mux := http.NewServeMux()
						h.gateway.Register(mux)
						mux.HandleFunc("POST /bedrock/model/{model}/converse", h.gateway.bedrockConverse)
						mux.HandleFunc("POST /bedrock/model/{model}/converse-stream", h.gateway.bedrockConverseStream)
						h.server = httptest.NewServer(mux)
						t.Cleanup(h.server.Close)
					}
					reply, contentType := []byte(tc.reply), "application/json"
					if mode == "streaming" {
						contentType = "text/event-stream"
						switch tc.stream {
						case "bedrock":
							path += "-stream"
							contentType = "application/vnd.amazon.eventstream"
							reply = cacheBedrockStream(t)
						case "responses":
							body = strings.TrimSuffix(body, "}") + `,"stream":true}`
							reply = nil
							for _, event := range streamedEvents("hello") {
								reply = append(reply, []byte("data: "+event+"\n\n")...)
							}
						default:
							if tc.name == "gemini" {
								path = strings.Replace(path, ":generateContent", ":streamGenerateContent?alt=sse", 1)
							} else {
								body = strings.TrimSuffix(body, "}") + `,"stream":true}`
							}
							var err error
							reply, err = fixtures.Files.ReadFile("streams/" + tc.stream + ".sse")
							if err != nil {
								t.Fatal(err)
							}
						}
					}
					h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
						w.Header().Set("Content-Type", contentType)
						_, _ = w.Write(reply)
					})
					var first []byte
					headers := map[string]string{}
					if tc.name == "anthropic" {
						headers["Anthropic-Version"] = "2023-06-01"
					}
					for i := range 2 {
						response := h.do(t.Context(), "POST", path, fullKey, []byte(body), headers)
						got, _ := io.ReadAll(response.Body)
						response.Body.Close()
						want := "miss"
						if i == 1 {
							want = "hit"
						}
						if response.StatusCode != 200 || response.Header.Get("X-OLP-Cache") != want {
							t.Fatalf("%d: status=%d cache=%s body=%s", i, response.StatusCode, response.Header.Get("X-OLP-Cache"), got)
						}
						if i == 0 {
							first = got
						} else if !bytes.Equal(first, got) || h.mock.count("a") != 1 {
							t.Fatal("replay changed the response or reached the provider")
						}
					}
				})
			}
		})
	}
}

func cacheBedrockStream(t *testing.T) []byte {
	t.Helper()
	var body bytes.Buffer
	for _, event := range []*eventstream.Message{
		bedrockEvent(t, "messageStart", map[string]any{"role": "assistant"}),
		bedrockEvent(t, "contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]string{"text": "hello"}}),
		bedrockEvent(t, "contentBlockStop", map[string]any{"contentBlockIndex": 0}),
		bedrockEvent(t, "messageStop", map[string]string{"stopReason": "end_turn"}),
		bedrockMetadata(`{"usage":{"inputTokens":2,"outputTokens":3,"totalTokens":5},"metrics":{"latencyMs":1}}`),
	} {
		if err := eventstream.NewEncoder().Encode(&body, *event); err != nil {
			t.Fatal(err)
		}
	}
	return body.Bytes()
}

func TestResponseCacheStrictEmbeddings(t *testing.T) {
	h, _ := enableTestCache(t, strictHarness(t, strictEmbeddings))
	h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"object":"list","model":"model-a","data":[{"object":"embedding","index":0,"embedding":[1,0]}],"usage":{"prompt_tokens":1,"total_tokens":1}}`)
	})
	for i := range 2 {
		response := h.do(t.Context(), "POST", "/v1/embeddings", fullKey, []byte(`{"model":"team-chat","input":"hello"}`), nil)
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		want := "miss"
		if i == 1 {
			want = "hit"
		}
		if response.StatusCode != 200 || response.Header.Get("X-OLP-Cache") != want {
			t.Fatalf("%d: %d %s", i, response.StatusCode, body)
		}
	}
}
