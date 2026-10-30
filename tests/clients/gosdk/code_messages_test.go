//go:build integration && codemode

package gosdk

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"slices"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
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
				client := h.anthropic(anthropicoption.WithHeader("X-Claude-Code-Session-Id", fmt.Sprintf("sdk-%t-%s", stream, source)))
				messages := []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("Read main.go"))}
				request := func(history []anthropic.MessageParam) anthropic.Message {
					t.Helper()
					params := anthropic.MessageNewParams{Model: anthropic.Model(model), MaxTokens: 64, Messages: history, Tools: []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{Name: "read_file", InputSchema: anthropic.ToolInputSchemaParam{Properties: map[string]any{"path": map[string]string{"type": "string"}}, Required: []string{"path"}}}}}}
					if !stream {
						message, err := client.Messages.New(timeout(t), params)
						if err != nil {
							t.Fatal(err)
						}
						return *message
					}
					events := client.Messages.NewStreaming(timeout(t), params)
					defer events.Close()
					message := anthropic.Message{}
					for events.Next() {
						if err := message.Accumulate(events.Current()); err != nil {
							t.Fatal(err)
						}
					}
					if err := events.Err(); err != nil {
						t.Fatal(err)
					}
					return message
				}
				first := request(messages)
				if first.StopReason != "tool_use" || len(first.Content) != 1 || first.Content[0].Type != "tool_use" {
					t.Fatalf("SDK tool reply %+v", first)
				}
				call := first.Content[0].AsToolUse()
				var arguments struct{ Path string }
				if err := json.Unmarshal(call.Input, &arguments); err != nil || call.ID != "call-read" || call.Name != "read_file" || arguments.Path != "main.go" {
					t.Fatalf("SDK tool arguments %+v %v", call, err)
				}
				history := slices.Concat(messages, []anthropic.MessageParam{first.ToParam(), anthropic.NewUserMessage(anthropic.NewToolResultBlock(call.ID, "package main", false))})
				last := request(history)
				if last.StopReason != "end_turn" || len(last.Content) != 1 || last.Content[0].AsText().Text != "File read" || last.Usage.InputTokens != 13 || last.Usage.OutputTokens != 2 {
					t.Fatalf("SDK result reply %+v", last)
				}
			})
		}
	}
}
