package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"
)

func TestLegacyCompletionsGateway(t *testing.T) {
	for _, vendor := range []string{"openai", "vllm", "tgi", "fireworks", "together", "chat_mapping"} {
		native := vendor != "chat_mapping"
		t.Run(vendor, func(t *testing.T) {
			h := strictHarness(t, func(s *routing.Snapshot) {
				for id, p := range s.Providers {
					p.VendorID = "openai"
					p.Kind = "openai"
					p.ProfileID = "openai-chat"
					if native {
						p.VendorID = vendor
						p.ProfileID = vendor + "-completions"
						if vendor != "openai" {
							p.Kind = "openai_compatible"
						}
					}
					s.Providers[id] = p
				}
				if !native {
					r := s.Routes[routeSlug]
					r.Fidelity.Mode = routing.FidelityTransformed
					s.Routes[routeSlug] = r
				}
			})
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				want := "/a/v1/completions"
				if !native {
					want = "/a/v1/chat/completions"
				}
				if r.URL.Path != want || !strings.Contains(string(body), modelA) || r.Header.Get("Authorization") != "Bearer "+secretA {
					t.Errorf("dispatch: %s %s", r.URL.Path, body)
				}
				w.Header().Set("Content-Type", "application/json")
				choices := `[{"index":0,"text":"hello","finish_reason":"stop"}]`
				object := "text_completion"
				if !native {
					choices = `[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]`
					object = "chat.completion"
				}
				io.WriteString(w, `{"id":"cmpl-1","object":"`+object+`","model":"`+modelA+`","choices":`+choices+`,"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
			})
			resp := h.do(t.Context(), http.MethodPost, "/v1/completions", fullKey, []byte(`{"model":"`+routeSlug+`","prompt":"hello","max_tokens":8}`), nil)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || !strings.Contains(string(body), `"text":"hello"`) || !strings.Contains(string(body), routeSlug) {
				t.Fatalf("response %d %s", resp.StatusCode, body)
			}
			for _, key := range []string{readKey, otherKey} {
				resp = h.do(t.Context(), http.MethodPost, "/v1/completions", key, []byte(`{"model":"`+routeSlug+`","prompt":"hello"}`), nil)
				resp.Body.Close()
				if resp.StatusCode < 400 {
					t.Fatal("unauthorized completion")
				}
			}
			if h.mock.count("a") != 1 {
				t.Fatalf("unauthorized dispatch: %d", h.mock.count("a"))
			}
			if !native {
				resp = h.do(t.Context(), http.MethodPost, "/v1/completions", fullKey, []byte(`{"model":"`+routeSlug+`","prompt":"hello","echo":true}`), nil)
				body, _ = io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode < 400 || h.mock.count("a") != 1 || h.mock.count("b") != 0 {
					t.Fatalf("unmapped request dispatched: %d %s", resp.StatusCode, body)
				}
			}
		})
	}
}

func TestLegacyCompletionAutomaticUsesMappedEstimate(t *testing.T) {
	for _, tightContext := range []bool{false, true} {
		t.Run(fmt.Sprint(tightContext), func(t *testing.T) {
			h := newHarness(t, Config{})
			routeEstimateTargets(h, estimateTarget{name: "a", model: "gpt-4o", slots: 1})
			if tightContext {
				for id, p := range h.rt.release.Snapshot.Providers {
					p.Models = map[string]json.RawMessage{"gpt-4o": json.RawMessage(`{"context_length":7}`)}
					h.rt.release.Snapshot.Providers[id] = p
				}
			}
			h.mock.set("a", completion("gpt-4o", "hello"))
			resp := h.do(t.Context(), http.MethodPost, "/v1/completions", fullKey, []byte(`{"model":"`+routeSlug+`","prompt":"hello","max_tokens":1}`), nil)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if tightContext {
				if resp.StatusCode < 400 || h.mock.count("a") != 0 {
					t.Fatalf("mapped context not checked: %d %s", resp.StatusCode, body)
				}
				return
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("automatic completion: %d %s", resp.StatusCode, body)
			}
			mapped, err := protocols.Parse(wire.Chat, []byte(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"hello"}],"max_tokens":1}`), "")
			if err != nil {
				t.Fatal(err)
			}
			want := estimate.Walk(mapped).Estimate(estimate.ForModel("gpt-4o"), nil).Input
			env := h.sink.last(t)
			if len(env.Attempts) != 1 || env.Attempts[0].EstimatedInputTokens != want {
				t.Fatalf("automatic estimate: %+v; want %d", env.Attempts, want)
			}
		})
	}
}

