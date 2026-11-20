package guardstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/guardrails"
)

func keywordEngine(t *testing.T, action guardrails.Action) *guardrails.Engine {
	t.Helper()
	revision := guardrails.Revision{ID: "revision", GuardrailID: "guard", ProjectID: "project", Type: "builtin.keywords", Configuration: json.RawMessage(`{"words":["forbidden"]}`), TimeoutMS: 1000, OnError: guardrails.FailClosed}
	engine, err := guardrails.Compile([]guardrails.Entry{{GuardrailID: revision.GuardrailID, RevisionID: revision.ID, Phase: guardrails.OutputStream, Action: action}}, map[string]guardrails.Revision{revision.ID: revision})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func sse(data string) []byte { return []byte("data: " + data + "\n\n") }

func bedrock(t *testing.T, event, payload string) []byte {
	t.Helper()
	var out bytes.Buffer
	message := eventstream.Message{Headers: eventstream.Headers{{Name: ":event-type", Value: eventstream.StringValue(event)}}, Payload: []byte(payload)}
	if err := eventstream.NewEncoder().Encode(&out, message); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

type exchange struct {
	family wire.Format
	frames [][]byte
	held   int
	copies int
}

func textExchanges() []exchange {
	return []exchange{
		{family: wire.Chat, held: 2, copies: 1, frames: [][]byte{
			sse(`{"choices":[{"index":0,"delta":{"content":"it is for"}}]}`),
			sse(`{"choices":[{"index":0,"delta":{"content":"bidden here"}}]}`),
			sse(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		}},
		{family: wire.Completions, held: 2, copies: 1, frames: [][]byte{
			sse(`{"choices":[{"index":0,"text":"it is for"}]}`),
			sse(`{"choices":[{"index":0,"text":"bidden here"}]}`),
			sse(`{"choices":[{"index":0,"text":"","finish_reason":"stop"}]}`),
		}},
		{family: wire.Anthropic, held: 0, copies: 1, frames: [][]byte{
			sse(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			sse(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"it is for"}}`),
			sse(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"bidden here"}}`),
			sse(`{"type":"content_block_stop","index":0}`),
		}},
		{family: wire.Responses, held: 2, copies: 3, frames: [][]byte{
			sse(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"it is for"}`),
			sse(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"bidden here"}`),
			sse(`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"it is forbidden here"}`),
			sse(`{"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"it is forbidden here"}]}]}}`),
		}},
		{family: wire.Gemini, held: 1, copies: 1, frames: [][]byte{
			sse(`{"candidates":[{"content":{"parts":[{"text":"it is for"}]}}]}`),
			sse(`{"candidates":[{"content":{"parts":[{"text":"bidden here"}]},"finishReason":"STOP"}]}`),
		}},
	}
}

func toolExchanges(t *testing.T) []exchange {
	return []exchange{
		{family: wire.Chat, held: 2, copies: 1, frames: [][]byte{
			sse(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"lookup","arguments":"{\"q\":\"for"}}]}}]}`),
			sse(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"bidden\"}"}}]}}]}`),
			sse(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
		}},
		{family: wire.Anthropic, held: 3, copies: 1, frames: [][]byte{
			sse(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"lookup","input":{}}}`),
			sse(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"for"}}`),
			sse(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"bidden\"}"}}`),
			sse(`{"type":"content_block_stop","index":0}`),
		}},
		{family: wire.Responses, held: 3, copies: 4, frames: [][]byte{
			sse(`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","name":"lookup","arguments":""}}`),
			sse(`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"q\":\"for"}`),
			sse(`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"bidden\"}"}`),
			sse(`{"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":0,"arguments":"{\"q\":\"forbidden\"}"}`),
			sse(`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","name":"lookup","arguments":"{\"q\":\"forbidden\"}"}}`),
			sse(`{"type":"response.completed","response":{"output":[{"id":"fc_1","type":"function_call","name":"lookup","arguments":"{\"q\":\"forbidden\"}"}]}}`),
		}},
		{family: wire.Gemini, held: 0, copies: 1, frames: [][]byte{
			sse(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"q":"forbidden"}}}]}}]}`),
		}},
		{family: wire.Bedrock, held: 3, copies: 1, frames: [][]byte{
			bedrock(t, "contentBlockStart", `{"contentBlockIndex":0,"start":{"toolUse":{"name":"lookup","toolUseId":"t1"}}}`),
			bedrock(t, "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"toolUse":{"input":"{\"q\":\"for"}}}`),
			bedrock(t, "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"toolUse":{"input":"bidden\"}"}}}`),
			bedrock(t, "contentBlockStop", `{"contentBlockIndex":0}`),
		}},
	}
}

type run struct {
	emitted  [][][]byte
	recorded []guardrails.Evidence
	err      error
}

func (r run) all() []byte {
	return bytes.Join(slices.Concat(r.emitted...), nil)
}

func play(t *testing.T, action guardrails.Action, strict bool, e exchange) run {
	t.Helper()
	result := run{}
	stream, err := New(Config{
		Engine: keywordEngine(t, action), Family: e.family, Window: 1 << 16, Strict: strict, ToolContext: t.Context(),
		Record: func(decisions []guardrails.Evidence) { result.recorded = append(result.recorded, decisions...) },
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, frame := range e.frames {
		frames, err := stream.Push(t.Context(), frame)
		if err != nil {
			result.err = err
			return result
		}
		result.emitted = append(result.emitted, frames)
	}
	frames, err := stream.Finish(t.Context())
	result.emitted, result.err = append(result.emitted, frames), err
	return result
}

func checkRedacted(t *testing.T, e exchange, redacted string) {
	t.Helper()
	got := play(t, guardrails.Redact, false, e)
	if got.err != nil {
		t.Fatal(got.err)
	}
	for i := range e.held {
		if len(got.emitted[i]) != 0 {
			t.Fatalf("frame %d escaped before its content was complete: %q", i, got.emitted[i])
		}
	}
	all := got.all()
	if bytes.Contains(all, []byte("bidden")) {
		t.Fatalf("the keyword reached the client: %q", all)
	}
	if copies := bytes.Count(all, []byte(redacted)); copies != e.copies {
		t.Fatalf("the redacted content appears %d times, want %d: %q", copies, e.copies, all)
	}
	if frames := len(slices.Concat(got.emitted...)); frames != len(e.frames) {
		t.Fatalf("%d frames in, %d frames out: %q", len(e.frames), frames, all)
	}
}

func TestTextSplitAcrossFramesIsRedactedOnceInEveryFamily(t *testing.T) {
	for _, e := range textExchanges() {
		t.Run(string(e.family), func(t *testing.T) { checkRedacted(t, e, "it is [REDACTED] here") })
	}
}

func TestToolArgumentsAreHeldUntilCompleteThenRedactedInEveryFamily(t *testing.T) {
	for _, e := range toolExchanges(t) {
		t.Run(string(e.family), func(t *testing.T) {
			redacted := `{\"q\":\"[REDACTED]\"}`
			if e.family == wire.Gemini {
				redacted = `{"q":"[REDACTED]"}`
			}
			checkRedacted(t, e, redacted)
		})
	}
}

