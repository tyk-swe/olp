package openai

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
)

func TestChatUsageModalityShares(t *testing.T) {
	audio, audioOut := int64(7), int64(11)
	completion, err := DecodeChat([]byte(`{
		"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-audio",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":20,"completion_tokens":30,"total_tokens":50,
			"prompt_tokens_details":{"cached_tokens":4,"audio_tokens":7},
			"completion_tokens_details":{"audio_tokens":11}}}`), "gpt-audio")
	if err != nil {
		t.Fatal(err)
	}
	if completion.Usage == nil || completion.Usage.AudioInputTokens == nil ||
		*completion.Usage.AudioInputTokens != audio || completion.Usage.AudioOutputTokens == nil ||
		*completion.Usage.AudioOutputTokens != audioOut {
		t.Fatalf("usage %+v", completion.Usage)
	}
}

func TestResponseUsageCountsCompletedToolItems(t *testing.T) {
	body := `{"id":"resp-1","object":"response","status":"completed","model":"gpt",
		"output":[
			{"type":"web_search_call","status":"completed"},
			{"type":"web_search_call","status":"completed"},
			{"type":"file_search_call","status":"completed"},
			{"type":"web_search_call","status":"failed"},
			{"type":"web_search_call","status":"in_progress"},
			{"type":"message","status":"completed","content":[]},
			{"type":"function_call","status":"completed","call_id":"c1","name":"f","arguments":"{}"}
		],
		"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	for _, decode := range []func([]byte) (*Usage, error){
		func(body []byte) (*Usage, error) {
			observed, err := ObserveResponseUsage(body)
			return observed.Usage, err
		},
		func(body []byte) (*Usage, error) {
			c, err := DecodeResponse(body, "gpt")
			if err != nil {
				return nil, err
			}
			return c.Usage, nil
		},
	} {
		usage, err := decode([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]int64{"web_search": 2, "file_search": 1}
		if !reflect.DeepEqual(usage.ToolCalls, want) {
			t.Fatalf("tool calls %v, want %v", usage.ToolCalls, want)
		}
	}
}

func TestResponseToolCallsThroughTerminalStreamEvent(t *testing.T) {
	var frame bytes.Buffer
	frame.WriteString("event: response.completed\n")
	frame.WriteString(`data: {"type":"response.completed","response":{"status":"completed","output":[` +
		`{"type":"web_search_call","status":"completed"},{"type":"web_search_call","status":"cancelled"}],` +
		`"usage":{"input_tokens":3,"output_tokens":4}}}` + "\n\n")
	terminal := false
	var usage *Usage
	_, err := stream(FamilyResponses, &frame, 1<<20, "gpt", false, true, func([]byte) error { return nil }, func(e oif.Event) error {
		observed, lifecycle, err := ObserveResponseEventUsage(e)
		if err != nil {
			return err
		}
		if lifecycle && observed.Usage != nil {
			terminal = true
			usage = observed.Usage
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !terminal || !reflect.DeepEqual(usage.ToolCalls, map[string]int64{"web_search": 1}) {
		t.Fatalf("terminal=%t usage %+v", terminal, usage)
	}
}
