package protocols

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/sse"
)

// envelopeWire exercises the fields D03 preserves: sticky event ids (including
// the empty reset), retry presence including zero, an admitted event name, and
// an independently authored native member riding on an empty-choices event.
// The pure usage-only event precedes every id: line so its envelope carries no
// observation and it remains suppressible.
const envelopeWire = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"provider-model","system_fingerprint":"fp","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}

id: first
retry: 0
data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"provider-model","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}],"prompt_filter_results":[{"content_filter_results":{"hate":{"filtered":false}}}]}

event: annotation
id: 
data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"provider-model","choices":[],"prompt_filter_results":[{"content_filter_results":{"hate":{"filtered":false}}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"provider-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

id: terminal
data: [DONE]

`

func TestNativeChatStreamPreservesEnvelopes(t *testing.T) {
	for _, includeUsage := range []bool{false, true} {
		var out bytes.Buffer
		var observed []oif.Event
		c, err := StreamWithEvents(openai.FamilyChat, openai.FamilyChat, strings.NewReader(envelopeWire), 8192, "team-model", includeUsage, func(frame []byte) error {
			out.Write(frame)
			return nil
		}, func(e oif.Event) error {
			observed = append(observed, e)
			return nil
		})
		if err != nil {
			t.Fatalf("includeUsage=%t: %v", includeUsage, err)
		}
		if c.Usage == nil || c.Usage.TotalTokens != 5 || c.FinishReason != "stop" {
			t.Fatalf("includeUsage=%t: completion lost usage or finish: %+v", includeUsage, c)
		}
		// Observation happens before client presentation: all five admitted
		// events reach the observer even when one is suppressed downstream.
		if len(observed) != 5 {
			t.Fatalf("includeUsage=%t: observer saw %d events, want 5", includeUsage, len(observed))
		}
		if observed[4].Framing().ID != "terminal" || observed[0].Name() != "" || observed[2].Name() != "annotation" {
			t.Fatalf("includeUsage=%t: observed framing lost: %+v", includeUsage, observed)
		}
		var frames []sse.Frame
		if err := sse.Decode(bytes.NewReader(out.Bytes()), 8192, func(f sse.Frame) error {
			frames = append(frames, f)
			return nil
		}); err != nil {
			t.Fatalf("includeUsage=%t: emitted stream invalid: %v\n%s", includeUsage, err, out.String())
		}
		// content, annotation residual, finish, [DONE]; +1 for the pure usage
		// event when usage was negotiated.
		wantFrames := 4
		if includeUsage {
			wantFrames = 5
		}
		if len(frames) != wantFrames {
			t.Fatalf("includeUsage=%t: emitted %d events, want %d:\n%s", includeUsage, len(frames), wantFrames, out.String())
		}
		if includeUsage && !strings.Contains(frames[0].Data, `"total_tokens":5`) {
			t.Fatalf("negotiated usage event suppressed: %s", frames[0].Data)
		}
		content := frames[wantFrames-4]
		if content.ID == nil || *content.ID != "first" || content.RetryMS == nil || *content.RetryMS != 0 {
			t.Fatalf("includeUsage=%t: first envelope lost: %+v", includeUsage, content)
		}
		if !strings.Contains(content.Data, "prompt_filter_results") || !strings.Contains(content.Data, `"model":"team-model"`) || strings.Contains(content.Data, "provider-model") {
			t.Fatalf("includeUsage=%t: native field or model overlay lost: %s", includeUsage, content.Data)
		}
		residual := frames[wantFrames-3]
		if residual.Event == nil || *residual.Event != "annotation" || residual.ID == nil || *residual.ID != "" {
			t.Fatalf("includeUsage=%t: annotation envelope lost: %+v", includeUsage, residual)
		}
		if !strings.Contains(residual.Data, "prompt_filter_results") || strings.Contains(residual.Data, `"usage"`) != includeUsage {
			t.Fatalf("includeUsage=%t: residual presentation wrong: %s", includeUsage, residual.Data)
		}
		terminal := frames[wantFrames-1]
		if terminal.Data != "[DONE]" || terminal.ID == nil || *terminal.ID != "terminal" {
			t.Fatalf("includeUsage=%t: terminal framing lost: %+v", includeUsage, terminal)
		}
		if !includeUsage && strings.Contains(out.String(), `"usage"`) {
			t.Fatalf("usage leaked to a client that did not negotiate it: %s", out.String())
		}
	}
}

func TestTranslatedChatStreamKeepsFieldScopedUsage(t *testing.T) {
	// Translation never carries SSE envelopes, but the empty-choices event
	// must still reach the translator, keep metering, and not corrupt the
	// destination stream. Anthropic has no usage opt-out: provider usage is
	// part of the destination contract and lands in message_delta.
	var out bytes.Buffer
	c, err := Stream(openai.FamilyChat, openai.FamilyAnthropic, strings.NewReader(envelopeWire), 8192, "team-model", false, func(frame []byte) error {
		out.Write(frame)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Usage == nil || c.Usage.TotalTokens != 5 || c.FinishReason != "stop" {
		t.Fatalf("translated completion lost usage or finish: %+v", c)
	}
	var events []string
	var messageDelta string
	if err := sse.Decode(bytes.NewReader(out.Bytes()), 8192, func(f sse.Frame) error {
		if f.Event != nil {
			events = append(events, *f.Event)
			if *f.Event == "message_delta" {
				messageDelta = f.Data
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("translated stream invalid: %v\n%s", err, out.String())
	}
	if len(events) == 0 || events[len(events)-1] != "message_stop" || events[0] != "message_start" {
		t.Fatalf("translated terminal sequence broken: %v", events)
	}
	if !strings.Contains(messageDelta, `"output_tokens":2`) || !strings.Contains(messageDelta, `"input_tokens":3`) {
		t.Fatalf("provider usage lost in translation: %s", messageDelta)
	}
	// The translated output must itself remain a streamable Anthropic stream.
	if _, err := Stream(openai.FamilyAnthropic, openai.FamilyAnthropic, bytes.NewReader(out.Bytes()), 8192, "team-model", true, func([]byte) error { return nil }); err != nil {
		t.Fatalf("translated stream does not round-trip: %v\n%s", err, out.String())
	}
}
