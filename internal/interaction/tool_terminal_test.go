package interaction

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/tests/fidelity"
)

// The traces and results below are authored from the documented Anthropic
// Messages terminal contract, not produced by the projection under test. The
// expected records are written out literally so a regression in the projection
// cannot generate its own oracle.
const terminalTextWire = `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"same words"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

`

func projectTerminalStream(t *testing.T, wire string) Delivery {
	t.Helper()
	plan := bind(t, toolsTemplate(t), request(t, openai.FamilyChat, toolSource), toolContext())
	projection, err := plan.NewToolProjection(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	completion, err := protocols.StreamWithEvents(openai.FamilyAnthropic, openai.FamilyAnthropic, strings.NewReader(wire), 1<<20, "route", true, func([]byte) error { return nil }, func(event oif.Event) error {
		_, e := projection.Observe(event)
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	_, delivery, err := projection.Complete(completion, "continuation_terminal")
	if err != nil {
		t.Fatal(err)
	}
	return delivery
}

func projectTerminalUnary(t *testing.T, body string) Delivery {
	t.Helper()
	unary := strings.Replace(toolSource, `"stream":true,`, "", 1)
	plan := bind(t, toolsTemplate(t), request(t, openai.FamilyChat, unary), toolContext())
	native, err := protocols.DecodeRequest(openai.FamilyAnthropic, openai.FamilyAnthropic, []byte(body), "route", "", plan.EffectiveRequest())
	if err != nil {
		t.Fatal(err)
	}
	_, delivery, err := plan.ProjectUnary(native, "continuation_terminal_unary", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return delivery
}

func terminalUsage() string {
	return `"usage":{"input_tokens":3,"output_tokens":7}`
}

func TestNegotiatedTerminalRecordDistinguishesNativeStops(t *testing.T) {
	// Identical visible text and usage; only the native terminal differs. The
	// compatible finish reason stays collapsed while the record keeps the
	// accepted native outcome distinct, including explicit null versus absent
	// stop_sequence.
	for _, tc := range []struct {
		name    string
		blocks  string
		delta   string
		content string
		result  string
		record  string
		finish  string
	}{
		{
			name:    "end turn with explicit null sequence",
			blocks:  terminalTextWire,
			delta:   `{"stop_reason":"end_turn","stop_sequence":null}`,
			content: `[{"type":"text","text":"same words"}]`,
			result:  `"stop_reason":"end_turn","stop_sequence":null`,
			record:  `{"stop_reason":"end_turn","stop_sequence":null,"finish_reason":"stop"}`,
			finish:  "stop",
		},
		{
			name:    "first configured stop sequence",
			blocks:  terminalTextWire,
			delta:   `{"stop_reason":"stop_sequence","stop_sequence":"FIRST-STOP"}`,
			content: `[{"type":"text","text":"same words"}]`,
			result:  `"stop_reason":"stop_sequence","stop_sequence":"FIRST-STOP"`,
			record:  `{"stop_reason":"stop_sequence","stop_sequence":"FIRST-STOP","finish_reason":"stop"}`,
			finish:  "stop",
		},
		{
			name:    "second configured stop sequence",
			blocks:  terminalTextWire,
			delta:   `{"stop_reason":"stop_sequence","stop_sequence":"SECOND-STOP"}`,
			content: `[{"type":"text","text":"same words"}]`,
			result:  `"stop_reason":"stop_sequence","stop_sequence":"SECOND-STOP"`,
			record:  `{"stop_reason":"stop_sequence","stop_sequence":"SECOND-STOP","finish_reason":"stop"}`,
			finish:  "stop",
		},
		{
			name:    "sequence member absent from terminal",
			blocks:  terminalTextWire,
			delta:   `{"stop_reason":"end_turn"}`,
			content: `[{"type":"text","text":"same words"}]`,
			result:  `"stop_reason":"end_turn"`,
			record:  `{"stop_reason":"end_turn","finish_reason":"stop"}`,
			finish:  "stop",
		},
		{
			name:    "output limit",
			blocks:  terminalTextWire,
			delta:   `{"stop_reason":"max_tokens","stop_sequence":null}`,
			content: `[{"type":"text","text":"same words"}]`,
			result:  `"stop_reason":"max_tokens","stop_sequence":null`,
			record:  `{"stop_reason":"max_tokens","stop_sequence":null,"finish_reason":"length"}`,
			finish:  "length",
		},
		{
			name:    "tool use terminal",
			blocks:  toolBlockWire,
			delta:   `{"stop_reason":"tool_use","stop_sequence":null}`,
			content: `[{"type":"tool_use","id":"call-weather","name":"weather","input":{"city":"Paris"}}]`,
			result:  `"stop_reason":"tool_use","stop_sequence":null`,
			record:  `{"stop_reason":"tool_use","stop_sequence":null,"finish_reason":"tool_calls"}`,
			finish:  "tool_calls",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := toolStartWire + tc.blocks +
				toolWireEvent("message_delta", `{"type":"message_delta","delta":`+tc.delta+`,"usage":{"output_tokens":7}}`) +
				toolStopWire
			streamDelivery := projectTerminalStream(t, wire)
			recordRaw, err := json.Marshal(streamDelivery.Terminal)
			if err != nil {
				t.Fatal(err)
			}
			if err := fidelity.Compare([]byte(tc.record), recordRaw); err != nil {
				t.Fatalf("stream terminal record %s: %v", recordRaw, err)
			}
			frame, err := oif.ParseJSON(streamDelivery.Frames[len(streamDelivery.Frames)-1], oif.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			observed, present := frame.Lookup("/olp/native_terminal")
			if !present {
				t.Fatalf("terminal frame lost the record: %s", frame.Bytes())
			}
			if err := fidelity.Compare([]byte(tc.record), observed.Bytes()); err != nil {
				t.Fatalf("extension terminal record %s: %v", observed.Bytes(), err)
			}
			if finish, _ := frame.Lookup("/choices/0/finish_reason"); finish.Raw() != `"`+tc.finish+`"` {
				t.Fatalf("client finish reason changed: %s", finish.Raw())
			}
			result := `{"id":"msg-terminal","type":"message","role":"assistant","model":"fixture-model","content":` + tc.content + `,` + tc.result + `,` + terminalUsage() + `}`
			unaryDelivery := projectTerminalUnary(t, result)
			recordRaw, err = json.Marshal(unaryDelivery.Terminal)
			if err != nil {
				t.Fatal(err)
			}
			if err := fidelity.Compare([]byte(tc.record), recordRaw); err != nil {
				t.Fatalf("unary terminal record %s: %v", recordRaw, err)
			}
			body, err := oif.ParseJSON(unaryDelivery.Body, oif.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			observed, present = body.Lookup("/olp/native_terminal")
			if !present {
				t.Fatalf("unary body lost the record: %s", body.Bytes())
			}
			if err := fidelity.Compare([]byte(tc.record), observed.Bytes()); err != nil {
				t.Fatalf("unary extension terminal record %s: %v", observed.Bytes(), err)
			}
		})
	}
}

func TestNegotiatedTerminalRecordIsDistinctAcrossStops(t *testing.T) {
	// The three negotiated stop outcomes share text, usage and the compatible
	// finish reason, so the records themselves must differ.
	records := map[string]json.RawMessage{}
	for _, delta := range []string{
		`{"stop_reason":"end_turn","stop_sequence":null}`,
		`{"stop_reason":"stop_sequence","stop_sequence":"FIRST-STOP"}`,
		`{"stop_reason":"stop_sequence","stop_sequence":"SECOND-STOP"}`,
	} {
		wire := toolStartWire + terminalTextWire +
			toolWireEvent("message_delta", `{"type":"message_delta","delta":`+delta+`,"usage":{"output_tokens":7}}`) +
			toolStopWire
		delivery := projectTerminalStream(t, wire)
		raw, _ := json.Marshal(delivery.Terminal)
		records[delta] = raw
	}
	seen := map[string]bool{}
	for delta, record := range records {
		if seen[string(record)] {
			t.Fatalf("terminal record collided for %s: %s", delta, record)
		}
		seen[string(record)] = true
	}
	if len(records) != 3 {
		t.Fatalf("missing terminal records: %v", records)
	}
}

func TestNegotiatedTerminalRecordRejectsContradictoryStops(t *testing.T) {
	base := toolStartWire + terminalTextWire
	for _, tc := range []struct {
		name, wire string
		// projection marks rejections raised by the negotiated fidelity
		// gate rather than the shared native grammar.
		projection bool
	}{
		{
			name:       "matched sequence under another reason",
			wire:       base + toolWireEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":"OOPS"},"usage":{"output_tokens":7}}`) + toolStopWire,
			projection: true,
		},
		{
			name: "sequence declared before a different reason",
			wire: base +
				toolWireEvent("message_delta", `{"type":"message_delta","delta":{"stop_sequence":"###"},"usage":{"output_tokens":4}}`) +
				toolWireEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`) +
				toolStopWire,
			projection: true,
		},
		{
			name:       "sequence reason without a matched sequence",
			wire:       base + toolWireEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"stop_sequence","stop_sequence":null},"usage":{"output_tokens":7}}`) + toolStopWire,
			projection: true,
		},
		{
			name:       "sequence reason with member absent",
			wire:       base + toolWireEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"stop_sequence"},"usage":{"output_tokens":7}}`) + toolStopWire,
			projection: true,
		},
		{
			name:       "refused terminal never reads as success",
			wire:       base + toolWireEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"refusal","stop_sequence":null},"usage":{"output_tokens":7}}`) + toolStopWire,
			projection: true,
		},
		{
			name:       "unqualified provider terminal",
			wire:       base + toolWireEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"pause_turn","stop_sequence":null},"usage":{"output_tokens":7}}`) + toolStopWire,
			projection: true,
		},
		{
			name: "contradictory repeated terminals",
			wire: base +
				toolWireEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"stop_sequence","stop_sequence":"a"},"usage":{"output_tokens":5}}`) +
				toolWireEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"stop_sequence","stop_sequence":"b"},"usage":{"output_tokens":7}}`) +
				toolStopWire,
		},
		{
			name:       "empty matched sequence",
			wire:       base + toolWireEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"stop_sequence","stop_sequence":""},"usage":{"output_tokens":7}}`) + toolStopWire,
			projection: true,
		},
		{
			name: "malformed sequence value",
			wire: base + toolWireEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"stop_sequence","stop_sequence":42},"usage":{"output_tokens":7}}`) + toolStopWire,
		},
		{
			name:       "terminal member outside the grammar",
			wire:       base + toolWireEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","confidence":0.9},"usage":{"output_tokens":7}}`) + toolStopWire,
			projection: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := bind(t, toolsTemplate(t), request(t, openai.FamilyChat, toolSource), toolContext())
			projection, err := plan.NewToolProjection(1 << 20)
			if err != nil {
				t.Fatal(err)
			}
			completion, streamErr := protocols.StreamWithEvents(openai.FamilyAnthropic, openai.FamilyAnthropic, strings.NewReader(tc.wire), 1<<20, "route", true, func([]byte) error { return nil }, func(event oif.Event) error {
				_, e := projection.Observe(event)
				return e
			})
			if streamErr == nil {
				t.Fatal("contradictory or unqualified terminal was accepted")
			}
			if tc.projection {
				assertReason(t, streamErr, "fidelity_protocol_violation")
			}
			if _, _, err := projection.Complete(completion, "continuation_rejected"); err == nil {
				t.Fatal("rejected trace produced a ready delivery")
			}
		})
	}
}

