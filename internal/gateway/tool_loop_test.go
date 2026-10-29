package gateway

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/mcp"
	"github.com/tyk-swe/olp/internal/protocols/sse"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

const nativeToolResult = "private-tool-result"

func TestGatewayToolExecutionRecoversCompressedInputAfterApproval(t *testing.T) {
	for _, previousRecovery := range []bool{false, true} {
		t.Run(fmt.Sprintf("previous_recovery_%t", previousRecovery), func(t *testing.T) {
			projectID, toolsetID := uuid.NewString(), uuid.NewString()
			alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			fixture := &workflowFixture{runID: uuid.NewString(), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "approval_required", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
			h := nativeWorkflowHarness(t, projectID, "openai", "openai-chat", "unary", routing.FidelityTransformed, []string{toolsetID}, true, fixture)
			approval := &approvalWorkflowFixture{workflowFixture: fixture, paused: make(chan agents.WorkflowApprovalRequest, 1)}
			h.gateway.WorkflowRuns = approval
			h.gateway.WorkflowTools = approval
			snapshot := h.rt.release.Snapshot
			snapshot.Guardrails = &guardrails.Registry{
				Definitions: map[string]guardrails.Revision{"revision": {ID: "revision", GuardrailID: "guard", ProjectID: projectID, Name: "Guard", Type: "headroom", Configuration: json.RawMessage(`{"endpoint":"https://example.com","credential_ref":"credential","content_egress":true,"parameters":{"model":"model"}}`), TimeoutMS: 1000, OnError: guardrails.FailClosed}},
				Policies:    map[string]guardrails.PolicyRevision{"policy-revision": {ID: "policy-revision", PolicyID: "policy", ProjectID: projectID, Entries: []guardrails.Entry{{GuardrailID: "guard", RevisionID: "revision", Phase: guardrails.Input, Action: guardrails.Redact}}}},
				Attachments: []guardrails.Attachment{{ID: "attachment", Scope: "route", ScopeID: snapshot.Routes[routeSlug].ID, PolicyRevisionID: "policy-revision"}},
			}
			if err := snapshot.Compile(); err != nil {
				t.Fatal(err)
			}
			h.gateway.GuardrailEvaluator = guardrails.EvaluatorFunc(func(_ context.Context, _ guardrails.Revision, evaluation guardrails.Evaluation) (guardrails.Verdict, error) {
				redactions := slices.Collect(func(yield func(guardrails.Redaction) bool) {
					for _, segment := range evaluation.Segments {
						if (segment.Role == "user" || segment.Role == "tool") && !yield(guardrails.Redaction{Segment: segment.ID, Start: 0, End: len(segment.Text), Replacement: "compressed summary"}) {
							return
						}
					}
				})
				return guardrails.Verdict{Action: guardrails.Redact, Redactions: redactions}, nil
			})
			const original = "approval-bound compressed context"
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Messages []struct {
						Role       string `json:"role"`
						Content    string `json:"content"`
						ToolCallID string `json:"tool_call_id"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				if slices.ContainsFunc(request.Messages, func(message struct {
					Role       string `json:"role"`
					Content    string `json:"content"`
					ToolCallID string `json:"tool_call_id"`
				}) bool {
					return message.Role == "tool" && message.ToolCallID == "approve-call"
				}) {
					if !slices.ContainsFunc(request.Messages, func(message struct {
						Role       string `json:"role"`
						Content    string `json:"content"`
						ToolCallID string `json:"tool_call_id"`
					}) bool {
						return message.Role == "tool" && message.ToolCallID == "recover-call" && strings.Contains(message.Content, original)
					}) {
						t.Error("the recovered content was not retained through approval")
					}
					completion(modelA, "approved recovery")(w, r)
					return
				}
				if previousRecovery {
					if slices.ContainsFunc(request.Messages, func(message struct {
						Role       string `json:"role"`
						Content    string `json:"content"`
						ToolCallID string `json:"tool_call_id"`
					}) bool {
						return message.Role == "tool" && message.ToolCallID == "recover-call"
					}) {
						fmt.Fprintf(w, `{"id":"second","object":"chat.completion","model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"approve-call","type":"function","function":{"name":%q,"arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`, alias)
						return
					}
					fmt.Fprint(w, `{"id":"first","object":"chat.completion","model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"recover-call","type":"function","function":{"name":"olp_recover_context","arguments":"{\"id\":0}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`)
					return
				}
				fmt.Fprintf(w, `{"id":"first","object":"chat.completion","model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"recover-call","type":"function","function":{"name":"olp_recover_context","arguments":"{\"id\":0}"}},{"id":"approve-call","type":"function","function":{"name":%q,"arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`, alias)
			})
			request := `{"model":"` + routeSlug + `","messages":[{"role":"user","content":"` + original + `"}]}`
			response := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(request), nil)
			defer response.Body.Close()
			var pause workflowPause
			if err := json.NewDecoder(response.Body).Decode(&pause); err != nil || response.StatusCode != http.StatusOK || pause.ContinuationToken == "" {
				t.Fatalf("approval pause failed: status=%d error=%v", response.StatusCode, err)
			}
			submission, err := json.Marshal(workflowResumeRequest{ContinuationToken: pause.ContinuationToken, Approved: true, Arguments: map[string]json.RawMessage{"approve-call": json.RawMessage(`{}`)}})
			if err != nil {
				t.Fatal(err)
			}
			resumed := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(strings.TrimSuffix(request, "}")+`,"olp_approval":`+string(submission)+`}`), nil)
			defer resumed.Body.Close()
			body, err := io.ReadAll(resumed.Body)
			if err != nil || resumed.StatusCode != http.StatusOK || !strings.Contains(string(body), "approved recovery") {
				t.Fatalf("approval recovery failed: status=%d body=%s error=%v", resumed.StatusCode, body, err)
			}
		})
	}
}

func TestGatewayToolExecutionRecoversCompressedInput(t *testing.T) {
	projectID, toolsetID := uuid.NewString(), uuid.NewString()
	fixture := &workflowFixture{
		runID: uuid.NewString(),
		tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{
			Alias: "published_tool", Classification: "read_only", InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
	}
	h := nativeWorkflowHarness(t, projectID, "openai", "openai-chat", "unary", routing.FidelityTransformed, []string{toolsetID}, true, fixture)
	snapshot := h.rt.release.Snapshot
	snapshot.Guardrails = &guardrails.Registry{
		Definitions: map[string]guardrails.Revision{"revision": {ID: "revision", GuardrailID: "guard", ProjectID: projectID, Name: "Guard", Type: "headroom", Configuration: json.RawMessage(`{"endpoint":"https://example.com","credential_ref":"credential","content_egress":true,"parameters":{"model":"model"}}`), TimeoutMS: 1000, OnError: guardrails.FailClosed}},
		Policies:    map[string]guardrails.PolicyRevision{"policy-revision": {ID: "policy-revision", PolicyID: "policy", ProjectID: projectID, Entries: []guardrails.Entry{{GuardrailID: "guard", RevisionID: "revision", Phase: guardrails.Input, Action: guardrails.Redact}}}},
		Attachments: []guardrails.Attachment{{ID: "attachment", Scope: "route", ScopeID: snapshot.Routes[routeSlug].ID, PolicyRevisionID: "policy-revision"}},
	}
	if err := snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	h.gateway.GuardrailEvaluator = guardrails.EvaluatorFunc(func(_ context.Context, _ guardrails.Revision, evaluation guardrails.Evaluation) (guardrails.Verdict, error) {
		redactions := slices.Collect(func(yield func(guardrails.Redaction) bool) {
			for _, segment := range evaluation.Segments {
				if segment.Role == "user" && !yield(guardrails.Redaction{Segment: segment.ID, Start: 0, End: len(segment.Text), Replacement: "compressed summary"}) {
					return
				}
			}
		})
		return guardrails.Verdict{Action: guardrails.Redact, Redactions: redactions}, nil
	})
	const original = "original context retained during this request"
	const later = "later context selected for recovery"
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		for _, message := range request.Messages {
			if message.Role == "user" && !strings.HasPrefix(message.Content, "compressed summary") {
				t.Errorf("user context was not compressed: %s", message.Content)
			}
			if message.Role == "tool" {
				if !strings.Contains(message.Content, later) || strings.Contains(message.Content, original) {
					t.Errorf("recovered tool output did not select the requested context: %s", message.Content)
				}
				completion(modelA, "recovered")(w, r)
				return
			}
		}
		if len(request.Messages) < 2 || !strings.Contains(request.Messages[0].Content, "recovery id: 0") || !strings.Contains(request.Messages[1].Content, "recovery id: 1") {
			t.Errorf("provider did not receive the compressed message: %+v", request.Messages)
		}
		if !slices.ContainsFunc(request.Tools, func(tool struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}) bool {
			return tool.Function.Name == "olp_recover_context"
		}) {
			t.Error("provider did not receive the compression recovery tool")
		}
		fmt.Fprint(w, `{"id":"first","object":"chat.completion","model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"recover-call","type":"function","function":{"name":"olp_recover_context","arguments":"{\"id\":1}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`)
	})
	response := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"`+original+`"},{"role":"user","content":"`+later+`"}]}`), nil)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), "recovered") {
		t.Fatalf("recovery request failed: status=%d body=%s error=%v", response.StatusCode, body, err)
	}
}

func TestGatewayToolExecutionGuardsRecoveredInput(t *testing.T) {
	projectID, toolsetID := uuid.NewString(), uuid.NewString()
	fixture := &workflowFixture{
		runID: uuid.NewString(),
		tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{
			Alias: "published_tool", Classification: "read_only", InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
	}
	h := nativeWorkflowHarness(t, projectID, "openai", "openai-chat", "unary", routing.FidelityTransformed, []string{toolsetID}, true, fixture)
	snapshot := h.rt.release.Snapshot
	snapshot.Guardrails = &guardrails.Registry{
		Definitions: map[string]guardrails.Revision{
			"compression": {ID: "compression", GuardrailID: "compress", ProjectID: projectID, Name: "Compressor", Type: "headroom", Configuration: json.RawMessage(`{"endpoint":"https://example.com","credential_ref":"credential","content_egress":true,"parameters":{"model":"model"}}`), TimeoutMS: 1000, OnError: guardrails.FailClosed},
			"block":       {ID: "block", GuardrailID: "blocked", ProjectID: projectID, Name: "Block", Type: "builtin.keywords", Configuration: json.RawMessage(`{"words":["private-original"]}`), TimeoutMS: 1000, OnError: guardrails.FailClosed},
		},
		Policies: map[string]guardrails.PolicyRevision{"policy": {ID: "policy", PolicyID: "policy-id", ProjectID: projectID, Entries: []guardrails.Entry{
			{GuardrailID: "compress", RevisionID: "compression", Phase: guardrails.Input, Action: guardrails.Redact},
			{GuardrailID: "blocked", RevisionID: "block", Phase: guardrails.Input, Action: guardrails.Block},
		}}},
		Attachments: []guardrails.Attachment{{ID: "attachment", Scope: "route", ScopeID: snapshot.Routes[routeSlug].ID, PolicyRevisionID: "policy"}},
	}
	if err := snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	h.gateway.GuardrailEvaluator = guardrails.EvaluatorFunc(func(_ context.Context, _ guardrails.Revision, evaluation guardrails.Evaluation) (guardrails.Verdict, error) {
		segment := evaluation.Segments[0]
		return guardrails.Verdict{Action: guardrails.Redact, Redactions: []guardrails.Redaction{{Segment: segment.ID, Start: 0, End: len(segment.Text), Replacement: "compressed summary"}}}, nil
	})
	dispatched := make(chan struct{}, 2)
	h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
		dispatched <- struct{}{}
		fmt.Fprint(w, `{"id":"first","object":"chat.completion","model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"recover-call","type":"function","function":{"name":"olp_recover_context","arguments":"{\"id\":0}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`)
	})
	response := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"private-original"}]}`), nil)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode == http.StatusOK || !strings.Contains(string(body), "guardrail_blocked") || strings.Contains(string(body), "private-original") {
		t.Fatalf("recovered content escaped input guardrails: status=%d body=%s error=%v", response.StatusCode, body, err)
	}
	select {
	case <-dispatched:
	default:
		t.Fatal("model did not request recovery")
	}
	select {
	case <-dispatched:
		t.Fatal("recovered content reached the model after the guardrail blocked it")
	default:
	}
}

func TestGatewayToolExecutionResponsesDoesNotExposeRecoveredContent(t *testing.T) {
	projectID, toolsetID := uuid.NewString(), uuid.NewString()
	fixture := &workflowFixture{
		runID: uuid.NewString(),
		tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{
			Alias: "published_tool", Classification: "read_only", InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
	}
	h := nativeWorkflowHarness(t, projectID, "openai", "openai-responses", "unary", routing.FidelityTransformed, []string{toolsetID}, true, fixture, true)
	snapshot := h.rt.release.Snapshot
	snapshot.Guardrails = &guardrails.Registry{
		Definitions: map[string]guardrails.Revision{"revision": {ID: "revision", GuardrailID: "guard", ProjectID: projectID, Name: "Guard", Type: "headroom", Configuration: json.RawMessage(`{"endpoint":"https://example.com","credential_ref":"credential","content_egress":true,"parameters":{"model":"model"}}`), TimeoutMS: 1000, OnError: guardrails.FailClosed}},
		Policies:    map[string]guardrails.PolicyRevision{"policy-revision": {ID: "policy-revision", PolicyID: "policy", ProjectID: projectID, Entries: []guardrails.Entry{{GuardrailID: "guard", RevisionID: "revision", Phase: guardrails.Input, Action: guardrails.Redact}}}},
		Attachments: []guardrails.Attachment{{ID: "attachment", Scope: "route", ScopeID: snapshot.Routes[routeSlug].ID, PolicyRevisionID: "policy-revision"}},
	}
	if err := snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	h.gateway.GuardrailEvaluator = guardrails.EvaluatorFunc(func(_ context.Context, _ guardrails.Revision, evaluation guardrails.Evaluation) (guardrails.Verdict, error) {
		segment := evaluation.Segments[0]
		return guardrails.Verdict{Action: guardrails.Redact, Redactions: []guardrails.Redaction{{Segment: segment.ID, Start: 0, End: len(segment.Text), Replacement: "compressed summary"}}}, nil
	})
	const original = "private original for this workflow"
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if bytes.Contains(request.Input, []byte(`"function_call_output"`)) {
			if !bytes.Contains(request.Input, []byte(original)) {
				t.Errorf("provider did not receive the recovered content: %s", request.Input)
			}
			fmt.Fprint(w, `{"id":"last","object":"response","status":"completed","model":"model-a","output":[{"type":"message","id":"msg","role":"assistant","status":"completed","content":[{"type":"output_text","text":"safe answer","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}`)
			return
		}
		fmt.Fprint(w, `{"id":"first","object":"response","status":"completed","model":"model-a","output":[{"type":"function_call","id":"fc","call_id":"recover-call","name":"olp_recover_context","arguments":"{\"id\":0}","status":"completed"}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`)
	})
	response := h.do(t.Context(), http.MethodPost, "/v1/responses", fullKey, []byte(`{"model":"`+routeSlug+`","input":"`+original+`"}`), nil)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), "safe answer") || strings.Contains(string(body), original) || strings.Contains(string(body), "olp_recover_context") {
		t.Fatalf("response exposed recovered content: status=%d body=%s error=%v", response.StatusCode, body, err)
	}
}

