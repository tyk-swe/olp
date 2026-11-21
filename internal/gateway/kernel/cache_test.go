package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/google/uuid"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/platform/coordination"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/rates"
	"github.com/tyk-swe/olp/internal/usage/routinginputs"
	"github.com/tyk-swe/olp/tests/fixtures"
)

// streamedItem is the output item the streaming-only upstream completes.
func streamedItem(text string) string {
	return `{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":` + fmt.Sprintf("%q", text) + `,"annotations":[]}]}`
}

// streamedEvents is a Responses stream that delivers its output item only in
// response.output_item.done and leaves the terminal response's output empty.
func streamedEvents(text string) []string {
	return []string{
		`{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","status":"in_progress","model":"` + modelA + `","output":[]}}`,
		`{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"in_progress","content":[]}}`,
		`{"type":"response.output_text.delta","sequence_number":2,"item_id":"msg_1","output_index":0,"content_index":0,"delta":` + fmt.Sprintf("%q", text) + `}`,
		`{"type":"response.output_item.done","sequence_number":3,"output_index":0,"item":` + streamedItem(text) + `}`,
		`{"type":"response.completed","sequence_number":4,"response":{"id":"resp_1","object":"response","status":"completed","model":"` + modelA + `","output":[],"usage":{"input_tokens":4,"output_tokens":6,"total_tokens":10}}}`,
	}
}

const benchChat = `{"id":"chatcmpl-bench","object":"chat.completion","created":1800000000,"model":"gpt-bench","choices":[{"index":0,"message":{"role":"assistant","content":"The capital of France is Paris, a city on the Seine known for its museums, its cafes and the tower that carries the name of its engineer."},"finish_reason":"stop"}],"usage":{"prompt_tokens":24,"completion_tokens":31,"total_tokens":55}}`

type cacheMemory struct {
	mu        sync.Mutex
	values    map[string]string
	fail      bool
	failWrite bool
	commands  int
}

func (*cacheMemory) Search(context.Context, ...string) ([]coordination.SearchDocument, error) {
	return nil, cache.ErrUnavailable
}

