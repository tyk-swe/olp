package openai

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestUnaryChatRejectsMalformedChoicesAtEveryPosition(t *testing.T) {
	for _, tc := range []struct{ name, choice string }{
		{"empty", `{}`},
		{"null", `null`},
		{"scalar", `1`},
		{"missing message", `{"index":1,"finish_reason":"stop"}`},
		{"null message", `{"index":1,"message":null,"finish_reason":"stop"}`},
		{"scalar message", `{"index":1,"message":"ok","finish_reason":"stop"}`},
		{"empty message", `{"index":1,"message":{},"finish_reason":"stop"}`},
		{"wrong role", `{"index":1,"message":{"role":"user","content":"ok"},"finish_reason":"stop"}`},
		{"missing index", `{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}`},
		{"negative index", `{"index":-1,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}`},
		{"fractional index", `{"index":1.5,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}`},
		{"missing finish", `{"index":1,"message":{"role":"assistant","content":"ok"}}`},
		{"null finish", `{"index":1,"message":{"role":"assistant","content":"ok"},"finish_reason":null}`},
		{"empty finish", `{"index":1,"message":{"role":"assistant","content":"ok"},"finish_reason":""}`},
		{"invalid content", `{"index":1,"message":{"role":"assistant","content":{}},"finish_reason":"stop"}`},
		{"invalid refusal", `{"index":1,"message":{"role":"assistant","refusal":3},"finish_reason":"stop"}`},
		{"invalid tool list", `{"index":1,"message":{"role":"assistant","tool_calls":{}},"finish_reason":"tool_calls"}`},
		{"invalid tool", `{"index":1,"message":{"role":"assistant","tool_calls":[null]},"finish_reason":"tool_calls"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, prefix := range []string{"", `{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"},`} {
				body := []byte(`{"choices":[` + prefix + tc.choice + `]}`)
				completion, err := DecodeChat(body, "route")
				var protocol *ProtocolError
				if completion != nil || !errors.As(err, &protocol) {
					t.Fatalf("malformed choice accepted: %s: %+v, %v", body, completion, err)
				}
			}
		})
	}
	if _, err := DecodeChat([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"a"},"finish_reason":"stop"},{"index":0,"message":{"role":"assistant","content":"b"},"finish_reason":"stop"}]}`), "route"); err == nil {
		t.Fatal("duplicate choice index accepted")
	}
}

func TestUnaryChatPreservesValidChoicesAndSummarizesTheFirst(t *testing.T) {
	body := []byte(`{"model":"upstream","vendor_metadata":{"keep":true},"choices":[{"index":0,"message":{"role":"assistant","content":"first"},"finish_reason":"stop"},{"index":1,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"weather","arguments":"{}"}}]},"finish_reason":"tool_calls"},{"index":2,"message":{"role":"assistant","content":null,"refusal":"refused"},"finish_reason":"content_filter"},{"index":3,"message":{"role":"assistant","content":""},"finish_reason":"length"}]}`)
	completion, err := DecodeChat(body, "route")
	if err != nil {
		t.Fatal(err)
	}
	if completion.OutputText != "first" || completion.FinishReason != "stop" || completion.Refusal != "" || len(completion.ToolCalls) != 0 {
		t.Fatalf("later choices changed the summary: %+v", completion)
	}
	var want, got map[string]json.RawMessage
	json.Unmarshal(body, &want)
	json.Unmarshal(completion.Body, &got)
	want["model"] = json.RawMessage(`"route"`)
	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(got)
	if string(wantJSON) != string(gotJSON) {
		t.Fatalf("valid choice data changed: %s", completion.Body)
	}
}
