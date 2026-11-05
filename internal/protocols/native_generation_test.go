package protocols

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
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
	if family, ok := NativeGenerationFamily("cohere-chat-v2"); !ok || family != wire.CohereChat {
		t.Fatal("cohere-chat-v2 is not a native generation dialect")
	}
	if _, ok := NativeGenerationFamily("cohere-embed-v2"); ok {
		t.Fatal("an operation dialect was taken for generation")
	}
}

type nativeStreamReader func([]byte) (int, error)

func (read nativeStreamReader) Read(buffer []byte) (int, error) { return read(buffer) }

func TestCohereStreamFinishesAtDeliveredMessageEnd(t *testing.T) {
	const source = `data: {"type":"message-start","id":"cohere-stream"}` + "\n\n" + `data: {"type":"message-end","delta":{"finish_reason":"COMPLETE","usage":{"billed_units":{"input_tokens":3,"output_tokens":2}}}}` + "\n\n"
	for _, trailing := range []error{io.EOF, io.ErrUnexpectedEOF, io.ErrClosedPipe} {
		t.Run(trailing.Error(), func(t *testing.T) {
			var emitted bytes.Buffer
			reader := io.MultiReader(strings.NewReader(source), nativeStreamReader(func([]byte) (int, error) {
				t.Error("read after the native terminal")
				return 0, trailing
			}))
			completion, err := Stream(wire.CohereChat, wire.CohereChat, reader, 8192, "route", true, func(frame []byte) error {
				_, err := emitted.Write(frame)
				return err
			})
			if err != nil || completion.UpstreamID != "cohere-stream" || completion.FinishReason != "stop" || !completion.UsageComplete || completion.Usage == nil || completion.Usage.TotalTokens != 5 {
				t.Fatalf("delivered terminal invalidated by trailing reader: completion=%+v error=%v", completion, err)
			}
			if !strings.Contains(emitted.String(), `"type":"message-end"`) {
				t.Fatalf("terminal was not delivered: %s", emitted.String())
			}
		})
	}
	for _, observerFails := range []bool{false, true} {
		_, err := StreamWithEvents(wire.CohereChat, wire.CohereChat, strings.NewReader(source), 8192, "route", true, func(frame []byte) error {
			if !observerFails && bytes.Contains(frame, []byte(`"type":"message-end"`)) {
				return io.ErrClosedPipe
			}
			return nil
		}, func(event oif.Event) error {
			if observerFails && str(event.Source().Fields()["type"]) == "message-end" {
				return io.ErrClosedPipe
			}
			return nil
		})
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("terminal callback failure was lost: observer=%t error=%v", observerFails, err)
		}
	}
	_, err := Stream(wire.CohereChat, wire.CohereChat, strings.NewReader(`data: {"type":"message-start","id":"cohere-stream"}`+"\n\n"), 8192, "route", true, func([]byte) error { return nil })
	var protocolErr *wire.ProtocolError
	if !errors.As(err, &protocolErr) || !protocolErr.Truncated {
		t.Fatalf("missing terminal was accepted: %v", err)
	}
	invalid := strings.Replace(source, `"output_tokens":2`, `"output_tokens":-1`, 1)
	_, err = Stream(wire.CohereChat, wire.CohereChat, strings.NewReader(invalid), 8192, "route", true, func([]byte) error { return nil })
	if !errors.As(err, &protocolErr) {
		t.Fatalf("invalid terminal was accepted: %v", err)
	}
}