func nativeWorkflowHarness(t *testing.T, projectID, surface, profile, mode, fidelity string, toolsets []string, certified bool, fixture *workflowFixture, responsesMCP ...bool) *harness {
	return nativeWorkflowHarnessWithProviders(t, projectID, surface, profile, mode, fidelity, toolsets, certified, fixture, nil, responsesMCP...)
}

func nativeWorkflowHarnessWithProviders(t *testing.T, projectID, surface, profile, mode, fidelity string, toolsets []string, certified bool, fixture *workflowFixture, configure func(routing.Provider) routing.Provider, responsesMCP ...bool) *harness {
	t.Helper()
	h := strictHarness(t, func(snapshot *routing.Snapshot) {
		route := snapshot.Routes[routeSlug]
		// The existing harness configures its shared snapshot through this callback.
		snapshot.Routes = map[string]routing.Route{routeSlug: {
			ID: route.ID, Slug: route.Slug, Operations: route.Operations, OverallTimeout: route.OverallTimeout,
			MaxAttempts: route.MaxAttempts, Targets: route.Targets, RoutingID: route.RoutingID,
			RevisionID: route.RevisionID, Revision: route.Revision, PublishedAt: route.PublishedAt,
			Policy: route.Policy, ProjectID: &projectID, ContentPolicy: route.ContentPolicy,
			Fidelity: routing.RouteFidelity{Mode: fidelity},
			Behavior: routing.Behavior{ToolExecution: "gateway", Toolsets: toolsets, ResponsesMCPItems: len(responsesMCP) > 0 && responsesMCP[0]},
		}}
		snapshot.Providers = maps.Collect(func(yield func(string, routing.Provider) bool) {
			for id, provider := range snapshot.Providers {
				if !yield(id, configureWorkflowProvider(routing.Provider{
					ID: provider.ID, Name: provider.Name, Kind: workflowProviderKind(surface, profile), Enabled: provider.Enabled,
					ActiveCredential: provider.ActiveCredential, RevisionID: provider.RevisionID,
					Endpoint: provider.Endpoint, AuthMode: provider.AuthMode, Slots: provider.Slots,
					ProfileID: profile, ProfileRevision: connectors.ProfileRevision,
					Capabilities: []routing.Capability{{Model: provider.Capabilities[0].Model, Operation: "generation", Surface: surface, Mode: mode, ToolCalls: certified}},
				}, configure)) {
					return
				}
			}
		})
	})
	h.rt.keys[fullKey] = access.Authority{ID: h.rt.keys[fullKey].ID, ProjectID: &projectID, Policy: h.rt.keys[fullKey].Policy}
	h.gateway.WorkflowRuns, h.gateway.WorkflowTools, h.gateway.Debits = fixture, fixture, &workflowDebitFixture{fakeSessionDebits: &fakeSessionDebits{}}
	return h
}

func configureWorkflowProvider(provider routing.Provider, configure func(routing.Provider) routing.Provider) routing.Provider {
	if configure == nil {
		return provider
	}
	return configure(provider)
}

func workflowProviderKind(surface, profile string) string {
	if profile == "compatible-responses" {
		return "openai_compatible"
	}
	return surface
}

