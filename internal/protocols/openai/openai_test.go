package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

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
	request, err := Parse(FamilyChat, read(t, "protocols/openai-chat-request.json"))
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
	paths := request.Extensions()
	want := append([]string(nil), expected.ExtensionPaths...)
	if !reflect.DeepEqual(sorted(paths), sorted(want)) {
		t.Fatalf("extensions %v, want %v", paths, want)
	}
	encoded, err := request.Encode(expected.UpstreamModel, map[string]json.RawMessage{"temperature": json.RawMessage("0.2"), "model": json.RawMessage(`"ignored"`)})
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

func TestRequestRejections(t *testing.T) {
	cases := map[string]struct {
		family Family
		body   string
		code   string
		param  string
	}{
		"both token limits":  {FamilyChat, `{"model":"r","messages":[{"role":"user","content":"x"}],"max_tokens":1,"max_completion_tokens":2}`, "invalid_value", "max_tokens"},
		"provider qualified": {FamilyChat, `{"model":"provider/model","messages":[{"role":"user","content":"x"}]}`, "invalid_value", "model"},
		"missing messages":   {FamilyChat, `{"model":"r"}`, "missing_required_parameter", "messages"},
		"stream type":        {FamilyChat, `{"model":"r","messages":[{"role":"user","content":"x"}],"stream":"yes"}`, "invalid_value", "stream"},
		"trailing document":  {FamilyChat, `{"model":"r","messages":[{"role":"user","content":"x"}]} {}`, "invalid_json", ""},
		"stateful reference": {FamilyResponses, `{"model":"r","input":"hi","previous_response_id":"resp_1"}`, "unsupported_stateful_reference", "previous_response_id"},
		"background":         {FamilyResponses, `{"model":"r","input":"hi","background":true}`, "unsupported_parameter", "background"},
		"empty input":        {FamilyResponses, `{"model":"r","input":[]}`, "invalid_value", "input"},
	}
	for name, c := range cases {
		_, err := Parse(c.family, []byte(c.body))
		var re *RequestError
		if !errors.As(err, &re) || re.Code != c.code || re.Param != c.param {
			t.Errorf("%s: got %v", name, err)
		}
	}
	if _, err := Parse(FamilyResponses, []byte(`{"model":"r","input":"hi","store":true,"vendor":{"x":1}}`)); err != nil {
		t.Fatalf("extensions rejected: %v", err)
	}
}

func TestSelectedOperationFamilies(t *testing.T) {
	cases := testutil.JSON[[]struct {
		Codec         string          `json:"codec"`
		ExpectedRoute string          `json:"expected_route"`
		Wire          json.RawMessage `json:"wire"`
	}](t, fixtures.Files, "protocols/selected-operation-families.json")
	seen := 0
	for _, c := range cases {
		var family Family
		switch c.Codec {
		case "openai_chat":
			family = FamilyChat
		case "openai_responses":
			family = FamilyResponses
		default:
			continue
		}
		seen++
		request, err := Parse(family, c.Wire)
		if err != nil || request.Route != c.ExpectedRoute {
			t.Errorf("%s: route=%v err=%v", c.Codec, request, err)
		}
	}
	if seen != 2 {
		t.Fatalf("expected both OpenAI generation codecs, saw %d", seen)
	}
}

func collect(frames *[][]byte) Emit {
	return func(frame []byte) error {
		*frames = append(*frames, append([]byte(nil), frame...))
		return nil
	}
}

func TestChatStreamFixture(t *testing.T) {
	expected := testutil.JSON[struct {
		FragmentBytes int    `json:"fragment_bytes"`
		Text          string `json:"text"`
		Done          bool   `json:"done"`
		TotalTokens   int64  `json:"total_tokens"`
	}](t, fixtures.Files, "streams/openai-chat.expected.json")
	var frames [][]byte
	completion, err := Stream(FamilyChat, testutil.Fragmented(read(t, "streams/openai-chat.sse"), expected.FragmentBytes), 1<<20, "team-chat", true, collect(&frames))
	if err != nil {
		t.Fatal(err)
	}
	if completion.OutputText != expected.Text || completion.FinishReason != "stop" || completion.Usage == nil || completion.Usage.TotalTokens != expected.TotalTokens {
		t.Fatalf("completion %+v", completion)
	}
	if len(frames) != 3 || string(frames[2]) != "data: [DONE]\n\n" {
		t.Fatalf("frames %q", frames)
	}
	for _, frame := range frames[:2] {
		if !bytes.Contains(frame, []byte(`"model":"team-chat"`)) || bytes.Contains(frame, []byte("gpt-fixture")) {
			t.Fatalf("model not rewritten: %s", frame)
		}
	}
}

