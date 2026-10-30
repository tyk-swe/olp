//go:build integration && codemode

package gosdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/openai/openai-go/v3"
	openaioption "github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

func TestCodeMessagesToolExchange(t *testing.T) {
	base := os.Getenv("OLP_CODE_MESSAGES_BASE_URL")
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" {
		t.Fatal("OLP_CODE_MESSAGES_BASE_URL must name the controlled code route")
	}
	h := harness{origin: parsed.Scheme + "://" + parsed.Host, anthropicBase: base, apiKey: os.Getenv("OLP_CODE_MESSAGES_API_KEY")}
	model := os.Getenv("OLP_CODE_MESSAGES_MODEL")
	if h.apiKey == "" || model == "" {
		t.Fatal("code route API key and model are required")
	}
	for _, stream := range []bool{false, true} {
		for _, source := range []string{"fragments", "complete"} {
			t.Run(fmt.Sprintf("%t/%s", stream, source), func(t *testing.T) {
				session := fmt.Sprintf("sdk-%t-%s", stream, source)
				client := h.anthropic(anthropicoption.WithHeader("X-Claude-Code-Session-Id", session))
				messages := []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("Read main.go"))}
				request := func(client *anthropic.Client, history []anthropic.MessageParam, stage string) anthropic.Message {
					t.Helper()
					params := anthropic.MessageNewParams{Model: anthropic.Model(model), MaxTokens: 64, Messages: history, Tools: []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{Name: "read_file", InputSchema: anthropic.ToolInputSchemaParam{Properties: map[string]any{"path": map[string]string{"type": "string"}}, Required: []string{"path"}}}}}}
					if !stream {
						message, err := client.Messages.New(timeout(t), params)
						if err != nil {
							t.Fatalf("SDK diagnostic %s %s", stage, codeMessagesRequestFailure(err))
						}
						return *message
					}
					events := client.Messages.NewStreaming(timeout(t), params)
					defer events.Close()
					message := anthropic.Message{}
					for events.Next() {
						if err := message.Accumulate(events.Current()); err != nil {
							t.Fatalf("SDK diagnostic %s invalid streamed message", stage)
						}
					}
					if err := events.Err(); err != nil {
						t.Fatalf("SDK diagnostic %s %s", stage, codeMessagesRequestFailure(err))
					}
					return message
				}
				first := request(client, messages, "tool")
				if first.StopReason != "tool_use" || len(first.Content) != 1 || first.Content[0].Type != "tool_use" {
					t.Fatal("SDK diagnostic tool invalid content")
				}
				call := first.Content[0].AsToolUse()
				var arguments struct{ Path string }
				if err := json.Unmarshal(call.Input, &arguments); err != nil || call.ID != "call-read" || call.Name != "read_file" || arguments.Path != "main.go" {
					t.Fatal("SDK diagnostic tool invalid arguments")
				}
				history := slices.Concat(messages, []anthropic.MessageParam{first.ToParam(), anthropic.NewUserMessage(anthropic.NewToolResultBlock(call.ID, "package main", false))})
				child := h.anthropic(anthropicoption.WithHeader("X-Claude-Code-Session-Id", session), anthropicoption.WithHeader("X-Claude-Code-Agent-Id", "reader"))
				last := request(child, history, "result")
				if last.StopReason != "end_turn" || len(last.Content) != 1 || last.Content[0].AsText().Text != "File read" || last.Usage.InputTokens != 13 || last.Usage.OutputTokens != 2 {
					t.Fatal("SDK diagnostic result invalid content")
				}
				resumed := h.anthropic(anthropicoption.WithHeader("X-Claude-Code-Session-Id", session), anthropicoption.WithHeader("X-Claude-Code-Agent-Id", "reader"))
				compacted := request(resumed, []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("CONTROLLED_COMPACT_SUMMARY"))}, "resume")
				if compacted.StopReason != "end_turn" || len(compacted.Content) != 1 || compacted.Content[0].AsText().Text != "File read" {
					t.Fatal("SDK diagnostic resume invalid content")
				}
			})
		}
	}
}