func TestGatewayToolExecutionResponsesMixedContractsReturnFinalOnly(t *testing.T) {
	for _, test := range []struct {
		name         string
		firstProfile string
		finalProfile string
	}{
		{name: "built-in then compatible", firstProfile: "openai-responses", finalProfile: "compatible-responses"},
		{name: "compatible then built-in", firstProfile: "compatible-responses", finalProfile: "openai-responses"},
	} {
		t.Run(test.name, func(t *testing.T) {
			toolsetID, projectID := uuid.NewString(), uuid.NewString()
			alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"id":"first","object":"response","status":"completed","output":[{"type":"function_call","id":"item-first","call_id":"call-first","name":%q,"arguments":"{}"}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`, alias)
			}))
			t.Cleanup(first.Close)
			fixture := &workflowFixture{
				runID: uuid.NewString(),
				tool:  WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "read_only", InputSchema: json.RawMessage(`{"type":"object"}`)}},
			}
			profileFor := func(name string) string {
				if name == "a" {
					return test.firstProfile
				}
				return test.finalProfile
			}
			endpointFor := func(provider routing.Provider) string {
				if provider.Name == "a" {
					return first.URL + "/v1"
				}
				return provider.Endpoint
			}
			h := nativeWorkflowHarnessWithProviders(t, projectID, "openai", "openai-responses", "unary", routing.FidelityTransformed, []string{toolsetID}, true, fixture, func(provider routing.Provider) routing.Provider {
				return routing.Provider{
					ID: provider.ID, Name: provider.Name, Kind: workflowProviderKind("openai", profileFor(provider.Name)), Enabled: provider.Enabled,
					ActiveCredential: provider.ActiveCredential, RevisionID: provider.RevisionID, Endpoint: endpointFor(provider), AuthMode: provider.AuthMode, Slots: provider.Slots,
					ProfileID: profileFor(provider.Name), ProfileRevision: provider.ProfileRevision, Capabilities: provider.Capabilities,
				}
			}, true)
			firstRoute := h.rt.Release().Snapshot.Routes[routeSlug]
			firstProvider := h.rt.Release().Snapshot.Providers[firstRoute.Targets[0].ProviderID]
			firstCredential := *firstProvider.Slots[0].CredentialID
			fixture.beforeCall = func() error {
				h.rt.mu.Lock()
				defer h.rt.mu.Unlock()
				h.rt.revoked[firstCredential] = true
				return nil
			}
			h.mock.set("b", func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, `{"id":"final","object":"response","status":"completed","output":[{"id":"msg-final","type":"message","role":"assistant","content":[{"type":"output_text","text":"fallback final"}]}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`)
			})
			response := h.do(t.Context(), http.MethodPost, "/v1/responses", fullKey, []byte(`{"model":"`+routeSlug+`","input":"hello"}`), nil)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK {
				t.Fatalf("mixed-profile workflow status=%d body=%s err=%v", response.StatusCode, body, err)
			}
			var result struct {
				Output []struct {
					Type string `json:"type"`
				} `json:"output"`
			}
			if err := json.Unmarshal(body, &result); err != nil || len(result.Output) != 1 || result.Output[0].Type != "message" || !strings.Contains(string(body), "fallback final") {
				t.Fatalf("mixed-profile workflow exposed MCP items: %s err=%v", body, err)
			}
		})
	}
}

type anthropicWorkflowRequest struct {
	Tools []struct {
		Name        string                     `json:"name"`
		InputSchema map[string]json.RawMessage `json:"input_schema"`
	} `json:"tools"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

type geminiWorkflowRequest struct {
	Tools []struct {
		Functions []struct {
			Name string `json:"name"`
		} `json:"functionDeclarations"`
	} `json:"tools"`
	Contents []struct {
		Parts []struct {
			ThoughtSignature string `json:"thoughtSignature"`
			FunctionCall     *struct {
				Name string `json:"name"`
			} `json:"functionCall"`
			FunctionResponse *struct {
				Name     string `json:"name"`
				Response struct {
					Output string `json:"output"`
				} `json:"response"`
			} `json:"functionResponse"`
		} `json:"parts"`
	} `json:"contents"`
}

func assertNativeToolOutput(t *testing.T, output string) {
	t.Helper()
	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil || len(result.Content) != 1 || result.Content[0].Text != nativeToolResult {
		t.Fatalf("provider received the wrong tool output: %q", output)
	}
}

func assertAnthropicWorkflowRequests(t *testing.T, first, second []byte, alias string, thinking bool) {
	t.Helper()
	var initial, continued anthropicWorkflowRequest
	if err := json.Unmarshal(first, &initial); err != nil || len(initial.Tools) != 1 || initial.Tools[0].Name != alias || string(initial.Tools[0].InputSchema["type"]) != `"object"` {
		t.Fatalf("provider did not receive the native tool definition: %s", first)
	}
	if err := json.Unmarshal(second, &continued); err != nil || len(continued.Messages) != 3 || continued.Messages[1].Role != "assistant" || continued.Messages[2].Role != "user" {
		t.Fatalf("provider did not receive native continuation messages: %s", second)
	}
	var assistant []struct {
		Type, ID, Name, Thinking, Signature string
		Input                               struct{ Value int }
	}
	if err := json.Unmarshal(continued.Messages[1].Content, &assistant); err != nil {
		t.Fatalf("invalid assistant content: %s", second)
	}
	if thinking {
		if len(assistant) != 2 || assistant[0].Type != "thinking" || assistant[0].Thinking != "private thought" || assistant[0].Signature != "signed" {
			t.Fatalf("provider lost the signed thinking block: %s", second)
		}
	}
	if len(assistant) < 1 || assistant[len(assistant)-1].Type != "tool_use" || assistant[len(assistant)-1].ID != "call-1" || assistant[len(assistant)-1].Name != alias || assistant[len(assistant)-1].Input.Value != 1 {
		t.Fatalf("provider lost the native tool call: %s", second)
	}
	var results []struct {
		Type      string `json:"type"`
		ToolUseID string `json:"tool_use_id"`
		Content   []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(continued.Messages[2].Content, &results); err != nil || len(results) != 1 || results[0].Type != "tool_result" || results[0].ToolUseID != "call-1" || len(results[0].Content) != 1 {
		t.Fatalf("provider did not receive the paired tool result: %s", second)
	}
	assertNativeToolOutput(t, results[0].Content[0].Text)
}

func assertGeminiWorkflowRequests(t *testing.T, first, second []byte, alias string) {
	t.Helper()
	var initial, continued geminiWorkflowRequest
	if err := json.Unmarshal(first, &initial); err != nil || len(initial.Tools) != 1 || len(initial.Tools[0].Functions) != 1 || initial.Tools[0].Functions[0].Name != alias {
		t.Fatalf("provider did not receive the native tool definition: %s", first)
	}
	if err := json.Unmarshal(second, &continued); err != nil || len(continued.Contents) != 3 {
		t.Fatalf("provider did not receive native continuation contents: %s", second)
	}
	parts := continued.Contents[1].Parts
	if len(parts) == 0 || parts[len(parts)-1].FunctionCall == nil || parts[len(parts)-1].FunctionCall.Name != alias || parts[len(parts)-1].ThoughtSignature != "signed" {
		t.Fatalf("provider did not receive the signed tool call: %s", second)
	}
	results := continued.Contents[2].Parts
	if len(results) != 1 || results[0].FunctionResponse == nil || results[0].FunctionResponse.Name != alias {
		t.Fatalf("provider did not receive the paired tool result: %s", second)
	}
	assertNativeToolOutput(t, results[0].FunctionResponse.Response.Output)
}

func TestGatewayToolExecutionAnthropicCompletesOneRun(t *testing.T) {
	toolsetID, projectID := uuid.NewString(), uuid.NewString()
	alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	fixture := &workflowFixture{runID: uuid.NewString(), result: json.RawMessage(`{"content":[{"type":"text","text":"private-tool-result"}]}`), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "read_only", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	h := nativeWorkflowHarness(t, projectID, "anthropic", "anthropic-messages", "unary", routing.FidelityTransformed, []string{toolsetID}, true, fixture)
	upstream := make(chan []byte, 2)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstream <- body
		w.Header().Set("Content-Type", "application/json")
		if h.mock.count("a") == 1 {
			fmt.Fprintf(w, `{"id":"first","type":"message","role":"assistant","model":"model-a","content":[{"type":"text","text":"intermediate"},{"type":"tool_use","id":"call-1","name":%q,"input":{"value":1}}],"stop_reason":"tool_use","usage":{"input_tokens":2,"output_tokens":3}}`, alias)
			return
		}
		io.WriteString(w, `{"id":"last","type":"message","role":"assistant","model":"model-a","content":[{"type":"text","text":"final answer"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":4}}`)
	})
	response := h.do(t.Context(), http.MethodPost, "/anthropic/v1/messages", fullKey, []byte(`{"model":"`+routeSlug+`","max_tokens":20,"messages":[{"role":"user","content":"hello"}]}`), nil)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "final answer") || strings.Contains(string(body), "intermediate") || strings.Contains(string(body), "call-1") || strings.Contains(string(body), nativeToolResult) || h.mock.count("a") != 2 {
		t.Fatalf("anthropic workflow did not return the final answer: status=%d body=%s calls=%d", response.StatusCode, body, h.mock.count("a"))
	}
	first := <-upstream
	second := <-upstream
	assertAnthropicWorkflowRequests(t, first, second, alias, false)
}

func TestGatewayToolExecutionGeminiCompletesOneRun(t *testing.T) {
	toolsetID, projectID := uuid.NewString(), uuid.NewString()
	alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	fixture := &workflowFixture{runID: uuid.NewString(), result: json.RawMessage(`{"content":[{"type":"text","text":"private-tool-result"}]}`), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "read_only", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	h := nativeWorkflowHarness(t, projectID, "gemini", "gemini-generation", "unary", routing.FidelityTransformed, []string{toolsetID}, true, fixture)
	upstream := make(chan []byte, 2)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstream <- body
		w.Header().Set("Content-Type", "application/json")
		if h.mock.count("a") == 1 {
			fmt.Fprintf(w, `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"intermediate"},{"functionCall":{"name":%q,"args":{"value":1}},"thoughtSignature":"signed"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}`, alias)
			return
		}
		io.WriteString(w, `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"final answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":4,"totalTokenCount":7}}`)
	})
	response := h.do(t.Context(), http.MethodPost, "/gemini/v1beta/models/"+routeSlug+":generateContent", fullKey, []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`), nil)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "final answer") || strings.Contains(string(body), "intermediate") || strings.Contains(string(body), alias) || strings.Contains(string(body), nativeToolResult) || h.mock.count("a") != 2 {
		t.Fatalf("gemini workflow did not return the final answer: status=%d body=%s calls=%d", response.StatusCode, body, h.mock.count("a"))
	}
	first := <-upstream
	second := <-upstream
	assertGeminiWorkflowRequests(t, first, second, alias)
}

func TestGatewayToolExecutionGeminiStreamsOnlyFinalAnswer(t *testing.T) {
	toolsetID, projectID := uuid.NewString(), uuid.NewString()
	alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	fixture := &workflowFixture{runID: uuid.NewString(), result: json.RawMessage(`{"content":[{"type":"text","text":"private-tool-result"}]}`), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "read_only", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	h := nativeWorkflowHarness(t, projectID, "gemini", "gemini-generation", "streaming", routing.FidelityTransformed, []string{toolsetID}, true, fixture)
	upstream := make(chan []byte, 2)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstream <- body
		w.Header().Set("Content-Type", "text/event-stream")
		if h.mock.count("a") == 1 {
			fmt.Fprintf(w, "data: {\"candidates\":[{\"index\":0,\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"intermediate\"},{\"functionCall\":{\"name\":%q,\"args\":{\"value\":1}},\"thoughtSignature\":\"signed\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":2,\"candidatesTokenCount\":3,\"totalTokenCount\":5}}\n\n", alias)
			return
		}
		io.WriteString(w, "data: {\"candidates\":[{\"index\":0,\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"final answer\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":4,\"totalTokenCount\":7}}\n\n")
	})
	response := h.do(t.Context(), http.MethodPost, "/gemini/v1beta/models/"+routeSlug+":streamGenerateContent?alt=sse", fullKey, []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`), nil)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "final answer") || strings.Contains(string(body), "intermediate") || strings.Contains(string(body), alias) || strings.Contains(string(body), nativeToolResult) || h.mock.count("a") != 2 {
		t.Fatalf("gemini stream exposed an intermediate step or missed the final answer: status=%d body=%s calls=%d", response.StatusCode, body, h.mock.count("a"))
	}
	first := <-upstream
	second := <-upstream
	assertGeminiWorkflowRequests(t, first, second, alias)
}

func TestGatewayToolExecutionGeminiRejectsMultipleStreamCandidates(t *testing.T) {
	toolsetID, projectID := uuid.NewString(), uuid.NewString()
	alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	fixture := &workflowFixture{runID: uuid.NewString(), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "read_only"}}}
	h := nativeWorkflowHarness(t, projectID, "gemini", "gemini-generation", "streaming", routing.FidelityTransformed, []string{toolsetID}, true, fixture)
	h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, `data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"first answer"}]},"finishReason":"STOP"},{"index":1,"content":{"role":"model","parts":[{"functionCall":{"name":%q,"args":{}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}`+"\n\n", alias)
	})
	response := h.do(t.Context(), http.MethodPost, "/gemini/v1beta/models/"+routeSlug+":streamGenerateContent?alt=sse", fullKey, []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`), nil)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode == http.StatusOK || strings.Contains(string(body), alias) {
		t.Fatalf("multiple stream candidates exposed an unexecuted tool call: status=%d body=%s", response.StatusCode, body)
	}
}

func TestGatewayToolExecutionAnthropicStreamsOnlyFinalAnswer(t *testing.T) {
	toolsetID, projectID := uuid.NewString(), uuid.NewString()
	alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	fixture := &workflowFixture{runID: uuid.NewString(), result: json.RawMessage(`{"content":[{"type":"text","text":"private-tool-result"}]}`), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "read_only", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	h := nativeWorkflowHarness(t, projectID, "anthropic", "anthropic-messages", "streaming", routing.FidelityTransformed, []string{toolsetID}, true, fixture)
	upstream := make(chan []byte, 2)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstream <- body
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"model-a\",\"usage\":{\"input_tokens\":2}}}\n\n")
		if h.mock.count("a") == 1 {
			io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"private thought\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"signed\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
			fmt.Fprintf(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call-1\",\"name\":%q,\"input\":{}}}\n\n", alias)
			io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"value\\\":1}\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":3}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return
		}
		io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"final answer\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	response := h.do(t.Context(), http.MethodPost, "/anthropic/v1/messages", fullKey, []byte(`{"model":"`+routeSlug+`","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello"}]}`), nil)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "final answer") || strings.Contains(string(body), "call-1") || strings.Contains(string(body), alias) || strings.Contains(string(body), nativeToolResult) || strings.Contains(string(body), "private thought") || h.mock.count("a") != 2 {
		t.Fatalf("anthropic stream exposed an intermediate step or missed the final answer: status=%d body=%s calls=%d", response.StatusCode, body, h.mock.count("a"))
	}
	first := <-upstream
	second := <-upstream
	assertAnthropicWorkflowRequests(t, first, second, alias, true)
}

func TestGatewayToolExecutionNativeRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, profile, surface, path, body, fidelity, code string
		certified                                          bool
	}{
		{"anthropic strict", "anthropic-messages", "anthropic", "/anthropic/v1/messages", `{"model":"` + routeSlug + `","max_tokens":20,"messages":[{"role":"user","content":"hello"}]}`, routing.FidelityStrict, "tool_execution_strict_route", true},
		{"gemini strict", "gemini-generation", "gemini", "/gemini/v1beta/models/" + routeSlug + ":generateContent", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`, routing.FidelityStrict, "tool_execution_strict_route", true},
		{"anthropic uncertified", "anthropic-messages", "anthropic", "/anthropic/v1/messages", `{"model":"` + routeSlug + `","max_tokens":20,"messages":[{"role":"user","content":"hello"}]}`, routing.FidelityTransformed, "upstream_unavailable", false},
		{"gemini uncertified", "gemini-generation", "gemini", "/gemini/v1beta/models/" + routeSlug + ":generateContent", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`, routing.FidelityTransformed, "upstream_unavailable", false},
		{"gemini candidates", "gemini-generation", "gemini", "/gemini/v1beta/models/" + routeSlug + ":generateContent", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generationConfig":{"candidateCount":2}}`, routing.FidelityTransformed, "tool_execution_choices_unsupported", true},
		{"gemini candidate alias", "gemini-generation", "gemini", "/gemini/v1beta/models/" + routeSlug + ":generateContent", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generationConfig":{"candidate_count":2}}`, routing.FidelityTransformed, "tool_execution_choices_unsupported", true},
		{"gemini snake case config", "gemini-generation", "gemini", "/gemini/v1beta/models/" + routeSlug + ":generateContent", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generation_config":{"candidate_count":2}}`, routing.FidelityTransformed, "tool_execution_choices_unsupported", true},
		{"gemini snake case config with camel candidate", "gemini-generation", "gemini", "/gemini/v1beta/models/" + routeSlug + ":generateContent", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generation_config":{"candidateCount":2}}`, routing.FidelityTransformed, "tool_execution_choices_unsupported", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projectID := uuid.NewString()
			fixture := &workflowFixture{runID: uuid.NewString()}
			h := nativeWorkflowHarness(t, projectID, tc.surface, tc.profile, "unary", tc.fidelity, nil, tc.certified, fixture)
			response := h.do(t.Context(), http.MethodPost, tc.path, fullKey, []byte(tc.body), nil)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode == http.StatusOK || !strings.Contains(string(body), tc.code) || h.mock.count("a") != 0 || h.mock.count("b") != 0 {
				t.Fatalf("native workflow refusal did not stop dispatch: status=%d body=%s", response.StatusCode, body)
			}
		})
	}
}

