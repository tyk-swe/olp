package openai

import (
	"errors"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/sse"
)

// decodedFrame is an independently re-parsed observation of an emitted frame.
type decodedFrame struct {
	event, data string
	id          *string
	retryMS     *uint64
}

func decodeEmitted(t *testing.T, frames [][]byte) []decodedFrame {
	t.Helper()
	out := []decodedFrame{}
	for _, frame := range frames {
		err := sse.Decode(strings.NewReader(string(frame)), 1<<20, func(f sse.Frame) error {
			out = append(out, decodedFrame{data: f.Data, id: f.ID, retryMS: f.RetryMS})
			if f.Event != nil {
				out[len(out)-1].event = *f.Event
			}
			return nil
		})
		if err != nil {
			t.Fatalf("emitted frame is not valid SSE: %q: %v", frame, err)
		}
	}
	return out
}

func TestChatStreamPreservesEventEnvelope(t *testing.T) {
	chunk := func(id string) string {
		return `{"id":"` + id + `","object":"chat.completion.chunk","created":1,"model":"gpt-upstream","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`
	}
	wire := "id: chunk-1\nretry: 0\ndata: " + chunk("c1") + "\n\n" +
		"event: annotation\nid: \ndata: " + chunk("c2") + "\n\n" +
		"retry: 2500\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"id: last\nretry: 750\ndata: [DONE]\n\n"
	var frames [][]byte
	completion, err := Stream(FamilyChat, strings.NewReader(wire), 1<<20, "team-chat", true, collect(&frames))
	if err != nil {
		t.Fatal(err)
	}
	if completion.FinishReason != "stop" {
		t.Fatalf("completion %+v", completion)
	}
	got := decodeEmitted(t, frames)
	if len(got) != 4 {
		t.Fatalf("emitted %d events, want 4: %q", len(got), frames)
	}
	// Presence and value are meaningful: retry: 0 is a real hint and an empty
	// id resets the client event buffer.
	if got[0].id == nil || *got[0].id != "chunk-1" || got[0].retryMS == nil || *got[0].retryMS != 0 || got[0].event != "" {
		t.Fatalf("first envelope lost: %+v", got[0])
	}
	if got[1].id == nil || *got[1].id != "" || got[1].event != "annotation" || got[1].retryMS != nil {
		t.Fatalf("empty id reset or event name lost: %+v", got[1])
	}
	// Event ids are sticky state in SSE: after the empty reset, later events
	// keep the reset buffer and re-emitting it is the faithful observation.
	if got[2].retryMS == nil || *got[2].retryMS != 2500 || got[2].id == nil || *got[2].id != "" {
		t.Fatalf("retry value or reset id lost: %+v", got[2])
	}
	if got[3].data != "[DONE]" || got[3].id == nil || *got[3].id != "last" || got[3].retryMS == nil || *got[3].retryMS != 750 {
		t.Fatalf("terminal framing lost: %+v", got[3])
	}
	// Payloads keep their source members and arrive in wire order; only the
	// published model binding is overlaid.
	if got[0].data != `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"team-chat","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}` {
		t.Fatalf("payload rewritten: %s", got[0].data)
	}
	if !strings.HasPrefix(got[1].data, `{"id":"c2",`) || !strings.Contains(got[2].data, `"finish_reason":"stop"`) {
		t.Fatalf("payload order changed: %q / %q", got[1].data, got[2].data)
	}
}