func TestChatStreamProtocolErrors(t *testing.T) {
	chunk := func(body string) string { return "data: " + body + "\n\n" }
	start := chunk(`{"id":"c","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"a"},"finish_reason":null}]}`)
	finish := chunk(`{"id":"c","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	usageOnly := chunk(`{"id":"c","object":"chat.completion.chunk","model":"m","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	annotation := chunk(`{"id":"c","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"vendor_annotation":{"score":1}}]}`)
	secondStart := chunk(`{"choices":[{"index":1,"delta":{"content":"b"},"finish_reason":null}]}`)
	secondFinish := chunk(`{"choices":[{"index":1,"delta":{},"finish_reason":"stop"}]}`)
	secondAnnotation := chunk(`{"choices":[{"index":1,"vendor_annotation":{"score":1}}]}`)
	done := "data: [DONE]\n\n"
	cases := map[string]struct {
		stream    string
		truncated bool
		ok        bool
	}{
		"eof without done":             {start + finish, true, false},
		"done before finish":           {start + done, true, false},
		"done without choices":         {done, true, false},
		"only second choice finished":  {chunk(`{"choices":[{"index":1,"delta":{},"finish_reason":"stop"}]}`) + done, true, false},
		"second choice unfinished":     {start + secondStart + finish + usageOnly + done, true, false},
		"second choice role only":      {finish + chunk(`{"choices":[{"index":1,"delta":{"role":"assistant"}}]}`) + done, true, false},
		"third choice unfinished":      {finish + secondFinish + chunk(`{"choices":[{"index":2,"delta":{"content":"c"}}]}`) + done, true, false},
		"all choices finished":         {start + secondStart + finish + secondFinish + usageOnly + done, false, true},
		"second choice finishes first": {secondStart + secondFinish + start + finish + done, false, true},
		"second choice annotation":     {finish + secondFinish + secondAnnotation + done, false, true},
		"content after finish":         {start + finish + start + done, false, false},
		"duplicate finish":             {start + finish + finish + done, false, false},
		"data after done is ignored":   {start + finish + done + start, false, true},
		"annotation after finish":      {start + finish + annotation + usageOnly + done, false, true},
		"missing usage":                {start + finish + done, false, true},
	}
	for name, c := range cases {
		var frames [][]byte
		completion, err := Stream(FamilyChat, strings.NewReader(c.stream), 1<<20, "r", true, collect(&frames))
		if c.ok {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			} else if name == "missing usage" && completion.Usage != nil {
				t.Errorf("%s: usage should be absent", name)
			} else if name == "annotation after finish" && (completion.Usage == nil || completion.Usage.TotalTokens != 2) {
				t.Errorf("%s: usage lost", name)
			}
			continue
		}
		var pe *ProtocolError
		if !errors.As(err, &pe) || pe.Truncated != c.truncated {
			t.Errorf("%s: got %v", name, err)
		}
		if c.truncated && bytes.Contains(bytes.Join(frames, nil), []byte("[DONE]")) {
			t.Errorf("%s: truncated stream emitted a success marker", name)
		}
	}
	oversized := chunk(`{"object":"chat.completion.chunk","choices":[],"pad":"` + strings.Repeat("x", 300) + `"}`)
	if _, err := Stream(FamilyChat, strings.NewReader(oversized), 256, "r", true, collect(new([][]byte))); !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("oversized event: %v", err)
	}
	errorEvent := chunk(`{"error":{"message":"quota exhausted","type":"insufficient_quota","code":"insufficient_quota"}}`)
	var ue *UpstreamError
	if _, err := Stream(FamilyChat, strings.NewReader(start+errorEvent), 1<<20, "r", true, collect(new([][]byte))); !errors.As(err, &ue) || ue.Code != "insufficient_quota" {
		t.Fatalf("error event: %v", err)
	}
}

func TestResponsesStream(t *testing.T) {
	event := func(kind, body string) string { return "event: " + kind + "\ndata: " + body + "\n\n" }
	created := event("response.created", `{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","model":"gpt-x","status":"in_progress","output":[]}}`)
	delta := event("response.output_text.delta", `{"type":"response.output_text.delta","sequence_number":1,"delta":"hi"}`)
	completed := event("response.completed", `{"type":"response.completed","sequence_number":2,"response":{"id":"resp_1","object":"response","model":"gpt-x","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hi there"}]}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5,"output_tokens_details":{"reasoning_tokens":1}}}}`)
	var frames [][]byte
	completion, err := Stream(FamilyResponses, testutil.Fragmented([]byte(created+delta+completed), 3), 1<<20, "r", true, collect(&frames))
	if err != nil {
		t.Fatal(err)
	}
	if completion.OutputText != "hi there" || completion.Usage == nil || completion.Usage.TotalTokens != 5 || *completion.Usage.ReasoningTokens != 1 || completion.FinishReason != "stop" {
		t.Fatalf("completion %+v", completion)
	}
	if len(frames) != 3 || !bytes.HasPrefix(frames[0], []byte("event: response.created\ndata: ")) || bytes.Contains(frames[2], []byte("gpt-x")) {
		t.Fatalf("frames %q", frames)
	}
	var pe *ProtocolError
	if _, err = Stream(FamilyResponses, strings.NewReader(created+delta), 1<<20, "r", true, collect(new([][]byte))); !errors.As(err, &pe) || !pe.Truncated {
		t.Fatalf("truncated stream: %v", err)
	}
	frames = nil
	if _, err = Stream(FamilyResponses, strings.NewReader(created+completed+delta), 1<<20, "r", true, collect(&frames)); err != nil || len(frames) != 2 {
		t.Fatalf("data after terminal was not ignored: frames=%q, err=%v", frames, err)
	}
	failed := event("response.failed", `{"type":"response.failed","sequence_number":2,"response":{"id":"resp_1","object":"response","status":"failed","output":[],"error":{"code":"server_error","message":"boom"}}}`)
	var ue *UpstreamError
	if _, err = Stream(FamilyResponses, strings.NewReader(created+failed), 1<<20, "r", true, collect(new([][]byte))); !errors.As(err, &ue) || ue.Code != "server_error" {
		t.Fatalf("failed response: %v", err)
	}
}