func TestGatewayToolExecutionNativeBounds(t *testing.T) {
	for _, dialect := range []struct {
		name, profile, path, body, tool, response string
	}{
		{"anthropic", "anthropic-messages", "/anthropic/v1/messages", `{"model":"` + routeSlug + `","max_tokens":20,"messages":[{"role":"user","content":"hello"}]}`, `{"type":"tool_use","id":"call-1","name":%q,"input":{}}`, `{"id":"bounded","type":"message","role":"assistant","model":"model-a","content":[%s],"stop_reason":"tool_use","usage":{"input_tokens":2,"output_tokens":3}}`},
		{"gemini", "gemini-generation", "/gemini/v1beta/models/" + routeSlug + ":generateContent", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`, `{"functionCall":{"name":%q,"args":{}}}`, `{"candidates":[{"content":{"role":"model","parts":[%s]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}`},
	} {
		for _, bound := range []struct {
			name, code    string
			calls, models int
		}{
			{"iterations", "tool_iteration_limit", 1, 8},
			{"calls", "tool_call_limit", 33, 1},
		} {
			t.Run(dialect.name+"/"+bound.name, func(t *testing.T) {
				toolsetID, projectID := uuid.NewString(), uuid.NewString()
				alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				fixture := &workflowFixture{runID: uuid.NewString(), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "read_only"}}, result: json.RawMessage(`{"content":[{"type":"text","text":"private-tool-result"}]}`), beforeCall: func() error {
					if bound.calls > 1 {
						return fmt.Errorf("tool was dispatched before the call limit refusal")
					}
					return nil
				}}
				h := nativeWorkflowHarness(t, projectID, dialect.name, dialect.profile, "unary", routing.FidelityTransformed, []string{toolsetID}, true, fixture)
				upstream := make(chan []byte, bound.models)
				h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
					request, _ := io.ReadAll(r.Body)
					upstream <- request
					parts := strings.TrimSuffix(strings.Repeat(fmt.Sprintf(dialect.tool, alias)+",", bound.calls), ",")
					fmt.Fprintf(w, dialect.response, parts)
				})
				response := h.do(t.Context(), http.MethodPost, dialect.path, fullKey, []byte(dialect.body), nil)
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), bound.code) || h.mock.count("a") != bound.models {
					t.Fatalf("native workflow did not enforce its %s bound: status=%d body=%s model_calls=%d", bound.name, response.StatusCode, body, h.mock.count("a"))
				}
				for step := 0; step < bound.models; step++ {
					request := <-upstream
					if dialect.name == "anthropic" {
						var observed anthropicWorkflowRequest
						if err := json.Unmarshal(request, &observed); err != nil || len(observed.Messages) != 1+2*step {
							t.Fatalf("anthropic model request omitted tool results at step %d: %s", step, request)
						}
						if step > 0 {
							var blocks []struct {
								Type    string `json:"type"`
								Content []struct {
									Text string `json:"text"`
								} `json:"content"`
							}
							if err := json.Unmarshal(observed.Messages[len(observed.Messages)-1].Content, &blocks); err != nil || len(blocks) != 1 || blocks[0].Type != "tool_result" || len(blocks[0].Content) != 1 {
								t.Fatalf("anthropic model request lacks a tool result at step %d: %s", step, request)
							}
							assertNativeToolOutput(t, blocks[0].Content[0].Text)
						}
					} else {
						var observed geminiWorkflowRequest
						if err := json.Unmarshal(request, &observed); err != nil || len(observed.Contents) != 1+2*step {
							t.Fatalf("gemini model request omitted tool results at step %d: %s", step, request)
						}
						if step > 0 {
							parts := observed.Contents[len(observed.Contents)-1].Parts
							if len(parts) != 1 || parts[0].FunctionResponse == nil {
								t.Fatalf("gemini model request lacks a tool result at step %d: %s", step, request)
							}
							assertNativeToolOutput(t, parts[0].FunctionResponse.Response.Output)
						}
					}
				}
			})
		}
	}
}

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

func TestGatewayToolExecutionRejectsMultipleChoicesBeforeDispatch(t *testing.T) {
	projectID := uuid.NewString()
	h := strictHarness(t, func(snapshot *routing.Snapshot) {
		route := snapshot.Routes[routeSlug]
		route.Fidelity.Mode, route.ToolExecution, route.ProjectID = routing.FidelityTransformed, "gateway", &projectID
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
	fixture := &workflowFixture{runID: uuid.NewString()}
	h.gateway.WorkflowRuns, h.gateway.WorkflowTools, h.gateway.Debits = fixture, fixture, &workflowDebitFixture{fakeSessionDebits: &fakeSessionDebits{}}
	response, body := h.chat(fullKey, nil, `,"n":2`)
	if response.StatusCode != http.StatusBadRequest || errorCode(t, body) != "tool_execution_choices_unsupported" || h.mock.count("a") != 0 || h.mock.count("b") != 0 {
		t.Fatalf("multiple choices were not refused before dispatch: status=%d body=%v model_calls=%d", response.StatusCode, body, h.mock.count("a")+h.mock.count("b"))
	}
}

func TestGatewayToolExecutionDistinguishesUnsentAndSentFailures(t *testing.T) {
	for _, test := range []struct {
		name string
		sent bool
	}{
		{name: "connection refused"},
		{name: "request sent", sent: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			endpoint := "http://" + listener.Addr().String() + "/v1"
			if err := listener.Close(); err != nil {
				t.Fatal(err)
			}
			projectID := uuid.NewString()
			h := strictHarness(t, func(snapshot *routing.Snapshot) {
				route := snapshot.Routes[routeSlug]
				route.Fidelity.Mode, route.ToolExecution, route.ProjectID = routing.FidelityTransformed, "gateway", &projectID
				snapshot.Routes[routeSlug] = route
				for id, provider := range snapshot.Providers {
					for i := range provider.Capabilities {
						provider.Capabilities[i].ToolCalls = true
					}
					if !test.sent && provider.Name == "a" {
						provider.Endpoint = endpoint
					}
					snapshot.Providers[id] = provider
				}
			})
			authority := h.rt.keys[fullKey]
			authority.ProjectID = &projectID
			h.rt.keys[fullKey] = authority
			fixture := &workflowFixture{runID: uuid.NewString()}
			h.gateway.WorkflowRuns, h.gateway.WorkflowTools, h.gateway.Debits = fixture, fixture, &workflowDebitFixture{fakeSessionDebits: &fakeSessionDebits{}}
			if test.sent {
				h.mock.set("a", status(http.StatusInternalServerError, `{"error":{"message":"failed"}}`))
			}
			response, body := h.chat(fullKey, nil)
			if test.sent {
				if response.StatusCode == http.StatusOK || h.mock.count("a") != 1 || h.mock.count("b") != 0 {
					t.Fatalf("sent request was retried: status=%d body=%v a=%d b=%d", response.StatusCode, body, h.mock.count("a"), h.mock.count("b"))
				}
			} else if response.StatusCode != http.StatusOK || h.mock.count("b") != 1 {
				t.Fatalf("unsent request did not fail over: status=%d body=%v b=%d", response.StatusCode, body, h.mock.count("b"))
			}
		})
	}
}

func TestGatewayToolExecutionRecordsProviderHealth(t *testing.T) {
	projectID := uuid.NewString()
	h := strictHarness(t, func(snapshot *routing.Snapshot) {
		route := snapshot.Routes[routeSlug]
		route.Fidelity.Mode, route.ToolExecution, route.ProjectID = routing.FidelityTransformed, "gateway", &projectID
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
	fixture := &workflowFixture{runID: uuid.NewString()}
	h.gateway.WorkflowRuns, h.gateway.WorkflowTools, h.gateway.Debits = fixture, fixture, &workflowDebitFixture{fakeSessionDebits: &fakeSessionDebits{}}
	now := time.Now()
	h.gateway.health.now = func() time.Time { return now }
	h.mock.set("a", status(http.StatusInternalServerError, `{"error":{"message":"failed"}}`))
	for range 5 {
		if response, _ := h.chat(fullKey, nil); response.StatusCode == http.StatusOK {
			t.Fatal("failing provider returned success")
		}
	}
	if response, body := h.chat(fullKey, nil); response.StatusCode != http.StatusOK || h.mock.count("b") != 1 {
		t.Fatalf("unhealthy provider was not bypassed: status=%d body=%v b=%d", response.StatusCode, body, h.mock.count("b"))
	}
	now = now.Add(31 * time.Second)
	if response, _ := h.chat(fullKey, nil); response.StatusCode == http.StatusOK {
		t.Fatal("failing half-open provider returned success")
	}
	if response, body := h.chat(fullKey, nil); response.StatusCode != http.StatusOK || h.mock.count("b") != 2 {
		t.Fatalf("failed probe was not released: status=%d body=%v b=%d", response.StatusCode, body, h.mock.count("b"))
	}
	now = now.Add(31 * time.Second)
	h.mock.set("a", completion(modelA, answerText))
	if response, body := h.chat(fullKey, nil); response.StatusCode != http.StatusOK || h.mock.count("a") != 7 {
		t.Fatalf("recovered provider could not serve: status=%d body=%v a=%d", response.StatusCode, body, h.mock.count("a"))
	}
}

type workflowDebitFixture struct {
	*fakeSessionDebits
}

func (f *workflowDebitFixture) SettleRecovery(context.Context, limits.DebitResult) error {
	return nil
}

func (f *workflowDebitFixture) PreviousRecovery(context.Context, string, string, []string) (bool, bool, error) {
	return false, false, nil
}

type delegatedWorkflowRuntime struct {
	*fakeRuntime
}

func (r delegatedWorkflowRuntime) CreateDelegation(context.Context, access.Authority, access.Authority, runtime.DelegationBounds) (runtime.DelegationGrant, error) {
	return runtime.DelegationGrant{}, runtime.ErrInvalidKey
}

func (r delegatedWorkflowRuntime) AuthenticateDelegation(_ context.Context, token, _ string) (*access.Authority, error) {
	return r.Authenticate(token)
}

func (r delegatedWorkflowRuntime) ReauthenticateDelegation(_ context.Context, token, _ string) (*access.Authority, error) {
	return r.Authenticate(token)
}

type delegatedWorkflowDebits struct {
	*workflowDebitFixture
	agentID string
}

type pairBudgetWorkflowTools struct {
	*workflowFixture
	pair string
}

func (f *pairBudgetWorkflowTools) Call(ctx context.Context, authority access.Authority, release *runtime.Release, tool WorkflowTool, run string, ordinal int, callID string, arguments json.RawMessage) (json.RawMessage, error) {
	if policy, ok := authority.ProjectAttributionBudgets["delegation_pair"][f.pair]; ok && policy.DailyCostLimit != nil && authority.Attribution["team"] == "core" {
		return nil, &mcp.WorkflowError{Status: http.StatusTooManyRequests, Code: "budget_exhausted", Message: "The pair budget is exhausted."}
	}
	return f.workflowFixture.Call(ctx, authority, release, tool, run, ordinal, callID, arguments)
}

func (d *delegatedWorkflowDebits) Admit(_ context.Context, _, _ string, requests []limits.Request) error {
	for _, request := range requests {
		if request.CostOwnerID == d.agentID && request.DailyCostLimit != nil {
			return &limits.ExceededError{Dimension: limits.DimensionDailyCost, Basis: limits.BasisCost}
		}
	}
	return nil
}

func TestGatewayToolExecutionRechecksDelegatedAgentBudgetBeforeLaterModel(t *testing.T) {
	toolsetID, projectID, agentID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	h := strictHarness(t, func(snapshot *routing.Snapshot) {
		route := snapshot.Routes[routeSlug]
		route.Fidelity.Mode, route.ToolExecution, route.Toolsets, route.ProjectID = routing.FidelityTransformed, "gateway", []string{toolsetID}, &projectID
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
	authority.DelegationAgentID = agentID
	authority.DelegationPairDigest = strings.Repeat("a", 64)
	authority.DelegationAgentLimits = &access.DelegationAgentLimits{ID: agentID, LookupID: agentID}
	h.rt.keys[fullKey] = authority
	h.gateway.Runtime = delegatedWorkflowRuntime{fakeRuntime: h.rt}
	fixture := &workflowFixture{runID: uuid.NewString(), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "read_only"}}, beforeCall: func() error {
		h.rt.mu.Lock()
		defer h.rt.mu.Unlock()
		current := h.rt.keys[fullKey]
		current.DelegationAgentLimits = &access.DelegationAgentLimits{ID: agentID, LookupID: agentID, Limits: access.AdmissionLimits{DailyCostLimit: stringptr("1")}}
		h.rt.keys[fullKey] = current
		return nil
	}}
	h.gateway.WorkflowRuns, h.gateway.WorkflowTools = fixture, fixture
	h.gateway.Debits = &delegatedWorkflowDebits{workflowDebitFixture: &workflowDebitFixture{fakeSessionDebits: &fakeSessionDebits{}}, agentID: agentID}
	h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"id":"step","object":"chat.completion","created":1,"model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":%q,"arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`, alias)
	})
	response, body := h.chat(fullKey, nil)
	if response.StatusCode != http.StatusTooManyRequests || h.mock.count("a") != 1 {
		t.Fatalf("workflow dispatched after agent budget changed: status=%d body=%v model_calls=%d", response.StatusCode, body, h.mock.count("a"))
	}
}

func TestGatewayToolExecutionRechecksDelegatedPairBudgetBeforeTool(t *testing.T) {
	toolsetID, projectID, agentID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	pair := strings.Repeat("a", 64)
	alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	h := strictHarness(t, func(snapshot *routing.Snapshot) {
		route := snapshot.Routes[routeSlug]
		route.Fidelity.Mode, route.ToolExecution, route.Toolsets, route.ProjectID = routing.FidelityTransformed, "gateway", []string{toolsetID}, &projectID
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
	authority.DelegationAgentID = agentID
	authority.DelegationPairDigest = pair
	authority.Policy.AllowedAttributionKeys = []string{"team"}
	authority.DelegationAgentLimits = &access.DelegationAgentLimits{ID: agentID, LookupID: agentID}
	h.rt.keys[fullKey] = authority
	h.gateway.Runtime = delegatedWorkflowRuntime{fakeRuntime: h.rt}
	fixture := &workflowFixture{runID: uuid.NewString(), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "read_only"}}}
	h.gateway.WorkflowRuns = fixture
	h.gateway.WorkflowTools = &pairBudgetWorkflowTools{workflowFixture: fixture, pair: pair}
	h.gateway.Debits = &workflowDebitFixture{fakeSessionDebits: &fakeSessionDebits{}}
	h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
		h.rt.mu.Lock()
		current := h.rt.keys[fullKey]
		current.ProjectAttributionBudgets = access.AttributionBudgets{"delegation_pair": {pair: {DailyCostLimit: stringptr("1")}}}
		h.rt.keys[fullKey] = current
		h.rt.mu.Unlock()
		fmt.Fprintf(w, `{"id":"step","object":"chat.completion","created":1,"model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":%q,"arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`, alias)
	})
	response, body := h.chat(fullKey, map[string]string{usage.AttributionHeader: `{"team":"core"}`})
	if response.StatusCode != http.StatusTooManyRequests || h.mock.count("a") != 1 {
		t.Fatalf("workflow continued after pair budget changed: status=%d body=%v model_calls=%d", response.StatusCode, body, h.mock.count("a"))
	}
}

func (f *workflowDebitFixture) MarkNotSent(ctx context.Context, id, claim string) error {
	return f.ReleaseUndispatched(ctx, id, claim)
}

func (f *workflowDebitFixture) SettleWorkflow(context.Context, *usage.Event, int) ([]limits.CostSnapshot, error) {
	return nil, nil
}

type workflowFixture struct {
	runID      string
	tool       WorkflowTool
	result     json.RawMessage
	calls      []json.RawMessage
	beforeCall func() error
}

type blockingWorkflowFixture struct {
	*workflowFixture
	revoke func()
}

func (f *blockingWorkflowFixture) Call(ctx context.Context, _ access.Authority, _ *runtime.Release, _ WorkflowTool, _ string, _ int, _ string, _ json.RawMessage) (json.RawMessage, error) {
	f.revoke()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *workflowFixture) Start(context.Context, access.Authority, string, string) (string, error) {
	return f.runID, nil
}

func (f *workflowFixture) Prepare(context.Context, access.Authority, limits.DebitIntent, int, []limits.DebitOwner) (limits.DebitState, error) {
	return limits.DebitPrepared, nil
}

func (f *workflowFixture) Resolve(_ context.Context, _ access.Authority, _ *runtime.Release, toolsets []string) ([]WorkflowTool, error) {
	if !slices.Contains(toolsets, f.tool.ToolsetID) {
		return nil, nil
	}
	return []WorkflowTool{f.tool}, nil
}