func codeMessagesRequestFailure(err error) string {
	var api *anthropic.Error
	if errors.As(err, &api) && api.StatusCode >= 100 && api.StatusCode <= 599 {
		return fmt.Sprintf("HTTP %d", api.StatusCode)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	return "request failed"
}

func TestCodeMessagesRequestFailureHidesPayload(t *testing.T) {
	api := &anthropic.Error{StatusCode: 502}
	if err := api.UnmarshalJSON([]byte(`{"error":{"message":"controlled-private-response"}}`)); err != nil {
		t.Fatal(err)
	}
	if diagnostic := codeMessagesRequestFailure(api); diagnostic != "HTTP 502" {
		t.Fatal("API error diagnostic lost its status or retained response content")
	}
	if diagnostic := codeMessagesRequestFailure(fmt.Errorf("controlled-private-response %w", context.DeadlineExceeded)); diagnostic != "timeout" {
		t.Fatal("timeout diagnostic lost its category or retained error content")
	}
}

func TestCodeChatToolExchange(t *testing.T) {
	base := os.Getenv("OLP_CODE_CHAT_BASE_URL")
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" {
		t.Fatal("OLP_CODE_CHAT_BASE_URL must name the controlled code route")
	}
	h := harness{origin: parsed.Scheme + "://" + parsed.Host, openAIBase: base, apiKey: os.Getenv("OLP_CODE_CHAT_API_KEY")}
	model := os.Getenv("OLP_CODE_CHAT_MODEL")
	if h.apiKey == "" || model == "" {
		t.Fatal("code route API key and model are required")
	}
	for _, stream := range []bool{false, true} {
		for _, source := range []string{"fragments", "complete"} {
			t.Run(fmt.Sprintf("%t/%s", stream, source), func(t *testing.T) {
				session := fmt.Sprintf("sdk-chat-%t-%s", stream, source)
				client := h.openAI(openaioption.WithHeader("X-Opencode-Session-Id", session))
				messages := []openai.ChatCompletionMessageParamUnion{openai.SystemMessage("Preserve my instructions"), openai.UserMessage("Read main.go")}
				request := func(client *openai.Client, history []openai.ChatCompletionMessageParamUnion, stage string) openai.ChatCompletion {
					t.Helper()
					params := openai.ChatCompletionNewParams{Model: model, Messages: history, ReasoningEffort: shared.ReasoningEffortHigh, MaxCompletionTokens: openai.Int(64), Tools: []openai.ChatCompletionToolUnionParam{openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{Name: "read_file", Parameters: shared.FunctionParameters{"type": "object", "properties": map[string]any{"path": map[string]string{"type": "string"}}, "required": []string{"path"}}})}}
					if !stream {
						completion, err := client.Chat.Completions.New(timeout(t), params)
						if err != nil {
							t.Fatalf("SDK diagnostic %s %s", stage, codeChatRequestFailure(err))
						}
						return *completion
					}
					params.StreamOptions = openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)}
					events := client.Chat.Completions.NewStreaming(timeout(t), params)
					defer events.Close()
					completion := openai.ChatCompletionAccumulator{}
					for events.Next() {
						if !completion.AddChunk(events.Current()) {
							t.Fatalf("SDK diagnostic %s invalid streamed message", stage)
						}
					}
					if err := events.Err(); err != nil {
						t.Fatalf("SDK diagnostic %s %s", stage, codeChatRequestFailure(err))
					}
					return completion.ChatCompletion
				}
				first := request(client, messages, "tool")
				if len(first.Choices) != 1 || first.Choices[0].FinishReason != "tool_calls" || len(first.Choices[0].Message.ToolCalls) != 1 {
					t.Fatal("SDK diagnostic tool invalid content")
				}
				call := first.Choices[0].Message.ToolCalls[0]
				var arguments struct{ Path string }
				if err := json.Unmarshal([]byte(call.Function.Arguments), &arguments); err != nil || call.ID != "call-read" || call.Function.Name != "read_file" || arguments.Path != "main.go" {
					t.Fatal("SDK diagnostic tool invalid arguments")
				}
				history := slices.Concat(messages, []openai.ChatCompletionMessageParamUnion{{OfAssistant: &openai.ChatCompletionAssistantMessageParam{ToolCalls: []openai.ChatCompletionMessageToolCallUnionParam{{OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{ID: call.ID, Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{Name: call.Function.Name, Arguments: call.Function.Arguments}}}}}}, openai.ToolMessage("package main", call.ID)})
				child := h.openAI(openaioption.WithHeader("X-Opencode-Session-Id", session+"-reader"), openaioption.WithHeader("X-Opencode-Parent-Session-Id", session))
				last := request(child, history, "result")
				if len(last.Choices) != 1 || last.Choices[0].FinishReason != "stop" || last.Choices[0].Message.Content != "File read" || last.Usage.PromptTokens != 13 || last.Usage.CompletionTokens != 2 {
					t.Fatal("SDK diagnostic result invalid content")
				}
				resumed := h.openAI(openaioption.WithHeader("X-Opencode-Session-Id", session+"-reader"), openaioption.WithHeader("X-Opencode-Parent-Session-Id", session))
				compacted := request(resumed, []openai.ChatCompletionMessageParamUnion{openai.UserMessage("CONTROLLED_COMPACT_SUMMARY")}, "resume")
				if len(compacted.Choices) != 1 || compacted.Choices[0].FinishReason != "stop" || compacted.Choices[0].Message.Content != "File read" {
					t.Fatal("SDK diagnostic resume invalid content")
				}
			})
		}
	}
}

func codeChatRequestFailure(err error) string {
	var api *openai.Error
	if errors.As(err, &api) && api.StatusCode >= 100 && api.StatusCode <= 599 {
		return fmt.Sprintf("HTTP %d", api.StatusCode)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	return "request failed"
}