func TestUnaryDecoding(t *testing.T) {
	chat := []byte(`{"id":"chatcmpl_1","object":"chat.completion","model":"gpt-x","system_fingerprint":"fp","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Oslo\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12,"prompt_tokens_details":{"cached_tokens":2}}}`)
	completion, err := DecodeChat(chat, "team-chat")
	if err != nil {
		t.Fatal(err)
	}
	if completion.FinishReason != "tool_calls" || len(completion.ToolCalls) != 1 || completion.ToolCalls[0].Name != "weather" || *completion.Usage.CachedInputTokens != 2 {
		t.Fatalf("completion %+v", completion)
	}
	var body map[string]any
	json.Unmarshal(completion.Body, &body)
	if body["model"] != "team-chat" || body["system_fingerprint"] != "fp" {
		t.Fatalf("body %s", completion.Body)
	}
	var ue *UpstreamError
	if _, err = DecodeChat([]byte(`{"error":{"message":"bad key","type":"invalid_request_error","code":"invalid_api_key"}}`), "r"); !errors.As(err, &ue) || ue.Code != "invalid_api_key" {
		t.Fatalf("error body: %v", err)
	}
	var pe *ProtocolError
	if _, err = DecodeChat([]byte(`{"object":"chat.completion","choices":[]}`), "r"); !errors.As(err, &pe) {
		t.Fatalf("empty choices: %v", err)
	}
	response := []byte(`{"id":"resp_1","object":"response","model":"gpt-x","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`)
	if completion, err = DecodeResponse(response, "r"); err != nil || completion.FinishReason != "length" || completion.OutputText != "partial" || !bytes.Contains(completion.Body, []byte(`"model":"r"`)) {
		t.Fatalf("response decode: %+v %v", completion, err)
	}
	if upstream := ParseErrorBody([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error","code":429}}`)); upstream == nil || upstream.Code != "429" {
		t.Fatalf("error parse: %+v", upstream)
	}
}
