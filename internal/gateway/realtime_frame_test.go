package gateway

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/oif"
)

// The prior strict observer validated with OIF and then decoded a Go struct.
// These cases pin its terminal/usage projection while the wire still forwards
// the original bytes, including on malformed or ambiguous native events.
func TestStrictRealtimeDecoderMatchesDuplicateSafeProjection(t *testing.T) {
	for _, frame := range []string{
		`{"type":"response.audio.delta","response_id":"resp_1","delta":"AQID","output_index":0}`,
		`{"type":"response.create"}`,
		`{"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
		`{"type":"response.done","response":{"id":"resp_1","usage":{"input_tokens":3,"output_tokens":2}}}`,
		`{"type":"response.done","response":{"id":"resp_1","usage":{"input_tokens":3,"output_tokens":2,"input_token_details":{"cached_tokens":1}}}}`,
		`{"type":"response.done","response":{"id":null,"usage":{"input_tokens":null,"output_tokens":0,"input_token_details":{"cached_tokens":null}}}}`,
		`{"type":"response.done","response_id":"resp_1","response":null}`,
		`{"Type":"response.done","Response_ID":"resp_1"}`,
		`{"type":"response.audio.delta","TYPE":"response.done","response_id":"resp_1"}`,
		`{"\u0074ype":"response.done","response":{"id":"resp_1"}}`,
		`{"type":"response.done","response":{"id":"resp_1","usage":{"input_tokens":3,"output_tokens":2},"unknown":{"nested":[true,null,0]}}}`,
		`{"type":"response.done","response":{"id":"resp_1","usage":{"input_tokens":-0,"output_tokens":2}}}`,
		`{"type":"response.done","response":{"id":"resp_1","usage":{"input_tokens":9223372036854775808,"output_tokens":2}}}`,
		`{"type":"response.done","response":{"id":"resp_1","usage":{"input_tokens":3.0,"output_tokens":2}}}`,
		`{"type":"response.done","response":{"id":"resp_1","usage":{"input_tokens":3,"output_tokens":2,"input_token_details":{"cached_tokens":1e0}}}}`,
		`{"type":"response.done","response":5}`,
		`{"type":true,"response_id":"resp_1"}`,
		`{"type":"response.done","response_id":5}`,
		`{"type":"response.audio.delta","type":"response.done"}`,
		`{"type":"response.done","response":{"id":"resp_1","usage":{"input_tokens":3,"input_tokens":4,"output_tokens":2}}}`,
		`{"type":"response.done","response":{"id":"resp_1","native":"\uD800"}}`,
		`{"type":"response.done","response":{"id":"\uD83D\uDE00"}}`,
	} {
		data := []byte(frame)
		var prior realtimeFrame
		doc, err := oif.ParseJSON(data, oif.Limits{MaxBytes: maxRealtimeTrackedFrameBytes})
		priorValid := err == nil && doc.Root().Kind() == oif.Object && json.Unmarshal(data, &prior) == nil
		var current realtimeFrame
		currentValid := decodeStrictRealtimeFrame(data, &current)
		if currentValid != priorValid || currentValid && !reflect.DeepEqual(current, prior) {
			t.Fatalf("native observation changed for %s: prior=%+v valid=%t current=%+v valid=%t", frame, prior, priorValid, current, currentValid)
		}
	}
}

func TestTransformedRealtimeUsagePrefilterPreservesNativeTerminalProjection(t *testing.T) {
	for _, raw := range []string{
		`{"type":"response.audio.delta","response_id":"resp_1","delta":"AQID","output_index":0}`,
		`{"type":"session.updated","event_id":"event_1"}`,
		`{"type":"response.audio.delta","delta":"response.done is only text"}`,
	} {
		state := realtimeResponseState{}
		if got := state.providerFrame(websocket.MessageText, []byte(raw), false); got != nil {
			t.Fatalf("nonterminal transformed frame fabricated usage: frame=%s usage=%+v", raw, got)
		}
	}
	for _, raw := range []string{
		`{"type":"response.done","response":{"id":"resp_1","usage":{"input_tokens":3,"output_tokens":2}}}`,
		`{"type":"response.\u0064one","response":{"id":"resp_1","usage":{"input_tokens":3,"output_tokens":2}}}`,
		`{"type":"response.audio.delta","type":"response.done","response":{"id":"resp_1","usage":{"input_tokens":3,"output_tokens":2}}}`,
		`{"TYPE":"response.done","response":{"id":"resp_1","usage":{"input_tokens":3,"output_tokens":2}}}`,
		`{"\u0074ype":"response.done","response":{"id":"resp_1","usage":{"input_tokens":3,"output_tokens":2}}}`,
	} {
		state := realtimeResponseState{}
		usage := state.providerFrame(websocket.MessageText, []byte(raw), false)
		if usage == nil || usage.InputTokens != 3 || usage.OutputTokens != 2 {
			t.Fatalf("legacy terminal usage was lost for %s: %+v", raw, usage)
		}
	}
}

func TestStrictRealtimeFrameAccountingRequiresUnambiguousNativeJSON(t *testing.T) {
	flat := []byte(`{"type":"response.audio.delta","event_id":"e1","response_id":"resp_1","delta":"AQID"}`)
	event, ok := realtimeFlatFrame(flat)
	if !ok || event.Type != "response.audio.delta" || event.ResponseID != "resp_1" {
		t.Fatalf("exact flat audio event missed the bounded native path: event=%+v ok=%t", event, ok)
	}
	for _, frame := range []string{
		`{"type":"response.done","response":5}`,
		`{"type":"response.audio.delta","type":"response.done"}`,
		`{"TYPE":"response.done"}`,
		`{"type":"response.audio.delta","delta":"\uD800"}`,
	} {
		if _, ok := realtimeFlatFrame([]byte(frame)); ok {
			t.Fatalf("ambiguous or structured frame bypassed full validation: %s", frame)
		}
	}
	for _, tc := range []struct {
		name  string
		frame string
	}{
		{"duplicate terminal type", `{"type":"response.audio.delta","type":"response.done","response":{"id":"resp_1","usage":{"input_tokens":3,"output_tokens":2}}}`},
		{"duplicate response identity", `{"type":"response.done","response":{"id":"resp_1","id":"resp_2","usage":{"input_tokens":3,"output_tokens":2}}}`},
		{"duplicate nested usage", `{"type":"response.done","response":{"id":"resp_1","usage":{"input_tokens":3,"input_tokens":4,"output_tokens":2}}}`},
		{"duplicate unknown nested member", `{"type":"response.done","response":{"id":"resp_1","usage":{"input_tokens":3,"output_tokens":2},"native":{"x":1,"x":2}}}`},
		{"fractional integer usage", `{"type":"response.done","response":{"id":"resp_1","usage":{"input_tokens":3.0,"output_tokens":2}}}`},
		{"invalid surrogate", `{"type":"response.done","response":{"id":"resp_1","usage":{"input_tokens":3,"output_tokens":2},"native":"\uD800"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := realtimeResponseState{}
			state.clientFrame(websocket.MessageText, []byte(`{"type":"response.create"}`))
			state.providerFrame(websocket.MessageText, []byte(`{"type":"response.created","response":{"id":"resp_1"}}`), true)
			if got := state.providerFrame(websocket.MessageText, []byte(tc.frame), true); got != nil || !state.pending() || !state.unknown {
				t.Fatalf("ambiguous frame discharged strict response: usage=%+v state=%+v", got, state)
			}
		})
	}
	state := realtimeResponseState{}
	state.clientFrame(websocket.MessageText, []byte(`{"type":"response.create"}`))
	state.providerFrame(websocket.MessageText, []byte(`{"type":"response.created","response":{"id":"resp_1"}}`), true)
	frame := []byte(`{"\u0074ype":"response.done","response":{"id":"resp_1","usage":{"input_tokens":3,"output_tokens":2,"input_token_details":{"cached_tokens":1}}}}`)
	got := state.providerFrame(websocket.MessageText, frame, true)
	if got == nil || got.InputTokens != 3 || got.OutputTokens != 2 || got.CachedInputTokens == nil || *got.CachedInputTokens != 1 || state.pending() {
		t.Fatalf("valid escaped key and native usage were lost: usage=%+v state=%+v", got, state)
	}
	state = realtimeResponseState{}
	state.clientFrame(websocket.MessageText, []byte(`{"TYPE":"response.create"}`))
	state.providerFrame(websocket.MessageText, []byte(`{"TYPE":"response.created","Response":{"ID":"resp_1"}}`), true)
	got = state.providerFrame(websocket.MessageText, []byte(`{"TYPE":"response.done","Response":{"ID":"resp_1","Usage":{"Input_Tokens":3,"Output_Tokens":2}}}`), true)
	if got == nil || got.InputTokens != 3 || got.OutputTokens != 2 || state.pending() {
		t.Fatalf("accepted case-folded native fields changed classification: usage=%+v state=%+v", got, state)
	}
}
