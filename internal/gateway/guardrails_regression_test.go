package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/observability"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"
)

func regressionGuardrailStream(t *testing.T, family wire.Format, action string) *guardrailStream {
	t.Helper()
	h := newHarness(t, Config{})
	installTestGuardrail(t, h, guardrails.OutputStream, action, "builtin.regex", `{"rules":[{"id":"secret","pattern":"secret"}]}`)
	route := h.rt.release.Snapshot.Routes[routeSlug]
	x := &call{request: request{release: h.rt.release}, family: family, routeLeg: routeLeg{route: &route}, guardrailEngine: h.rt.release.Snapshot.GuardrailEngine(routeSlug, ""), guardrailMasks: &guardrails.Masking{}, guardrailCtx: t.Context()}
	g, err := newGuardrailStream(x)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func collectGuardrailStream(t *testing.T, stream *guardrailStream, frames []string) [][]byte {
	t.Helper()
	var out [][]byte
	for _, frame := range frames {
		next, err := stream.push(t.Context(), []byte("data: "+frame+"\n\n"))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, next...)
	}
	next, err := stream.finish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return append(out, next...)
}

func TestGuardrailResponsesCompletionSnapshots(t *testing.T) {
	for _, kind := range []string{"output_text", "refusal"} {
		t.Run(kind, func(t *testing.T) {
			g := regressionGuardrailStream(t, wire.Responses, guardrails.Redact)
			key := "text"
			if kind == "refusal" {
				key = "refusal"
			}
			part := `{"type":"` + kind + `","` + key + `":"secret"}`
			frames := []string{
				`{"type":"response.` + kind + `.delta","output_index":0,"content_index":0,"delta":"sec"}`,
				`{"type":"response.` + kind + `.delta","output_index":0,"content_index":0,"delta":"ret"}`,
				`{"type":"response.` + kind + `.done","output_index":0,"content_index":0,"` + key + `":"secret"}`,
				`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":` + part + `}`,
				`{"type":"response.output_item.done","output_index":0,"item":{"id":"item","type":"message","content":[` + part + `]}}`,
				`{"type":"response.completed","sequence_number":9007199254740993,"response":{"output":[{"type":"message","content":[` + part + `]}]}}`,
			}
			out := collectGuardrailStream(t, g, frames)
			if len(out) != len(frames) {
				t.Fatalf("lost frames: %d", len(out))
			}
			var delta strings.Builder
			for i, frame := range out {
				fields, err := guardrailFrameFields(wire.Responses, frame)
				if err != nil || len(fields) != 1 {
					t.Fatalf("frame %d: %s %v", i, frame, err)
				}
				if i < 2 {
					delta.WriteString(fields[0].text)
				} else if fields[0].text != "[REDACTED]" {
					t.Fatalf("unredacted snapshot: %s", frame)
				}
			}
			if delta.String() != "[REDACTED]" || !bytes.Contains(out[len(out)-1], []byte(`9007199254740993`)) {
				t.Fatalf("inconsistent stream: %s", bytes.Join(out, nil))
			}
		})
	}
	// A completion event must be checked even if the upstream sent no delta.
	g := regressionGuardrailStream(t, wire.Responses, guardrails.Block)
	if _, err := g.push(t.Context(), []byte("data: {\"type\":\"response.output_text.done\",\"text\":\"secret\"}\n\n")); err == nil {
		t.Fatal("uninspected completion passed")
	}
}

