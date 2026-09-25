package gateway

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/tests/fidelity"
)

// The stored payloads below are authored against the committed carrier
// grammar directly, not produced by the projection under test.
func storedContinuationPayload(delivery string) []byte {
	return []byte(`{"version":"chat-anthropic-tools-v1","source":{"model":"route"},"receipt":{},"binding":"model","delivery":` + delivery + `}`)
}

func TestStoredContinuationTerminalCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name     string
		delivery string
		record   string // empty when the committed delivery predates the record
	}{
		{
			name:     "committed end_turn record survives",
			delivery: `{"stream":true,"frames":[],"native_terminal":{"stop_reason":"end_turn","stop_sequence":null,"finish_reason":"stop"}}`,
			record:   `{"stop_reason":"end_turn","stop_sequence":null,"finish_reason":"stop"}`,
		},
		{
			name:     "committed matched sequence survives",
			delivery: `{"stream":true,"frames":[],"native_terminal":{"stop_reason":"stop_sequence","stop_sequence":"###","finish_reason":"stop"}}`,
			record:   `{"stop_reason":"stop_sequence","stop_sequence":"###","finish_reason":"stop"}`,
		},
		{
			name:     "absent sequence member survives absent",
			delivery: `{"stream":false,"body":{"id":"m"},"native_terminal":{"stop_reason":"end_turn","finish_reason":"stop"}}`,
			record:   `{"stop_reason":"end_turn","finish_reason":"stop"}`,
		},
		{
			name:     "output limit record survives",
			delivery: `{"stream":true,"frames":[],"native_terminal":{"stop_reason":"max_tokens","stop_sequence":null,"finish_reason":"length"}}`,
			record:   `{"stop_reason":"max_tokens","stop_sequence":null,"finish_reason":"length"}`,
		},
		{
			name:     "historical delivery without the record",
			delivery: `{"stream":true,"frames":[{"id":"m"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, err := decodeStoredContinuation(storedContinuationPayload(tc.delivery))
			if err != nil {
				t.Fatal(err)
			}
			recovered := recoveredTerminal(state.Delivery)
			if tc.record == "" {
				if state.Delivery.Terminal != nil {
					t.Fatal("historical delivery gained a terminal record")
				}
				if recovered != "unavailable" {
					t.Fatalf("historical terminal reads %v want unavailable", recovered)
				}
				return
			}
			raw, err := json.Marshal(recovered)
			if err != nil {
				t.Fatal(err)
			}
			if err := fidelity.Compare([]byte(tc.record), raw); err != nil {
				t.Fatalf("recovered terminal %s: %v", raw, err)
			}
			// The committed delivery itself keeps the same record.
			if state.Delivery.Terminal == nil {
				t.Fatal("committed terminal record was dropped")
			}
		})
	}
}

// The committed actionability claim decodes only when it satisfies its own
// grammar and names exactly the retained assistant's ordered calls.
func storedContinuationWithAssistant(assistant, delivery string) []byte {
	return []byte(`{"version":"chat-anthropic-tools-v1","source":{"model":"route"},"receipt":{},"binding":"model","interaction":{"version":"chat-anthropic-tools-v1","source":{},"native_request":{},"blocks":[],"assistant":` + assistant + `},"delivery":` + delivery + `}`)
}

func TestStoredContinuationActionsCompatibility(t *testing.T) {
	assistant := `{"role":"assistant","content":"beforeafter","tool_calls":[{"id":"call-weather","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Paris\"}"}},{"id":"call-clock","type":"function","function":{"name":"clock","arguments":"{\"zone\":\"Europe/Paris\"}"}}]}`
	for _, tc := range []struct {
		name      string
		assistant string
		delivery  string
		claim     string // empty when the committed delivery predates the claim
	}{
		{
			name:      "ordered claim survives",
			assistant: assistant,
			delivery:  `{"stream":true,"frames":[],"actions":{"tool_calls":["call-weather","call-clock"]}}`,
			claim:     `{"tool_calls":["call-weather","call-clock"]}`,
		},
		{
			name:      "explicit empty claim survives a final turn",
			assistant: `{"role":"assistant","content":"done"}`,
			delivery:  `{"stream":false,"body":{"id":"m"},"actions":{"tool_calls":[]}}`,
			claim:     `{"tool_calls":[]}`,
		},
		{
			name:     "historical delivery without the claim",
			delivery: `{"stream":true,"frames":[{"id":"m"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := storedContinuationPayload(tc.delivery)
			if tc.assistant != "" {
				payload = storedContinuationWithAssistant(tc.assistant, tc.delivery)
			}
			state, err := decodeStoredContinuation(payload)
			if err != nil {
				t.Fatal(err)
			}
			recovered := recoveredActions(state.Delivery)
			if tc.claim == "" {
				if state.Delivery.Actions != nil {
					t.Fatal("historical delivery gained an action claim")
				}
				if recovered != "unavailable" {
					t.Fatalf("historical actions read %v want unavailable", recovered)
				}
				return
			}
			raw, err := json.Marshal(recovered)
			if err != nil {
				t.Fatal(err)
			}
			if err := fidelity.Compare([]byte(tc.claim), raw); err != nil {
				t.Fatalf("recovered actions %s: %v", raw, err)
			}
			if state.Delivery.Actions == nil {
				t.Fatal("committed action claim was dropped")
			}
		})
	}
}

