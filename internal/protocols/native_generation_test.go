package protocols

import (
	"errors"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func TestNativeGenerationRequestsAreValidatedForTheirRoute(t *testing.T) {
	for _, test := range []struct {
		family wire.Format
		body   string
		valid  bool
	}{
		{wire.MistralFIM, `{"prompt":"def f():","suffix":"\n","max_tokens":16,"stream":true}`, true},
		{wire.MistralFIM, `{"model":"code","prompt":"x"}`, true},
		{wire.MistralFIM, `{"model":"other","prompt":"x"}`, false},
		{wire.MistralFIM, `{"suffix":"x"}`, false},
		{wire.MistralFIM, `{"prompt":"x","max_tokens":-1}`, false},
		{wire.MistralFIM, `{"prompt":"x","stream":"yes"}`, false},
		{wire.CohereChat, `{"messages":[{"role":"user","content":"hi"}],"documents":[{"data":{"text":"d"}}]}`, true},
		{wire.CohereChat, `{"messages":[]}`, false},
		{wire.CohereChat, `{"messages":[{"role":"developer","content":"hi"}]}`, false},
	} {
		request, err := Parse(test.family, []byte(test.body), "code")
		if (err == nil) != test.valid {
			t.Fatalf("%s %s: %v", test.family, test.body, err)
		}
		if err == nil && (request.Route != "code" || request.Family.Surface() != "native" || request.Stream != strings.Contains(test.body, `"stream":true`)) {
			t.Fatalf("%s request = %+v", test.family, request)
		}
	}
	if family, ok := NativeGenerationFamily("mistral-fim"); !ok || family != wire.MistralFIM {
		t.Fatal("mistral-fim is not a native generation dialect")
	}
	if _, ok := NativeGenerationFamily("cohere-embed-v2"); ok {
		t.Fatal("an operation dialect was taken for generation")
	}
}

func TestCohereChatResultsAreMeteredByBilledUnits(t *testing.T) {
	body := `{"id":"c1","finish_reason":"MAX_TOKENS","message":{"role":"assistant","content":[{"type":"thinking","thinking":"..."},{"type":"text","text":"Hel"},{"type":"text","text":"lo"}],"citations":[{"start":0,"end":5,"text":"Hello","sources":[]}]},"usage":{"billed_units":{"input_tokens":5.0,"output_tokens":2},"tokens":{"input_tokens":71,"output_tokens":2}}}`
	c, err := DecodeRequest(wire.CohereChat, wire.CohereChat, []byte(body), "route", "", nil)
	if err != nil || c.OutputText != "Hello" || c.FinishReason != "length" || c.Usage.InputTokens != 5 || c.Usage.OutputTokens != 2 || string(c.Body) != body {
		t.Fatalf("decoded %+v %v", c, err)
	}
	for _, invalid := range []string{
		`{"id":"c1","message":{"role":"assistant"},"usage":{"billed_units":{"input_tokens":1,"output_tokens":1}}}`,
		`{"id":"c1","finish_reason":"COMPLETE","message":{"role":"assistant"},"usage":{"tokens":{"input_tokens":1}}}`,
		`{"id":"c1","finish_reason":"COMPLETE","message":{"role":"assistant"},"usage":{"billed_units":{"input_tokens":1.5,"output_tokens":1}}}`,
	} {
		if _, err := DecodeRequest(wire.CohereChat, wire.CohereChat, []byte(invalid), "route", "", nil); err == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}
}

func cohereStream(events ...string) string {
	var b strings.Builder
	for _, event := range events {
		kind := event[strings.Index(event, `"type":"`)+8:]
		kind = kind[:strings.Index(kind, `"`)]
		b.WriteString("event: " + kind + "\ndata: " + event + "\n\n")
	}
	return b.String()
}

func TestCohereChatStreamsEndWithMessageEnd(t *testing.T) {
	start := `{"delta":{"message":{"role":"assistant"}},"id":"s1","type":"message-start"}`
	delta := `{"delta":{"message":{"content":{"text":"Hi"}}},"index":0,"type":"content-delta"}`
	end := `{"delta":{"finish_reason":"COMPLETE","usage":{"billed_units":{"input_tokens":3,"output_tokens":1},"tokens":{"input_tokens":9,"output_tokens":1}}},"type":"message-end"}`
	var frames []string
	c, err := Stream(wire.CohereChat, wire.CohereChat, strings.NewReader(cohereStream(start, delta, end)), 1<<16, "route", true, func(frame []byte) error {
		frames = append(frames, string(frame))
		return nil
	})
	if err != nil || c.OutputText != "" || c.Usage.InputTokens != 3 || c.FinishReason != "stop" || c.UpstreamID != "s1" || len(frames) != 3 || !strings.Contains(frames[1], `"text":"Hi"`) || !strings.Contains(frames[2], "message-end") {
		t.Fatalf("stream = %+v %v %q", c, err, frames)
	}
	_, err = Stream(wire.CohereChat, wire.CohereChat, strings.NewReader(cohereStream(start, delta)), 1<<16, "route", true, func([]byte) error { return nil })
	if truncated, ok := errors.AsType[*wire.ProtocolError](err); !ok || !truncated.Truncated {
		t.Fatalf("a stream without message-end ended as %v", err)
	}
	failed := `{"delta":{"error":"internal error","finish_reason":"ERROR"},"type":"message-end"}`
	if _, err = Stream(wire.CohereChat, wire.CohereChat, strings.NewReader(cohereStream(start, failed)), 1<<16, "route", true, func([]byte) error { return nil }); err == nil {
		t.Fatal("a stream that ended in error completed")
	}
	mismatched := "event: content-end\ndata: " + delta + "\n\n"
	if _, err = Stream(wire.CohereChat, wire.CohereChat, strings.NewReader(mismatched), 1<<16, "route", true, func([]byte) error { return nil }); err == nil {
		t.Fatal("an event whose name disagrees with its type was accepted")
	}
	if _, err = Stream(wire.CohereChat, wire.Chat, strings.NewReader(cohereStream(start, delta, end)), 1<<16, "route", true, func([]byte) error { return nil }); err == nil {
		t.Fatal("a Cohere stream was translated")
	}
}

func TestCohereGatewayStreamsRetainOnlyMetadata(t *testing.T) {
	start := `{"id":"s1","type":"message-start"}`
	text := strings.Repeat("a", 400)
	delta := `{"delta":{"message":{"content":{"text":"` + text + `"}}},"type":"content-delta"}`
	end := `{"delta":{"finish_reason":"COMPLETE","usage":{"billed_units":{"input_tokens":3,"output_tokens":100}}},"type":"message-end"}`
	stream := cohereStream(start) + strings.Repeat(cohereStream(delta), 100) + cohereStream(end)
	forwarded := 0
	c, err := Stream(wire.CohereChat, wire.CohereChat, strings.NewReader(stream), 1024, "route", true, func(frame []byte) error {
		forwarded += strings.Count(string(frame), text)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if forwarded != 100 || c.OutputText != "" || len(c.Body) != 0 || len(c.ToolCalls) != 0 || c.Usage.OutputTokens != 100 || c.FinishReason != "stop" || c.UpstreamID != "s1" {
		t.Fatalf("forwarded %d deltas, retained %+v", forwarded, c)
	}
}