func TestChatStreamUsageFilteringIsFieldScoped(t *testing.T) {
	finish := `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-upstream","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	usageOnly := `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-upstream","system_fingerprint":"fp","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
	annotatedUsage := `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-upstream","choices":[],"prompt_filter_results":[{"content_filter_results":{"hate":{"filtered":false}}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
	for _, includeUsage := range []bool{false, true} {
		var frames [][]byte
		wire := "data: " + finish + "\n\n" +
			"data: " + usageOnly + "\n\n" +
			"id: observed\ndata: " + annotatedUsage + "\n\n" +
			"data: [DONE]\n\n"
		completion, err := Stream(FamilyChat, strings.NewReader(wire), 1<<20, "team-chat", includeUsage, collect(&frames))
		if err != nil {
			t.Fatalf("includeUsage=%t: %v", includeUsage, err)
		}
		if completion.Usage == nil || completion.Usage.TotalTokens != 5 {
			t.Fatalf("includeUsage=%t: metering lost usage: %+v", includeUsage, completion.Usage)
		}
		got := decodeEmitted(t, frames)
		// finish, optional usage-only, annotated residual, [DONE]
		wantFrames := 3
		if includeUsage {
			wantFrames = 4
		}
		if len(got) != wantFrames || got[len(got)-1].data != "[DONE]" {
			t.Fatalf("includeUsage=%t: emitted events %q", includeUsage, frames)
		}
		residual := got[wantFrames-2]
		if !strings.Contains(residual.data, "prompt_filter_results") {
			t.Fatalf("includeUsage=%t: additional native field dropped: %q", includeUsage, residual.data)
		}
		if residual.id == nil || *residual.id != "observed" {
			t.Fatalf("includeUsage=%t: residual envelope dropped: %+v", includeUsage, residual)
		}
		if strings.Contains(residual.data, `"usage"`) != includeUsage {
			t.Fatalf("includeUsage=%t: usage presentation wrong: %q", includeUsage, residual.data)
		}
		if includeUsage && !strings.Contains(got[1].data, `"total_tokens":5`) {
			t.Fatalf("negotiated usage event suppressed: %q", got[1].data)
		}
	}
}

func TestChatStreamSuppressesOnlyPureUsageEvents(t *testing.T) {
	finish := "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
	done := "data: [DONE]\n\n"
	cases := map[string]struct {
		event      string
		emitted    bool
		usageTotal int64 // 0 expects no observed usage (e.g. usage:null)
	}{
		"envelope only":       {"data: " + `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","system_fingerprint":"fp","service_tier":"default","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n", false, 2},
		"extra member":        {"data: " + `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2},"vendor_extra":{"opaque":1e400}}` + "\n\n", true, 2},
		"usage null envelope": {"data: " + `{"id":"c","choices":[],"usage":null}` + "\n\n", false, 0},
		"envelope id present": {"id: 99\nretry: 0\ndata: " + `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n", true, 2},
		"nonempty choices":    {"data: " + `{"choices":[{"index":0,"delta":{"content":"kept"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n", true, 2},
	}
	for name, c := range cases {
		// The event precedes the finish chunk so content deltas stay admitted.
		wire := c.event + finish + done
		var frames [][]byte
		completion, err := Stream(FamilyChat, strings.NewReader(wire), 1<<20, "r", false, collect(&frames))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.usageTotal == 0 && completion.Usage != nil || c.usageTotal > 0 && (completion.Usage == nil || completion.Usage.TotalTokens != c.usageTotal) {
			t.Fatalf("%s: metering lost usage: %+v", name, completion.Usage)
		}
		got := decodeEmitted(t, frames)
		if got[len(got)-1].data != "[DONE]" {
			t.Fatalf("%s: missing terminal event: %q", name, frames)
		}
		if emitted := len(got) == 3; emitted != c.emitted {
			t.Fatalf("%s: emitted=%v want %v: %q", name, emitted, c.emitted, frames)
		}
		if c.emitted {
			if strings.Contains(got[0].data, `"usage"`) {
				t.Fatalf("%s: negotiated usage leaked: %q", name, got[0].data)
			}
			if name == "extra member" && !strings.Contains(got[0].data, `"vendor_extra":{"opaque":1e400}`) {
				t.Fatalf("%s: native field lost: %q", name, got[0].data)
			}
			if name == "envelope id present" && (got[0].id == nil || *got[0].id != "99" || got[0].retryMS == nil || *got[0].retryMS != 0) {
				t.Fatalf("%s: envelope lost: %+v", name, got[0])
			}
		}
	}
	// Usage without a choices array is an admission failure before
	// presentation applies, not a suppressible event.
	var pe *ProtocolError
	if _, err := Stream(FamilyChat, strings.NewReader("data: {\"usage\":{\"prompt_tokens\":1}}\n\n"+done), 1<<20, "r", false, collect(new([][]byte))); !errors.As(err, &pe) {
		t.Fatalf("choices-less usage event admitted: %v", err)
	}
}

func TestChatStreamTerminalErrorKeepsEarlierEnvelopes(t *testing.T) {
	wire := "id: first\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n" +
		"event: failure\nid: doomed\ndata: {\"error\":{\"message\":\"quota exhausted\",\"type\":\"insufficient_quota\",\"code\":\"insufficient_quota\"}}\n\n"
	var frames [][]byte
	_, err := Stream(FamilyChat, strings.NewReader(wire), 1<<20, "r", true, collect(&frames))
	var ue *UpstreamError
	if !errors.As(err, &ue) || ue.Code != "insufficient_quota" {
		t.Fatalf("error event: %v", err)
	}
	got := decodeEmitted(t, frames)
	if len(got) != 1 || got[0].id == nil || *got[0].id != "first" || !strings.Contains(got[0].data, `"partial"`) {
		t.Fatalf("emitted prefix changed: %q", frames)
	}
}
