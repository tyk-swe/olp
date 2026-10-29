package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestGatewayToolExecutionRejectsStrictRoutesBeforeDispatch(t *testing.T) {
	h := strictHarness(t, func(snapshot *routing.Snapshot) {
		route := snapshot.Routes[routeSlug]
		if err := json.Unmarshal([]byte(`{"tool_execution":"gateway"}`), &route); err != nil {
			t.Fatal(err)
		}
		snapshot.Routes[routeSlug] = route
	})
	response, body := h.chat(fullKey, nil)
	if response.StatusCode != http.StatusBadRequest || errorCode(t, body) != "tool_execution_strict_route" {
		t.Fatalf("strict route accepted gateway tool execution: %d %v", response.StatusCode, body)
	}
	if h.mock.count("a") != 0 || h.mock.count("b") != 0 {
		t.Fatal("strict route dispatched a model request")
	}
}

func TestGatewayToolExecutionCannotWidenRouteToolsets(t *testing.T) {
	allowed, other := uuid.NewString(), uuid.NewString()
	h := strictHarness(t, func(snapshot *routing.Snapshot) {
		route := snapshot.Routes[routeSlug]
		route.Fidelity.Mode = routing.FidelityTransformed
		if err := json.Unmarshal([]byte(`{"tool_execution":"gateway","toolsets":["`+allowed+`"]}`), &route); err != nil {
			t.Fatal(err)
		}
		snapshot.Routes[routeSlug] = route
	})
	response, body := h.chat(fullKey, nil, `,"olp_toolsets":["`+other+`"]`)
	if response.StatusCode != http.StatusBadRequest || errorCode(t, body) != "toolset_not_permitted" {
		t.Fatalf("request widened the route toolsets: %d %v", response.StatusCode, body)
	}
	if h.mock.count("a") != 0 || h.mock.count("b") != 0 {
		t.Fatal("request dispatched before toolset authorization")
	}
}

func TestGatewayToolExecutionRequiresDurableAccounting(t *testing.T) {
	h := strictHarness(t, func(snapshot *routing.Snapshot) {
		route := snapshot.Routes[routeSlug]
		route.Fidelity.Mode = routing.FidelityTransformed
		if err := json.Unmarshal([]byte(`{"tool_execution":"gateway"}`), &route); err != nil {
			t.Fatal(err)
		}
		snapshot.Routes[routeSlug] = route
	})
	response, body := h.chat(fullKey, nil)
	if response.StatusCode != http.StatusServiceUnavailable || errorCode(t, body) != "tool_execution_unavailable" {
		t.Fatalf("gateway execution ran without durable accounting: %d %v", response.StatusCode, body)
	}
	if h.mock.count("a") != 0 || h.mock.count("b") != 0 {
		t.Fatal("gateway execution dispatched without durable accounting")
	}
}

func TestGatewayToolExecutionRejectsUncertifiedModel(t *testing.T) {
	projectID := uuid.NewString()
	h := strictHarness(t, func(snapshot *routing.Snapshot) {
		route := snapshot.Routes[routeSlug]
		route.Fidelity.Mode = routing.FidelityTransformed
		route.ToolExecution = "gateway"
		route.ProjectID = &projectID
		snapshot.Routes[routeSlug] = route
	})
	authority := h.rt.keys[fullKey]
	authority.ProjectID = &projectID
	h.rt.keys[fullKey] = authority
	fixture := &workflowFixture{runID: uuid.NewString()}
	h.gateway.WorkflowRuns, h.gateway.WorkflowTools, h.gateway.Debits = fixture, fixture, &workflowDebitFixture{fakeSessionDebits: &fakeSessionDebits{}}
	response, body := h.chat(fullKey, nil)
	if response.StatusCode == http.StatusOK || h.mock.count("a") != 0 || h.mock.count("b") != 0 {
		t.Fatalf("uncertified model was dispatched: status=%d body=%v", response.StatusCode, body)
	}
}

