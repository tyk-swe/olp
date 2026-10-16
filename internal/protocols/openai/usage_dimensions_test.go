package openai

import (
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

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
	for _, decode := range []func([]byte) (*wire.Usage, error){
		func(body []byte) (*wire.Usage, error) {
			observed, err := ObserveResponseUsage(body)
			return observed.Usage, err
		},
		func(body []byte) (*wire.Usage, error) {
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
