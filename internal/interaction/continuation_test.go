package interaction

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/tests/fidelity"
)

const toolSource = `{"model":"route","stream":true,"tools":[{"type":"function","function":{"name":"weather","description":"Weather in a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}},{"type":"function","function":{"name":"clock","description":"Time in a zone","parameters":{"type":"object","properties":{"zone":{"type":"string"}},"required":["zone"]}}}],"messages":[{"role":"user","content":"Weather and time in Paris?"}]}`

func toolsTemplate(t *testing.T) *Template {
	config := configuration(t, "anthropic-messages")
	config.Model = "fixture-model"
	config.Provider.OperationDefaults = map[string]connectors.DefaultSet{"generation": {Dialect: "anthropic-messages", Values: map[string]json.RawMessage{"max_tokens": json.RawMessage(`2048`)}, NativeOptions: map[string]json.RawMessage{"thinking": json.RawMessage(`{"type":"enabled","budget_tokens":1024}`)}}}
	return template(t, config)
}
func toolContext() Context {
	return Context{ContinuationVersion: ContinuationV1, DurableContinuation: true, AllowProviderState: true}
}
func TestNegotiatedToolGoldenReconstructsFullNativeNextTurn(t *testing.T) {
	plan := bind(t, toolsTemplate(t), request(t, openai.FamilyChat, toolSource), toolContext())
	raw, err := os.ReadFile("../../tests/fixtures/fidelity/v1/anthropic-tool-workflow.sse")
	if err != nil {
		t.Fatal(err)
	}
	projection, err := plan.NewToolProjection(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	var early [][]byte
	completion, err := protocols.StreamWithEvents(openai.FamilyAnthropic, openai.FamilyAnthropic, bytes.NewReader(raw), 1<<20, "route", true, func([]byte) error { return nil }, func(event oif.Event) error {
		frames, err := projection.Observe(event)
		for _, f := range frames {
			early = append(early, f)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, frame := range early {
		if bytes.Contains(frame, []byte("tool_calls")) || bytes.Contains(frame, []byte("signature")) || bytes.Contains(frame, []byte("continuation_")) {
			t.Fatal("actionable or opaque dependency escaped barrier")
		}
	}
	state, delivery, err := projection.Complete(completion, "continuation_fixture")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(state.Blocks, []byte("opaque-fixture-signature-do-not-log")) {
		t.Fatal("native signature missing")
	}
	if len(delivery.Frames) == 0 || len(early) == 0 {
		t.Fatal("missing incremental observations or ready delivery")
	}
	messages := []json.RawMessage{json.RawMessage(`{"role":"user","content":"Weather and time in Paris?"}`), state.Assistant, json.RawMessage(`{"role":"tool","tool_call_id":"call-weather","content":"sunny"}`), json.RawMessage(`{"role":"tool","tool_call_id":"call-clock","content":"14:00"}`)}
	next := plan.Prepared().Request().Document().Fields()
	delete(next, "stream")
	next["messages"], _ = json.Marshal(messages)
	nextRaw, _ := json.Marshal(next)
	ctx := toolContext()
	ctx.Continuation = state
	nextPlan := bind(t, toolsTemplate(t), request(t, openai.FamilyChat, string(nextRaw)), ctx)
	golden, err := os.ReadFile("../../tests/fixtures/fidelity/v1/anthropic-tool-next-request.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := fidelity.Compare(golden, nextPlan.Body()); err != nil {
		t.Fatal(err)
	}
	for _, changed := range []string{strings.Replace(string(nextRaw), "sunny", "sunny", 1), strings.Replace(string(nextRaw), "Weather and time in Paris?", "edited", 1), strings.Replace(string(nextRaw), "call-weather", "tampered", 1)} {
		_, err := toolsTemplate(t).Bind(request(t, openai.FamilyChat, changed), ctx)
		if changed == string(nextRaw) {
			if err != nil {
				t.Fatal(err)
			}
		} else if err == nil {
			t.Fatal("edited history accepted")
		}
	}
}

func TestNegotiatedToolGuardsDependenciesAndVersion(t *testing.T) {
	tpl := toolsTemplate(t)
	for _, ctx := range []Context{{}, {ContinuationVersion: "future-v2", DurableContinuation: true}, {ContinuationVersion: ContinuationV1}} {
		_, err := tpl.Bind(request(t, openai.FamilyChat, toolSource), ctx)
		if err == nil {
			t.Fatal("unqualified carrier accepted")
		}
	}
	for _, change := range []string{strings.Replace(toolSource, `"stream":true`, `"stream":true,"reasoning_effort":"high"`, 1), strings.Replace(toolSource, `"stream":true`, `"stream":true,"max_tokens":2048`, 1)} {
		_, err := tpl.Bind(request(t, openai.FamilyChat, change), toolContext())
		assertReason(t, err, "reasoning_budget")
	}
	raw, err := os.ReadFile("../../tests/fixtures/fidelity/v1/anthropic-tool-workflow.sse")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, stream string
		limit        int
	}{
		{"missing terminal", strings.Replace(string(raw), "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", "", 1), 1 << 20},
		{"unknown terminal", strings.Replace(string(raw), `"stop_reason":"tool_use"`, `"stop_reason":"new-provider-terminal"`, 1), 1 << 20},
		{"late signature missing", strings.Replace(string(raw), "opaque-fixture-signature-do-not-log", "", 1), 1 << 20},
		{"bounded queue", string(raw), 256},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := bind(t, tpl, request(t, openai.FamilyChat, toolSource), toolContext())
			projection, err := plan.NewToolProjection(tc.limit)
			if err != nil {
				t.Fatal(err)
			}
			completion, streamErr := protocols.StreamWithEvents(openai.FamilyAnthropic, openai.FamilyAnthropic, strings.NewReader(tc.stream), 1<<20, "route", true, func([]byte) error { return nil }, func(event oif.Event) error {
				frames, e := projection.Observe(event)
				for _, frame := range frames {
					if bytes.Contains(frame, []byte("tool_calls")) {
						t.Fatal("unsafe actionable output")
					}
				}
				return e
			})
			_, _, completeErr := projection.Complete(completion, "continuation_fixture")
			if streamErr == nil || completeErr == nil {
				t.Fatalf("unsafe result stream=%v complete=%v", streamErr, completeErr)
			}
		})
	}
}

func TestNegotiatedUnaryUsesSameNativeOrderedProjection(t *testing.T) {
	source := strings.Replace(toolSource, `"stream":true,`, "", 1)
	plan := bind(t, toolsTemplate(t), request(t, openai.FamilyChat, source), toolContext())
	body := []byte(`{"id":"message-final","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"Both tools completed."}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":30,"output_tokens":4}}`)
	native, err := protocols.DecodeRequest(openai.FamilyAnthropic, openai.FamilyAnthropic, body, "route", "", plan.EffectiveRequest())
	if err != nil {
		t.Fatal(err)
	}
	state, delivery, err := plan.ProjectUnary(native, "continuation_final", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if string(state.Assistant) != `{"content":"Both tools completed.","role":"assistant"}` || delivery.Stream || len(delivery.Frames) > 0 || !bytes.Contains(delivery.Body, []byte("Both tools completed.")) {
		t.Fatalf("wrong unary projection: %s", delivery.Body)
	}
}
