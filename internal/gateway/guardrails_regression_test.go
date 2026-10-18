package gateway

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/observability"
	"github.com/tyk-swe/olp/internal/protocols/wire"
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

func TestLegacyCompletionStreamGuardrails(t *testing.T) {
	frames := []string{
		`{"sequence":9007199254740993,"choices":[{"index":1,"text":"sec","finish_reason":""}]}`,
		`{"choices":[{"index":1,"text":"ret","finish_reason":"stop"}]}`,
	}
	g := regressionGuardrailStream(t, wire.Completions, guardrails.Redact)
	out := collectGuardrailStream(t, g, frames)
	all := string(bytes.Join(out, nil))
	if strings.Contains(all, "secret") || !strings.Contains(all, "[REDACTED]") || !strings.Contains(all, "9007199254740993") {
		t.Fatalf("unsafe completion stream: %s", all)
	}
	g = regressionGuardrailStream(t, wire.Completions, guardrails.Block)
	if _, err := g.push(t.Context(), []byte("data: "+frames[0]+"\n\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.push(t.Context(), []byte("data: "+frames[1]+"\n\n")); err == nil {
		t.Fatal("split completion text bypassed blocking")
	}
	g = regressionGuardrailStream(t, wire.Completions, guardrails.Block)
	if _, err := g.push(t.Context(), []byte(`data: {"choices":[{"index":0,"text":"safe","logprobs":{"tokens":["secret"]}}]}`+"\n\n")); err == nil {
		t.Fatal("unsolicited logprobs bypassed surface refusal")
	}
}

// Candidate identities survive changes in their array positions.

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