type workflowDebitFixture struct {
	*fakeSessionDebits
	settled []string
}

func (f *workflowDebitFixture) SettleWorkflow(_ context.Context, event *usage.Event, attempt int) ([]limits.CostSnapshot, error) {
	if attempt < 0 || attempt >= len(event.Attempts) || event.Attempts[attempt].Execution == nil {
		return nil, fmt.Errorf("missing debit-backed attempt")
	}
	f.settled = append(f.settled, event.Attempts[attempt].Execution.DispatchID)
	return nil, nil
}

type workflowFixture struct {
	runID      string
	tool       WorkflowTool
	intents    []limits.DebitIntent
	ordinals   []int
	calls      []json.RawMessage
	selections [][]string
	beforeCall func() error
}

func (f *workflowFixture) Start(context.Context, access.Authority, string, string) (string, error) {
	return f.runID, nil
}

func (f *workflowFixture) Prepare(_ context.Context, _ access.Authority, intent limits.DebitIntent, ordinal int, _ []limits.DebitOwner) (limits.DebitState, error) {
	f.intents = append(f.intents, intent)
	f.ordinals = append(f.ordinals, ordinal)
	return limits.DebitPrepared, nil
}

func (f *workflowFixture) Resolve(_ context.Context, _ access.Authority, _ *runtime.Release, toolsets []string) ([]WorkflowTool, error) {
	f.selections = append(f.selections, append([]string(nil), toolsets...))
	if len(toolsets) == 0 {
		return nil, nil
	}
	return []WorkflowTool{f.tool}, nil
}

func (f *workflowFixture) Call(_ context.Context, _ access.Authority, _ *runtime.Release, _ WorkflowTool, runID string, ordinal int, _ string, arguments json.RawMessage) (json.RawMessage, error) {
	if runID != f.runID {
		return nil, fmt.Errorf("wrong run")
	}
	if f.beforeCall != nil {
		if err := f.beforeCall(); err != nil {
			return nil, err
		}
	}
	f.ordinals = append(f.ordinals, ordinal)
	f.calls = append(f.calls, arguments)
	return json.RawMessage(`{"content":[{"type":"text","text":"result"}]}`), nil
}