func TestNegotiatedTerminalRecordRejectsLateEvents(t *testing.T) {
	// A repeated terminal event or any event after message_stop is a protocol
	// violation, not a second outcome: the committed record keeps the first
	// accepted terminal and the late event never amends it.
	plan := bind(t, toolsTemplate(t), request(t, openai.FamilyChat, toolSource), toolContext())
	projection, err := plan.NewToolProjection(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	wire := toolStartWire + terminalTextWire +
		toolWireEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"stop_sequence","stop_sequence":"###"},"usage":{"output_tokens":7}}`) +
		toolStopWire
	completion, err := protocols.StreamWithEvents(openai.FamilyAnthropic, openai.FamilyAnthropic, strings.NewReader(wire), 1<<20, "route", true, func([]byte) error { return nil }, func(event oif.Event) error {
		_, e := projection.Observe(event)
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	_, delivery, err := projection.Complete(completion, "continuation_late")
	if err != nil {
		t.Fatal(err)
	}
	recordRaw, _ := json.Marshal(delivery.Terminal)
	if err := fidelity.Compare([]byte(`{"stop_reason":"stop_sequence","stop_sequence":"###","finish_reason":"stop"}`), recordRaw); err != nil {
		t.Fatalf("committed terminal record %s: %v", recordRaw, err)
	}
	for _, name := range []string{"message_stop", "message_delta"} {
		raw := `{"type":"` + name + `"}`
		if name == "message_delta" {
			raw = `{"type":"message_delta","delta":{"stop_reason":"max_tokens"}}`
		}
		doc, e := oif.ParseJSON([]byte(raw), oif.Limits{MaxBytes: 1 << 20})
		if e != nil {
			t.Fatal(e)
		}
		event, e := oif.NewEvent(openai.Descriptor(openai.FamilyAnthropic, true), doc, name, 100)
		if e != nil {
			t.Fatal(e)
		}
		if _, e := projection.Observe(event); e == nil {
			t.Fatalf("event %s admitted after the terminal boundary", name)
		}
	}
}

func TestNegotiatedUnaryTerminalRejectsUnqualifiedStops(t *testing.T) {
	unary := strings.Replace(toolSource, `"stream":true,`, "", 1)
	for _, tc := range []struct{ name, result string }{
		{"refused result", `"stop_reason":"refusal","stop_sequence":null`},
		{"unqualified future stop", `"stop_reason":"pause_turn","stop_sequence":null`},
		{"sequence reason without match", `"stop_reason":"stop_sequence","stop_sequence":null`},
		{"sequence reason with empty match", `"stop_reason":"stop_sequence","stop_sequence":""`},
		{"matched sequence under end_turn", `"stop_reason":"end_turn","stop_sequence":"OOPS"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := bind(t, toolsTemplate(t), request(t, openai.FamilyChat, unary), toolContext())
			body := `{"id":"msg-bad","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"same words"}],` + tc.result + `,` + terminalUsage() + `}`
			native, err := protocols.DecodeRequest(openai.FamilyAnthropic, openai.FamilyAnthropic, []byte(body), "route", "", plan.EffectiveRequest())
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = plan.ProjectUnary(native, "continuation_rejected_unary", 1<<20)
			assertReason(t, err, "fidelity_protocol_violation")
		})
	}
}

