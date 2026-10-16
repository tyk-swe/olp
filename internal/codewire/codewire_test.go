package codewire

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/codemode"
)

func refusal(err error) string {
	if r, ok := errors.AsType[*codemode.Refusal](err); ok {
		return r.Code
	}
	return fmt.Sprint(err)
}

func stream(observer Observer, body string) []codemode.Observation {
	var observed []codemode.Observation
	events := NewEvents(MaxBody, func(data []byte) {
		if o := observer.Event(data); o.Terminal || o.Status != 0 {
			observed = append(observed, o)
		}
	})
	for i := range len(body) {
		events.Write([]byte{body[i]})
	}
	if o, ok := observer.Finish(); ok {
		observed = append(observed, o)
	}
	return observed
}

func TestMessagesUsageIncludesCacheTrafficAndEndsOnStop(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":30,\"cache_creation_input_tokens\":5,\"output_tokens\":1}}}\n\n" +
		"event: ping\ndata: {\"type\": \"ping\"}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"private\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	observed := stream(NewMessages(), body)
	if len(observed) != 1 || !observed[0].Successful || observed[0].Outcome.Kind != "completed" {
		t.Fatalf("observations: %+v", observed)
	}
	u := observed[0].Usage
	if *u.Input != 45 || *u.Cached != 30 || *u.Output != 7 || *u.Total != 52 || u.Reasoning != nil || u.Validate() != nil {
		t.Fatalf("usage: %+v", u)
	}
	truncated := strings.Replace(strings.TrimSuffix(body, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"), "end_turn", "max_tokens", 1)
	if observed := stream(NewMessages(), truncated); len(observed) != 1 || observed[0].Outcome.Kind != "incomplete" || observed[0].Successful || *observed[0].Usage.Total != 52 {
		t.Fatalf("stop without message_stop: %+v", observed)
	}
	if observed := stream(NewMessages(), strings.SplitAfter(body, "\n\n")[0]); len(observed) != 0 {
		t.Fatalf("interrupted stream reported an outcome: %+v", observed)
	}
	errorEvent := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"private\"}}\n\n"
	if observed := stream(NewMessages(), errorEvent); len(observed) != 1 || observed[0].Status != 529 || observed[0].Outcome.Kind != "failed" || observed[0].Usage.Total != nil {
		t.Fatalf("error event: %+v", observed)
	}
	unary := NewMessages().Unary([]byte(`{"type":"message","stop_reason":"tool_use","usage":{"input_tokens":3,"output_tokens":4}}`))
	if !unary.Terminal || !unary.Successful || *unary.Usage.Total != 7 || unary.Usage.Cached != nil {
		t.Fatalf("unary: %+v", unary)
	}
	if o := NewMessages().Unary([]byte(`{"type":"error","error":{"type":"rate_limit_error"}}`)); !o.Terminal || o.Status != 0 || o.Outcome.Kind != "failed" {
		t.Fatalf("unary error: %+v", o)
	}
}

func TestChatUsageNeedsTheFinalChunkAndEndsOnDone(t *testing.T) {
	chunk := func(s string) string { return "data: " + s + "\n\n" }
	body := chunk(`{"id":"c1","choices":[{"index":0,"delta":{"content":"private"},"finish_reason":null}]}`) +
		chunk(`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":null}`) +
		chunk(`{"id":"c1","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":6,"total_tokens":26,"prompt_tokens_details":{"cached_tokens":12},"completion_tokens_details":{"reasoning_tokens":2}}}`) +
		"data: [DONE]\n\n"
	observed := stream(NewChat(), body)
	if len(observed) != 1 || !observed[0].Successful || *observed[0].Usage.Total != 26 || *observed[0].Usage.Cached != 12 || *observed[0].Usage.Reasoning != 2 {
		t.Fatalf("observations: %+v", observed)
	}
	withoutUsage := strings.Replace(body, chunk(`{"id":"c1","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":6,"total_tokens":26,"prompt_tokens_details":{"cached_tokens":12},"completion_tokens_details":{"reasoning_tokens":2}}}`), "", 1)
	if observed := stream(NewChat(), withoutUsage); len(observed) != 1 || observed[0].Usage.Total != nil || !observed[0].Terminal {
		t.Fatalf("missing usage became known: %+v", observed)
	}
	if observed := stream(NewChat(), strings.TrimSuffix(body, "data: [DONE]\n\n")); len(observed) != 1 || *observed[0].Usage.Total != 26 {
		t.Fatalf("finished stream without [DONE]: %+v", observed)
	}
	if observed := stream(NewChat(), chunk(`{"choices":[{"delta":{"content":"x"}}]}`)); len(observed) != 0 {
		t.Fatalf("interrupted stream reported an outcome: %+v", observed)
	}
	if observed := stream(NewChat(), chunk(`{"error":{"code":"1302","message":"private"}}`)); len(observed) != 1 || observed[0].Status != 0 || observed[0].Outcome.Kind != "failed" {
		t.Fatalf("vendor error became a status: %+v", observed)
	}
	if o := NewChat().Unary([]byte(`{"choices":[{"finish_reason":"length"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`)); o.Outcome.Kind != "incomplete" || o.Successful || *o.Usage.Total != 3 {
		t.Fatalf("unary: %+v", o)
	}
	if o := NewChat().Unary([]byte(`{"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":9}}`)); o.Usage.Total != nil {
		t.Fatalf("contradictory usage became known: %+v", o)
	}
}