func TestGatewayToolExecutionCompletesOneRun(t *testing.T) {
	toolsetID, otherToolsetID := uuid.NewString(), uuid.NewString()
	projectID := uuid.NewString()
	alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	h := strictHarness(t, func(snapshot *routing.Snapshot) {
		route := snapshot.Routes[routeSlug]
		route.Fidelity.Mode = routing.FidelityTransformed
		route.ToolExecution, route.Toolsets = "gateway", []string{toolsetID, otherToolsetID}
		route.ProjectID = &projectID
		snapshot.Routes[routeSlug] = route
		for id, provider := range snapshot.Providers {
			for i := range provider.Capabilities {
				provider.Capabilities[i].ToolCalls = true
			}
			snapshot.Providers[id] = provider
		}
	})
	authority := h.rt.keys[fullKey]
	authority.ProjectID = &projectID
	h.rt.keys[fullKey] = authority
	fixture := &workflowFixture{runID: uuid.NewString(), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "read_only"}}}
	debits := &workflowDebitFixture{fakeSessionDebits: &fakeSessionDebits{}}
	fixture.beforeCall = func() error {
		if len(debits.settled) != 1 {
			return fmt.Errorf("first model attempt was not settled before tool dispatch")
		}
		return nil
	}
	h.gateway.WorkflowRuns, h.gateway.WorkflowTools, h.gateway.Debits = fixture, fixture, debits
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if h.mock.count("a") == 1 {
			var request map[string]json.RawMessage
			if err := json.Unmarshal(body, &request); err != nil {
				t.Error(err)
			}
			if _, found := request["olp_toolsets"]; found {
				t.Error("gateway toolset selection reached the provider")
			}
			fmt.Fprintf(w, `{"id":"one","object":"chat.completion","created":1,"model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":%q,"arguments":"{\"value\":1}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`, alias)
			return
		}
		if len(debits.settled) != 1 {
			t.Error("first model attempt was not settled before the next model dispatch")
		}
		var request struct {
			Messages []struct {
				Role       string `json:"role"`
				ToolCallID string `json:"tool_call_id"`
				Content    string `json:"content"`
			} `json:"messages"`
			Toolsets json.RawMessage `json:"olp_toolsets"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Error(err)
		}
		last := request.Messages[len(request.Messages)-1]
		if last.Role != "tool" || last.ToolCallID != "call-1" || last.Content == "" || len(request.Toolsets) != 0 {
			t.Errorf("invalid continuation sent to model: %s", body)
		}
		completion("model-a", "finished")(w, r)
	})
	response, body := h.chat(fullKey, nil, `,"olp_toolsets":["`+toolsetID+`"]`)
	if response.StatusCode != http.StatusOK || h.mock.count("a") != 2 || len(fixture.calls) != 1 || string(fixture.calls[0]) != `{"value":1}` {
		t.Fatalf("workflow did not complete: status=%d body=%v model_calls=%d tool_calls=%v", response.StatusCode, body, h.mock.count("a"), fixture.calls)
	}
	if len(debits.settled) != 2 || debits.settled[0] == debits.settled[1] {
		t.Fatalf("model debits were not settled exactly once: %v", debits.settled)
	}
	if len(fixture.selections) != 1 || len(fixture.selections[0]) != 1 || fixture.selections[0][0] != toolsetID {
		t.Fatalf("request did not narrow the route toolsets: %v", fixture.selections)
	}
	if len(fixture.intents) != 2 || len(fixture.ordinals) != 3 || fixture.ordinals[0] != 0 || fixture.ordinals[1] != 1 || fixture.ordinals[2] != 2 || fixture.intents[0].RunID != fixture.runID || fixture.intents[1].RunID != fixture.runID || fixture.intents[0].ID == fixture.intents[1].ID {
		t.Fatalf("workflow attempts did not share one run: intents=%v ordinals=%v", fixture.intents, fixture.ordinals)
	}
}

func TestGatewayToolExecutionStreamsOnlyFinalAnswer(t *testing.T) {
	toolsetID, projectID := uuid.NewString(), uuid.NewString()
	alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	h := strictHarness(t, func(snapshot *routing.Snapshot) {
		route := snapshot.Routes[routeSlug]
		route.Fidelity.Mode = routing.FidelityTransformed
		route.ToolExecution, route.Toolsets = "gateway", []string{toolsetID}
		route.ProjectID = &projectID
		snapshot.Routes[routeSlug] = route
		for id, provider := range snapshot.Providers {
			for i := range provider.Capabilities {
				provider.Capabilities[i].ToolCalls = true
			}
			snapshot.Providers[id] = provider
		}
	})
	authority := h.rt.keys[fullKey]
	authority.ProjectID = &projectID
	h.rt.keys[fullKey] = authority
	fixture := &workflowFixture{runID: uuid.NewString(), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "read_only"}}}
	h.gateway.WorkflowRuns, h.gateway.WorkflowTools, h.gateway.Debits = fixture, fixture, &workflowDebitFixture{fakeSessionDebits: &fakeSessionDebits{}}
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if h.mock.count("a") == 1 {
			fmt.Fprintf(w, "data: {\"id\":\"first\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"model-a\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":\"{\\\"value\\\":1}\"}}]},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", alias)
			return
		}
		io.WriteString(w, "data: {\"id\":\"last\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"model-a\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"final answer\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	})
	response := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"hi"}],"stream":true}`), nil)
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "final answer") || !strings.Contains(string(body), "[DONE]") || strings.Contains(string(body), "call-1") || strings.Contains(string(body), alias) || len(fixture.calls) != 1 || h.mock.count("a") != 2 {
		t.Fatalf("stream exposed an intermediate step or missed final output: status=%d body=%s model_calls=%d tool_calls=%d", response.StatusCode, body, h.mock.count("a"), len(fixture.calls))
	}
}