func (m *cacheMemory) Do(_ context.Context, a ...string) (any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commands++
	if m.fail || m.failWrite && a[0] == "EVAL" && a[2] == "1" {
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
		if a[2] != "1" {
			n, err := strconv.Atoi(a[2])
			if err != nil {
				return nil, err
			}
			v := []string{}
			for i := range n {
				if m.values[a[3+i]] == "" {
					m.values[a[3+i]] = a[3+n+i]
				}
				v = append(v, m.values[a[3+i]])
			}
			base := 3 + 2*n
			key := a[base] + ":" + strings.Join(v, ":")
			bucket := a[base+1] + ":" + strings.Join(v, ":")
			value := ""
			if a[base+2] == "1" {
				value = m.values[key]
			}
			reply := []any{key, bucket, value}
			for _, generation := range v {
				reply = append(reply, generation)
			}
			return reply, nil
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
	keys, err := secrets.ParseRingWithUnwrapper(context.Background(), []byte(`{"active_version":1,"keys":[{"version":1,"key":"`+strings.Repeat("1", 64)+`"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	h.gateway.Deps.Cache = &cache.Store{Client: m, Keys: keys, Auth: secrets.NewAuthKey(bytes.Repeat([]byte{2}, 32), "test"), Installation: "test"}
	h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
		r := s.Routes[routeSlug]
		r.Cache = &cache.Policy{Mode: "exact"}
		s.Routes[routeSlug] = r
	})
	return h, m
}

func TestCacheAdmissionReportsARealAnthropicResetTime(t *testing.T) {
	requestWindowEnd := time.Date(2026, time.March, 10, 12, 1, 0, 0, time.UTC)
	tokenWindowEnd := requestWindowEnd.Add(time.Minute)
	requests := limits.RateState{RequestLimit: 10, RequestRemaining: 9, ResetAt: requestWindowEnd}
	for _, test := range []struct {
		name   string
		tokens limits.RateState
		want   time.Time
	}{
		{"no token lease", limits.RateState{}, requestWindowEnd},
		{"token lease", limits.RateState{TokenLimit: 100, TokenRemaining: 50, ResetAt: tokenWindowEnd}, tokenWindowEnd},
	} {
		t.Run(test.name, func(t *testing.T) {
			var (
				buf  fault.HeaderBuffer
				list fault.HeaderList
			)
			headers := http.Header{}
			list.Apply(headers, list.Rate(buf[:0], "anthropic", withCacheRequestAllowance(test.tokens, requests), 0))
			if got := headers.Get("Anthropic-Ratelimit-Requests-Limit"); got != "10" {
				t.Fatalf("request limit header = %q", got)
			}
			if got := headers.Get("Anthropic-Ratelimit-Requests-Reset"); got != test.want.Format(time.RFC3339) {
				t.Fatalf("request reset header = %q, want %s", got, test.want.Format(time.RFC3339))
			}
		})
	}
}

func TestResponseCacheUnaryReplayControlsAndOutage(t *testing.T) {
	h, m := cacheHarness(t)
	h.rt.mu.Lock()
	key := h.rt.keys[fullKey]
	key.Policy.ResponseMetadata = true
	h.rt.keys[fullKey] = key
	h.rt.mu.Unlock()
	var first string
	for i, tc := range []struct {
		name, prompt, control, result string
		reason                        usage.CacheLookupReason
		calls                         int
		failRead, failWrite           bool
	}{
		{"healthy empty cache", "hello", "", "miss", "", 1, false, false},
		{"replay", "hello", "", "hit", "", 0, false, false},
		{"refresh", "hello", "no-cache", "miss", usage.CacheLookupRefresh, 1, false, false},
		{"opt out takes precedence over refresh", "hello", "no-store,no-cache", "bypass", usage.CacheLookupNoStore, 1, false, false},
		{"lookup failure", "hello", "", "bypass", usage.CacheLookupUnavailable, 1, true, false},
		{"begin refresh failure", "hello", "no-cache", "bypass", usage.CacheLookupUnavailable, 1, true, false},
		{"write failure still reports an ordinary miss", "another prompt", "", "miss", "", 1, false, true},
		{"write failure preserves the refresh reason", "hello", "no-cache", "miss", usage.CacheLookupRefresh, 1, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m.mu.Lock()
			m.fail, m.failWrite = tc.failRead, tc.failWrite
			m.mu.Unlock()
			body := []byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%q}]}`, routeSlug, tc.prompt))
			before := h.mock.count("a") + h.mock.count("b")
			r := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, body, map[string]string{"X-OLP-Cache": tc.control})
			defer r.Body.Close()
			b, err := io.ReadAll(r.Body)
			if err != nil || r.StatusCode != http.StatusOK || r.Header.Get("X-OLP-Cache") != tc.result {
				t.Fatalf("status=%d cache=%q body=%s read error=%v", r.StatusCode, r.Header.Get("X-OLP-Cache"), b, err)
			}
			ev := h.sink.await(t, i+1)[i]
			accounting, err := events.UsageEvent(ev)
			if err != nil || accounting == nil || accounting.CacheResult != tc.result || accounting.CacheLookupReason != tc.reason {
				t.Fatalf("unexpected cache accounting result=%q reason=%q, error=%v", ev.CacheResult, ev.CacheLookupReason, err)
			}
			if calls := h.mock.count("a") + h.mock.count("b") - before; calls != tc.calls {
				t.Fatalf("provider dispatches=%d want=%d", calls, tc.calls)
			}
			if i == 0 {
				first = string(b)
			}
			if tc.result == "hit" {
				if string(b) != first || r.Header.Get("X-OLP-Cost") != "0" || r.Header.Get("X-OLP-Attempts") != "0" {
					t.Fatalf("incorrect replay: headers=%v body=%s", r.Header, b)
				}
				if len(ev.Attempts) != 1 || ev.Attempts[0].Class != "cache_hit" || ev.Attempts[0].Cache == nil {
					t.Fatalf("cache accounting %#v", ev.Attempts)
				}
			}
		})
	}
}

