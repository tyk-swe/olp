package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func TestChatStreamUsageOptionPreservesInternalAccounting(t *testing.T) {
	for _, options := range []string{"", "null", "{}", `{"include_usage":false}`, `{"include_usage":true}`} {
		for _, usageOnly := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/usage-only=%t", options, usageOnly), func(t *testing.T) {
				h := newHarness(t, Config{})
				// Provider defaults may request usage, but only the caller opts
				// into seeing it in the downstream stream.
				for id, provider := range h.rt.release.Snapshot.Providers {
					provider.ParameterDefaults = map[string]json.RawMessage{"stream_options": json.RawMessage(`{"include_usage":true}`)}
					h.rt.release.Snapshot.Providers[id] = provider
				}
				h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
					var input struct {
						StreamOptions struct {
							IncludeUsage bool `json:"include_usage"`
						} `json:"stream_options"`
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil || !input.StreamOptions.IncludeUsage {
						t.Errorf("upstream usage was not requested: %v", err)
					}
					w.Header().Set("Content-Type", "text/event-stream")
					const usage = `{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}`
					chunk := `{"id":"c","object":"chat.completion.chunk","model":"model-a","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":"stop"}],"usage":`
					if usageOnly {
						io.WriteString(w, "data: "+chunk+"null}\n\n")
						io.WriteString(w, `data: {"id":"c","object":"chat.completion.chunk","model":"model-a","choices":[],"usage":`+usage+"}\n\n")
					} else {
						io.WriteString(w, "data: "+chunk+usage+"}\n\n")
					}
					io.WriteString(w, "data: [DONE]\n\n")
				})
				body := `{"model":"team-chat","messages":[{"role":"user","content":"hi"}],"stream":true`
				if options != "" {
					body += `,"stream_options":` + options
				}
				resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(body+"}"), nil)
				defer resp.Body.Close()
				raw, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				wantUsage := options == `{"include_usage":true}`
				if resp.StatusCode != http.StatusOK || !strings.HasSuffix(string(raw), "data: [DONE]\n\n") || !strings.Contains(string(raw), `"content":"hello"`) || strings.Contains(string(raw), modelA) {
					t.Fatalf("stream did not complete correctly: %d %s", resp.StatusCode, raw)
				}
				if strings.Contains(string(raw), `"usage":`) != wantUsage || strings.Contains(string(raw), `"choices":[]`) != (wantUsage && usageOnly) {
					t.Fatalf("client usage option was ignored: %s", raw)
				}
				env := h.sink.last(t)
				if env.Outcome != "success" || env.Usage == nil || env.Usage.TotalTokens != 5 || len(env.Attempts) != 1 || env.Attempts[0].Usage == nil || env.Attempts[0].Usage.TotalTokens != 5 {
					t.Fatalf("internal usage accounting was lost: %+v", env)
				}
			})
		}
	}
}

func TestFailedStreamsRetainObservedUsage(t *testing.T) {
	for _, family := range []openai.Family{openai.FamilyChat, openai.FamilyResponses} {
		for _, usage := range []struct {
			name string
			want *openai.Usage
		}{
			{"unreported", nil},
			{"zero", &openai.Usage{}},
			{"reported", &openai.Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5}},
		} {
			t.Run(string(family)+"/"+usage.name, func(t *testing.T) {
				h := newHarness(t, Config{})
				usageJSON := "null"
				if usage.want != nil {
					names := []string{"prompt_tokens", "completion_tokens"}
					if family == openai.FamilyResponses {
						names = []string{"input_tokens", "output_tokens"}
					}
					usageJSON = fmt.Sprintf("{%q:%d,%q:%d,\"total_tokens\":%d}", names[0], usage.want.InputTokens, names[1], usage.want.OutputTokens, usage.want.TotalTokens)
				}
				body := `{"model":"team-chat","messages":[{"role":"user","content":"hi"}],"stream":true}`
				// Usage-only chat frames are hidden unless requested by the
				// client, but must still survive EOF without [DONE].
				wire := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":\"stop\"}]}\n\n"
				wire += "data: {\"choices\":[],\"usage\":" + usageJSON + "}\n\n"
				if family == openai.FamilyResponses {
					body = `{"model":"team-chat","input":"hi","stream":true}`
					wire = "data: {\"type\":\"response.created\"}\n\n"
					wire += "data: {\"type\":\"response.failed\",\"response\":{\"object\":\"response\",\"status\":\"failed\",\"output\":[],\"usage\":" + usageJSON + ",\"error\":{\"message\":\"failed\"}}}\n\n"
				}
				h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, wire)
				})
				r := httptest.NewRequest(http.MethodPost, "/v1"+endpointPath(family), strings.NewReader(body)).WithContext(t.Context())
				r.Header.Set("Authorization", "Bearer "+fullKey)
				r.Header.Set("Content-Type", "application/json")
				w := &unaryResponseWriter{ResponseRecorder: httptest.NewRecorder(), beforeDelivery: func() {}}
				h.gateway.inference(family)(w, r)
				if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "[DONE]") || strings.Contains(w.Body.String(), "response.completed") || !strings.Contains(w.Body.String(), `"code":"provider_protocol_error"`) {
					t.Fatalf("failed stream was not terminated with an error: %d %s", w.Code, w.Body.String())
				}
				env := h.sink.last(t)
				if env.Outcome != "failure" || !env.Committed || len(env.Attempts) != 1 || env.Attempts[0].Class != classProtocol || h.mock.count("b") != 0 {
					t.Fatalf("failed stream outcome changed: %+v", env)
				}
				if !reflect.DeepEqual(env.Usage, usage.want) || !reflect.DeepEqual(env.Attempts[0].Usage, usage.want) {
					t.Fatalf("observed usage lost: request=%+v attempt=%+v want=%+v", env.Usage, env.Attempts[0].Usage, usage.want)
				}
			})
		}
	}
}

func TestStreamClientWriteFailureRetainsObservedUsage(t *testing.T) {
	for _, family := range []openai.Family{openai.FamilyChat, openai.FamilyResponses} {
		t.Run(string(family), func(t *testing.T) {
			h := newHarness(t, Config{})
			body := `{"model":"team-chat","messages":[{"role":"user","content":"hi"}],"stream":true}`
			wire := "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\ndata: [DONE]\n\n"
			if family == openai.FamilyResponses {
				body = `{"model":"team-chat","input":"hi","stream":true}`
				wire = "data: {\"type\":\"response.completed\",\"response\":{\"object\":\"response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5}}}\n\n"
			}
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, wire)
			})
			r := httptest.NewRequest(http.MethodPost, "/v1"+endpointPath(family), strings.NewReader(body)).WithContext(t.Context())
			r.Header.Set("Authorization", "Bearer "+fullKey)
			r.Header.Set("Content-Type", "application/json")
			w := &unaryResponseWriter{ResponseRecorder: httptest.NewRecorder(), writeErr: io.ErrClosedPipe, beforeDelivery: func() {}}
			h.gateway.inference(family)(w, r)
			env := h.sink.last(t)
			if env.Outcome != "cancelled" || env.Status != 0 || len(env.Attempts) != 1 || env.Attempts[0].Class != classCancelled || h.mock.count("b") != 0 {
				t.Fatalf("client failure outcome changed: %+v", env)
			}
			if env.Usage == nil || env.Usage.TotalTokens != 5 || env.Attempts[0].Usage == nil || env.Attempts[0].Usage.TotalTokens != 5 {
				t.Fatalf("client failure discarded usage: %+v", env)
			}
		})
	}
}