func TestGatewayToolExecutionBounds(t *testing.T) {
	for _, test := range []struct {
		name           string
		callsPerStep   int
		wantCode       string
		wantModelCalls int
		wantToolCalls  int
	}{
		{name: "iterations", callsPerStep: 1, wantCode: "tool_iteration_limit", wantModelCalls: 8, wantToolCalls: 7},
		{name: "calls", callsPerStep: 33, wantCode: "tool_call_limit", wantModelCalls: 1, wantToolCalls: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			toolsetID, projectID := uuid.NewString(), uuid.NewString()
			alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			h := strictHarness(t, func(snapshot *routing.Snapshot) {
				route := snapshot.Routes[routeSlug]
				route.Fidelity.Mode, route.ToolExecution, route.ProjectID = routing.FidelityTransformed, "gateway", &projectID
				route.Toolsets = []string{toolsetID}
				snapshot.Routes[routeSlug] = route
				for id, provider := range snapshot.Providers {
					for i := range provider.Capabilities {
						provider.Capabilities[i].ToolCalls = true
					}
					snapshot.Providers[id] = provider
				}
			})
			authority := h.rt.keys[fullKey]
			authority.ProjectID = &projectID
			h.rt.keys[fullKey] = authority
			fixture := &workflowFixture{runID: uuid.NewString(), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "read_only"}}}
			h.gateway.WorkflowRuns, h.gateway.WorkflowTools, h.gateway.Debits = fixture, fixture, &workflowDebitFixture{fakeSessionDebits: &fakeSessionDebits{}}
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				calls := make([]map[string]any, test.callsPerStep)
				for i := range calls {
					calls[i] = map[string]any{"id": fmt.Sprintf("call-%d-%d", h.mock.count("a"), i), "type": "function", "function": map[string]string{"name": alias, "arguments": `{}`}}
				}
				json.NewEncoder(w).Encode(map[string]any{"id": "bounded", "object": "chat.completion", "created": 1, "model": "model-a", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": nil, "tool_calls": calls}, "finish_reason": "tool_calls"}}, "usage": map[string]int{"prompt_tokens": 2, "completion_tokens": 3, "total_tokens": 5}})
			})
			response, body := h.chat(fullKey, nil)
			if response.StatusCode != http.StatusBadRequest || errorCode(t, body) != test.wantCode || h.mock.count("a") != test.wantModelCalls || len(fixture.calls) != test.wantToolCalls {
				t.Fatalf("workflow limit was not enforced: status=%d body=%v model_calls=%d tool_calls=%d", response.StatusCode, body, h.mock.count("a"), len(fixture.calls))
			}
		})
	}
}