func (f *workflowFixture) Call(_ context.Context, _ access.Authority, _ *runtime.Release, _ WorkflowTool, runID string, _ int, _ string, arguments json.RawMessage) (json.RawMessage, error) {
	if runID != f.runID {
		return nil, fmt.Errorf("wrong run")
	}
	if f.beforeCall != nil {
		if err := f.beforeCall(); err != nil {
			return nil, err
		}
	}
	f.calls = append(f.calls, arguments)
	if len(f.result) != 0 {
		return f.result, nil
	}
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
			var tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if err := json.Unmarshal(request["tools"], &tools); err != nil || len(tools) != 1 || tools[0].Function.Name != alias {
				t.Errorf("selected tools were not advertised: %s", request["tools"])
			}
			fmt.Fprintf(w, `{"id":"one","object":"chat.completion","created":1,"model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":%q,"arguments":"{\"value\":1}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`, alias)
			return
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
		requestFields  string
		headers        map[string]string
		routeLimits    *limits.WorkflowBounds
		keyLimits      *limits.WorkflowBounds
		pricedModel    bool
		inputRate      string
		providerFees   map[string]string
		priceTiers     []usage.PriceTier
		windows        *usage.PriceWindows
		toolPrice      *string
		wantCode       string
		wantStatus     int
		wantModelCalls int
	}{
		{name: "iterations", callsPerStep: 1, wantCode: "tool_iteration_limit", wantModelCalls: 8},
		{name: "calls", callsPerStep: 33, wantCode: "tool_call_limit", wantModelCalls: 1},
		{name: "caller iteration limit", callsPerStep: 1, requestFields: `,"olp_run_limits":{"max_iterations":1}`, wantCode: "tool_iteration_limit", wantModelCalls: 1},
		{name: "key is smallest", callsPerStep: 1, routeLimits: &limits.WorkflowBounds{MaxIterations: new(int64(4))}, keyLimits: &limits.WorkflowBounds{MaxIterations: new(int64(2))}, requestFields: `,"olp_run_limits":{"max_iterations":3}`, wantCode: "tool_iteration_limit", wantModelCalls: 2},
		{name: "route is smallest", callsPerStep: 1, routeLimits: &limits.WorkflowBounds{MaxIterations: new(int64(3))}, keyLimits: &limits.WorkflowBounds{MaxIterations: new(int64(4))}, requestFields: `,"olp_run_limits":{"max_iterations":6}`, wantCode: "tool_iteration_limit", wantModelCalls: 3},
		{name: "caller lowers raised default", callsPerStep: 1, routeLimits: &limits.WorkflowBounds{MaxIterations: new(int64(16))}, keyLimits: &limits.WorkflowBounds{MaxIterations: new(int64(20))}, requestFields: `,"olp_run_limits":{"max_iterations":12}`, wantCode: "tool_iteration_limit", wantModelCalls: 12},
		{name: "zero tool calls", callsPerStep: 1, requestFields: `,"olp_run_limits":{"max_tool_calls":0}`, wantCode: "tool_call_limit", wantModelCalls: 1},
		{name: "key tool call limit is smallest", callsPerStep: 2, routeLimits: &limits.WorkflowBounds{MaxToolCalls: new(int64(3))}, keyLimits: &limits.WorkflowBounds{MaxToolCalls: new(int64(1))}, requestFields: `,"olp_run_limits":{"max_tool_calls":2}`, wantCode: "tool_call_limit", wantModelCalls: 1},
		{name: "unbounded model cannot satisfy cost limit", callsPerStep: 1, pricedModel: true, requestFields: `,"olp_run_limits":{"max_run_cost":"1"}`, wantCode: "run_cost_unbounded"},
		{name: "per-attempt cost remains independent", callsPerStep: 1, pricedModel: true, headers: map[string]string{"X-OLP-Routing": `{"max_cost":"0.05"}`}, requestFields: `,"max_tokens":8,"olp_run_limits":{"max_run_cost":"1"}`, wantCode: "cost_limit_exceeded"},
		{name: "zero cost ceiling prevents charged model", callsPerStep: 1, pricedModel: true, requestFields: `,"max_tokens":8,"olp_run_limits":{"max_run_cost":"0"}`, wantCode: "run_cost_limit"},
		{name: "key run cost is smallest", callsPerStep: 1, pricedModel: true, routeLimits: &limits.WorkflowBounds{MaxRunCost: new("0.5")}, keyLimits: &limits.WorkflowBounds{MaxRunCost: new("0.05")}, requestFields: `,"max_tokens":8,"olp_run_limits":{"max_run_cost":"0.2"}`, wantCode: "run_cost_limit"},
		{name: "unpriced tool cannot satisfy cost limit", callsPerStep: 1, pricedModel: true, requestFields: `,"max_tokens":8,"olp_run_limits":{"max_run_cost":"1"}`, wantCode: "run_cost_unbounded", wantModelCalls: 1},
		{name: "next tool exceeds cost limit", callsPerStep: 1, pricedModel: true, toolPrice: new("0.9"), requestFields: `,"max_tokens":8,"olp_run_limits":{"max_run_cost":"0.5"}`, wantCode: "run_cost_limit", wantModelCalls: 1},
		{name: "next model exceeds remaining cost", callsPerStep: 1, pricedModel: true, toolPrice: new("0.1"), requestFields: `,"max_tokens":8,"olp_run_limits":{"max_run_cost":"0.25"}`, wantCode: "run_cost_limit", wantModelCalls: 1},
		{name: "unbounded provider tool fees", callsPerStep: 1, pricedModel: true, providerFees: map[string]string{"search": "1"}, requestFields: `,"max_tokens":8,"olp_run_limits":{"max_run_cost":"1"}`, wantCode: "run_cost_unbounded"},
		{name: "higher price window exceeds ceiling", callsPerStep: 1, pricedModel: true, windows: &usage.PriceWindows{TimeZone: "UTC", Periods: []usage.PriceWindowPeriod{{Days: []int{((int(time.Now().UTC().Weekday())+6)%7+1)%7 + 1}, Start: "00:00", End: "24:00", Multiplier: "3"}}}, requestFields: `,"max_tokens":8,"olp_run_limits":{"max_run_cost":"0.2"}`, wantCode: "run_cost_limit"},
		{name: "expensive lower price tier exceeds ceiling", callsPerStep: 1, pricedModel: true, inputRate: "1000", priceTiers: []usage.PriceTier{{AboveInputTokens: 500, InputPerMillion: new("0"), OutputPerMillion: new("0")}}, requestFields: `,"max_tokens":8,"olp_run_limits":{"max_run_cost":"0.2"}`, wantCode: "run_cost_limit"},
		{name: "model input worst case exceeds ceiling", callsPerStep: 1, pricedModel: true, inputRate: "1000", requestFields: `,"max_tokens":8,"olp_run_limits":{"max_run_cost":"0.2"}`, wantCode: "run_cost_limit"},
		{name: "unpriced model cannot satisfy cost limit", callsPerStep: 1, requestFields: `,"max_tokens":8,"olp_run_limits":{"max_run_cost":"1"}`, wantCode: "run_cost_unbounded", wantModelCalls: 0},
		{name: "concurrency fails closed without limiter", callsPerStep: 1, requestFields: `,"olp_run_limits":{"max_concurrent_runs":1}`, wantCode: "distributed_limits_unavailable", wantStatus: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			toolsetID, projectID := uuid.NewString(), uuid.NewString()
			alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			h := strictHarness(t, func(snapshot *routing.Snapshot) {
				route := snapshot.Routes[routeSlug]
				route.Fidelity.Mode, route.ToolExecution, route.ProjectID = routing.FidelityTransformed, "gateway", &projectID
				route.Toolsets = []string{toolsetID}
				route.RunLimits = test.routeLimits
				snapshot.Routes[routeSlug] = route
				for id, provider := range snapshot.Providers {
					provider.Models = map[string]json.RawMessage{modelA: json.RawMessage(`{"context_length":1000}`)}
					for i := range provider.Capabilities {
						provider.Capabilities[i].ToolCalls = true
					}
					snapshot.Providers[id] = provider
				}
			})
			if test.pricedModel {
				h.rt.inputs = &usage.RoutingInputs{Prices: []usage.RoutingPrice{{Price: usage.Price{
					ProviderKind: "openai_compatible", Model: modelA, Operation: "generation", Currency: "usd",
					InputPerMillion: new(cmp.Or(test.inputRate, "0")), OutputPerMillion: new("0"), PerRequest: new("0.1"),
					ToolCalls: test.providerFees, Windows: test.windows, Tiers: test.priceTiers,
				}, EffectiveAt: time.Now().Add(-time.Minute)}}, RefreshedAt: time.Now()}
			}
			authority := h.rt.keys[fullKey]
			authority.ProjectID = &projectID
			authority.Policy.RunLimits = test.keyLimits
			h.rt.keys[fullKey] = authority
			fixture := &workflowFixture{runID: uuid.NewString(), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "read_only", UnitPrice: test.toolPrice}}}
			h.gateway.WorkflowRuns, h.gateway.WorkflowTools, h.gateway.Debits = fixture, fixture, &workflowDebitFixture{fakeSessionDebits: &fakeSessionDebits{}}
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				calls := make([]map[string]any, test.callsPerStep)
				for i := range calls {
					calls[i] = map[string]any{"id": fmt.Sprintf("call-%d-%d", h.mock.count("a"), i), "type": "function", "function": map[string]string{"name": alias, "arguments": `{}`}}
				}
				json.NewEncoder(w).Encode(map[string]any{"id": "bounded", "object": "chat.completion", "created": 1, "model": "model-a", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": nil, "tool_calls": calls}, "finish_reason": "tool_calls"}}, "usage": map[string]int{"prompt_tokens": 2, "completion_tokens": 3, "total_tokens": 5}})
			})

			response, body := h.chat(fullKey, test.headers, test.requestFields)
			if response.StatusCode != cmp.Or(test.wantStatus, http.StatusBadRequest) || errorCode(t, body) != test.wantCode || h.mock.count("a") != test.wantModelCalls {
				t.Fatalf("workflow limit was not enforced: status=%d body=%v model_calls=%d", response.StatusCode, body, h.mock.count("a"))
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
				t.Fatalf("delegated quota failed during workflow: status=%d body=%v model_calls=%d", response.StatusCode, body, h.mock.count("a"))
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

func TestGatewayToolExecutionTracksUnboundedRunsForConcurrency(t *testing.T) {
	toolsetID, projectID := uuid.NewString(), uuid.NewString()
	h := strictHarness(t, func(snapshot *routing.Snapshot) {
		route := snapshot.Routes[routeSlug]
		route.Fidelity.Mode = routing.FidelityTransformed
		route.ToolExecution, route.Toolsets, route.ProjectID = "gateway", []string{toolsetID}, &projectID
		route.OverallTimeout = 20_000
		for i := range route.Targets {
			route.Targets[i].Timeout = 15_000
		}
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
	admitThrough(t, h)
	started, release, finished := make(chan struct{}, 2), make(chan struct{}, 2), make(chan int, 2)
	defer close(release)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		if h.mock.count("a") <= 2 {
			started <- struct{}{}
			<-release
		}
		completion(modelA, "done")(w, r)
	})
	for range 2 {
		go func() {
			response, _ := h.chat(fullKey, nil, `,"olp_toolsets":[]`)
			finished <- response.StatusCode
		}()
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatal("unbounded workflow did not reach the model")
		}
	}
	bounded := `,"olp_toolsets":[],"olp_run_limits":{"max_concurrent_runs":1}`
	assertBounded := func() {
		t.Helper()
		response, body := h.chat(fullKey, nil, bounded)
		if response.StatusCode != http.StatusTooManyRequests || h.mock.count("a") != 2 {
			t.Fatalf("bounded workflow was not refused: status=%d body=%v model_calls=%d", response.StatusCode, body, h.mock.count("a"))
		}
	}
	releaseOne := func() {
		t.Helper()
		release <- struct{}{}
		select {
		case status := <-finished:
			if status != http.StatusOK {
				t.Fatalf("unbounded workflow returned %d", status)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("unbounded workflow did not finish")
		}
	}
	assertBounded()
	releaseOne()
	assertBounded()
	releaseOne()
	response, body := h.chat(fullKey, nil, bounded)
	if response.StatusCode != http.StatusOK || h.mock.count("a") != 3 {
		t.Fatalf("bounded workflow could not start after release: status=%d body=%v model_calls=%d", response.StatusCode, body, h.mock.count("a"))
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
				t.Fatalf("workflow continued after authority changed: status=%d body=%v model_calls=%d", response.StatusCode, body, h.mock.count("a"))
			}
		})
	}
}

