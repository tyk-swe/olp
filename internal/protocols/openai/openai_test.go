package openai

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/tests/fixtures"
)

func read(t *testing.T, name string) []byte {
	t.Helper()
	data, err := fixtures.Files.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestChatRequestFixture(t *testing.T) {
	expected := testutil.JSON[struct {
		Route          string   `json:"route"`
		MessageCount   int      `json:"message_count"`
		ToolCount      int      `json:"tool_count"`
		ExtensionPaths []string `json:"extension_paths"`
		UpstreamModel  string   `json:"upstream_model"`
	}](t, fixtures.Files, "protocols/openai-chat-request.expected.json")
	request, err := Parse(wire.Chat, read(t, "protocols/openai-chat-request.json"))
	if err != nil {
		t.Fatal(err)
	}
	if request.Route != expected.Route || !request.Stream {
		t.Fatalf("route=%q stream=%v", request.Route, request.Stream)
	}
	var messages, tools []json.RawMessage
	json.Unmarshal(request.Field("messages"), &messages)
	json.Unmarshal(request.Field("tools"), &tools)
	if len(messages) != expected.MessageCount || len(tools) != expected.ToolCount {
		t.Fatalf("messages=%d tools=%d", len(messages), len(tools))
	}
	paths := Extensions(request)
	want := append([]string(nil), expected.ExtensionPaths...)
	if !reflect.DeepEqual(sorted(paths), sorted(want)) {
		t.Fatalf("extensions %v, want %v", paths, want)
	}
	encoded, err := Encode(request, expected.UpstreamModel, map[string]json.RawMessage{"temperature": json.RawMessage("0.2"), "model": json.RawMessage(`"ignored"`)})
	if err != nil {
		t.Fatal(err)
	}
	var upstream map[string]any
	if err := json.Unmarshal(encoded, &upstream); err != nil {
		t.Fatal(err)
	}
	if upstream["model"] != expected.UpstreamModel || upstream["service_tier"] != "priority" || upstream["temperature"] != 0.2 {
		t.Fatalf("upstream document %s", encoded)
	}
	if options, _ := upstream["stream_options"].(map[string]any); options["include_usage"] != true {
		t.Fatalf("stream_options not forced: %s", encoded)
	}
	first := upstream["messages"].([]any)[0].(map[string]any)
	tool := upstream["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if first["vendor_message_flag"] != true || tool["strict"] != true {
		t.Fatalf("extensions dropped: %s", encoded)
	}
}

func sorted(items []string) []string {
	out := append([]string(nil), items...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func collect(frames *[][]byte) wire.Emit {
	return func(frame []byte) error {
		*frames = append(*frames, append([]byte(nil), frame...))
		return nil
	}
}
