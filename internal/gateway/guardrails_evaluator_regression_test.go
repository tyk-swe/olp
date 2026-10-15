package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"
)

func TestGuardrailStreamLogprobsRefusedBeforeDispatch(t *testing.T) {
	for _, strict := range []bool{false, true} {
		for _, defaults := range []bool{false, true} {
			t.Run(fmt.Sprintf("strict=%t/defaults=%t", strict, defaults), func(t *testing.T) {
				h := newHarness(t, Config{})
				if strict {
					h = strictHarness(t, nil)
				}
				fields := `,"logprobs":true,"top_logprobs":1`
				if defaults {
					fields = ""
					for id, p := range h.rt.release.Snapshot.Providers {
						p.ParameterDefaults = map[string]json.RawMessage{"logprobs": json.RawMessage(`true`), "top_logprobs": json.RawMessage(`1`)}
						if strict {
							p.OperationDefaults = map[string]connectors.DefaultSet{"generation": {Dialect: "openai-chat", Values: p.ParameterDefaults}}
							p.ParameterDefaults = nil
						}
						h.rt.release.Snapshot.Providers[id] = p
					}
				}
				installTestGuardrail(t, h, guardrails.OutputStream, guardrails.Block, "builtin.keywords", `{"words":["secret"]}`)
				response := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"hi"}],"stream":true`+fields+`}`), nil)
				defer response.Body.Close()
				body, _ := io.ReadAll(response.Body)
				if response.StatusCode != 422 || !bytes.Contains(body, []byte("guardrail_surface_unavailable")) || h.mock.count("a")+h.mock.count("b") != 0 {
					t.Fatalf("logprobs dispatched: %d %s", response.StatusCode, body)
				}
			})
		}
	}
}

func TestGuardrailStreamRejectsUnexpectedLogprobs(t *testing.T) {
	for _, kind := range []string{"content", "refusal"} {
		g := regressionGuardrailStream(t, wire.Chat, guardrails.Block)
		frame := `data: {"choices":[{"index":0,"delta":{"content":"safe"},"logprobs":{"` + kind + `":[{"token":"safe","top_logprobs":[{"token":"secret","logprob":-1}]}]}}]}` + "\n\n"
		if out, err := g.push(t.Context(), []byte(frame)); err == nil || len(out) != 0 {
			t.Fatalf("uninspected %s logprobs passed: %s %v", kind, bytes.Join(out, nil), err)
		}
	}
	g := regressionGuardrailStream(t, wire.Chat, guardrails.Block)
	frames := []string{`{"choices":[{"index":0,"delta":{"content":"safe"},"logprobs":null}]}`}
	if out := collectGuardrailStream(t, g, frames); len(out) != 1 || string(out[0]) != "data: "+frames[0]+"\n\n" {
		t.Fatalf("changed stream without log-probability text: %s", bytes.Join(out, nil))
	}
}

func TestGuardrailChatToolCompletionStaysWithinCandidate(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			g := regressionGuardrailStream(t, wire.Chat, guardrails.Block)
			delta := func(name, arguments string) string {
				fn, _ := json.Marshal(map[string]string{"name": name, "arguments": arguments})
				if legacy {
					return `{"function_call":` + string(fn) + `}`
				}
				return `{"tool_calls":[{"index":0,"function":` + string(fn) + `}]}`
			}
			frames := []string{
				`{"choices":[{"index":1,"delta":` + delta("read", `{"value":"safe"}`) + `},{"index":10,"delta":` + delta("read", `{"value":`) + `}]}`,
				`{"choices":[{"index":1,"delta":{},"finish_reason":"tool_calls"}]}`,
				`{"choices":[{"index":10,"delta":` + delta("", `"safe"}`) + `}]}`,
				`{"choices":[{"index":10,"delta":{},"finish_reason":"tool_calls"}]}`,
			}
			out := collectGuardrailStream(t, g, frames)
			if len(out) != len(frames) {
				t.Fatalf("lost candidate frames: %s", bytes.Join(out, nil))
			}
			for i := range frames {
				if string(out[i]) != "data: "+frames[i]+"\n\n" {
					t.Fatalf("changed passing frame: %s", out[i])
				}
			}
		})
	}
}

func modelGuardrailHarness(t *testing.T, operation string) (*harness, guardrails.Revision) {
	t.Helper()
	h := newHarness(t, Config{})
	h.republish(func(s *routing.Snapshot, _, b routing.Provider) {
		original := s.Routes[routeSlug]
		triage(s, b, routing.Predicate{})
		s.Routes[routeSlug] = original
		route := s.Routes[classifierSlug]
		if operation != "generation" {
			b.Capabilities = []routing.Capability{{Model: modelB, Operation: operation, Surface: "native", Mode: "unary"}}
			b.ProfileID, b.ProfileRevision = "tei-classification", "1"
			if operation == "moderation" {
				b.Kind = "openai"
				b.ProfileID = "openai-moderation"
			}
			s.Providers[b.ID] = b
			route.Operations = []string{operation}
			route.Fidelity = routing.RouteFidelity{Mode: routing.FidelityStrict}
		}
		s.Routes[classifierSlug] = route
	})
	installTestGuardrail(t, h, guardrails.Input, guardrails.Block, "model", `{"route":"team-classifier","rule":{"pointer":"/unsafe","equals":true,"category":"model.unsafe"}}`)
	h.mock.set("b", completion(modelB, `{"unsafe":true}`))
	return h, h.rt.release.Snapshot.Guardrails.Definitions["revision"]
}

func TestModelGuardrailUsesFullPinnedRelease(t *testing.T) {
	h, definition := modelGuardrailHarness(t, "generation")
	snapshot := h.rt.release.Snapshot
	route := snapshot.Routes[routeSlug]
	target := route.Targets[0]
	provider := snapshot.Providers[target.ProviderID]
	retained, _, ok := snapshot.PinnedCurrent(route, provider, target, provider.Slots[0], "", "openai", "unary")
	if !ok {
		t.Fatal("could not pin serving route")
	}
	if _, exists := retained.Routes[classifierSlug]; exists {
		t.Fatal("fixture did not narrow the serving snapshot")
	}
	parent := &call{request: request{id: uuid.NewString(), minted: true, release: h.rt.release}, historicalSnapshot: retained, keyID: h.rt.keys[fullKey].ID, authority: h.rt.keys[fullKey]}
	ctx := context.WithValue(t.Context(), guardrailCallerKey{}, parent)
	verdict, err := h.gateway.modelGuardrail(ctx, definition, guardrails.Evaluation{Phase: guardrails.Input, Segments: []guardrails.Segment{{ID: "s", Text: "hello"}}})
	if err != nil || len(verdict.Categories) != 1 || verdict.Categories[0] != "model.unsafe" || h.mock.count("b") != 1 {
		t.Fatalf("retained request lost evaluator route: %+v %v", verdict, err)
	}
}

func TestModelGuardrailPreservesEndUserDigest(t *testing.T) {
	h, _ := modelGuardrailHarness(t, "generation")
	identifyHarness(h, "header")
	response, body := h.chat(fullKey, map[string]string{endUserHeader: "customer-123"})
	if response.StatusCode != http.StatusBadRequest || errorCode(t, body) != "guardrail_blocked" {
		t.Fatalf("model evaluation failed: %d %v", response.StatusCode, body)
	}
	if len(h.sink.envs) != 2 {
		t.Fatalf("missing evaluator accounting: %d", len(h.sink.envs))
	}
	child, parent := h.sink.envs[0], h.sink.envs[1]
	if child.EndUserDigest == "" || child.EndUserDigest != parent.EndUserDigest || child.ParentRequestID != parent.AccountingID {
		t.Fatalf("evaluator lost end-user attribution: child=%q parent=%q", child.EndUserDigest, parent.EndUserDigest)
	}
}

func TestModelGuardrailKeylessAdmissionChecksSystemAccounting(t *testing.T) {
	for _, operation := range []string{"generation", "classification", "moderation"} {
		for _, scope := range []string{"installation", "organization", "project"} {
			t.Run(operation+"/"+scope, func(t *testing.T) {
				h, definition := modelGuardrailHarness(t, operation)
				project := costText(uuid.NewString())
				authority := access.Authority{InstallationID: uuid.NewString(), OrganizationID: costText(uuid.NewString()), ProjectID: project}
				budget := &access.BudgetPolicy{DailyCostLimit: costText("1")}
				switch scope {
				case "installation":
					authority.InstallationBudget = budget
				case "organization":
					authority.OrganizationBudget = budget
				case "project":
					authority.ProjectBudget = budget
				}
				h.gateway.Runtime = budgetRuntime{fakeRuntime: h.rt, authority: authority}
				checked := false
				h.gateway.Admission = &Admission{CostAccountingReady: func(context.Context) error { checked = true; return errors.New("accounting unavailable") }}
				parent := &call{request: request{id: uuid.NewString(), minted: true, release: h.rt.release}, actor: "playground", authorize: func(*routing.Route) *Error { return nil }}
				ctx := context.WithValue(t.Context(), guardrailCallerKey{}, parent)
				_, err := h.gateway.modelGuardrail(ctx, definition, guardrails.Evaluation{Phase: guardrails.Input, Segments: []guardrails.Segment{{ID: "s", Text: "hello"}}})
				var failure *Error
				if !checked || !errors.As(err, &failure) || failure.Code != "cost_accounting_incomplete" || h.mock.count("b") != 0 {
					t.Fatalf("keyless evaluator bypassed accounting: checked=%t error=%v calls=%d", checked, err, h.mock.count("b"))
				}
				if strings.Contains(fmt.Sprint(err), "accounting unavailable") {
					t.Fatal("internal accounting error leaked")
				}
			})
		}
	}
}

func TestModelGuardrailKeylessAdmissionReservesSystemBudgets(t *testing.T) {
	for _, scope := range []string{"installation", "organization", "project"} {
		for _, exhausted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/exhausted=%t", scope, exhausted), func(t *testing.T) {
				h, definition := modelGuardrailHarness(t, "generation")
				authority := access.Authority{InstallationID: uuid.NewString(), OrganizationID: costText(uuid.NewString()), ProjectID: costText(uuid.NewString())}
				budget := &access.BudgetPolicy{DailyCostLimit: costText("1")}
				switch scope {
				case "installation":
					authority.InstallationBudget = budget
				case "organization":
					authority.OrganizationBudget = budget
				case "project":
					authority.ProjectBudget = budget
				}
				h.gateway.Runtime = budgetRuntime{fakeRuntime: h.rt, authority: authority}
				reservations := make(chan bool, 1)
				h.gateway.Admission = newAdmission(t, accountingStreamFunc(func(_ context.Context, args ...string) (any, error) {
					if len(args) == 13 {
						reservations <- true
						if exhausted {
							return []any{int64(1), int64(0), "daily_cost", int64(1000), int64(20_000), int64(24_000)}, nil
						}
						return costAllowed, nil
					}
					if len(args) == 8 {
						return costSettled, nil
					}
					return released, nil
				}))
				parent := &call{request: request{id: uuid.NewString(), minted: true, release: h.rt.release}, actor: "playground", authorize: func(*routing.Route) *Error { return nil }}
				ctx := context.WithValue(t.Context(), guardrailCallerKey{}, parent)
				_, err := h.gateway.modelGuardrail(ctx, definition, guardrails.Evaluation{Phase: guardrails.Input, Segments: []guardrails.Segment{{ID: "s", Text: "hello"}}})
				if len(reservations) != 1 {
					t.Fatal("evaluator did not reserve the system budget")
				}
				if exhausted {
					var failure *Error
					if !errors.As(err, &failure) || failure.Code != "budget_exhausted" || failure.Param == nil || *failure.Param != "budget."+scope || h.mock.count("b") != 0 {
						t.Fatalf("evaluator bypassed exhausted %s budget: %v calls=%d", scope, err, h.mock.count("b"))
					}
				} else if err != nil || h.mock.count("b") != 1 {
					t.Fatalf("available system budget refused: %v calls=%d", err, h.mock.count("b"))
				}
			})
		}
	}
}