func TestGatewayToolExecutionCancelsLongCallsAfterRevocation(t *testing.T) {
	for _, test := range []struct {
		name       string
		model      bool
		removeTool bool
	}{
		{name: "model", model: true},
		{name: "tool"},
		{name: "tool permission", removeTool: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			toolsetID, projectID := uuid.NewString(), uuid.NewString()
			alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			h := strictHarness(t, func(snapshot *routing.Snapshot) {
				route := snapshot.Routes[routeSlug]
				route.Fidelity.Mode, route.ToolExecution, route.Toolsets, route.ProjectID = routing.FidelityTransformed, "gateway", []string{toolsetID}, &projectID
				route.OverallTimeout = 12000
				for i := range route.Targets {
					route.Targets[i].Timeout = 10000
				}
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
			authority.Policy.Scopes = append(authority.Policy.Scopes, "tools")
			authority.Policy.AllowedToolsets = []string{toolsetID}
			h.rt.keys[fullKey] = authority
			revoke := func() {
				h.rt.mu.Lock()
				if test.removeTool {
					authority := h.rt.keys[fullKey]
					authority.Policy.AllowedToolsets = nil
					h.rt.keys[fullKey] = authority
				} else {
					delete(h.rt.keys, fullKey)
				}
				h.rt.mu.Unlock()
			}
			fixture := &workflowFixture{runID: uuid.NewString(), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "read_only"}}}
			h.gateway.WorkflowRuns, h.gateway.Debits = fixture, &workflowDebitFixture{fakeSessionDebits: &fakeSessionDebits{}}
			if test.model {
				h.gateway.WorkflowTools = fixture
				h.mock.set("a", func(_ http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					revoke()
					<-r.Context().Done()
				})
			} else {
				h.gateway.WorkflowTools = &blockingWorkflowFixture{workflowFixture: fixture, revoke: revoke}
				h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
					fmt.Fprintf(w, `{"id":"step","object":"chat.completion","created":1,"model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":%q,"arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`, alias)
				})
			}
			response, body := h.chat(fullKey, nil)
			wantStatus := http.StatusUnauthorized
			if test.removeTool {
				wantStatus = http.StatusForbidden
			}
			if response.StatusCode != wantStatus || h.mock.count("a") != 1 {
				t.Fatalf("revoked long call continued: status=%d body=%v model_calls=%d", response.StatusCode, body, h.mock.count("a"))
			}
		})
	}
}

func TestGatewayToolExecutionResponsesCompletesWithFinalOutput(t *testing.T) {
	toolsetID, projectID := uuid.NewString(), uuid.NewString()
	alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	fixture := &workflowFixture{runID: uuid.NewString(), result: json.RawMessage(`{"content":[{"type":"text","text":"private-tool-result"}]}`), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "read_only", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	h := nativeWorkflowHarness(t, projectID, "openai", "openai-responses", "unary", routing.FidelityTransformed, []string{toolsetID}, true, fixture)
	upstream := make(chan []byte, 2)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstream <- body
		w.Header().Set("Content-Type", "application/json")
		if h.mock.count("a") == 1 {
			fmt.Fprintf(w, `{"id":"first","object":"response","status":"completed","model":"model-a","output":[{"type":"function_call","id":"fc-1","call_id":"call-1","name":%q,"arguments":"{}","status":"completed"}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`, alias)
			return
		}
		io.WriteString(w, `{"id":"last","object":"response","status":"completed","model":"model-a","output":[{"type":"message","id":"msg-1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"final answer","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}`)
	})
	response := h.do(t.Context(), http.MethodPost, "/v1/responses", fullKey, []byte(`{"model":"`+routeSlug+`","input":"hello"}`), nil)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("Responses workflow failed: status=%d body=%s error=%v", response.StatusCode, body, err)
	}
	var result struct {
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &result); err != nil || len(result.Output) != 1 || result.Output[0].Type != "message" || len(result.Output[0].Content) != 1 || result.Output[0].Content[0].Text != "final answer" {
		t.Fatalf("Responses workflow did not return only its final answer: %s", body)
	}
	first, second := <-upstream, <-upstream
	var initial, continued struct {
		Tools []struct{ Type, Name string } `json:"tools"`
		Input []struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
			Output string `json:"output"`
		} `json:"input"`
	}
	if err := json.Unmarshal(first, &initial); err != nil || len(initial.Tools) != 1 || initial.Tools[0].Type != "function" || initial.Tools[0].Name != alias {
		t.Fatalf("provider did not receive Responses function definitions: %s", first)
	}
	if err := json.Unmarshal(second, &continued); err != nil || len(continued.Input) < 3 || continued.Input[len(continued.Input)-1].Type != "function_call_output" || continued.Input[len(continued.Input)-1].CallID != "call-1" {
		t.Fatalf("provider did not receive the paired tool output: %s", second)
	}
	assertNativeToolOutput(t, continued.Input[len(continued.Input)-1].Output)
}

func TestGatewayToolExecutionResponsesUncertifiedContractReturnsFinalOnly(t *testing.T) {
	toolsetID, projectID := uuid.NewString(), uuid.NewString()
	alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	fixture := &workflowFixture{runID: uuid.NewString(), result: json.RawMessage(`{"content":[{"type":"text","text":"private-tool-result"}]}`), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "read_only", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	h := nativeWorkflowHarness(t, projectID, "openai", "compatible-responses", "unary", routing.FidelityTransformed, []string{toolsetID}, true, fixture, true)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if h.mock.count("a") == 1 {
			fmt.Fprintf(w, `{"id":"first","object":"response","status":"completed","model":"model-a","output":[{"type":"function_call","id":"fc-1","call_id":"call-1","name":%q,"arguments":"{}","status":"completed"}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`, alias)
			return
		}
		io.WriteString(w, `{"id":"last","object":"response","status":"completed","model":"model-a","output":[{"type":"message","id":"msg-1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"final answer","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}`)
	})
	response := h.do(t.Context(), http.MethodPost, "/v1/responses", fullKey, []byte(`{"model":"`+routeSlug+`","input":"hello"}`), nil)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("Responses workflow failed: status=%d body=%s error=%v", response.StatusCode, body, err)
	}
	var result struct {
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &result); err != nil || len(result.Output) != 1 || result.Output[0].Type != "message" || len(result.Output[0].Content) != 1 || result.Output[0].Content[0].Text != "final answer" {
		t.Fatalf("uncertified Responses contract did not return only the final answer: %s", body)
	}
}

func TestGatewayToolExecutionResponsesProjectsMCPItems(t *testing.T) {
	toolsetID, projectID := uuid.NewString(), uuid.NewString()
	alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	fixture := &workflowFixture{runID: uuid.NewString(), result: json.RawMessage(`{"content":[{"type":"text","text":"private-tool-result"}]}`), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "read_only", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	h := nativeWorkflowHarness(t, projectID, "openai", "openai-responses", "unary", routing.FidelityTransformed, []string{toolsetID}, true, fixture, true)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if h.mock.count("a") == 1 {
			fmt.Fprintf(w, `{"id":"first","object":"response","status":"completed","model":"model-a","output":[{"type":"function_call","id":"fc-1","call_id":"call-1","name":%q,"arguments":"{}","status":"completed"}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`, alias)
			return
		}
		io.WriteString(w, `{"id":"last","object":"response","status":"completed","model":"model-a","output":[{"type":"message","id":"msg-1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"final answer","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}`)
	})
	response := h.do(t.Context(), http.MethodPost, "/v1/responses", fullKey, []byte(`{"model":"`+routeSlug+`","input":"hello"}`), nil)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("Responses workflow failed: status=%d body=%s error=%v", response.StatusCode, body, err)
	}
	var result struct {
		Output []struct {
			Type        string `json:"type"`
			ID          string `json:"id"`
			ServerLabel string `json:"server_label"`
			Name        string `json:"name"`
			Arguments   string `json:"arguments"`
			Output      string `json:"output"`
			Tools       []struct {
				Name string `json:"name"`
			} `json:"tools"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &result); err != nil || len(result.Output) != 3 {
		t.Fatalf("Responses workflow did not project MCP items: %s", body)
	}
	listed, called, final := result.Output[0], result.Output[1], result.Output[2]
	if listed.Type != "mcp_list_tools" || listed.ID == "" || listed.ServerLabel != toolsetID || len(listed.Tools) != 1 || listed.Tools[0].Name != alias {
		t.Fatalf("wrong authorized tool listing: %s", body)
	}
	if called.Type != "mcp_call" || called.ID == "" || called.ServerLabel != toolsetID || called.Name != alias || called.Arguments != "{}" {
		t.Fatalf("wrong authorized tool call: %s", body)
	}
	assertNativeToolOutput(t, called.Output)
	if final.Type != "message" || len(final.Content) != 1 || final.Content[0].Text != "final answer" {
		t.Fatalf("final answer is missing: %s", body)
	}
}

func TestGatewayToolExecutionResponsesStreamReturnsFinalAnswer(t *testing.T) {
	testResponsesWorkflowStream(t, false)
}

func TestGatewayToolExecutionResponsesStreamProjectsMCPItems(t *testing.T) {
	testResponsesWorkflowStream(t, true)
}

type approvalWorkflowFixture struct {
	*workflowFixture
	paused chan agents.WorkflowApprovalRequest
}

func (f *approvalWorkflowFixture) Pause(_ context.Context, _ access.Authority, request agents.WorkflowApprovalRequest, _ int) (string, error) {
	if f.paused != nil {
		f.paused <- request
	}
	return "olpa_fixture", nil
}

func (f *approvalWorkflowFixture) Continue(ctx context.Context, _ access.Authority, token string, _ json.RawMessage, _ map[string]json.RawMessage, _ int) (agents.WorkflowContinuation, error) {
	if f.paused == nil || token != "olpa_fixture" {
		return agents.WorkflowContinuation{}, agents.ErrApproval
	}
	select {
	case request := <-f.paused:
		approvals := slices.Collect(func(yield func(agents.WorkflowApproval) bool) {
			for _, call := range request.Calls {
				if !yield(agents.WorkflowApproval{CallID: call.CallID, ID: uuid.NewString(), Binding: agents.ApprovalBinding{ToolsetID: call.ToolsetID, ToolsetRevisionID: call.ToolsetRevisionID, ToolAlias: call.ToolAlias}}) {
					return
				}
			}
		})
		return agents.WorkflowContinuation{RunID: request.RunID, RouteRevisionID: request.RouteRevisionID, StartOrdinal: request.StartOrdinal, NextOrdinal: request.StartOrdinal, State: request.State, Approvals: approvals, ExpiresAt: request.ExpiresAt}, nil
	case <-ctx.Done():
		return agents.WorkflowContinuation{}, ctx.Err()
	}
}

func (f *approvalWorkflowFixture) CallApproved(context.Context, access.Authority, *runtime.Release, WorkflowTool, string, int, string, json.RawMessage, agents.WorkflowApproval) (json.RawMessage, error) {
	return json.RawMessage(`{"content":[{"type":"text","text":"private-tool-result"}]}`), nil
}

func TestGatewayToolExecutionNativeApprovalPauseAndResume(t *testing.T) {
	for _, test := range []struct {
		name, surface, profile, path, request, completion, final, classification string
	}{
		{name: "anthropic unclassified", surface: "anthropic", profile: "anthropic-messages", path: "/anthropic/v1/messages", request: `{"model":"` + routeSlug + `","max_tokens":20,"messages":[{"role":"user","content":"hello"}]}`, completion: `{"id":"first","type":"message","role":"assistant","model":"model-a","content":[{"type":"thinking","thinking":"private thought","signature":"signed"},{"type":"tool_use","id":"call-1","name":%q,"input":{"value":1}}],"stop_reason":"tool_use","usage":{"input_tokens":2,"output_tokens":3}}`, final: `{"id":"last","type":"message","role":"assistant","model":"model-a","content":[{"type":"text","text":"final answer"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":4}}`},
		{name: "gemini approval required", surface: "gemini", profile: "gemini-generation", path: "/gemini/v1beta/models/" + routeSlug + ":generateContent", request: `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`, completion: `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"private thought"},{"functionCall":{"name":%q,"args":{"value":1}},"thoughtSignature":"signed"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}`, final: `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"final answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":4,"totalTokenCount":7}}`, classification: "approval_required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			toolsetID, projectID := uuid.NewString(), uuid.NewString()
			alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			price := "0.25"
			fixture := &workflowFixture{runID: uuid.NewString(), beforeCall: func() error { return fmt.Errorf("tool dispatched before approval") }, tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: test.classification, UnitPrice: &price, InputSchema: json.RawMessage(`{"type":"object"}`)}}}
			h := nativeWorkflowHarness(t, projectID, test.surface, test.profile, "unary", routing.FidelityTransformed, []string{toolsetID}, true, fixture)
			approval := &approvalWorkflowFixture{workflowFixture: fixture, paused: make(chan agents.WorkflowApprovalRequest, 1)}
			h.gateway.WorkflowRuns = approval
			h.gateway.WorkflowTools = approval
			upstream := make(chan []byte, 2)
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				request, _ := io.ReadAll(r.Body)
				upstream <- request
				w.Header().Set("Content-Type", "application/json")
				if h.mock.count("a") == 1 {
					fmt.Fprintf(w, test.completion, alias)
					return
				}
				io.WriteString(w, test.final)
			})
			response := h.do(t.Context(), http.MethodPost, test.path, fullKey, []byte(test.request), nil)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			var pause struct {
				Type              string `json:"type"`
				ContinuationToken string `json:"continuation_token"`
				Calls             []struct {
					ID, Name  string
					Arguments map[string]int
				}
			}
			if err != nil || json.Unmarshal(body, &pause) != nil || response.StatusCode != http.StatusOK || pause.Type != "tool_approval_required" || pause.ContinuationToken == "" || len(pause.Calls) != 1 {
				t.Fatalf("native approval pause failed: status=%d type=%q calls=%d error=%v", response.StatusCode, pause.Type, len(pause.Calls), err)
			}
			if pause.Calls[0].ID == "" || pause.Calls[0].Name != alias || pause.Calls[0].Arguments["value"] != 1 || strings.Contains(string(body), "private thought") {
				t.Fatal("native approval pause omitted the requested tool or exposed private thinking")
			}
			submission, err := json.Marshal(workflowResumeRequest{ContinuationToken: pause.ContinuationToken, Approved: true, Arguments: map[string]json.RawMessage{pause.Calls[0].ID: json.RawMessage(`{"value":1}`)}})
			if err != nil {
				t.Fatal(err)
			}
			resumeBody := strings.TrimSuffix(test.request, "}") + `,"olp_approval":` + string(submission) + `}`
			resumed := h.do(t.Context(), http.MethodPost, test.path, fullKey, []byte(resumeBody), nil)
			defer resumed.Body.Close()
			final, err := io.ReadAll(resumed.Body)
			if err != nil || resumed.StatusCode != http.StatusOK || !strings.Contains(string(final), "final answer") || strings.Contains(string(final), nativeToolResult) || strings.Contains(string(final), "private thought") {
				t.Fatalf("native approval resume failed: status=%d error=%v", resumed.StatusCode, err)
			}
			first, second := <-upstream, <-upstream
			if strings.Contains(string(second), "olp_approval") {
				t.Fatal("approval control reached the provider")
			}
			if test.surface == "anthropic" {
				assertAnthropicWorkflowRequests(t, first, second, alias, true)
			} else {
				assertGeminiWorkflowRequests(t, first, second, alias)
			}
		})
	}
}

