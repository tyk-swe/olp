package openai

import (
	"fmt"
	"strings"
)

// responsesEvents renders Responses events as an upstream streams them.
func responsesEvents(events ...string) string {
	var stream strings.Builder
	for _, event := range events {
		kind, _, _ := strings.Cut(strings.TrimPrefix(event, `{"type":"`), `"`)
		fmt.Fprintf(&stream, "event: %s\ndata: %s\n\n", kind, event)
	}
	return stream.String()
}

const (
	messageItem  = `{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello there","annotations":[]}]}`
	callItem     = `{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_1","name":"weather","arguments":"{\"city\":\"Paris\"}"}`
	streamUsage  = `{"input_tokens":7,"output_tokens":5,"total_tokens":12}`
	createdEvent = `{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","status":"in_progress","model":"acme-large","output":[]}}`
)

func terminalEvent(kind, status, output string) string {
	return `{"type":"` + kind + `","sequence_number":9,"response":{"id":"resp_1","object":"response","status":"` + status + `","model":"acme-large","output":` + output + `,"usage":` + streamUsage + `}}`
}

func itemDone(index int, item string) string {
	return fmt.Sprintf(`{"type":"response.output_item.done","sequence_number":%d,"output_index":%d,"item":%s}`, 4+index, index, item)
}

// Completed items count toward the bound as they arrive.
