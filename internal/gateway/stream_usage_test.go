package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/upstream"
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
	for _, family := range []wire.Format{wire.Chat, wire.Responses} {
		for _, usage := range []struct {
			name string
			want *wire.Usage
		}{
			{"unreported", nil},
			{"zero", &wire.Usage{}},
			{"reported", &wire.Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5}},
		} {
			t.Run(string(family)+"/"+usage.name, func(t *testing.T) {
				h := newHarness(t, Config{})
				usageJSON := "null"
				if usage.want != nil {
					names := []string{"prompt_tokens", "completion_tokens"}
					if family == wire.Responses {
						names = []string{"input_tokens", "output_tokens"}
					}
					usageJSON = fmt.Sprintf("{%q:%d,%q:%d,\"total_tokens\":%d}", names[0], usage.want.InputTokens, names[1], usage.want.OutputTokens, usage.want.TotalTokens)
				}
				body := `{"model":"team-chat","messages":[{"role":"user","content":"hi"}],"stream":true}`
				// Usage-only chat frames are hidden unless requested by the
				// client, but must still survive EOF without [DONE].
				encodedWire := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":\"stop\"}]}\n\n"
				encodedWire += "data: {\"choices\":[],\"usage\":" + usageJSON + "}\n\n"
				if family == wire.Responses {
					body = `{"model":"team-chat","input":"hi","stream":true}`
					encodedWire = "data: {\"type\":\"response.created\"}\n\n"
					encodedWire += "data: {\"type\":\"response.failed\",\"response\":{\"object\":\"response\",\"status\":\"failed\",\"output\":[],\"usage\":" + usageJSON + ",\"error\":{\"message\":\"failed\"}}}\n\n"
				}
				h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, encodedWire)
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
	for _, family := range []wire.Format{wire.Chat, wire.Responses} {
		t.Run(string(family), func(t *testing.T) {
			h := newHarness(t, Config{})
			body := `{"model":"team-chat","messages":[{"role":"user","content":"hi"}],"stream":true}`
			encodedWire := "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\ndata: [DONE]\n\n"
			if family == wire.Responses {
				body = `{"model":"team-chat","input":"hi","stream":true}`
				encodedWire = "data: {\"type\":\"response.completed\",\"response\":{\"object\":\"response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5}}}\n\n"
			}
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, encodedWire)
			})
			r := httptest.NewRequest(http.MethodPost, "/v1"+endpointPath(family), strings.NewReader(body)).WithContext(t.Context())
			r.Header.Set("Authorization", "Bearer "+fullKey)
			r.Header.Set("Content-Type", "application/json")
			w := &unaryResponseWriter{ResponseRecorder: httptest.NewRecorder(), writeErr: io.ErrClosedPipe, beforeDelivery: func() {}}
			h.gateway.inference(family)(w, r)
			env := h.sink.last(t)
			wantClass := classCancelled
			if family == wire.Responses {
				wantClass = classSuccess
			}
			if env.Outcome != "cancelled" || env.Status != 0 || len(env.Attempts) != 1 || env.Attempts[0].Class != wantClass || h.mock.count("b") != 0 {
				t.Fatalf("client failure outcome changed: %+v", env)
			}
			if env.Usage == nil || env.Usage.TotalTokens != 5 || env.Attempts[0].Usage == nil || env.Attempts[0].Usage.TotalTokens != 5 {
				t.Fatalf("client failure discarded usage: %+v", env)
			}
			wantSuccesses := int64(0)
			if family == wire.Responses {
				wantSuccesses = 1
			}
			stats := h.gateway.Health().ProviderHealth(time.Minute)[env.Attempts[0].ProviderID]
			if stats.Attempts != 1 || stats.Successes != wantSuccesses || stats.TransportErrors != 0 || stats.ServerErrors != 0 {
				t.Fatalf("client failure changed native provider evidence: %+v", stats)
			}
		})
	}
}