func TestStoredContinuationRejectsUncorrespondingActions(t *testing.T) {
	assistant := `{"role":"assistant","content":"x","tool_calls":[{"id":"call-weather","type":"function","function":{"name":"weather","arguments":"{}"}},{"id":"call-clock","type":"function","function":{"name":"clock","arguments":"{}"}}]}`
	for _, tc := range []struct{ name, assistant, delivery string }{
		{"unknown claim member", assistant, `{"stream":true,"frames":[],"actions":{"tool_calls":["call-weather","call-clock"],"hosted":[]}}`},
		{"claim drops a committed call", assistant, `{"stream":true,"frames":[],"actions":{"tool_calls":["call-weather"]}}`},
		{"claim invents a call", assistant, `{"stream":true,"frames":[],"actions":{"tool_calls":["call-weather","call-clock","call-extra"]}}`},
		{"claim reorders calls", assistant, `{"stream":true,"frames":[],"actions":{"tool_calls":["call-clock","call-weather"]}}`},
		{"claim without any assistant calls", `{"role":"assistant","content":"done"}`, `{"stream":true,"frames":[],"actions":{"tool_calls":["call-weather"]}}`},
		{"non-array claim", assistant, `{"stream":true,"frames":[],"actions":{"tool_calls":"call-weather"}}`},
		{"empty call identity", assistant, `{"stream":true,"frames":[],"actions":{"tool_calls":["call-weather",""]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeStoredContinuation(storedContinuationWithAssistant(tc.assistant, tc.delivery)); !errors.Is(err, resources.ErrContract) {
				t.Fatalf("uncorresponding claim decoded: %v", err)
			}
		})
	}
}

func TestStoredContinuationRejectsMalformedTerminal(t *testing.T) {
	for _, tc := range []struct{ name, delivery string }{
		{"matched sequence under another reason", `{"stream":true,"frames":[],"native_terminal":{"stop_reason":"end_turn","stop_sequence":"###","finish_reason":"stop"}}`},
		{"unqualified native reason", `{"stream":true,"frames":[],"native_terminal":{"stop_reason":"pause_turn","finish_reason":"stop"}}`},
		{"invented reason", `{"stream":true,"frames":[],"native_terminal":{"stop_reason":"invented","finish_reason":"stop"}}`},
		{"sequence reason without a match", `{"stream":true,"frames":[],"native_terminal":{"stop_reason":"stop_sequence","finish_reason":"stop"}}`},
		{"sequence reason with explicit null", `{"stream":true,"frames":[],"native_terminal":{"stop_reason":"stop_sequence","stop_sequence":null,"finish_reason":"stop"}}`},
		{"unqualified client finish", `{"stream":true,"frames":[],"native_terminal":{"stop_reason":"end_turn","finish_reason":"incompatible"}}`},
		{"non-string sequence", `{"stream":true,"frames":[],"native_terminal":{"stop_reason":"stop_sequence","stop_sequence":42,"finish_reason":"stop"}}`},
		{"empty matched sequence", `{"stream":true,"frames":[],"native_terminal":{"stop_reason":"stop_sequence","stop_sequence":"","finish_reason":"stop"}}`},
		{"missing native reason", `{"stream":true,"frames":[],"native_terminal":{"stop_sequence":"###","finish_reason":"stop"}}`},
		{"missing client finish", `{"stream":true,"frames":[],"native_terminal":{"stop_reason":"end_turn"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeStoredContinuation(storedContinuationPayload(tc.delivery)); !errors.Is(err, resources.ErrContract) {
				t.Fatalf("malformed terminal decoded: %v", err)
			}
		})
	}
}
