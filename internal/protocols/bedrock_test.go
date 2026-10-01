package protocols

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"io"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func TestNativeConverseRetainsResultAndAWSFrames(t *testing.T) {
	body := []byte(`{"output":{"message":{"role":"assistant","content":[{"text":"native"}]}},"stopReason":"end_turn","usage":{"inputTokens":1,"outputTokens":2,"totalTokens":3},"future":{"integer":9007199254740993}}`)
	completion, err := Decode(openai.FamilyBedrock, openai.FamilyBedrock, body, "route", "")
	if err != nil || !bytes.Equal(completion.Body, body) {
		t.Fatalf("native result changed: %v %+v", err, completion)
	}
	var source bytes.Buffer
	for _, event := range []struct{ name, body string }{
		{"messageStart", `{"role":"assistant"}`},
		{"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"native"},"future":9007199254740993}`},
		{"contentBlockStop", `{"contentBlockIndex":0}`},
		{"messageStop", `{"stopReason":"end_turn"}`},
		{"metadata", `{"usage":{"inputTokens":1,"outputTokens":2,"totalTokens":3}}`},
	} {
		headers := eventstream.Headers{{Name: ":message-type", Value: eventstream.StringValue("event")}, {Name: ":event-type", Value: eventstream.StringValue(event.name)}, {Name: "future-header", Value: eventstream.StringValue("preserved")}}
		if err := eventstream.NewEncoder().Encode(&source, eventstream.Message{Headers: headers, Payload: []byte(event.body)}); err != nil {
			t.Fatal(err)
		}
	}
	var observed bytes.Buffer
	_, err = Stream(openai.FamilyBedrock, openai.FamilyBedrock, bytes.NewReader(source.Bytes()), 4096, "route", true, func(frame []byte) error { _, err := observed.Write(frame); return err })
	if err != nil || !bytes.Equal(observed.Bytes(), source.Bytes()) {
		t.Fatalf("AWS native framing changed: %v", err)
	}
}

func TestConverseRejectsAmbiguousReservedAndExtensionHeaders(t *testing.T) {
	for _, name := range []string{":message-type", ":event-type", ":exception-type", "future-header"} {
		headers := eventstream.Headers{{Name: name, Value: eventstream.StringValue("first")}, {Name: name, Value: eventstream.StringValue("last")}}
		var wire bytes.Buffer
		if err := eventstream.NewEncoder().Encode(&wire, eventstream.Message{Headers: headers, Payload: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadBedrockEvent(&wire, 4096); err == nil {
			t.Fatalf("duplicate %s was decoded last-wins", name)
		}
	}
}

func TestBedrockInlineImagesAcrossSurfaces(t *testing.T) {
	for _, tc := range []struct {
		family openai.Family
		body   string
	}{
		{openai.FamilyChat, `{"model":"route","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,aW1hZ2U="}}]}]}`},
		{openai.FamilyResponses, `{"model":"route","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,aW1hZ2U="}]}]}`},
		{openai.FamilyInputTokens, `{"model":"route","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,aW1hZ2U="}]}]}`},
		{openai.FamilyAnthropic, `{"model":"route","max_tokens":16,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}}]}]}`},
		{openai.FamilyAnthropicCount, `{"model":"route","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}}]}]}`},
		{openai.FamilyGemini, `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"aW1hZ2U="}}]}]}`},
		{openai.FamilyGeminiStream, `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"aW1hZ2U="}}]}]}`},
		{openai.FamilyGeminiCount, `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"aW1hZ2U="}}]}]}`},
	} {
		t.Run(string(tc.family), func(t *testing.T) {
			request, err := Parse(tc.family, []byte(tc.body), "route")
			if err != nil {
				t.Fatal(err)
			}
			body, wire, err := Encode(request, "bedrock", "amazon-bedrock", "wire-model", nil)
			if err != nil {
				t.Fatal(err)
			}
			wantWire := openai.Family("bedrock")
			if tc.family.Operation() == "token_count" {
				wantWire = "bedrock_count"
				if !bytes.Contains(body, []byte(`"converse"`)) {
					t.Fatalf("missing CountTokens converse envelope: %s", body)
				}
			}
			if wire != wantWire || !bytes.Contains(body, []byte(`"image":{"format":"png","source":{"bytes":"aW1hZ2U="}}`)) {
				t.Fatalf("lost inline image: %s %s", wire, body)
			}
		})
	}
}

func TestBedrockImageFormatsAndRefusals(t *testing.T) {
	for _, format := range []string{"png", "jpeg", "gif", "webp"} {
		t.Run(format, func(t *testing.T) {
			part := Part{MIME: "image/" + format, URL: "data:image/" + format + ";base64,aW1hZ2U="}
			body, err := encodeBedrock(&Generation{Messages: []Message{{Role: "user", Parts: []Part{part}}}}, "model", "generation")
			if err != nil || !bytes.Contains(raw(body), []byte(fmt.Sprintf(`"format":%q`, format))) {
				t.Fatalf("image format lost: %s %v", raw(body), err)
			}
		})
	}
	for _, tc := range []struct {
		name, role, mime, url, detail string
	}{
		{"remote URL", "user", "image/png", "https://example.com/image.png", ""},
		{"image detail", "user", "image/png", "data:image/png;base64,aW1hZ2U=", "high"},
		{"unsupported MIME", "user", "image/svg+xml", "data:image/svg+xml;base64,aW1hZ2U=", ""},
		{"invalid base64", "user", "image/png", "data:image/png;base64,invalid!", ""},
		{"system image", "system", "image/png", "data:image/png;base64,aW1hZ2U=", ""},
		{"tool result image", "tool", "image/png", "data:image/png;base64,aW1hZ2U=", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := Message{Role: tc.role, Parts: []Part{{Text: "result"}, {MIME: tc.mime, URL: tc.url, Detail: tc.detail}}}
			if tc.role == "tool" {
				message.ToolID = "tool-1"
			}
			for _, operation := range []string{"generation", "token_count"} {
				if _, err := encodeBedrock(&Generation{Messages: []Message{message}}, "model", operation); err == nil {
					t.Errorf("%s accepted unsupported %s", operation, tc.name)
				}
			}
		})
	}
}

func encodeBedrockMessages(t *testing.T, family openai.Family, body string) []map[string]json.RawMessage {
	t.Helper()
	request, err := Parse(family, []byte(body), "route")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _, err := Encode(request, "bedrock", "amazon-bedrock", "wire-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(wire.Messages); i++ {
		if string(wire.Messages[i]["role"]) == string(wire.Messages[i-1]["role"]) {
			t.Fatalf("adjacent %s messages: %s", wire.Messages[i]["role"], encoded)
		}
	}
	return wire.Messages
}

func TestBedrockMergesAdjacentSameRoleMessages(t *testing.T) {
	messages := encodeBedrockMessages(t, openai.FamilyChat, `{"model":"route","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}},
			{"id":"b","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"a","content":"x"},
		{"role":"tool","tool_call_id":"b","content":"y"},
		{"role":"user","content":"next"}]}`)
	if len(messages) != 3 {
		t.Fatalf("messages = %d", len(messages))
	}
	var content []map[string]json.RawMessage
	if err := json.Unmarshal(messages[2]["content"], &content); err != nil {
		t.Fatal(err)
	}
	if len(content) != 3 || !bytes.Contains(content[0]["toolResult"], []byte(`"toolUseId":"a"`)) ||
		!bytes.Contains(content[1]["toolResult"], []byte(`"toolUseId":"b"`)) || string(content[2]["text"]) != `"next"` {
		t.Fatalf("last user turn = %s", messages[2]["content"])
	}

	messages = encodeBedrockMessages(t, openai.FamilyResponses, `{"model":"route","input":[
		{"role":"user","content":"hi"},
		{"type":"function_call","call_id":"a","name":"f","arguments":"{}"},
		{"type":"function_call","call_id":"b","name":"f","arguments":"{}"},
		{"type":"function_call_output","call_id":"a","output":"x"},
		{"type":"function_call_output","call_id":"b","output":"y"}]}`)
	if len(messages) != 3 || bytes.Count(messages[1]["content"], []byte(`"toolUse"`)) != 2 ||
		bytes.Count(messages[2]["content"], []byte(`"toolResult"`)) != 2 {
		t.Fatalf("responses turns = %v", messages)
	}
}

func TestBedrockSkipsEmptyTextParts(t *testing.T) {
	g := &Generation{Messages: []Message{
		{Role: "system", Parts: []Part{{Text: ""}}},
		{Role: "user", Parts: []Part{{Text: "hi"}}},
		{Role: "assistant", Parts: []Part{{Text: ""}}, Calls: []openai.ToolCall{{ID: "call-1", Name: "lookup", Arguments: "{}"}}},
		{Role: "tool", ToolID: "call-1", Parts: []Part{{Text: "ok"}}},
	}}
	for _, operation := range []string{"generation", "token_count"} {
		body, err := encodeBedrock(g, "model", operation)
		if err != nil {
			t.Fatal(err)
		}
		encoded := raw(body)
		if bytes.Contains(encoded, []byte(`{"text":""}`)) || !bytes.Contains(encoded, []byte(`"toolUse"`)) || bytes.Contains(encoded, []byte(`"system"`)) {
			t.Fatalf("%s: %s", operation, encoded)
		}
	}
	empty := &Generation{Messages: []Message{{Role: "user", Parts: []Part{{Text: ""}}}}}
	if _, err := encodeBedrock(empty, "model", "generation"); err == nil {
		t.Fatal("accepted a message with no content")
	}
}

func TestBedrockOmitsEmptyToolDescription(t *testing.T) {
	g := &Generation{
		Messages: []Message{{Role: "user", Parts: []Part{{Text: "hi"}}}},
		Tools: []Tool{
			{Name: "f", Schema: json.RawMessage(`{"type":"object"}`)},
			{Name: "g", Description: "d", Schema: json.RawMessage(`{"type":"object"}`)},
		},
	}
	body, err := encodeBedrock(g, "model", "generation")
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		ToolConfig struct {
			Tools []struct {
				ToolSpec map[string]json.RawMessage `json:"toolSpec"`
			} `json:"tools"`
		} `json:"toolConfig"`
	}
	if err := json.Unmarshal(raw(body), &wire); err != nil {
		t.Fatal(err)
	}
	tools := wire.ToolConfig.Tools
	if len(tools) != 2 {
		t.Fatalf("tools = %s", raw(body))
	}
	if _, ok := tools[0].ToolSpec["description"]; ok {
		t.Fatalf("empty description sent: %s", raw(body))
	}
	if string(tools[1].ToolSpec["description"]) != `"d"` {
		t.Fatalf("description lost: %s", raw(body))
	}
}

func TestReadBedrockEventTruncatedAfterPrelude(t *testing.T) {
	encode := func(buffer *bytes.Buffer, event, payload string) {
		t.Helper()
		headers := eventstream.Headers{{Name: ":message-type", Value: eventstream.StringValue("event")}, {Name: ":event-type", Value: eventstream.StringValue(event)}}
		if err := eventstream.NewEncoder().Encode(buffer, eventstream.Message{Headers: headers, Payload: []byte(payload)}); err != nil {
			t.Fatal(err)
		}
	}
	var metadata bytes.Buffer
	encode(&metadata, "metadata", `{"usage":{"inputTokens":1,"outputTokens":2}}`)
	prelude := metadata.Bytes()[:12]
	if _, err := ReadBedrockEvent(bytes.NewReader(prelude), 4096); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v", err)
	}

	var stream bytes.Buffer
	encode(&stream, "messageStart", `{"role":"assistant"}`)
	encode(&stream, "messageStop", `{"stopReason":"end_turn"}`)
	stream.Write(prelude)
	_, err := Stream(openai.FamilyBedrock, openai.FamilyChat, bytes.NewReader(stream.Bytes()), 4096, "route", true, func([]byte) error { return nil })
	var protocolErr *openai.ProtocolError
	if !errors.As(err, &protocolErr) || !protocolErr.Truncated {
		t.Fatalf("stream err = %v", err)
	}
}
