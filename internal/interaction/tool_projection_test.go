package interaction

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// The projection keeps four separately bounded resources: retained native
// dependency bytes, projected delivery bytes retained for replay, live
// transient event memory, and the aggregate admitted-event work counter that
// also covers events the grammar drops.
func TestToolProjectionPingsSpendWorkButRetainNothing(t *testing.T) {
	ping := toolWireEvent("ping", `{"type":"ping"}`)
	tail := toolWireEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":10}}`) + toolStopWire
	base := toolStartWire + toolBlockWire + tail
	pinged := toolStartWire + ping + ping + toolBlockWire + ping + ping + ping + tail

	type run struct {
		state    *Continuation
		delivery Delivery
		accounts Accounting
	}
	runs := map[string]run{}
	for name, wire := range map[string]string{"plain": base, "pinged": pinged} {
		plan := bind(t, toolsTemplate(t), request(t, openai.FamilyChat, toolSource), toolContext())
		projection, err := plan.NewToolProjection(1 << 20)
		if err != nil {
			t.Fatal(err)
		}
		var transientAfterPing []int
		completion, err := protocols.StreamWithEvents(openai.FamilyAnthropic, openai.FamilyAnthropic, strings.NewReader(wire), 1<<20, "route", true, func([]byte) error { return nil }, func(event oif.Event) error {
			before := projection.Accounting().TransientEventBytes
			frames, e := projection.Observe(event)
			if e == nil && event.Name() == "ping" {
				if len(frames) != 0 {
					t.Fatal("a dropped ping produced client output")
				}
				// The source envelope's transient credit must release as
				// soon as the event is no longer retained.
				transientAfterPing = append(transientAfterPing, projection.Accounting().TransientEventBytes-before)
			}
			return e
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, delta := range transientAfterPing {
			if delta != 0 {
				t.Fatalf("%s: ping left %d transient bytes held", name, delta)
			}
		}
		state, delivery, err := projection.Complete(completion, "continuation_pings")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		runs[name] = run{state, delivery, projection.Accounting()}
	}
	plain, extra := runs["plain"], runs["pinged"]
	if !bytes.Equal(plain.state.Blocks, extra.state.Blocks) || !bytes.Equal(plain.state.Assistant, extra.state.Assistant) {
		t.Fatal("pings changed the retained native dependency")
	}
	if len(plain.delivery.Frames) != len(extra.delivery.Frames) {
		t.Fatal("pings changed the projected delivery")
	}
	for i := range plain.delivery.Frames {
		if !bytes.Equal(plain.delivery.Frames[i], extra.delivery.Frames[i]) {
			t.Fatal("pings changed a projected frame")
		}
	}
	if plain.accounts.RetainedDependencyBytes != extra.accounts.RetainedDependencyBytes {
		t.Fatalf("discarded pings consumed retained dependency: %d vs %d", plain.accounts.RetainedDependencyBytes, extra.accounts.RetainedDependencyBytes)
	}
	if plain.accounts.ProjectedDeliveryBytes != extra.accounts.ProjectedDeliveryBytes {
		t.Fatalf("discarded pings consumed delivery bytes: %d vs %d", plain.accounts.ProjectedDeliveryBytes, extra.accounts.ProjectedDeliveryBytes)
	}
	if extra.accounts.AdmittedEventWorkBytes <= plain.accounts.AdmittedEventWorkBytes {
		t.Fatal("discarded pings escaped aggregate event-work accounting")
	}
	if extra.accounts.TransientEventBytes != 0 {
		t.Fatalf("transient credit leaked: %d", extra.accounts.TransientEventBytes)
	}
}

// A stream of admitted but discarded events can exhaust the event-work budget
// while retained dependency stays empty — the constrained resource must be
// reported as event work, not bytes of state.
func TestToolProjectionEventWorkExhaustsIndependentlyOfRetainedState(t *testing.T) {
	pings := ""
	for i := 0; i < 80; i++ {
		pings += toolWireEvent("ping", fmt.Sprintf(`{"type":"ping","n":%d}`, i))
	}
	wire := toolStartWire + pings + toolBlockWire
	plan := bind(t, toolsTemplate(t), request(t, openai.FamilyChat, toolSource), toolContext())
	projection, err := plan.NewToolProjection(1024)
	if err != nil {
		t.Fatal(err)
	}
	var emitted [][]byte
	_, err = protocols.StreamWithEvents(openai.FamilyAnthropic, openai.FamilyAnthropic, strings.NewReader(wire), 1<<20, "route", true, func([]byte) error { return nil }, func(event oif.Event) error {
		frames, e := projection.Observe(event)
		emitted = append(emitted, frames...)
		return e
	})
	var exhausted *Exhaustion
	if !errors.As(err, &exhausted) || exhausted.Resource != "admitted_event_work_bytes" || exhausted.Category != LimitEventWork || exhausted.Limit != 1024 {
		t.Fatalf("event work exhaustion misattributed: %v", err)
	}
	if projection.Accounting().RetainedDependencyBytes != 0 {
		t.Fatal("dropped events charged retained dependency")
	}
	for _, frame := range emitted {
		if bytes.Contains(frame, []byte("tool_calls")) {
			t.Fatal("exhausted stream produced actionable output")
		}
	}
}

// Retained dependency is the persisted interaction representation — request
// source, native request, assistant message and closed blocks — not a counter
// of all input bytes. A large request exhausts it while event work stays low.
func TestToolProjectionRetainedDependencyExhaustionIsAttributed(t *testing.T) {
	large := strings.Replace(toolSource, "Weather in a city", "Weather in a city "+strings.Repeat("x", 8192), 1)
	wire := toolStartWire + toolBlockWire +
		toolWireEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":10}}`) + toolStopWire
	plan := bind(t, toolsTemplate(t), request(t, openai.FamilyChat, large), toolContext())
	projection, err := plan.NewToolProjection(4096)
	if err != nil {
		t.Fatal(err)
	}
	completion, err := protocols.StreamWithEvents(openai.FamilyAnthropic, openai.FamilyAnthropic, strings.NewReader(wire), 1<<20, "route", true, func([]byte) error { return nil }, func(event oif.Event) error {
		_, e := projection.Observe(event)
		return e
	})
	if err != nil {
		t.Fatalf("small event work must not exhaust: %v", err)
	}
	if projection.Accounting().AdmittedEventWorkBytes >= 4096 {
		t.Fatal("event work reached the limit; the test cannot attribute dependency exhaustion")
	}
	_, _, err = projection.Complete(completion, "continuation_dependency")
	var exhausted *Exhaustion
	if !errors.As(err, &exhausted) || exhausted.Resource != "retained_dependency_bytes" || exhausted.Category != LimitBytes {
		t.Fatalf("dependency exhaustion misattributed: %v", err)
	}
}

// Delivery is the replayed client output: splitting the same text into more
// fragments honestly grows projected delivery while the retained dependency —
// the reconstructed native block — stays identical.
func TestToolProjectionFragmentationGrowsDeliveryNotDependency(t *testing.T) {
	textBlock := func(deltas ...string) string {
		wire := toolWireEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		for _, delta := range deltas {
			encoded, _ := json.Marshal(delta)
			wire += toolWireEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":`+string(encoded)+`}}`)
		}
		return wire + toolWireEvent("content_block_stop", `{"type":"content_block_stop","index":0}`)
	}
	tail := toolWireEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`) + toolStopWire
	// Escaped characters must survive serialization into both representations.
	compact := toolStartWire + textBlock(`a "quoted" line`+"\n"+`break`) + tail
	split := toolStartWire + textBlock(`a `, `"quoted"`, ` line`, "\n", `break`) + tail

	runs := map[string]Accounting{}
	states := map[string]*Continuation{}
	var frames int
	for name, wire := range map[string]string{"compact": compact, "split": split} {
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
			t.Fatalf("%s: %v", name, err)
		}
		state, delivery, err := projection.Complete(completion, "continuation_fragmented")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Contains(delivery.Frames[len(delivery.Frames)-1], []byte(`finish_reason`)) {
			t.Fatalf("%s: terminal frame missing", name)
		}
		states[name], runs[name] = state, projection.Accounting()
		if name == "split" {
			frames = len(delivery.Frames)
		}
	}
	if !bytes.Equal(states["compact"].Blocks, states["split"].Blocks) {
		t.Fatal("fragmentation changed the retained native dependency")
	}
	if runs["compact"].RetainedDependencyBytes != runs["split"].RetainedDependencyBytes {
		t.Fatal("fragmentation changed the retained dependency charge")
	}
	if runs["split"].ProjectedDeliveryBytes <= runs["compact"].ProjectedDeliveryBytes {
		t.Fatalf("extra fragments did not grow replay bytes honestly: %d vs %d", runs["split"].ProjectedDeliveryBytes, runs["compact"].ProjectedDeliveryBytes)
	}
	if frames <= 2 {
		t.Fatal("fragmented stream did not retain its exact frame boundaries")
	}
}
