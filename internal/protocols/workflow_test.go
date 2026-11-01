package protocols

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func TestAnthropicWorkflowPreservesStreamedToolAndSignedThinking(t *testing.T) {
	// Anthropic streaming format: https://platform.claude.com/docs/en/build-with-claude/streaming (2026-10-29).
	stream := strings.Join([]string{
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"check"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"signed"}}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call-1","name":"lookup","input":{}}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"query\":"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"weather\"}"}}`,
	}, "\n\n") + "\n\n"
	body, err := WorkflowStreamBody(wire.Anthropic, strings.NewReader(stream), 4096)
	if err != nil {
		t.Fatal(err)
	}
	completion := &wire.Completion{Body: body, ToolCalls: []wire.ToolCall{{ID: "call-1", Name: "lookup", Arguments: `{"query":"weather"}`}}}
	turns, err := WorkflowTurns(wire.Anthropic, completion, []WorkflowResult{{CallID: "call-1", Content: "sunny"}})
	if err != nil || len(turns) != 2 {
		t.Fatalf("continuation = %s, %v", turns, err)
	}
	var assistant struct {
		Role    string `json:"role"`
		Content []struct {
			Type      string            `json:"type"`
			Thinking  string            `json:"thinking"`
			Signature string            `json:"signature"`
			ID        string            `json:"id"`
			Name      string            `json:"name"`
			Input     map[string]string `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(turns[0], &assistant); err != nil || assistant.Role != "assistant" || len(assistant.Content) != 2 {
		t.Fatalf("assistant continuation = %s, %v", turns[0], err)
	}
	if assistant.Content[0].Type != "thinking" || assistant.Content[0].Thinking != "check" || assistant.Content[0].Signature != "signed" || assistant.Content[1].Type != "tool_use" || assistant.Content[1].ID != completion.ToolCalls[0].ID || assistant.Content[1].Name != completion.ToolCalls[0].Name || !reflect.DeepEqual(assistant.Content[1].Input, map[string]string{"query": "weather"}) {
		t.Fatalf("streamed content changed: %+v", assistant.Content)
	}
	var result struct {
		Role    string `json:"role"`
		Content []struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
			Content   []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"content"`
	}
	if err := json.Unmarshal(turns[1], &result); err != nil || result.Role != "user" || len(result.Content) != 1 || result.Content[0].Type != "tool_result" || result.Content[0].ToolUseID != completion.ToolCalls[0].ID || len(result.Content[0].Content) != 1 || result.Content[0].Content[0].Text != "sunny" {
		t.Fatalf("tool result continuation = %s, %v", turns[1], err)
	}
}

func TestGeminiWorkflowPreservesStreamedCallsAndPairedResults(t *testing.T) {
	// Gemini GenerateContent function calls: https://ai.google.dev/api/generate-content (2026-10-29).
	stream := strings.Join([]string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"checking"}]}}]}`,
		`data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"call-1","name":"lookup","args":{"query":"weather"}},"thoughtSignature":"signed"}]}}]}`,
		`data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"call-2","name":"lookup","args":{"query":"traffic"}}}]}}]}`,
	}, "\n\n") + "\n\n"
	body, err := WorkflowStreamBody(wire.GeminiStream, strings.NewReader(stream), 4096)
	if err != nil {
		t.Fatal(err)
	}
	completion := &wire.Completion{Body: body, ToolCalls: []wire.ToolCall{{ID: "call-1", Name: "lookup", Arguments: `{"query":"weather"}`}, {ID: "call-2", Name: "lookup", Arguments: `{"query":"traffic"}`}}}
	turns, err := WorkflowTurns(wire.Gemini, completion, []WorkflowResult{{CallID: "call-1", Content: "sunny"}, {CallID: "call-2", Content: "clear"}})
	if err != nil || len(turns) != 2 {
		t.Fatalf("continuation = %s, %v", turns, err)
	}
	var assistant struct {
		Role  string `json:"role"`
		Parts []struct {
			Text             string `json:"text"`
			ThoughtSignature string `json:"thoughtSignature"`
			FunctionCall     *struct {
				ID   string            `json:"id"`
				Name string            `json:"name"`
				Args map[string]string `json:"args"`
			} `json:"functionCall"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(turns[0], &assistant); err != nil || assistant.Role != "model" || len(assistant.Parts) != 3 || assistant.Parts[0].Text != "checking" || assistant.Parts[1].ThoughtSignature != "signed" || assistant.Parts[1].FunctionCall == nil || assistant.Parts[1].FunctionCall.ID != completion.ToolCalls[0].ID || assistant.Parts[1].FunctionCall.Name != completion.ToolCalls[0].Name || assistant.Parts[1].FunctionCall.Args["query"] != "weather" || assistant.Parts[2].FunctionCall == nil || assistant.Parts[2].FunctionCall.ID != completion.ToolCalls[1].ID {
		t.Fatalf("assistant continuation = %s, %v", turns[0], err)
	}
	var result struct {
		Role  string `json:"role"`
		Parts []struct {
			FunctionResponse struct {
				ID       string            `json:"id"`
				Name     string            `json:"name"`
				Response map[string]string `json:"response"`
			} `json:"functionResponse"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(turns[1], &result); err != nil || result.Role != "user" || len(result.Parts) != 2 || result.Parts[0].FunctionResponse.ID != completion.ToolCalls[0].ID || result.Parts[0].FunctionResponse.Name != completion.ToolCalls[0].Name || result.Parts[0].FunctionResponse.Response["output"] != "sunny" || result.Parts[1].FunctionResponse.ID != completion.ToolCalls[1].ID || result.Parts[1].FunctionResponse.Response["output"] != "clear" {
		t.Fatalf("tool result continuation = %s, %v", turns[1], err)
	}
}

func TestNativeWorkflowRejectsInvalidStreamsAndContinuations(t *testing.T) {
	for _, tc := range []struct {
		name   string
		family wire.Format
		stream string
		limit  int
	}{
		{"bad JSON", wire.Anthropic, "data: {\n\n", 1024},
		{"unknown block", wire.Anthropic, "data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n", 1024},
		{"unsupported delta", wire.Anthropic, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"unknown\"}}\n\n", 1024},
		{"truncated tool input", wire.Anthropic, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"input\":{}}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\"}}\n\n", 1024},
		{"multiple candidates", wire.GeminiStream, "data: {\"candidates\":[{},{}]}\n\n", 1024},
		{"oversized event", wire.GeminiStream, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"oversized\"}]}}]}\n\n", 16},
		{"unsupported family", wire.Chat, "data: {}\n\n", 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := WorkflowStreamBody(tc.family, strings.NewReader(tc.stream), tc.limit); err == nil {
				t.Fatal("accepted an invalid workflow stream")
			}
		})
	}
	if WorkflowSingleCandidate([]byte(`{"candidates":[{},{}]}`)) || WorkflowSingleCandidate([]byte(`invalid`)) || !WorkflowSingleCandidate([]byte(`{"candidates":[{}]}`)) {
		t.Fatal("failed to reject a malformed or ambiguous response")
	}
	if _, err := WorkflowTurns(wire.Gemini, &wire.Completion{Body: []byte(`{"candidates":[{},{}]}`)}, nil); err == nil {
		t.Fatal("accepted an ambiguous assistant continuation")
	}
	if _, err := WorkflowTurns(wire.Anthropic, &wire.Completion{Body: []byte(`{}`)}, nil); err == nil {
		t.Fatal("accepted an assistant continuation without content")
	}
}

