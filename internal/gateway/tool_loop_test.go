package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

const nativeToolResult = "private-tool-result"

func nativeWorkflowHarness(t *testing.T, projectID, surface, profile, mode, fidelity string, toolsets []string, certified bool, fixture *workflowFixture) *harness {
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
			Behavior: routing.Behavior{ToolExecution: "gateway", Toolsets: toolsets},
		}}
		snapshot.Providers = maps.Collect(func(yield func(string, routing.Provider) bool) {
			for id, provider := range snapshot.Providers {
				if !yield(id, routing.Provider{
					ID: provider.ID, Name: provider.Name, Kind: surface, Enabled: provider.Enabled,
					ActiveCredential: provider.ActiveCredential, RevisionID: provider.RevisionID,
					Endpoint: provider.Endpoint, AuthMode: provider.AuthMode, Slots: provider.Slots,
					ProfileID: profile, ProfileRevision: connectors.ProfileRevision,
					Capabilities: []routing.Capability{{Model: provider.Capabilities[0].Model, Operation: "generation", Surface: surface, Mode: mode, ToolCalls: certified}},
				}) {
					return
				}
			}
		})
	})
	h.rt.keys[fullKey] = access.Authority{ID: h.rt.keys[fullKey].ID, ProjectID: &projectID, Policy: h.rt.keys[fullKey].Policy}
	h.gateway.WorkflowRuns, h.gateway.WorkflowTools, h.gateway.Debits = fixture, fixture, &workflowDebitFixture{fakeSessionDebits: &fakeSessionDebits{}}
	return h
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