func TestExactCacheUsesEffectiveChatChoiceCount(t *testing.T) {
	for _, tc := range []struct {
		name, defaultN, callerFields string
		choices, calls               int
		results                      [2]string
		reason                       usage.CacheLookupReason
	}{
		{"effective multiple choices", "2", "", 2, 2, [2]string{"bypass", "bypass"}, usage.CacheLookupMultipleChoices},
		{"requested multiple choices", "1", `,"n":2`, 2, 2, [2]string{"bypass", "bypass"}, usage.CacheLookupMultipleChoices},
		{"one choice", "1", "", 1, 1, [2]string{"miss", "hit"}, ""},
		{"caller overrides default", "2", `,"n":1`, 1, 1, [2]string{"miss", "hit"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, memory := cacheHarness(t)
			h.republish(func(s *routing.Snapshot, a, _ routing.Provider) {
				a.ParameterDefaults = map[string]json.RawMessage{"n": json.RawMessage(tc.defaultN)}
				s.Providers[a.ID] = a
			})
			choices := [...]string{
				`{"index":0,"message":{"role":"assistant","content":"first"},"finish_reason":"stop"}`,
				`{"index":1,"message":{"role":"assistant","content":"second"},"finish_reason":"stop"}`,
			}
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					N int `json:"n"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.N != tc.choices {
					t.Errorf("provider request n = %d, want %d (decode error: %v)", request.N, tc.choices, err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":%q,"choices":[%s],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`, modelA, strings.Join(choices[:request.N], ","))
			})
			body := []byte(`{"model":"` + routeSlug + `","messages":[{"role":"user","content":"hello"}]` + tc.callerFields + `}`)
			for i, want := range tc.results {
				response := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, body, nil)
				var reply struct {
					Choices []json.RawMessage `json:"choices"`
				}
				err := json.NewDecoder(response.Body).Decode(&reply)
				response.Body.Close()
				if got := response.Header.Get("X-OLP-Cache"); response.StatusCode != http.StatusOK || got != want || err != nil || len(reply.Choices) != tc.choices {
					t.Errorf("request %d: status=%d cache=%q choices=%d decode error=%v, want status=200 cache=%q choices=%d", i+1, response.StatusCode, got, len(reply.Choices), err, want, tc.choices)
				}
				if ev := h.sink.await(t, i+1)[i]; ev.CacheResult != want || ev.CacheLookupReason != tc.reason {
					t.Errorf("request %d: accounting cache=%q reason=%q, want %q / %q", i+1, ev.CacheResult, ev.CacheLookupReason, want, tc.reason)
				}
			}
			if got := h.mock.count("a"); got != tc.calls {
				t.Errorf("provider calls = %d, want %d", got, tc.calls)
			}
			if tc.choices > 1 {
				memory.mu.Lock()
				commands := memory.commands
				memory.mu.Unlock()
				if commands != 0 {
					t.Errorf("cache backend received %d commands for a bypassed request", commands)
				}
			}
		})
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
							provider.Kind, provider.ProfileID = connectors.Kind(tc.kind), tc.profile
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
						serveBedrock(t, h)
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

var (
	BedrockConverse       func(*Server) http.HandlerFunc
	BedrockConverseStream func(*Server) http.HandlerFunc
)

func serveBedrock(t *testing.T, h *harness) {
	t.Helper()
	credentials := map[string][]byte{}
	for _, provider := range h.rt.release.Snapshot().Providers() {
		credentials[*provider.ActiveCredential] = []byte(`{"access_key_id":"TESTBEDROCKKEY123456","secret_access_key":"test-bedrock-secret-for-cache"}`)
	}
	var err error
	h.rt.release, err = release.NewRelease(h.rt.release.ID, h.rt.release.Sequence, h.rt.release.Snapshot().Clone(), credentials)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	RegisterGateway(h.gateway, mux)
	mux.HandleFunc("POST /bedrock/model/{model}/converse", BedrockConverse(h.gateway))
	mux.HandleFunc("POST /bedrock/model/{model}/converse-stream", BedrockConverseStream(h.gateway))
	h.server = httptest.NewServer(mux)
	t.Cleanup(h.server.Close)
}

