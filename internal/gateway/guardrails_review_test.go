package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func TestGuardrailMigratedStrictInputKeepsScalarMatching(t *testing.T) {
	for _, tc := range []struct{ name, pattern, fields string }{
		{"enum", "^secret$", `,"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"value":{"enum":["secret"]}}}}}]`},
		{"schema_key", "^secret$", `,"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"secret":{"type":"string"}}}}}]`},
		{"number", "^42$", `,"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","maxProperties":42}}}]`},
		{"boolean", "^false$", `,"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","additionalProperties":false}}}]`},
		{"decoded_argument", "^secret$", `,"messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"value\":\"secret\"}"}}]}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := strictHarness(t, nil)
			setRoutePolicy(h, contentpolicy.Rule{ID: "legacy", Phase: guardrails.Input, Action: guardrails.Block, Pattern: tc.pattern})
			if err := h.rt.release.Snapshot.Compile(); err != nil {
				t.Fatal(err)
			}
			resp, body := h.chat(fullKey, nil, tc.fields)
			if resp.StatusCode != 400 || errorCode(t, body) != "content_policy_blocked" {
				t.Fatalf("legacy fixture did not block: %d %v", resp.StatusCode, body)
			}
			config, _ := json.Marshal(guardrails.RegexConfig{Rules: []guardrails.Pattern{{ID: "legacy", Pattern: tc.pattern}}})
			installTestGuardrail(t, h, guardrails.Input, guardrails.Block, "builtin.regex", string(config))
			registry := h.rt.release.Snapshot.Guardrails
			definition := registry.Definitions["revision"]
			definition.LegacyRuleID = "legacy"
			registry.Definitions["revision"] = definition
			registry.MigratedRoutes = map[string]bool{h.rt.release.Snapshot.Routes[routeSlug].ID: true}
			if err := h.rt.release.Snapshot.Compile(); err != nil {
				t.Fatal(err)
			}
			resp, body = h.chat(fullKey, nil, tc.fields)
			if resp.StatusCode != 400 || errorCode(t, body) != "content_policy_blocked" || h.mock.count("a")+h.mock.count("b") != 0 {
				t.Fatalf("migration weakened scalar matching: %d %v", resp.StatusCode, body)
			}
		})
	}
}

func TestGuardrailResponsesTerminalToolArguments(t *testing.T) {
	args := `{"account_id":9007199254740993,"note":"secret"}`
	escaped, _ := json.Marshal(args)
	item := `{"id":"fc_1","type":"function_call","call_id":"call_1","name":"read","arguments":` + string(escaped) + `}`
	for _, terminal := range []string{"response.completed", "response.incomplete", "response.failed"} {
		for _, deltas := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/deltas=%v", terminal, deltas), func(t *testing.T) {
				g := regressionGuardrailStream(t, wire.Responses, guardrails.Redact)
				frames := []string{}
				if deltas {
					frames = append(frames,
						`{"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","name":"read","arguments":""}}`,
						`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":`+string(escaped)+`}`,
						`{"type":"response.function_call_arguments.done","item_id":"fc_1","arguments":`+string(escaped)+`}`,
						`{"type":"response.output_item.done","item":`+item+`}`)
				}
				frames = append(frames, `{"type":"`+terminal+`","response":{"output":[`+item+`]}}`)
				out := collectGuardrailStream(t, g, frames)
				all := string(bytes.Join(out, nil))
				if len(out) != len(frames) || strings.Contains(all, "secret") || !strings.Contains(all, "[REDACTED]") || !strings.Contains(all, "9007199254740993") {
					t.Fatalf("unsafe terminal arguments: %s", all)
				}
				blocked := regressionGuardrailStream(t, wire.Responses, guardrails.Block)
				if _, err := blocked.push(t.Context(), []byte("data: "+frames[len(frames)-1]+"\n\n")); err == nil {
					t.Fatal("terminal-only tool escaped blocking")
				}
			})
		}
	}
}

func TestGuardrailPinnedCurrentPreservesEnforcement(t *testing.T) {
	h := strictHarness(t, nil)
	installTestGuardrail(t, h, guardrails.Input, guardrails.Block, "builtin.keywords", `{"words":["secret"]}`)
	snapshot := h.rt.release.Snapshot
	route := snapshot.Routes[routeSlug]
	target := route.Targets[0]
	provider := snapshot.Providers[target.ProviderID]
	retained, _, ok := snapshot.PinnedCurrent(route, provider, target, provider.Slots[0], "", "openai", "unary")
	if !ok || retained.Guardrails == nil || retained.GuardrailEngine(routeSlug, "") == nil {
		t.Fatal("retained snapshot dropped guardrails")
	}
	route = retained.Routes[routeSlug]
	parsed, err := protocols.Parse(wire.Chat, []byte(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"secret"}]}`), routeSlug)
	if err != nil {
		t.Fatal(err)
	}
	x := &call{request: request{release: h.rt.release}, historicalSnapshot: retained, routeLeg: routeLeg{route: &route}, generation: generationCall{parsed: parsed}}
	if err := h.gateway.bindGuardrails(t.Context(), x); err != nil {
		t.Fatal(err)
	}
	if _, _, err := x.inspectGuardrailInput(parsed.OIF().Document()); err == nil {
		t.Fatal("retained request bypassed mandatory input policy")
	}
}