func TestDeliveryTerminalRecordCarrierCompatibility(t *testing.T) {
	// A committed delivery round-trips the record; a delivery committed before
	// the record existed decodes with it absent, never synthesized.
	record := `{"stop_reason":"stop_sequence","stop_sequence":"###","finish_reason":"stop"}`
	raw := `{"stream":true,"frames":[{"olp":{}}],"native_terminal":` + record + `}`
	var delivery Delivery
	if err := json.Unmarshal([]byte(raw), &delivery); err != nil {
		t.Fatal(err)
	}
	if delivery.Terminal == nil || !delivery.Terminal.Valid() {
		t.Fatalf("stored terminal record did not decode: %s", raw)
	}
	encoded, _ := json.Marshal(delivery)
	if err := fidelity.Compare([]byte(raw), encoded); err != nil {
		t.Fatalf("stored terminal record changed shape: %s", encoded)
	}
	for _, legacy := range []string{
		`{"stream":true,"frames":[]}`,
		`{"stream":false,"body":{"id":"m"}}`,
	} {
		var old Delivery
		if err := json.Unmarshal([]byte(legacy), &old); err != nil {
			t.Fatal(err)
		}
		if old.Terminal != nil {
			t.Fatalf("historical delivery gained a terminal record: %s", legacy)
		}
	}
}