func TestLegacyCompletionMappedDefaultReservation(t *testing.T) {
	for _, field := range []string{"", `,"max_tokens":null`} {
		source, err := protocols.Parse(wire.Completions, []byte(`{"model":"route","prompt":"hello"`+field+`}`), "")
		if err != nil {
			t.Fatal(err)
		}
		body, family, err := protocols.EncodeTarget(source, wire.Chat, "openai", "openai", "upstream", nil)
		if err != nil {
			t.Fatal(err)
		}
		target, err := protocols.Parse(family, body, "")
		if err != nil {
			t.Fatal(err)
		}
		x := &call{generation: generationCall{parsed: source}}
		admitted, demand := x.preparedEstimate("gpt-4o", x.summarize(target))
		if admitted.reply != 16 || admitted.reserve > 100 || demand.MaxOutputTokens == nil || *demand.MaxOutputTokens != 16 {
			t.Fatalf("mapped default over-reserved: %+v %+v", admitted, demand)
		}
	}
}

func TestLegacyCompletionNativePolicyCoverage(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, phase := range []string{guardrails.Input, guardrails.Output} {
			for _, blocked := range []bool{false, true} {
				t.Run(fmt.Sprintf("legacy=%v/%s/blocked=%v", legacy, phase, blocked), func(t *testing.T) {
					h := strictHarness(t, func(s *routing.Snapshot) {
						for id, p := range s.Providers {
							p.Kind, p.VendorID, p.ProfileID = "openai", "openai", "openai-completions"
							s.Providers[id] = p
						}
						if legacy {
							r := s.Routes[routeSlug]
							r.ContentPolicy = &contentpolicy.Policy{Rules: []contentpolicy.Rule{{ID: "secret", Phase: phase, Action: contentpolicy.ActionBlock, Pattern: "secret"}}}
							s.Routes[routeSlug] = r
						}
					})
					if !legacy {
						installTestGuardrail(t, h, phase, guardrails.Block, "builtin.regex", `{"rules":[{"id":"secret","pattern":"secret"}]}`)
					}
					prompt, reply := "hello", "safe"
					if blocked {
						if phase == guardrails.Input {
							prompt = "secret"
						} else {
							reply = "secret"
						}
					}
					h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path != "/a/v1/completions" {
							t.Errorf("not native: %s", r.URL.Path)
						}
						w.Header().Set("Content-Type", "application/json")
						body, _ := json.Marshal(map[string]any{"id": "c", "object": "text_completion", "model": modelA, "choices": []any{map[string]any{"index": 0, "text": reply, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 2, "completion_tokens": 1, "total_tokens": 3}})
						w.Write(body)
					})
					body, _ := json.Marshal(map[string]any{"model": routeSlug, "prompt": prompt, "max_tokens": 8})
					resp := h.do(t.Context(), http.MethodPost, "/v1/completions", fullKey, body, nil)
					out, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					if blocked {
						if resp.StatusCode < 400 {
							t.Fatalf("policy bypass: %s", out)
						}
					} else if resp.StatusCode != http.StatusOK {
						t.Fatalf("native text refused: %d %s", resp.StatusCode, out)
					}
					if blocked && phase == guardrails.Input && h.mock.count("a")+h.mock.count("b") != 0 {
						t.Fatal("blocked input dispatched")
					}
				})
			}
		}
	}
}

func tgiCompletionHarness(t *testing.T) *harness {
	return strictHarness(t, func(s *routing.Snapshot) {
		for id, p := range s.Providers {
			p.Kind, p.VendorID, p.ProfileID = "openai_compatible", "tgi", "tgi-completions"
			s.Providers[id] = p
		}
	})
}

func TestTGICompletionRefusalsBeforeDispatch(t *testing.T) {
	for _, fields := range []string{`"prompt":"hello","n":2`, `"prompt":"hello","best_of":1`, `"prompt":"hello","echo":true`, `"prompt":"hello","logprobs":0`, `"prompt":["one","two"],"stream":true`} {
		h := tgiCompletionHarness(t)
		resp := h.do(t.Context(), http.MethodPost, "/v1/completions", fullKey, []byte(`{"model":"`+routeSlug+`",`+fields+`}`), nil)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 400 || h.mock.count("a")+h.mock.count("b") != 0 {
			t.Fatalf("TGI request dispatched: %d %s", resp.StatusCode, body)
		}
	}
}

func TestTGICompletionStreamEmptyFinishReason(t *testing.T) {
	h := tgiCompletionHarness(t)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"object":"text_completion","model":"`+modelA+`","choices":[{"index":0,"text":"hel","finish_reason":"","logprobs":null}]}`+"\n\n")
		io.WriteString(w, `data: {"object":"text_completion","model":"`+modelA+`","choices":[{"index":0,"text":"lo","finish_reason":"eos_token","logprobs":null}],"usage":{"prompt_tokens":2,"completion_tokens":2,"total_tokens":4}}`+"\n\ndata: [DONE]\n\n")
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/completions", fullKey, []byte(`{"model":"`+routeSlug+`","prompt":"hello","max_tokens":8,"stream":true}`), nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"text":"hel"`) || !strings.Contains(string(body), `"text":"lo"`) || !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("TGI stream: %d %s", resp.StatusCode, body)
	}
	if usage := h.sink.last(t).Usage; usage == nil || usage.TotalTokens != 4 {
		t.Fatalf("TGI usage: %+v", usage)
	}
}