func TestGatewayToolExecutionStreamingApprovalPause(t *testing.T) {
	for _, test := range []struct {
		name, surface, profile, path, request, upstream, terminal string
	}{
		{name: "chat", surface: "openai", profile: "openai-chat", path: "/v1/chat/completions", request: `{"model":"` + routeSlug + `","messages":[{"role":"user","content":"hello"}],"stream":true}`, upstream: `data: {"id":"first","object":"chat.completion.chunk","created":1,"model":"model-a","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":%q,"arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}` + "\n\ndata: [DONE]\n\n", terminal: "[DONE]"},
		{name: "anthropic", surface: "anthropic", profile: "anthropic-messages", path: "/anthropic/v1/messages", request: `{"model":"` + routeSlug + `","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello"}]}`, upstream: `event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg","type":"message","role":"assistant","model":"model-a","usage":{"input_tokens":2}}}` + "\n\n" + `event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call-1","name":%q,"input":{}}}` + "\n\n" + `event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n" + `event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":3}}` + "\n\n" + `event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n", terminal: "message_stop"},
		{name: "gemini", surface: "gemini", profile: "gemini-generation", path: "/gemini/v1beta/models/" + routeSlug + ":streamGenerateContent?alt=sse", request: `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`, upstream: `data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":%q,"args":{}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}` + "\n\n", terminal: "STOP"},
		{name: "uncertified responses", surface: "openai", profile: "compatible-responses", path: "/v1/responses", request: `{"model":"` + routeSlug + `","input":"hello","stream":true}`, upstream: `data: {"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"type":"function_call","id":"fc-1","call_id":"call-1","name":%q,"arguments":"{}","status":"completed"}}` + "\n\n" + `data: {"type":"response.completed","sequence_number":1,"response":{"id":"first","object":"response","status":"completed","model":"model-a","output":[],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}` + "\n\n", terminal: "response.incomplete"},
	} {
		t.Run(test.name, func(t *testing.T) {
			toolsetID, projectID := uuid.NewString(), uuid.NewString()
			alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			fixture := &workflowFixture{runID: uuid.NewString(), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "approval_required", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
			h := nativeWorkflowHarness(t, projectID, test.surface, test.profile, "streaming", routing.FidelityTransformed, []string{toolsetID}, true, fixture, true)
			h.gateway.WorkflowRuns = &approvalWorkflowFixture{workflowFixture: fixture}
			h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, test.upstream, alias)
			})
			response := h.do(t.Context(), http.MethodPost, test.path, fullKey, []byte(test.request), nil)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
				t.Fatalf("pause is not an SSE response: status=%d body=%s error=%v", response.StatusCode, body, err)
			}
			var found bool
			err = sse.Decode(strings.NewReader(string(body)), 65536, func(frame sse.Frame) error {
				if frame.Data == "[DONE]" {
					return nil
				}
				var data struct {
					Approval *workflowPause `json:"olp_approval"`
					Response struct {
						Approval *workflowPause `json:"olp_approval"`
					} `json:"response"`
				}
				if err := json.Unmarshal([]byte(frame.Data), &data); err != nil {
					return err
				}
				pause := data.Approval
				if pause == nil {
					pause = data.Response.Approval
				}
				if pause != nil && pause.Type == "tool_approval_required" && pause.ContinuationToken == "olpa_fixture" && len(pause.Calls) == 1 && pause.Calls[0].Name == alias {
					found = true
				}
				return nil
			})
			if err != nil || !found || !strings.Contains(string(body), test.terminal) || strings.Contains(string(body), "mcp_approval_request") || len(fixture.calls) != 0 {
				t.Fatalf("invalid approval stream: body=%s error=%v", body, err)
			}
		})
	}
}

func TestGatewayToolExecutionResponsesApprovalRequest(t *testing.T) {
	toolsetID, projectID := uuid.NewString(), uuid.NewString()
	alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	price := "0.25"
	fixture := &workflowFixture{runID: uuid.NewString(), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "approval_required", UnitPrice: &price, InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	h := nativeWorkflowHarness(t, projectID, "openai", "openai-responses", "unary", routing.FidelityTransformed, []string{toolsetID}, true, fixture, true)
	h.gateway.WorkflowRuns = &approvalWorkflowFixture{workflowFixture: fixture}
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"first","object":"response","status":"completed","model":"model-a","output":[{"type":"function_call","id":"fc-1","call_id":"call-1","name":%q,"arguments":"{}","status":"completed"}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`, alias)
	})
	response := h.do(t.Context(), http.MethodPost, "/v1/responses", fullKey, []byte(`{"model":"`+routeSlug+`","input":"hello"}`), nil)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("approval response failed: status=%d error=%v", response.StatusCode, err)
	}
	var result struct {
		Type              string `json:"type"`
		ContinuationToken string `json:"continuation_token"`
		Output            []struct {
			Type        string `json:"type"`
			ID          string `json:"id"`
			Name        string `json:"name"`
			ServerLabel string `json:"server_label"`
			Arguments   string `json:"arguments"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &result); err != nil || result.Type != "tool_approval_required" || result.ContinuationToken == "" || len(result.Output) != 1 {
		t.Fatalf("expected Responses approval output and continuation, got type=%q outputs=%d error=%v", result.Type, len(result.Output), err)
	}
	item := result.Output[0]
	if item.Type != "mcp_approval_request" || item.ID == "" || item.Name != alias || item.ServerLabel != toolsetID || item.Arguments != "{}" {
		t.Fatalf("invalid approval request: %+v", item)
	}
	if len(fixture.calls) != 0 {
		t.Fatal("approval-required tool dispatched before confirmation")
	}
}

func TestGatewayToolExecutionResponsesUncertifiedApprovalRemainsFinalOnly(t *testing.T) {
	toolsetID, projectID := uuid.NewString(), uuid.NewString()
	alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	price := "0.25"
	fixture := &workflowFixture{runID: uuid.NewString(), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "approval_required", UnitPrice: &price, InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"first","object":"response","status":"completed","model":"model-a","output":[{"type":"function_call","id":"fc-1","call_id":"call-1","name":%q,"arguments":"{}","status":"completed"}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`, alias)
	}))
	t.Cleanup(first.Close)
	h := nativeWorkflowHarnessWithProviders(t, projectID, "openai", "openai-responses", "unary", routing.FidelityTransformed, []string{toolsetID}, true, fixture, func(provider routing.Provider) routing.Provider {
		if provider.Name != "a" {
			return provider
		}
		return routing.Provider{ID: provider.ID, Name: provider.Name, Kind: "openai_compatible", Enabled: provider.Enabled, ActiveCredential: provider.ActiveCredential, RevisionID: provider.RevisionID, Endpoint: first.URL + "/v1", AuthMode: provider.AuthMode, Slots: provider.Slots, ProfileID: "compatible-responses", ProfileRevision: provider.ProfileRevision, Capabilities: provider.Capabilities}
	}, true)
	h.mock.set("b", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"last","object":"response","status":"completed","model":"model-b","output":[{"type":"message","id":"msg-1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"final answer","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}`)
	})
	approval := &approvalWorkflowFixture{workflowFixture: fixture, paused: make(chan agents.WorkflowApprovalRequest, 1)}
	h.gateway.WorkflowRuns, h.gateway.WorkflowTools = approval, approval
	request := `{"model":"` + routeSlug + `","input":"hello"}`
	initial := h.do(t.Context(), http.MethodPost, "/v1/responses", fullKey, []byte(request), nil)
	body, err := io.ReadAll(initial.Body)
	initial.Body.Close()
	var pause workflowPause
	if err != nil || initial.StatusCode != http.StatusOK || json.Unmarshal(body, &pause) != nil || pause.ContinuationToken == "" || len(pause.Calls) != 1 || strings.Contains(string(body), "mcp_") {
		t.Fatalf("uncertified pause was not final-only: status=%d body=%s error=%v", initial.StatusCode, body, err)
	}
	firstRoute := h.rt.Release().Snapshot.Routes[routeSlug]
	firstProvider := h.rt.Release().Snapshot.Providers[firstRoute.Targets[0].ProviderID]
	firstCredential := *firstProvider.Slots[0].CredentialID
	h.rt.mu.Lock()
	h.rt.revoked[firstCredential] = true
	h.rt.mu.Unlock()
	submission, err := json.Marshal(workflowResumeRequest{ContinuationToken: pause.ContinuationToken, Approved: true, Arguments: map[string]json.RawMessage{pause.Calls[0].ID: json.RawMessage(`{}`)}})
	if err != nil {
		t.Fatal(err)
	}
	resume := strings.TrimSuffix(request, "}") + `,"olp_approval":` + string(submission) + `}`
	final := h.do(t.Context(), http.MethodPost, "/v1/responses", fullKey, []byte(resume), nil)
	defer final.Body.Close()
	body, err = io.ReadAll(final.Body)
	if err != nil || final.StatusCode != http.StatusOK || !strings.Contains(string(body), "final answer") || strings.Contains(string(body), "mcp_") || strings.Contains(string(body), "private-tool-result") {
		t.Fatalf("uncertified contract was upgraded after resume: status=%d body=%s error=%v", final.StatusCode, body, err)
	}
}

func TestGatewayToolExecutionResponsesStreamingApproval(t *testing.T) {
	toolsetID, projectID := uuid.NewString(), uuid.NewString()
	alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	price := "0.25"
	fixture := &workflowFixture{runID: uuid.NewString(), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "approval_required", UnitPrice: &price, InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	h := nativeWorkflowHarness(t, projectID, "openai", "openai-responses", "streaming", routing.FidelityTransformed, []string{toolsetID}, true, fixture, true)
	h.gateway.WorkflowRuns = &approvalWorkflowFixture{workflowFixture: fixture}
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		item := fmt.Sprintf(`{"type":"function_call","id":"fc-1","call_id":"call-1","name":%q,"arguments":"{}","status":"completed"}`, alias)
		fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"sequence_number\":0,\"output_index\":0,\"item\":%s}\n\ndata: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"id\":\"first\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"model-a\",\"output\":[%s],\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5}}}\n\n", item, item)
	})
	client := openai.NewClient(option.WithBaseURL(h.server.URL+"/v1/"), option.WithAPIKey(fullKey))
	stream := client.Responses.NewStreaming(t.Context(), responses.ResponseNewParams{Model: routeSlug, Input: responses.ResponseNewParamsInputUnion{OfString: openai.String("hello")}})
	defer stream.Close()
	events := slices.Collect(func(yield func(responses.ResponseStreamEventUnion) bool) {
		for stream.Next() {
			if !yield(stream.Current()) {
				return
			}
		}
	})
	if err := stream.Err(); err != nil || len(events) < 2 {
		t.Fatalf("official SDK did not receive approval stream: %v (%d events)", err, len(events))
	}
	item := events[0].Item.AsMcpApprovalRequest()
	terminal := events[len(events)-1].Response
	if events[0].Type != "response.output_item.added" || item.ID == "" || item.Name != alias || item.ServerLabel != toolsetID || item.Arguments != "{}" || events[len(events)-1].Type != "response.incomplete" || len(terminal.Output) != 1 || terminal.Output[0].AsMcpApprovalRequest().ID != item.ID || !strings.Contains(terminal.RawJSON(), "continuation_token") {
		t.Fatalf("official SDK received an invalid approval stream: events=%d type=%q item=%q terminal=%q", len(events), events[0].Type, item.ID, events[len(events)-1].Type)
	}
}