func TestGatewayToolExecutionReconcilesModelTokensBeforeNextStep(t *testing.T) {
	toolsetID, projectID := uuid.NewString(), uuid.NewString()
	alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	h := strictHarness(t, func(snapshot *routing.Snapshot) {
		route := snapshot.Routes[routeSlug]
		route.Fidelity.Mode = routing.FidelityTransformed
		route.ToolExecution, route.Toolsets = "gateway", []string{toolsetID}
		route.ProjectID = &projectID
		snapshot.Routes[routeSlug] = route
		for id, provider := range snapshot.Providers {
			for i := range provider.Capabilities {
				provider.Capabilities[i].ToolCalls = true
			}
			snapshot.Providers[id] = provider
		}
	})
	authority := h.rt.keys[fullKey]
	authority.ProjectID = &projectID
	authority.LookupID = "lookup_test_key"
	authority.Policy.TokensPerMinute = perMinute(10_000)
	h.rt.keys[fullKey] = authority
	admitThrough(t, h)
	fixture := &workflowFixture{runID: uuid.NewString(), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "read_only"}}}
	h.gateway.WorkflowRuns, h.gateway.WorkflowTools, h.gateway.Debits = fixture, fixture, &workflowDebitFixture{fakeSessionDebits: &fakeSessionDebits{}}
	h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"id":"one","object":"chat.completion","created":1,"model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":%q,"arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":9989,"completion_tokens":1,"total_tokens":9990}}`, alias)
	})
	resp, body := h.chat(fullKey, nil, `,"max_tokens":1`)
	if resp.StatusCode != http.StatusTooManyRequests || h.mock.count("a") != 1 || len(fixture.calls) != 1 {
		t.Fatalf("expected second model step to be denied by the token limit, got status=%d body=%v model_calls=%d tool_calls=%d", resp.StatusCode, body, h.mock.count("a"), len(fixture.calls))
	}
}

func TestGatewayToolExecutionAppliesDelegatedAgentQuotasAcrossSteps(t *testing.T) {
	for _, test := range []struct {
		name   string
		limits access.AdmissionLimits
		usage  string
		status int
	}{
		{name: "tokens", limits: access.AdmissionLimits{TokensPerMinute: perMinute(10_000)}, usage: `{"prompt_tokens":9989,"completion_tokens":1,"total_tokens":9990}`, status: http.StatusTooManyRequests},
		{name: "requests", limits: access.AdmissionLimits{RequestsPerMinute: perMinute(1)}, usage: `{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}`, status: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			toolsetID, projectID := uuid.NewString(), uuid.NewString()
			alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			h := strictHarness(t, func(snapshot *routing.Snapshot) {
				route := snapshot.Routes[routeSlug]
				route.Fidelity.Mode = routing.FidelityTransformed
				route.ToolExecution, route.Toolsets, route.ProjectID = "gateway", []string{toolsetID}, &projectID
				snapshot.Routes[routeSlug] = route
				for id, provider := range snapshot.Providers {
					for i := range provider.Capabilities {
						provider.Capabilities[i].ToolCalls = true
					}
					snapshot.Providers[id] = provider
				}
			})
			authority := h.rt.keys[fullKey]
			authority.ProjectID = &projectID
			authority.LookupID = "lookup_test_key"
			authority.DelegationAgentLimits = &access.DelegationAgentLimits{ID: uuid.NewString(), LookupID: "lookup_test_agent", Limits: test.limits}
			h.rt.keys[fullKey] = authority
			admitThrough(t, h)
			fixture := &workflowFixture{runID: uuid.NewString(), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "read_only"}}}
			h.gateway.WorkflowRuns, h.gateway.WorkflowTools, h.gateway.Debits = fixture, fixture, &workflowDebitFixture{fakeSessionDebits: &fakeSessionDebits{}}
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				if h.mock.count("a") == 1 {
					fmt.Fprintf(w, `{"id":"one","object":"chat.completion","created":1,"model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":%q,"arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":%s}`, alias, test.usage)
					return
				}
				completion("model-a", "finished")(w, r)
			})
			response, body := h.chat(fullKey, nil, `,"max_tokens":1`)
			if response.StatusCode != test.status || len(fixture.calls) != 1 {
				t.Fatalf("delegated quota failed during workflow: status=%d body=%v model_calls=%d tool_calls=%d", response.StatusCode, body, h.mock.count("a"), len(fixture.calls))
			}
			if test.status == http.StatusTooManyRequests {
				if h.mock.count("a") != 1 {
					t.Fatalf("agent token quota allowed another model call: %d", h.mock.count("a"))
				}
				return
			}
			if h.mock.count("a") != 2 {
				t.Fatalf("agent request quota counted model steps as requests: %d", h.mock.count("a"))
			}
			response, body = h.chat(fullKey, nil, `,"max_tokens":1`)
			if response.StatusCode != http.StatusTooManyRequests || h.mock.count("a") != 2 {
				t.Fatalf("agent request quota was reset between requests: status=%d body=%v model_calls=%d", response.StatusCode, body, h.mock.count("a"))
			}
		})
	}
}

func TestGatewayToolExecutionBypassesResponseCache(t *testing.T) {
	toolsetID, projectID := uuid.NewString(), uuid.NewString()
	h := strictHarness(t, func(snapshot *routing.Snapshot) {
		route := snapshot.Routes[routeSlug]
		route.Fidelity.Mode = routing.FidelityTransformed
		route.ToolExecution, route.Toolsets = "gateway", []string{toolsetID}
		route.ProjectID = &projectID
		snapshot.Routes[routeSlug] = route
		for id, provider := range snapshot.Providers {
			for i := range provider.Capabilities {
				provider.Capabilities[i].ToolCalls = true
			}
			snapshot.Providers[id] = provider
		}
	})
	enableTestCache(t, h)
	authority := h.rt.keys[fullKey]
	authority.ProjectID = &projectID
	h.rt.keys[fullKey] = authority
	fixture := &workflowFixture{runID: uuid.NewString(), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Classification: "read_only"}}}
	h.gateway.WorkflowRuns, h.gateway.WorkflowTools, h.gateway.Debits = fixture, fixture, &workflowDebitFixture{fakeSessionDebits: &fakeSessionDebits{}}
	h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":"done","object":"chat.completion","created":1,"model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
	})
	for range 2 {
		resp, body := h.chat(fullKey, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("workflow failed: status=%d body=%v", resp.StatusCode, body)
		}
	}
	if h.mock.count("a") != 2 {
		t.Fatalf("identical workflows must each call the model, got %d calls", h.mock.count("a"))
	}
}

func TestGatewayToolExecutionDoesNotMirrorWorkflow(t *testing.T) {
	toolsetID, projectID := uuid.NewString(), uuid.NewString()
	h := strictHarness(t, func(snapshot *routing.Snapshot) {
		route := snapshot.Routes[routeSlug]
		route.Fidelity.Mode = routing.FidelityTransformed
		route.ToolExecution, route.Toolsets = "gateway", []string{toolsetID}
		route.ProjectID = &projectID
		route.Targets[1].Shadow = &routing.Shadow{SampleRate: 1}
		snapshot.Routes[routeSlug] = route
		for id, provider := range snapshot.Providers {
			for i := range provider.Capabilities {
				provider.Capabilities[i].ToolCalls = true
			}
			snapshot.Providers[id] = provider
		}
	})
	authority := h.rt.keys[fullKey]
	authority.ProjectID = &projectID
	h.rt.keys[fullKey] = authority
	fixture := &workflowFixture{runID: uuid.NewString(), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Classification: "read_only"}}}
	h.gateway.WorkflowRuns, h.gateway.WorkflowTools, h.gateway.Debits = fixture, fixture, &workflowDebitFixture{fakeSessionDebits: &fakeSessionDebits{}}
	h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":"done","object":"chat.completion","created":1,"model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
	})
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("workflow failed: status=%d body=%v", resp.StatusCode, body)
	}
	h.gateway.WaitShadows()
	if h.mock.count("a") != 1 || h.mock.count("b") != 0 {
		t.Fatalf("model calls a=%d b=%d", h.mock.count("a"), h.mock.count("b"))
	}
}

func TestGatewayToolExecutionAllowsEmptyToolsetSelection(t *testing.T) {
	toolsetID, projectID := uuid.NewString(), uuid.NewString()
	h := strictHarness(t, func(snapshot *routing.Snapshot) {
		route := snapshot.Routes[routeSlug]
		route.Fidelity.Mode = routing.FidelityTransformed
		route.ToolExecution, route.Toolsets = "gateway", []string{toolsetID}
		route.ProjectID = &projectID
		snapshot.Routes[routeSlug] = route
		for id, provider := range snapshot.Providers {
			for i := range provider.Capabilities {
				provider.Capabilities[i].ToolCalls = true
			}
			snapshot.Providers[id] = provider
		}
	})
	authority := h.rt.keys[fullKey]
	authority.ProjectID = &projectID
	h.rt.keys[fullKey] = authority
	fixture := &workflowFixture{runID: uuid.NewString(), tool: WorkflowTool{ToolsetID: toolsetID}}
	h.gateway.WorkflowRuns, h.gateway.WorkflowTools, h.gateway.Debits = fixture, fixture, &workflowDebitFixture{fakeSessionDebits: &fakeSessionDebits{}}
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		var request map[string]json.RawMessage
		if err := json.Unmarshal(body, &request); err != nil {
			t.Error(err)
		}
		if len(request["tools"]) != 0 || len(request["tool_choice"]) != 0 {
			t.Errorf("unselected tools were sent to the model: %s", body)
		}
		fmt.Fprint(w, `{"id":"done","object":"chat.completion","created":1,"model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
	})
	resp, body := h.chat(fullKey, nil, `,"olp_toolsets":[],"tools":[{"type":"function","function":{"name":"unpermitted","parameters":{"type":"object"}}}],"tool_choice":"required"`)
	if resp.StatusCode != http.StatusOK || h.mock.count("a") != 1 || len(fixture.calls) != 0 {
		t.Fatalf("empty selection failed: status=%d body=%v calls=%v", resp.StatusCode, body, fixture.calls)
	}
}