func TestGuardrailStreamCandidatesStaySeparate(t *testing.T) {
	for _, family := range []wire.Format{wire.Chat, wire.GeminiStream} {
		for _, redacted := range []bool{false, true} {
			t.Run(string(family)+"/"+map[bool]string{true: "redaction", false: "no_cross_match"}[redacted], func(t *testing.T) {
				g := regressionGuardrailStream(t, family, guardrails.Redact)
				first, second := "sec", "ret"
				wantFirst, wantSecond := first, second
				if redacted {
					first, second, wantFirst, wantSecond = "safe", "secret", "safe", "[REDACTED]"
				}
				var frames []string
				if family == wire.Chat {
					frames = []string{`{"choices":[{"index":0,"delta":{"content":"` + first + `"}},{"index":1,"delta":{"content":"` + second + `"}}]}`}
				} else {
					frames = []string{`{"candidates":[{"index":0,"content":{"parts":[{"text":"` + first + `"}]}},{"index":1,"content":{"parts":[{"text":"` + second + `"}]}}]}`}
				}
				out := collectGuardrailStream(t, g, frames)
				fields, err := guardrailFrameFields(family, out[0])
				if err != nil || len(fields) != 2 || fields[0].text != wantFirst || fields[1].text != wantSecond {
					t.Fatalf("candidate corruption: %s %v", out, err)
				}
			})
		}
		// Candidate identities survive changes in their array positions.
		g := regressionGuardrailStream(t, family, guardrails.Redact)
		frames := []string{
			`{"choices":[{"index":0,"delta":{"content":"safe "}},{"index":1,"delta":{"content":"sec"}}]}`,
			`{"choices":[{"index":1,"delta":{"content":"ret"}},{"index":0,"delta":{"content":"text"}}]}`,
		}
		if family != wire.Chat {
			frames = []string{
				`{"candidates":[{"index":0,"content":{"parts":[{"text":"safe "}]}},{"index":1,"content":{"parts":[{"text":"sec"}]}}]}`,
				`{"candidates":[{"index":1,"content":{"parts":[{"text":"ret"}]}},{"index":0,"content":{"parts":[{"text":"text"}]}}]}`,
			}
		}
		texts := map[string]string{}
		for _, frame := range collectGuardrailStream(t, g, frames) {
			fields, _ := guardrailFrameFields(family, frame)
			for _, field := range fields {
				texts[field.id] += field.text
			}
		}
		prefix, suffix := "chat_", "_content"
		if family != wire.Chat {
			prefix, suffix = "gemini_", "_0"
		}
		if texts[prefix+"0"+suffix] != "safe text" || texts[prefix+"1"+suffix] != "[REDACTED]" {
			t.Fatalf("mixed candidate chunks: %v", texts)
		}
	}
}

func TestGuardrailCompletedCandidateDoesNotHoldBackOtherCandidates(t *testing.T) {
	for _, family := range []wire.Format{wire.Chat, wire.GeminiStream} {
		t.Run(string(family), func(t *testing.T) {
			g := regressionGuardrailStream(t, family, guardrails.Redact)
			first := `{"choices":[{"index":0,"delta":{"content":"safe"},"finish_reason":"stop"},{"index":1,"delta":{"content":"other"}}]}`
			chunk := `{"choices":[{"index":1,"delta":{"content":"` + strings.Repeat("a", 1024) + `"}}]}`
			if family != wire.Chat {
				first = `{"candidates":[{"index":0,"content":{"parts":[{"text":"safe"}]},"finishReason":"STOP"},{"index":1,"content":{"parts":[{"text":"other"}]}}]}`
				chunk = `{"candidates":[{"index":1,"content":{"parts":[{"text":"` + strings.Repeat("a", 1024) + `"}]}}]}`
			}
			frames := []string{first}
			for range 80 {
				frames = append(frames, chunk)
			}
			out := collectGuardrailStream(t, g, frames)
			if len(out) != len(frames) {
				t.Fatalf("lost frames after a candidate finished: %d", len(out))
			}
		})
	}
}

func TestGuardrailToolRedactionPreservesNumbers(t *testing.T) {
	for _, family := range []wire.Format{wire.Chat, wire.Anthropic, wire.GeminiStream} {
		t.Run(string(family), func(t *testing.T) {
			g := regressionGuardrailStream(t, family, guardrails.Redact)
			args := `{"account_id":9007199254740993,"note":"secret"}`
			escaped, _ := json.Marshal(args)
			var frames []string
			switch family {
			case wire.Chat:
				frames = []string{`{"sequence":9007199254740993,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"read","arguments":` + string(escaped) + `}}]}}]}`, `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`}
			case wire.Anthropic:
				frames = []string{`{"type":"content_block_start","index":0,"sequence":9007199254740993,"content_block":{"type":"tool_use","name":"read","input":` + args + `}}`, `{"type":"content_block_stop","index":0}`}
			case wire.GeminiStream:
				frames = []string{`{"sequence":9007199254740993,"candidates":[{"content":{"parts":[{"functionCall":{"name":"read","args":` + args + `}}]}}]}`}
			}
			out := collectGuardrailStream(t, g, frames)
			all := string(bytes.Join(out, nil))
			if strings.Contains(all, "secret") || strings.Contains(all, "9007199254740992") || strings.Count(all, "9007199254740993") != 2 || !strings.Contains(all, "[REDACTED]") {
				t.Fatalf("changed unrelated numeric data: %s", all)
			}
		})
	}
}