func TestNativeCompletionOutputGuardrailsRefuseLogprobs(t *testing.T) {
	for _, requested := range []bool{true, false} {
		h := strictHarness(t, func(s *routing.Snapshot) {
			for id, p := range s.Providers {
				p.Kind, p.VendorID, p.ProfileID = "openai", "openai", "openai-completions"
				s.Providers[id] = p
			}
		})
		installTestGuardrail(t, h, guardrails.Output, guardrails.Block, "builtin.regex", `{"rules":[{"id":"secret","pattern":"secret"}]}`)
		h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"c","object":"text_completion","model":"`+modelA+`","choices":[{"index":0,"text":"safe","finish_reason":"stop","logprobs":{"top_logprobs":[{"secret":-1}]}}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
		})
		extra := ""
		if requested {
			extra = `,"logprobs":1`
		}
		resp := h.do(t.Context(), http.MethodPost, "/v1/completions", fullKey, []byte(`{"model":"`+routeSlug+`","prompt":"hello","max_tokens":8`+extra+`}`), nil)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 400 || strings.Contains(string(body), `"secret"`) {
			t.Fatalf("token alternatives escaped output guardrail: %d %s", resp.StatusCode, body)
		}
		if requested && h.mock.count("a")+h.mock.count("b") != 0 {
			t.Fatal("unsupported requested logprobs dispatched")
		}
	}
}

func TestLegacyCompletionOutputPolicy(t *testing.T) {
	for _, action := range []string{contentpolicy.ActionBlock, contentpolicy.ActionRedact} {
		t.Run(action, func(t *testing.T) {
			h := newHarness(t, Config{})
			setRoutePolicy(h, contentpolicy.Rule{ID: "secret", Phase: contentpolicy.PhaseOutput, Pattern: "secret", Action: action, Replacement: "[REDACTED]"})
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"c","object":"chat.completion","model":"`+modelA+`","choices":[{"index":0,"message":{"role":"assistant","content":"secret"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
			})
			resp := h.do(t.Context(), http.MethodPost, "/v1/completions", fullKey, []byte(`{"model":"`+routeSlug+`","prompt":"hello","max_tokens":8}`), nil)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if action == contentpolicy.ActionBlock {
				if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "content_policy_blocked") {
					t.Fatalf("output block: %d %s", resp.StatusCode, body)
				}
			} else if resp.StatusCode != http.StatusOK || strings.Contains(string(body), "secret") || !strings.Contains(string(body), "[REDACTED]") {
				t.Fatalf("output redaction: %d %s", resp.StatusCode, body)
			}
		})
	}
}

func TestLegacyCompletionParallelGuardrailBlocksTerminalFrame(t *testing.T) {
	h := newHarness(t, Config{})
	installTestGuardrail(t, h, guardrails.InputParallel, guardrails.Block, "webhook", `{"endpoint":"https://example.com","credential_ref":"key","content_egress":true}`)
	release := make(chan struct{})
	h.gateway.GuardrailEvaluator = guardrails.EvaluatorFunc(func(ctx context.Context, _ guardrails.Revision, _ guardrails.Evaluation) (guardrails.Verdict, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return guardrails.Verdict{}, ctx.Err()
		}
		return guardrails.Verdict{Action: "block"}, nil
	})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"started\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		w.(http.Flusher).Flush()
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/completions", fullKey, []byte(`{"model":"`+routeSlug+`","prompt":"hello","max_tokens":8,"stream":true}`), nil)
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	first, err := reader.ReadString('\n')
	close(release)
	if err != nil || !strings.Contains(first, "started") {
		t.Fatalf("first output: %s %v", first, err)
	}
	rest, _ := io.ReadAll(reader)
	if strings.Contains(string(rest), "[DONE]") || !strings.Contains(string(rest), "guardrail_blocked") {
		t.Fatalf("unsafe terminal delivery: %s", rest)
	}
}

func TestLegacyCompletionStreamRestoresInputMask(t *testing.T) {
	h := newHarness(t, Config{})
	installTestGuardrail(t, h, guardrails.Input, guardrails.Mask, "builtin.regex", `{"rules":[{"id":"secret","pattern":"secret"}]}`)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if len(request.Messages) != 1 || strings.Contains(request.Messages[0].Content, "secret") {
			t.Errorf("unmasked upstream body: %+v", request)
			return
		}
		body, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": request.Messages[0].Content}, "finish_reason": "stop"}}})
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: "+string(body)+"\n\ndata: [DONE]\n\n")
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/completions", fullKey, []byte(`{"model":"`+routeSlug+`","prompt":"a secret","max_tokens":16,"stream":true}`), nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "secret") || strings.Contains(string(body), "[OLP_") || !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("mask restoration: %d %s", resp.StatusCode, body)
	}
}
