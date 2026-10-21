package protocols

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func TestChatGeminiMultipleCandidatesSurviveUnaryAndStreaming(t *testing.T) {
	source := []byte(`{"id":"multi","choices":[{"index":0,"message":{"role":"assistant","content":"alpha"},"finish_reason":"stop"},{"index":1,"message":{"role":"assistant","content":"beta"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":2,"total_tokens":4}}`)
	c, err := Decode(wire.Chat, wire.Gemini, source, "team-model", "")
	if err != nil {
		t.Fatal(err)
	}
	f, _ := object(c.Body)
	if len(arr(f["candidates"])) != 2 || !bytes.Contains(c.Body, []byte("beta")) {
		t.Fatalf("candidates %s", c.Body)
	}
	if _, err = Decode(wire.Chat, wire.Anthropic, source, "team-model", ""); err == nil {
		t.Fatal("silently dropped a candidate")
	}
	streamed := "data: " + `{"id":"multi","choices":[{"index":0,"delta":{"content":"alpha"},"finish_reason":"stop"},{"index":1,"delta":{"content":"beta"},"finish_reason":"stop"}]}` + "\n\ndata: " + `{"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":2,"total_tokens":4}}` + "\n\ndata: [DONE]\n\n"
	var gem bytes.Buffer
	_, err = Stream(wire.Chat, wire.Gemini, strings.NewReader(streamed), 8192, "team-model", true, func(b []byte) error { gem.Write(b); return nil })
	if err != nil {
		t.Fatal(err)
	}
	var chat bytes.Buffer
	summary, err := Stream(wire.Gemini, wire.Chat, bytes.NewReader(gem.Bytes()), 8192, "team-model", true, func(b []byte) error { chat.Write(b); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if summary.Usage == nil || summary.Usage.TotalTokens != 4 || strings.Count(chat.String(), "[DONE]") != 1 || !strings.Contains(chat.String(), `"index":1`) || !strings.Contains(chat.String(), `"role":"assistant"`) {
		t.Fatalf("lost candidate/usage/completion: %s", chat.String())
	}
	if _, err = Stream(wire.Chat, wire.Chat, bytes.NewReader(chat.Bytes()), 8192, "team-model", true, func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestCountingKeepsBedrockToolsAndInvalidGeneratedArgumentsRefuseTranslation(t *testing.T) {
	request, err := Parse(wire.InputTokens, []byte(`{"model":"team-model","input":"hello","tools":[{"type":"function","name":"weather","parameters":{"type":"object","properties":{}}}]}`), "team-model")
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := Encode(request, "bedrock", "amazon-bedrock", "wire-model", nil)
	if err != nil || !bytes.Contains(body, []byte("toolConfig")) || bytes.Contains(body, []byte("inferenceConfig")) {
		t.Fatalf("count input lost tool tokens: %s %v", body, err)
	}
	malformed := []byte(`{"id":"t","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call","type":"function","function":{"name":"weather","arguments":"not-json"}}]},"finish_reason":"tool_calls"}]}`)
	if _, err = Decode(wire.Chat, wire.Anthropic, malformed, "team-model", ""); err == nil {
		t.Fatal("fabricated a native tool input from malformed generated arguments")
	}
}

func TestGeminiParallelResultsKeepDistinctCallRelationships(t *testing.T) {
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"weather","args":{"city":"Paris"}}},{"functionCall":{"name":"weather","args":{"city":"London"}}}]},{"role":"user","parts":[{"functionResponse":{"name":"weather","response":{"temperature":20}}},{"functionResponse":{"name":"weather","response":{"temperature":16}}}]}]}`)
	request, err := Parse(wire.Gemini, input, "team-model")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _, err := Encode(request, "openai", "openai", "wire-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	document, _ := object(encoded)
	messages := arr(document["messages"])
	assistant, _ := object(messages[0])
	calls := arr(assistant["tool_calls"])
	if len(messages) != 3 || len(calls) != 2 {
		t.Fatalf("lost parallel tools: %s", encoded)
	}
	for i, call := range calls {
		tool, _ := object(call)
		result, _ := object(messages[i+1])
		if str(tool["id"]) != str(result["tool_call_id"]) {
			t.Fatalf("tool result attached to another call: %s", encoded)
		}
	}
	for _, mime := range []string{"", "text/plain"} {
		bad := []byte(`{"contents":[{"parts":[{"text":"hello"}]}],"generationConfig":{"responseMimeType":"` + mime + `","responseSchema":{"type":"object"}}}`)
		request, err = Parse(wire.Gemini, bad, "team-model")
		if err != nil {
			continue
		}
		if _, _, err = Encode(request, "openai", "openai", "wire-model", nil); err == nil {
			t.Fatal("response schema was silently dropped")
		}
	}
}