func TestNativeWorkflowDefinitionsPreservePublishedToolSchemas(t *testing.T) {
	// Tool schemas: https://platform.claude.com/docs/en/claude_api_primer and https://ai.google.dev/api/generate-content (2026-10-29).
	tools := []Tool{{Name: "lookup", Description: "Find a result", Schema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`)}}
	anthropic, err := WorkflowDefinitions(wire.Anthropic, tools)
	if err != nil {
		t.Fatal(err)
	}
	var anthropicTools []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	if err := json.Unmarshal(anthropic, &anthropicTools); err != nil || len(anthropicTools) != 1 || anthropicTools[0].Name != tools[0].Name || anthropicTools[0].Description != tools[0].Description {
		t.Fatalf("Anthropic tool definitions = %s, %v", anthropic, err)
	}
	gemini, err := WorkflowDefinitions(wire.Gemini, tools)
	if err != nil {
		t.Fatal(err)
	}
	var geminiTools []struct {
		FunctionDeclarations []struct {
			Name                 string          `json:"name"`
			Description          string          `json:"description"`
			Parameters           json.RawMessage `json:"parameters"`
			ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema"`
		} `json:"functionDeclarations"`
	}
	if err := json.Unmarshal(gemini, &geminiTools); err != nil || len(geminiTools) != 1 || len(geminiTools[0].FunctionDeclarations) != 1 || geminiTools[0].FunctionDeclarations[0].Name != tools[0].Name || geminiTools[0].FunctionDeclarations[0].Description != tools[0].Description {
		t.Fatalf("Gemini tool definitions = %s, %v", gemini, err)
	}
	geminiSchema := func() json.RawMessage {
		if len(geminiTools[0].FunctionDeclarations[0].ParametersJSONSchema) > 0 {
			return geminiTools[0].FunctionDeclarations[0].ParametersJSONSchema
		}
		return geminiTools[0].FunctionDeclarations[0].Parameters
	}()
	var sourceSchema, anthropicSchema, decodedGeminiSchema map[string]any
	if json.Unmarshal(tools[0].Schema, &sourceSchema) != nil || json.Unmarshal(anthropicTools[0].InputSchema, &anthropicSchema) != nil || json.Unmarshal(geminiSchema, &decodedGeminiSchema) != nil || !reflect.DeepEqual(anthropicSchema, sourceSchema) || !reflect.DeepEqual(decodedGeminiSchema, sourceSchema) {
		t.Fatalf("tool schemas differ: source=%s, Anthropic=%s, Gemini=%s", tools[0].Schema, anthropicTools[0].InputSchema, geminiSchema)
	}
	if _, err := WorkflowDefinitions(wire.Chat, tools); err == nil {
		t.Fatal("accepted unsupported native tool definitions")
	}
	if _, err := WorkflowTurns(wire.Gemini, &wire.Completion{ToolCalls: []wire.ToolCall{{ID: "call-1", Name: tools[0].Name, Arguments: `{}`}}}, []WorkflowResult{{CallID: "different-call", Content: "result"}}); err == nil {
		t.Fatal("accepted a result for a different tool call")
	}
}