func TestGatewayToolExecutionStopsAfterAuthorityChanges(t *testing.T) {
	for _, test := range []struct {
		name          string
		changeAtModel bool
		wantStatus    int
		wantToolCalls int
	}{
		{name: "route access removed before tool", changeAtModel: true, wantStatus: http.StatusForbidden},
		{name: "key revoked before next model", wantStatus: http.StatusUnauthorized, wantToolCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			toolsetID, projectID := uuid.NewString(), uuid.NewString()
			alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			h := strictHarness(t, func(snapshot *routing.Snapshot) {
				route := snapshot.Routes[routeSlug]
				route.Fidelity.Mode = routing.FidelityTransformed
				route.ToolExecution, route.Toolsets, route.ProjectID = "gateway", []string{toolsetID}, &projectID
				snapshot.Routes[routeSlug] = route
				for id, provider := range snapshot.Providers {
					for i := range provider.Capabilities {
						provider.Capabilities[i].ToolCalls = true
					}
					snapshot.Providers[id] = provider
				}
			})
			authority := h.rt.keys[fullKey]
			authority.ProjectID = &projectID
			h.rt.keys[fullKey] = authority
			fixture := &workflowFixture{runID: uuid.NewString(), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "read_only"}}}
			if !test.changeAtModel {
				fixture.beforeCall = func() error {
					h.rt.mu.Lock()
					delete(h.rt.keys, fullKey)
					h.rt.mu.Unlock()
					return nil
				}
			}
			h.gateway.WorkflowRuns, h.gateway.WorkflowTools, h.gateway.Debits = fixture, fixture, &workflowDebitFixture{fakeSessionDebits: &fakeSessionDebits{}}
			h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
				if test.changeAtModel {
					h.rt.mu.Lock()
					current := h.rt.keys[fullKey]
					current.Policy.AllowedRoutes = []string{"different-route"}
					h.rt.keys[fullKey] = current
					h.rt.mu.Unlock()
				}
				fmt.Fprintf(w, `{"id":"step","object":"chat.completion","created":1,"model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":%q,"arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`, alias)
			})
			response, body := h.chat(fullKey, nil)
			if response.StatusCode != test.wantStatus || h.mock.count("a") != 1 || len(fixture.calls) != test.wantToolCalls {
				t.Fatalf("workflow continued after authority changed: status=%d body=%v model_calls=%d tool_calls=%d", response.StatusCode, body, h.mock.count("a"), len(fixture.calls))
			}
		})
	}
}