func bedrockEvent(t *testing.T, kind string, payload any) *eventstream.Message {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	h := eventstream.Headers{}
	h.Set(":message-type", eventstream.StringValue("event"))
	h.Set(":event-type", eventstream.StringValue(kind))
	return &eventstream.Message{Headers: h, Payload: raw}
}

func bedrockMetadata(payload string) *eventstream.Message {
	h := eventstream.Headers{}
	h.Set(":message-type", eventstream.StringValue("event"))
	h.Set(":event-type", eventstream.StringValue("metadata"))
	return &eventstream.Message{Headers: h, Payload: []byte(payload)}
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

func TestCachedTransformedBedrockRejectsInvalidUsage(t *testing.T) {
	for _, mode := range []string{"none", "exact", "prompt"} {
		for _, valid := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/valid=%t", mode, valid), func(t *testing.T) {
				h := strictHarness(t, func(snapshot *routing.Snapshot) {
					for id, provider := range snapshot.Providers {
						provider.Kind, provider.ProfileID, provider.ProfileRevision, provider.VendorID = "bedrock", "bedrock-converse", connectors.ProfileRevision, ""
						provider.CloudRegion, provider.AuthMode = "us-east-1", "static"
						provider.Capabilities = []routing.Capability{{Model: provider.Capabilities[0].Model, Operation: "generation", Surface: "bedrock", Mode: "unary"}}
						snapshot.Providers[id] = provider
					}
					route := snapshot.Routes[routeSlug]
					route.Fidelity.Mode = routing.FidelityTransformed
					if mode == "prompt" {
						route.PromptCache = &cache.PromptPolicy{}
					}
					snapshot.Routes[routeSlug] = route
				})
				if mode == "exact" {
					enableTestCache(t, h)
				}
				serveBedrock(t, h)
				usage := `{"inputTokens":2}`
				if valid {
					usage = `{"inputTokens":2,"outputTokens":3,"totalTokens":5}`
				}
				h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"output":{"message":{"role":"assistant","content":[{"text":"hello"}]}},"stopReason":"end_turn","usage":%s}`, usage)
				})
				response := h.do(t.Context(), "POST", "/bedrock/model/team-chat/converse", fullKey, []byte(`{"messages":[{"role":"user","content":[{"text":"hello"}]}],"inferenceConfig":{"maxTokens":16}}`), nil)
				body, _ := io.ReadAll(response.Body)
				response.Body.Close()
				want := http.StatusBadGateway
				if valid {
					want = http.StatusOK
				}
				if response.StatusCode != want {
					t.Fatalf("status=%d body=%s, want %d", response.StatusCode, body, want)
				}
				evidence := h.sink.last(t)
				if valid {
					if evidence.Outcome != "success" || len(evidence.Attempts) != 1 || evidence.Attempts[0].Usage == nil {
						t.Fatalf("successful usage missing: %+v", evidence)
					}
				} else if evidence.Outcome != "failure" || len(evidence.Attempts) != 1 || evidence.Attempts[0].Class != "protocol" {
					t.Fatalf("malformed usage recorded as success: %+v", evidence)
				}
			})
		}
	}
}

type cachedAnswer struct {
	result, age, body string
	asked             bool
}

type cacheCallers struct {
	t *testing.T
	h *harness
}

func (c cacheCallers) ask(key, body string, headers map[string]string) cachedAnswer {
	c.t.Helper()
	before := c.h.mock.count("a") + c.h.mock.count("b")
	r := c.h.do(c.t.Context(), http.MethodPost, "/v1/chat/completions", key, []byte(body), headers)
	defer r.Body.Close()
	raw, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusOK {
		c.t.Fatalf("%d %s", r.StatusCode, raw)
	}
	return cachedAnswer{result: r.Header.Get("X-OLP-Cache"), age: r.Header.Get("Age"), body: string(raw), asked: c.h.mock.count("a")+c.h.mock.count("b") != before}
}

func (c cacheCallers) stores(what, key, body string, headers map[string]string) cachedAnswer {
	c.t.Helper()
	answer := c.ask(key, body, headers)
	if answer.result != "miss" || !answer.asked || answer.age != "" {
		c.t.Fatalf("%s: got %s (provider asked %v, age %q), want a miss that asks the provider", what, answer.result, answer.asked, answer.age)
	}
	return answer
}

func (c cacheCallers) replays(what, key, body string, headers map[string]string, stored cachedAnswer) {
	c.t.Helper()
	answer := c.ask(key, body, headers)
	if answer.result != "hit" || answer.asked || answer.body != stored.body {
		c.t.Fatalf("%s: got %s (provider asked %v), want the stored reply without asking the provider", what, answer.result, answer.asked)
	}
	if age, err := strconv.Atoi(answer.age); err != nil || age < 0 {
		c.t.Fatalf("%s: a replayed reply states age %q", what, answer.age)
	}
}

func (c cacheCallers) publish(policy cache.Policy, edit func(*routing.Route)) {
	c.t.Helper()
	c.h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
		route := s.Routes[routeSlug]
		route.Cache = &policy
		if edit != nil {
			edit(&route)
		}
		s.Routes[routeSlug] = route
	})
}

func TestACachedReplyAnswersOnlyTheScopeHeadersAndReleaseThatStoredIt(t *testing.T) {
	h, _ := cacheHarness(t)
	project := uuid.NewString()
	inference := keyauthority.KeyPolicy{Scopes: []string{"inference"}}
	h.rt.mu.Lock()
	h.rt.keys["colleague"] = keyauthority.Authority{ID: uuid.NewString(), Policy: inference}
	h.rt.keys["member"] = keyauthority.Authority{ID: uuid.NewString(), ProjectID: &project, Policy: inference}
	h.rt.keys["teammate"] = keyauthority.Authority{ID: uuid.NewString(), ProjectID: &project, Policy: inference}
	h.rt.mu.Unlock()
	callers := cacheCallers{t: t, h: h}
	chat := func(more, content string) string {
		return `{"model":"` + routeSlug + `",` + more + `"messages":[{"role":"user","content":"` + content + `"}]}`
	}
	hello := chat("", "hello")

	stored := callers.stores("the first request", fullKey, hello, nil)
	callers.replays("the same request", fullKey, hello, nil, stored)
	callers.replays("a header no provider reads", fullKey, hello, map[string]string{"User-Agent": "another-client"}, stored)
	callers.stores("another key under key scope", "colleague", hello, nil)
	beta := map[string]string{"OpenAI-Beta": "responses=v9"}
	optedIn := callers.stores("a provider opt-in header", fullKey, hello, beta)
	callers.replays("the same opt-in header", fullKey, hello, beta, optedIn)
	callers.stores("another message", fullKey, chat("", "goodbye"), nil)

	callers.publish(cache.Policy{Mode: "exact"}, func(route *routing.Route) { route.OverallTimeout = 4000 })
	callers.stores("the same request after the route changed", fullKey, hello, nil)

	h.republish(func(s *routing.Snapshot, a, _ routing.Provider) {
		a.RevisionID = uuid.NewString()
		s.Providers[a.ID] = a
	})
	callers.stores("the same request after the provider changed", fullKey, hello, nil)

	callers.publish(cache.Policy{Mode: "exact", Scope: "route"}, nil)
	shared := callers.stores("the first request under route scope", fullKey, hello, nil)
	callers.replays("another key under route scope", "colleague", hello, nil, shared)

	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusInternalServerError)
	})
	if answer := callers.ask(fullKey, chat("", "served elsewhere"), nil); answer.result != "miss" || h.mock.count("b") != 1 {
		t.Fatalf("fallback answered %s after %d calls to the second provider", answer.result, h.mock.count("b"))
	}
	h.mock.set("a", completion(modelA, answerText))
	callers.stores("a request the fallback provider answered", fullKey, chat("", "served elsewhere"), nil)

	callers.publish(cache.Policy{Mode: "exact", Scope: "project"}, func(route *routing.Route) { route.ProjectID = &project })
	owned := callers.stores("the first request under project scope", "member", hello, nil)
	callers.replays("another key of the project", "teammate", hello, nil, owned)
	callers.publish(cache.Policy{Mode: "exact"}, nil)
	callers.stores("a project key under key scope", "member", hello, nil)
	callers.stores("another project key under key scope", "teammate", hello, nil)

	stream, err := fixtures.Files.ReadFile("streams/openai-chat.sse")
	if err != nil {
		t.Fatal(err)
	}
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(stream)
	})
	callers.publish(cache.Policy{Mode: "exact", Streaming: "replay"}, nil)
	plain := callers.stores("a stream", "member", chat(`"stream":true,`, "hello"), nil)
	callers.replays("the same stream", "member", chat(`"stream":true,`, "hello"), nil, plain)
	metered := callers.stores("the same stream asking for usage", "member", chat(`"stream":true,"stream_options":{"include_usage":true},`, "hello"), nil)
	if strings.Contains(plain.body, `"usage":{`) || !strings.Contains(metered.body, `"usage":{`) {
		t.Fatalf("usage reached the caller that did not ask or missed the one that did:\n%s\n%s", plain.body, metered.body)
	}
	callers.replays("the same stream asking for usage", "member", chat(`"stream":true,"stream_options":{"include_usage":true},`, "hello"), nil, metered)
}

func TestACacheHitReportsTheSpendItAvoidedAndCountsAgainstBudgetsOnlyWhenTheRouteSaysSo(t *testing.T) {
	h, _ := cacheHarness(t)
	callers := cacheCallers{t: t, h: h}
	var kind string
	h.republish(func(_ *routing.Snapshot, a, _ routing.Provider) { kind = string(a.Kind) })
	h.rt.inputs = &routinginputs.Inputs{RefreshedAt: time.Now(), Prices: []rates.RoutingPrice{
		*rates.NewRoutingPrice(rates.RoutingPrice{Price: rates.Price{ProviderKind: kind, Model: modelA, Operation: "generation", InputPerMillion: new("1000000"), OutputPerMillion: new("2000000"), Currency: "USD"}}),
	}}
	body := `{"model":"` + routeSlug + `","messages":[{"role":"user","content":"hello"}]}`
	hit := func(policy cache.Policy) *usage.CacheEvidence {
		t.Helper()
		callers.publish(policy, nil)
		stored := callers.stores("the first request", fullKey, body, nil)
		callers.replays("the same request", fullKey, body, nil, stored)
		attempts := h.sink.last(t).Attempts
		if len(attempts) != 1 || attempts[0].Class != "cache_hit" || attempts[0].Cache == nil {
			t.Fatalf("a replayed reply recorded %#v", attempts)
		}
		return attempts[0].Cache
	}
	free := hit(cache.Policy{Mode: "exact"})
	if free.AvoidedCost != "8" || free.Currency != "USD" || free.BudgetCost != "" || free.BudgetCharge != "" {
		t.Fatalf("a hit that counts against nothing recorded %#v, want 2 input and 3 output tokens avoided at 1 and 2 each", free)
	}
	counted := hit(cache.Policy{Mode: "exact", CountHitsAgainst: []string{"requests", "budgets"}})
	if counted.AvoidedCost != "8" || counted.BudgetCost != "8" || counted.BudgetCharge != "8" {
		t.Fatalf("a hit that counts against budgets recorded %#v", counted)
	}
}
