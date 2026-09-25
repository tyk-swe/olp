package interaction

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// The traces below are authored from the documented Anthropic Messages
// streaming grammar; the negotiated path must accept the same legal traces the
// native codec accepts and reject the same invalid neighbors.
const (
	toolStartWire = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"wire-model","content":[],"usage":{"input_tokens":3,"output_tokens":1}}}

`
	toolBlockWire = `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call-weather","name":"weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"Paris\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

`
	toolStopWire = `event: message_stop
data: {"type":"message_stop"}

`
)

func toolWireEvent(name, data string) string {
	return "event: " + name + "\ndata: " + data + "\n\n"
}

func TestNegotiatedToolAdmitsLegalMultiUpdateTrace(t *testing.T) {
	wire := toolStartWire + toolBlockWire +
		// Usage updates may arrive before the final reason; an omitted
		// field keeps its cumulative value.
		toolWireEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":null,"stop_sequence":null},"usage":{"output_tokens":8,"cache_read_input_tokens":2}}`) +
		toolWireEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null}}`) +
		// A consistent update after the initial reason remains legal.
		toolWireEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":10}}`) +
		toolStopWire
	plan := bind(t, toolsTemplate(t), request(t, openai.FamilyChat, toolSource), toolContext())
	projection, err := plan.NewToolProjection(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	var early [][]byte
	completion, err := protocols.StreamWithEvents(openai.FamilyAnthropic, openai.FamilyAnthropic, strings.NewReader(wire), 1<<20, "route", true, func([]byte) error { return nil }, func(event oif.Event) error {
		frames, e := projection.Observe(event)
		early = append(early, frames...)
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, frame := range early {
		if bytes.Contains(frame, []byte("tool_calls")) {
			t.Fatal("actionable tool output escaped the barrier")
		}
	}
	state, delivery, err := projection.Complete(completion, "continuation_updates")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(state.Blocks, []byte(`"input":{"city":"Paris"}`)) {
		t.Fatalf("native tool arguments lost: %s", state.Blocks)
	}
	terminal, err := oif.ParseJSON(delivery.Frames[len(delivery.Frames)-1], oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if finish, _ := terminal.Lookup("/choices/0/finish_reason"); finish.Kind() != oif.String || finish.Raw() != `"tool_calls"` {
		t.Fatalf("terminal finish was not tool_calls: %s", terminal.Bytes())
	}
	nativeUsage, present := terminal.Lookup("/olp/native_usage")
	if !present {
		t.Fatal("native usage missing from ready delivery")
	}
	var decoded map[string]json.RawMessage
	if json.Unmarshal(nativeUsage.Bytes(), &decoded) != nil {
		t.Fatal(err)
	}
	if string(decoded["output_tokens"]) != "10" || string(decoded["input_tokens"]) != "3" || string(decoded["cache_read_input_tokens"]) != "2" {
		t.Fatalf("cumulative native usage lost or rewound: %s", nativeUsage.Bytes())
	}
	if completion.Usage == nil || completion.Usage.OutputTokens != 10 || completion.Usage.InputTokens != 5 {
		t.Fatalf("normalized usage lost native categories: %+v", completion.Usage)
	}
}

func TestNegotiatedToolRejectsInvalidTraceNeighbors(t *testing.T) {
	base := toolStartWire + toolBlockWire
	for _, tc := range []struct {
		name, wire string
	}{
		{"duplicate message_start", toolStartWire + toolStartWire},
		{"event name disagrees with type", base + `event: message_stop
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}

`},
		{"delta at invalid block position", base + toolWireEvent("content_block_delta", `{"type":"content_block_delta","index":7,"delta":{"type":"text_delta","text":"x"}}`)},
		{"message_delta while a block is open", toolStartWire +
			toolWireEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) +
			toolWireEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`)},
		{"contradictory terminal facts", base +
			toolWireEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`) +
			toolWireEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"max_tokens"}}`) +
			toolStopWire},
		{"unsupported terminal", base + toolWireEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"provider_invented"}}`) + toolStopWire},
		{"malformed usage count", base + toolWireEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":"many"}}`) + toolStopWire},
		{"EOF without native terminal", base + toolWireEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":4}}`)},
		{"incomplete tool arguments", toolStartWire +
			toolWireEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call-weather","name":"weather","input":{}}}`) +
			toolWireEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{bad"}}`) +
			toolWireEvent("content_block_stop", `{"type":"content_block_stop","index":0}`) +
			toolWireEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`) +
			toolStopWire},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := bind(t, toolsTemplate(t), request(t, openai.FamilyChat, toolSource), toolContext())
			projection, err := plan.NewToolProjection(1 << 20)
			if err != nil {
				t.Fatal(err)
			}
			completion, streamErr := protocols.StreamWithEvents(openai.FamilyAnthropic, openai.FamilyAnthropic, strings.NewReader(tc.wire), 1<<20, "route", true, func([]byte) error { return nil }, func(event oif.Event) error {
				frames, e := projection.Observe(event)
				for _, frame := range frames {
					if bytes.Contains(frame, []byte("tool_calls")) {
						t.Fatal("malformed event produced actionable output")
					}
				}
				return e
			})
			_, _, completeErr := projection.Complete(completion, "continuation_rejected")
			if streamErr == nil || completeErr == nil {
				t.Fatalf("invalid trace accepted: stream=%v complete=%v", streamErr, completeErr)
			}
		})
	}
}