func TestGuardrailTransformedAnthropicRefusesOpaqueInput(t *testing.T) {
	h := anthropicStrictHarness(t)
	route := h.rt.release.Snapshot.Routes[routeSlug]
	route.Fidelity = routing.RouteFidelity{Mode: routing.FidelityTransformed}
	h.rt.release.Snapshot.Routes[routeSlug] = route
	installTestGuardrail(t, h, guardrails.Input, guardrails.Block, "builtin.keywords", `{"words":["forbidden"]}`)
	response := h.do(t.Context(), "POST", messagesPath, fullKey, []byte(`{"model":"`+routeSlug+`","max_tokens":20,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}]}`), nil)
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 422 || !bytes.Contains(body, []byte("guardrail_surface_unavailable")) || h.mock.count("a")+h.mock.count("b") != 0 {
		t.Fatalf("opaque input escaped coverage: %d %s", response.StatusCode, body)
	}
}

func TestGuardrailApplyRespectsInferenceAdmission(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(map[bool]string{true: "queued", false: "direct"}[queued], func(t *testing.T) {
			h := newHarness(t, Config{})
			installTestGuardrail(t, h, guardrails.Input, guardrails.Block, "builtin.keywords", `{"words":["secret"]}`)
			key := h.rt.keys[fullKey]
			project := "project"
			key.Policy.Scopes = append(key.Policy.Scopes, "guardrails")
			key.ProjectID = &project
			h.rt.keys[fullKey] = key
			policy := h.rt.release.Snapshot.Guardrails.Policies["policy-revision"]
			policy.ProjectID = project
			h.rt.release.Snapshot.Guardrails.Policies[policy.ID] = policy
			pool := observability.NewPool(1)
			h.gateway.admission = pool
			if queued {
				pool.Queue(1, time.Second)
			}
			held := pool.Enter()
			defer held.Release()
			ctx := t.Context()
			if queued {
				permit := pool.Enter()
				defer permit.Release()
				ctx = observability.WithPermit(ctx, permit)
			}
			ctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
			defer cancel()
			req := httptest.NewRequest("POST", "/v1/guardrails/apply", strings.NewReader(`{"policy_revision_id":"policy-revision","phase":"input","segments":[{"id":"s","text":"hello"}]}`)).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer "+fullKey)
			out := httptest.NewRecorder()
			h.gateway.applyGuardrails(out, req)
			if out.Code != http.StatusServiceUnavailable || !strings.Contains(out.Body.String(), "overloaded") || pool.Admitted() != 1 {
				t.Fatalf("admission bypass: %d %s", out.Code, out.Body)
			}
		})
	}
}

func TestGuardrailConsoleEvaluationRespectsInferenceAdmission(t *testing.T) {
	h := newHarness(t, Config{})
	installTestGuardrail(t, h, guardrails.Input, guardrails.Block, "builtin.keywords", `{"words":["secret"]}`)
	registry := h.rt.release.Snapshot.Guardrails
	policy := registry.Policies["policy-revision"]
	pool := observability.NewPool(1)
	h.gateway.admission = pool
	held := pool.Enter()
	defer held.Release()
	input := guardrails.ApplyInput{Policy: &policy, Definitions: registry.Definitions, Phase: guardrails.Input, Segments: []guardrails.Segment{{ID: "s", Text: "hello"}}}
	r := httptest.NewRequest("POST", "/api/v1/guardrails/evaluate", nil)
	if _, err := h.gateway.EvaluateGuardrails(r, access.Principal{}, input); err == nil || pool.Admitted() != 1 {
		t.Fatalf("console evaluation bypassed admission: %v", err)
	}
	held.Release()
	if _, err := h.gateway.EvaluateGuardrails(r, access.Principal{}, input); err != nil || pool.Admitted() != 0 {
		t.Fatalf("admission permit was not released: %v", err)
	}
}