func TestGatewayToolExecutionResponsesOfficialSDKDecodesMCPItems(t *testing.T) {
	toolsetID, projectID := uuid.NewString(), uuid.NewString()
	alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	fixture := &workflowFixture{runID: uuid.NewString(), result: json.RawMessage(`{"content":[{"type":"text","text":"private-tool-result"}]}`), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "read_only", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	h := nativeWorkflowHarness(t, projectID, "openai", "openai-responses", "streaming", routing.FidelityTransformed, []string{toolsetID}, true, fixture, true)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if h.mock.count("a") == 1 {
			item := fmt.Sprintf(`{"type":"function_call","id":"fc-1","call_id":"call-1","name":%q,"arguments":"{}","status":"completed"}`, alias)
			fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"sequence_number\":0,\"output_index\":0,\"item\":%s}\n\ndata: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"id\":\"first\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"model-a\",\"output\":[%s],\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5}}}\n\n", item, item)
			return
		}
		for _, event := range streamedEvents("final answer") {
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
	})
	client := openai.NewClient(option.WithBaseURL(h.server.URL+"/v1/"), option.WithAPIKey(fullKey))
	stream := client.Responses.NewStreaming(t.Context(), responses.ResponseNewParams{
		Model: routeSlug,
		Input: responses.ResponseNewParamsInputUnion{OfString: openai.String("hello")},
	})
	defer stream.Close()
	events := slices.Collect(func(yield func(responses.ResponseStreamEventUnion) bool) {
		for stream.Next() {
			if !yield(stream.Current()) {
				return
			}
		}
	})
	if err := stream.Err(); err != nil || len(events) < 6 {
		t.Fatalf("official Responses SDK did not receive workflow stream: %v (%d events)", err, len(events))
	}
	listing := events[1].Item.AsMcpListTools()
	call := events[3].Item.AsMcpCall()
	terminal := events[len(events)-1].Response
	if events[1].Type != "response.output_item.added" || listing.ID == "" || listing.ServerLabel != toolsetID || len(listing.Tools) != 1 || listing.Tools[0].Name != alias {
		t.Fatalf("official SDK failed to decode MCP listing: %+v", listing)
	}
	if events[3].Type != "response.output_item.added" || call.ID == "" || call.ServerLabel != toolsetID || call.Name != alias || call.Arguments != "{}" || call.Status != "completed" || !strings.Contains(call.Output, "private-tool-result") {
		t.Fatalf("official SDK failed to decode MCP call: %+v", call)
	}
	if events[len(events)-1].Type != "response.completed" || len(terminal.Output) != 3 || terminal.Output[0].AsMcpListTools().ID != listing.ID || terminal.Output[1].AsMcpCall().ID != call.ID || terminal.OutputText() != "final answer" {
		t.Fatalf("official SDK received inconsistent terminal output: %+v", terminal)
	}
}

func testResponsesWorkflowStream(t *testing.T, includeItems bool) {
	t.Helper()
	toolsetID, projectID := uuid.NewString(), uuid.NewString()
	alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	fixture := &workflowFixture{runID: uuid.NewString(), result: json.RawMessage(`{"content":[{"type":"text","text":"private-tool-result"}]}`), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "read_only", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	h := nativeWorkflowHarness(t, projectID, "openai", "openai-responses", "streaming", routing.FidelityTransformed, []string{toolsetID}, true, fixture, includeItems)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if h.mock.count("a") == 1 {
			item := fmt.Sprintf(`{"type":"function_call","id":"fc-1","call_id":"call-1","name":%q,"arguments":"{}","status":"completed"}`, alias)
			fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"sequence_number\":0,\"output_index\":0,\"item\":%s}\n\ndata: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"id\":\"first\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"model-a\",\"output\":[%s],\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5}}}\n\n", item, item)
			return
		}
		for _, event := range streamedEvents("final answer") {
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
	})
	response := h.do(t.Context(), http.MethodPost, "/v1/responses", fullKey, []byte(`{"model":"`+routeSlug+`","input":"hello","stream":true}`), nil)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), "final answer") || strings.Contains(string(body), "function_call") {
		t.Fatalf("Responses stream leaked intermediate output or lost final answer: status=%d body=%s error=%v", response.StatusCode, body, err)
	}
	if !includeItems {
		if strings.Contains(string(body), "private-tool-result") || strings.Contains(string(body), "mcp_call") {
			t.Fatalf("undeclared Responses stream exposed MCP items: %s", body)
		}
		return
	}
	type streamItem struct {
		Type        string `json:"type"`
		ID          string `json:"id"`
		ServerLabel string `json:"server_label"`
		Name        string `json:"name"`
		Arguments   string `json:"arguments"`
		Output      string `json:"output"`
		Status      string `json:"status"`
		Tools       []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	type streamEvent struct {
		Type        string     `json:"type"`
		Sequence    int        `json:"sequence_number"`
		OutputIndex *int       `json:"output_index"`
		Item        streamItem `json:"item"`
		Response    struct {
			Output []streamItem `json:"output"`
		} `json:"response"`
	}
	var events []streamEvent
	if err := sse.Decode(strings.NewReader(string(body)), 1<<20, func(frame sse.Frame) error {
		if frame.Control {
			return nil
		}
		var event streamEvent
		if err := json.Unmarshal([]byte(frame.Data), &event); err != nil {
			return err
		}
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatalf("invalid Responses stream: %v", err)
	}
	if len(events) < 6 {
		t.Fatalf("missing Responses MCP events: %s", body)
	}
	for index, event := range events {
		if event.Sequence != index {
			t.Fatalf("noncontiguous event sequence: %s", body)
		}
	}
	final := events[len(events)-1]
	if final.Type != "response.completed" || len(final.Response.Output) != 3 || final.Response.Output[0].Type != "mcp_list_tools" || final.Response.Output[1].Type != "mcp_call" || final.Response.Output[2].Type != "message" {
		t.Fatalf("terminal response omitted ordered MCP items: %s", body)
	}
	for index := 0; index < 2; index++ {
		added, done, terminal := events[1+2*index], events[2+2*index], final.Response.Output[index]
		if added.Type != "response.output_item.added" || done.Type != "response.output_item.done" || added.OutputIndex == nil || done.OutputIndex == nil || *added.OutputIndex != index || *done.OutputIndex != index || terminal.ID == "" || !reflect.DeepEqual(added.Item, terminal) || !reflect.DeepEqual(done.Item, terminal) {
			t.Fatalf("stream phases disagree with terminal output: %s", body)
		}
	}
	listing, call := final.Response.Output[0], final.Response.Output[1]
	if listing.ServerLabel != toolsetID || len(listing.Tools) != 1 || listing.Tools[0].Name != alias || call.ServerLabel != toolsetID || call.Name != alias || call.Arguments != "{}" || call.Status != "completed" || !strings.Contains(call.Output, "private-tool-result") {
		t.Fatalf("stream item content is incorrect: %s", body)
	}

}

func TestGatewayToolExecutionResponsesProjectsToolFailure(t *testing.T) {
	toolsetID, projectID := uuid.NewString(), uuid.NewString()
	alias := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	fixture := &workflowFixture{runID: uuid.NewString(), result: json.RawMessage(`{"content":[{"type":"text","text":"tool refused"}],"isError":true}`), tool: WorkflowTool{ToolsetID: toolsetID, RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{Alias: alias, Classification: "read_only", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	h := nativeWorkflowHarness(t, projectID, "openai", "openai-responses", "unary", routing.FidelityTransformed, []string{toolsetID}, true, fixture, true)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if h.mock.count("a") == 1 {
			fmt.Fprintf(w, `{"id":"first","object":"response","status":"completed","model":"model-a","output":[{"type":"function_call","id":"fc-1","call_id":"call-1","name":%q,"arguments":"{}","status":"completed"}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`, alias)
			return
		}
		io.WriteString(w, `{"id":"last","object":"response","status":"completed","model":"model-a","output":[{"type":"message","id":"msg-1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"final answer","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}`)
	})
	response := h.do(t.Context(), http.MethodPost, "/v1/responses", fullKey, []byte(`{"model":"`+routeSlug+`","input":"hello"}`), nil)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("Responses workflow failed: status=%d body=%s error=%v", response.StatusCode, body, err)
	}
	var result struct {
		Output []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
			Error  struct {
				Type    string          `json:"type"`
				Content json.RawMessage `json:"content"`
			} `json:"error"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &result); err != nil || len(result.Output) != 3 {
		t.Fatalf("Responses workflow missing expected output: %s", body)
	}
	call, final := result.Output[1], result.Output[2]
	if call.Type != "mcp_call" || call.Status != "failed" || call.Error.Type != "mcp_tool_execution_error" || !strings.Contains(string(call.Error.Content), "tool refused") {
		t.Fatalf("tool failure was not represented as an MCP error: %s", body)
	}
	if final.Type != "message" || len(final.Content) != 1 || final.Content[0].Text != "final answer" {
		t.Fatalf("final answer is missing: %s", body)
	}
}

type multipleWorkflowTools struct {
	tools   []WorkflowTool
	results map[string]json.RawMessage
}

func (f *multipleWorkflowTools) Resolve(_ context.Context, _ access.Authority, _ *runtime.Release, toolsets []string) ([]WorkflowTool, error) {
	return slices.Collect(func(yield func(WorkflowTool) bool) {
		for _, tool := range f.tools {
			if slices.Contains(toolsets, tool.ToolsetID) && !yield(tool) {
				return
			}
		}
	}), nil
}

func (f *multipleWorkflowTools) Call(_ context.Context, _ access.Authority, _ *runtime.Release, tool WorkflowTool, _ string, _ int, _ string, _ json.RawMessage) (json.RawMessage, error) {
	return f.results[tool.Definition.Alias], nil
}

func TestGatewayToolExecutionResponsesNarrowsAndOrdersMCPItems(t *testing.T) {
	selected, excluded, project := uuid.NewString(), uuid.NewString(), uuid.NewString()
	first, second, other := "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_cccccccccccccccccccccccccccccccc", "t_dddddddddddddddddddddddddddddddd_eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	schema := json.RawMessage(`{"type":"object"}`)
	fixture := &workflowFixture{runID: uuid.NewString()}
	h := nativeWorkflowHarness(t, project, "openai", "openai-responses", "unary", routing.FidelityTransformed, []string{selected, excluded}, true, fixture, true)
	h.gateway.WorkflowTools = &multipleWorkflowTools{tools: []WorkflowTool{
		{ToolsetID: selected, Definition: routing.MCPToolsetTool{Alias: first, Classification: "read_only", InputSchema: schema}},
		{ToolsetID: selected, Definition: routing.MCPToolsetTool{Alias: second, Classification: "read_only", InputSchema: schema}},
		{ToolsetID: excluded, Definition: routing.MCPToolsetTool{Alias: other, Classification: "read_only", InputSchema: schema}},
	}, results: map[string]json.RawMessage{
		first:  json.RawMessage(`{"content":[{"type":"text","text":"first result"}]}`),
		second: json.RawMessage(`{"content":[{"type":"text","text":"second result"}]}`),
	}}
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if h.mock.count("a") == 1 {
			var request struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Tools) != 2 || request.Tools[0].Name != first || request.Tools[1].Name != second {
				t.Errorf("provider received wrong narrowed toolset: %+v (%v)", request.Tools, err)
			}
			fmt.Fprintf(w, `{"id":"first","object":"response","status":"completed","model":"model-a","output":[{"type":"function_call","id":"fc-1","call_id":"call-1","name":%q,"arguments":"{\"n\":1}","status":"completed"},{"type":"function_call","id":"fc-2","call_id":"call-2","name":%q,"arguments":"{\"n\":2}","status":"completed"}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`, first, second)
			return
		}
		io.WriteString(w, `{"id":"last","object":"response","status":"completed","model":"model-a","output":[{"type":"message","id":"msg-1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"final answer","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}`)
	})
	request := fmt.Appendf(nil, `{"model":%q,"input":"hello","olp_toolsets":[%q]}`, routeSlug, selected)
	response := h.do(t.Context(), http.MethodPost, "/v1/responses", fullKey, request, nil)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("workflow failed: status=%d body=%s err=%v", response.StatusCode, body, err)
	}
	var result struct {
		Output []struct {
			Type        string `json:"type"`
			ServerLabel string `json:"server_label"`
			Name        string `json:"name"`
			Arguments   string `json:"arguments"`
			Output      string `json:"output"`
			Tools       []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &result); err != nil || len(result.Output) != 4 {
		t.Fatalf("wrong output: %s (%v)", body, err)
	}
	listing, call1, call2, final := result.Output[0], result.Output[1], result.Output[2], result.Output[3]
	if listing.Type != "mcp_list_tools" || listing.ServerLabel != selected || len(listing.Tools) != 2 || listing.Tools[0].Name != first || listing.Tools[1].Name != second {
		t.Fatalf("wrong selected listing: %s", body)
	}
	if call1.Type != "mcp_call" || call1.ServerLabel != selected || call1.Name != first || call1.Arguments != `{"n":1}` || !strings.Contains(call1.Output, "first result") || call2.Type != "mcp_call" || call2.ServerLabel != selected || call2.Name != second || call2.Arguments != `{"n":2}` || !strings.Contains(call2.Output, "second result") || final.Type != "message" {
		t.Fatalf("wrong call order or content: %s", body)
	}
}

func TestGatewayRejectsApprovalOnNonWorkflowRouteBeforeDispatch(t *testing.T) {
	h := strictHarness(t, nil)
	response := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"hello"}],"olp_approval":{"continuation_token":"olpa_test","approved":true,"arguments":{"call":{}}}}`), nil)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "tool_approval_invalid") || h.mock.count("a") != 0 || h.mock.count("b") != 0 {
		t.Fatalf("approval control on ordinary route was not refused before dispatch: status=%d body=%s error=%v", response.StatusCode, body, err)
	}
}