func TestResponsesClientWriteFailureBeforeTerminalRetainsPartialEvidence(t *testing.T) {
	h := newHarness(t, Config{})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"partial-response\",\"object\":\"response\",\"status\":\"in_progress\",\"output\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5}}}\n\n")
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"team-chat","input":"hi","stream":true}`)).WithContext(t.Context())
	r.Header.Set("Authorization", "Bearer "+fullKey)
	r.Header.Set("Content-Type", "application/json")
	w := &unaryResponseWriter{ResponseRecorder: httptest.NewRecorder(), writeErr: io.ErrClosedPipe, beforeDelivery: func() {}}
	h.gateway.inference(wire.Responses)(w, r)
	env := h.sink.last(t)
	if env.Outcome != "cancelled" || len(env.Attempts) != 1 || env.Attempts[0].Class != classCancelled || h.mock.count("b") != 0 {
		t.Fatalf("partial native work became a completed provider outcome: %+v", env)
	}
	fact := env.Attempts[0]
	if !fact.UsageObserved || fact.UsageComplete || !fact.BillingUncertain || fact.Usage == nil || fact.Usage.TotalTokens != 5 {
		t.Fatalf("delivery failure changed partial native accounting evidence: %+v", fact)
	}
	stats := h.gateway.Health().ProviderHealth(time.Minute)[fact.ProviderID]
	if stats.Attempts != 1 || stats.Successes != 0 || stats.TransportErrors != 0 || stats.ServerErrors != 0 {
		t.Fatalf("partial native work changed provider health: %+v", stats)
	}
}

func TestResponseNativeWriterFailureKeepsRetainedRetryFence(t *testing.T) {
	for _, retained := range []bool{true, false} {
		for _, phase := range []string{"completed", "failed", "partial"} {
			t.Run(fmt.Sprintf("retained=%t/%s", retained, phase), func(t *testing.T) {
				h := newHarness(t, Config{})
				kind, status := phase, phase
				if phase == "partial" {
					kind, status = "created", "in_progress"
				}
				nativeFailure := ""
				if phase == "failed" {
					nativeFailure = `,"error":{"type":"server_error","code":"server_error","message":"native generation failed"}`
				}
				encodedWire := fmt.Sprintf("data: {\"type\":\"response.%s\",\"response\":{\"id\":\"native-response\",\"object\":\"response\",\"status\":%q,\"output\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5}%s}}\n\n", kind, status, nativeFailure)
				h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, encodedWire)
				})
				parsed, err := openai.Parse(wire.Responses, fmt.Appendf(nil, `{"model":"team-chat","input":"hi","stream":true,"store":%t}`, retained))
				if err != nil {
					t.Fatal(err)
				}
				release := h.rt.Release()
				route := release.Snapshot.Routes[routeSlug]
				target := route.Targets[0]
				provider := release.Snapshot.Providers[target.ProviderID]
				x := &call{
					request: request{release: release, startedAt: time.Now()},
					family:  wire.Responses,

					providerState: true, routeLeg: routeLeg{route: &route}, generation: generationCall{parsed: parsed,

						emit: func([]byte) error { return errClientWrite }},
				}
				attempt := routing.Attempt{ProviderID: provider.ID, ProviderRevisionID: provider.RevisionID, UpstreamModel: target.ProviderModel, Timeout: 2 * time.Second}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				fact, _, failure := h.gateway.attempt(ctx, x, attempt, &provider, provider.Slots[0], AttemptFact{Ordinal: 1, StartedAt: time.Now()})
				if failure == nil || failure.class != classCancelled || !failure.dispatched || !failure.accepted || h.mock.count("a") != 1 {
					t.Fatalf("native client failure changed actual delivery outcome: failure=%+v fact=%+v", failure, fact)
				}
				wantAcceptance, wantClass := upstream.Terminal, classSuccess
				if phase == "failed" {
					wantClass = classUpstreamServer
				} else if phase == "partial" {
					wantAcceptance, wantClass = upstream.Accepted, classCancelled
				}
				if failure.acceptance != wantAcceptance || fact.Class != wantClass {
					t.Fatalf("native outcome changed retry evidence: failure=%+v fact=%+v", failure, fact)
				}
				if failure.noRetry != retained {
					t.Fatalf("accepted retained invocation lost its retry fence: retained=%t phase=%s noRetry=%t", retained, phase, failure.noRetry)
				}
			})
		}
	}
}