func checkBlocked(t *testing.T, e exchange) run {
	t.Helper()
	got := play(t, guardrails.Block, false, e)
	if got.err == nil {
		t.Fatalf("blocked content passed: %q", got.all())
	}
	if all := got.all(); bytes.Contains(all, []byte("for")) || bytes.Contains(all, []byte("lookup")) {
		t.Fatalf("blocked content reached the client: %q", all)
	}
	return got
}

func TestBlockedTextNeverReachesTheClient(t *testing.T) {
	for _, e := range textExchanges() {
		t.Run(string(e.family), func(t *testing.T) { checkBlocked(t, e) })
	}
}

func TestBlockedToolCallNeverReachesTheClientAndIsRecorded(t *testing.T) {
	for _, e := range toolExchanges(t) {
		t.Run(string(e.family), func(t *testing.T) {
			if got := checkBlocked(t, e); len(got.recorded) == 0 {
				t.Fatal("the blocking decision was not recorded")
			}
		})
	}
}

func TestStrictRoutesRefuseARewriteInsteadOfChangingOrLeakingFrames(t *testing.T) {
	for _, e := range slices.Concat(textExchanges(), toolExchanges(t)) {
		t.Run(string(e.family), func(t *testing.T) {
			got := play(t, guardrails.Redact, true, e)
			if !errors.Is(got.err, guardrails.ErrSurface) {
				t.Fatalf("error = %v, output %q", got.err, got.all())
			}
			if all := got.all(); bytes.Contains(all, []byte("bidden")) || bytes.Contains(all, []byte("[REDACTED]")) {
				t.Fatalf("a strict route changed or leaked content: %q", all)
			}
		})
	}
}

func TestBedrockTextThatMustChangeIsRefused(t *testing.T) {
	got := play(t, guardrails.Redact, false, exchange{family: wire.Bedrock, frames: [][]byte{
		bedrock(t, "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"it is for"}}`),
		bedrock(t, "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"bidden here"}}`),
		bedrock(t, "contentBlockStop", `{"contentBlockIndex":0}`),
	}})
	if !errors.Is(got.err, guardrails.ErrSurface) || len(got.all()) != 0 {
		t.Fatalf("error = %v, output %q", got.err, got.all())
	}
}

func TestLogprobsAreRefusedBecauseTheyRepeatUninspectedText(t *testing.T) {
	for family, frame := range map[wire.Format]string{
		wire.Chat:        `{"choices":[{"index":0,"delta":{"content":"fine"},"logprobs":{"content":[{"token":"fine"}]}}]}`,
		wire.Completions: `{"choices":[{"index":0,"text":"fine","logprobs":{"tokens":["fine"]}}]}`,
	} {
		got := play(t, guardrails.Redact, false, exchange{family: family, frames: [][]byte{sse(frame)}})
		if !errors.Is(got.err, guardrails.ErrSurface) || len(got.all()) != 0 {
			t.Fatalf("%s: error = %v, output %q", family, got.err, got.all())
		}
	}
}
