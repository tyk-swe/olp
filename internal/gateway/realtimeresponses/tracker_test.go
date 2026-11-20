package realtimeresponses_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/realtimeresponses"
)

const create = `{"type":"response.create"}`

const fullUsage = `{"input_tokens":40,"output_tokens":9,` +
	`"input_token_details":{"cached_tokens":12,"audio_tokens":30,"image_tokens":4,"cached_tokens_details":{"audio_tokens":8,"image_tokens":2}},` +
	`"output_token_details":{"audio_tokens":7}}`

func done(id, usage string) string {
	return `{"type":"response.done","response":{"id":"` + id + `","usage":` + usage + `}}`
}

func created(id string) string {
	return `{"type":"response.created","response":{"id":"` + id + `"}}`
}

func provider(tracker *realtimeresponses.Tracker, strict bool, frames ...string) *wire.Usage {
	var last *wire.Usage
	for _, frame := range frames {
		last = tracker.ProviderFrame(websocket.MessageText, []byte(frame), strict)
	}
	return last
}

func client(tracker *realtimeresponses.Tracker, frames ...string) {
	for _, frame := range frames {
		tracker.ClientFrame(websocket.MessageText, []byte(frame))
	}
}

func eachMode(t *testing.T, run func(t *testing.T, strict bool)) {
	t.Helper()
	for name, strict := range map[string]bool{"strict": true, "transformed": false} {
		t.Run(name, func(t *testing.T) { run(t, strict) })
	}
}

func TestARequestedResponseIsOwedUntilItsTerminalEventReportsUsage(t *testing.T) {
	eachMode(t, func(t *testing.T, strict bool) {
		var tracker realtimeresponses.Tracker
		if tracker.Pending() {
			t.Fatal("a new session owes a response")
		}
		client(&tracker, create)
		if !tracker.Pending() {
			t.Fatal("a requested response is not owed")
		}
		if usage := provider(&tracker, strict, created("resp_1"), `{"type":"response.output_audio.delta","response_id":"resp_1","delta":"AAAA"}`); usage != nil {
			t.Fatalf("a non-terminal frame reported usage %+v", usage)
		}
		if !tracker.Pending() {
			t.Fatal("a started response is not owed")
		}
		usage := provider(&tracker, strict, done("resp_1", fullUsage))
		if usage == nil {
			t.Fatal("the terminal event reported no usage")
		}
		got, _ := json.Marshal(usage)
		want := `{"input_tokens":40,"output_tokens":9,"total_tokens":0,"cached_input_tokens":12,"audio_input_tokens":30,` +
			`"cached_audio_input_tokens":8,"audio_output_tokens":7,"image_input_tokens":4,"cached_image_input_tokens":2}`
		if string(got) != want {
			t.Fatalf("usage = %s, want %s", got, want)
		}
		if tracker.Pending() {
			t.Fatal("a finished response is still owed")
		}
	})
}

func TestOutputDetailsAreReadWhenInputDetailsAreAbsent(t *testing.T) {
	eachMode(t, func(t *testing.T, strict bool) {
		var tracker realtimeresponses.Tracker
		usage := provider(&tracker, strict, done("resp_1", `{"input_tokens":4,"output_tokens":9,"output_token_details":{"audio_tokens":7}}`))
		if usage == nil || usage.AudioOutputTokens == nil || *usage.AudioOutputTokens != 7 {
			t.Fatalf("usage = %+v, want 7 audio output tokens", usage)
		}
		if broken := provider(&tracker, strict, done("resp_2", `{"input_tokens":4,"output_tokens":9,"output_token_details":"7"}`)); broken != nil {
			t.Fatalf("malformed output details reported usage %+v", broken)
		}
	})
}

func TestATerminalEventWithoutTrustworthyUsageLeavesTheSessionIncomplete(t *testing.T) {
	cases := map[string]string{
		"no usage":                  `{"type":"response.done","response":{"id":"resp_1"}}`,
		"no response":               `{"type":"response.done"}`,
		"missing output tokens":     done("resp_1", `{"input_tokens":4}`),
		"negative input tokens":     done("resp_1", `{"input_tokens":-1,"output_tokens":2}`),
		"cached exceeds input":      done("resp_1", `{"input_tokens":4,"output_tokens":2,"input_token_details":{"cached_tokens":5}}`),
		"negative cached tokens":    done("resp_1", `{"input_tokens":4,"output_tokens":2,"input_token_details":{"cached_tokens":-1}}`),
		"fractional tokens":         done("resp_1", `{"input_tokens":4.5,"output_tokens":2}`),
		"text tokens":               done("resp_1", `{"input_tokens":"4","output_tokens":2}`),
		"usage is not an object":    done("resp_1", `[1,2]`),
		"details are not an object": done("resp_1", `{"input_tokens":4,"output_tokens":2,"input_token_details":7}`),
	}
	for name, frame := range cases {
		t.Run(name, func(t *testing.T) {
			eachMode(t, func(t *testing.T, strict bool) {
				var tracker realtimeresponses.Tracker
				client(&tracker, create)
				if usage := provider(&tracker, strict, created("resp_1"), frame); usage != nil {
					t.Fatalf("reported usage %+v", usage)
				}
				if !tracker.Pending() {
					t.Fatal("the session ended complete without usage it can bill")
				}
			})
		})
	}
}

func TestAStrictSessionRefusesFramesItCannotReadUnambiguously(t *testing.T) {
	cases := map[string]string{
		"duplicate type":             `{"type":"response.created","type":"response.done","response":{"id":"resp_1"}}`,
		"type alias":                 `{"Type":"session.updated","type":"response.done","response":{"id":"resp_1","usage":{"input_tokens":1,"output_tokens":1}}}`,
		"duplicate usage counter":    done("resp_1", `{"input_tokens":1,"input_tokens":9,"output_tokens":1}`),
		"duplicate unobserved field": `{"type":"session.updated","note":"a","note":"b"}`,
		"type is not text":           `{"type":7}`,
		"response is not an object":  `{"type":"response.done","response":"resp_1"}`,
		"not an object":              `["response.done"]`,
		"truncated":                  `{"type":"response.done"`,
		"trailing data":              `{"type":"session.updated"} {}`,
	}
	for name, frame := range cases {
		t.Run(name, func(t *testing.T) {
			var tracker realtimeresponses.Tracker
			if usage := provider(&tracker, true, frame); usage != nil {
				t.Fatalf("reported usage %+v", usage)
			}
			if !tracker.Pending() {
				t.Fatal("an unreadable provider frame left the session complete")
			}
		})
	}
}

func TestATransformedSessionReadsOnlyTerminalFrames(t *testing.T) {
	var tracker realtimeresponses.Tracker
	if usage := provider(&tracker, false, `not json`, `{"type":"session.updated","note":"a","note":"b"}`, created("resp_1")); usage != nil {
		t.Fatalf("reported usage %+v", usage)
	}
	if tracker.Pending() {
		t.Fatal("a transformed session tracked a frame other than response.done")
	}
	if usage := provider(&tracker, false, done("resp_1", `{"input_tokens":3,"output_tokens":2}`)); usage == nil || usage.InputTokens != 3 || usage.OutputTokens != 2 {
		t.Fatalf("usage = %+v, want 3 input and 2 output tokens", usage)
	}
	if usage := provider(&tracker, false, `{"type":"response.done"`); usage != nil || !tracker.Pending() {
		t.Fatalf("a truncated terminal frame gave usage %+v and pending %v", usage, tracker.Pending())
	}
}

func TestFramesAreReadTheSameThroughTheFastAndTheFullDecoder(t *testing.T) {
	cases := map[string]string{
		"flat":               `{"type":"response.create"}`,
		"flat with spacing":  " {\n\t\"event_id\" : \"evt_1\" , \"type\" : \"response.create\" }\r\n",
		"flat with scalars":  `{"type":"response.create","sequence":4,"final":true,"note":null}`,
		"nested":             `{"type":"response.create","response":{"modalities":["audio"]}}`,
		"escaped type":       `{"type":"response.create"}`,
		"escaped other text": `{"type":"response.create","note":"a\"b"}`,
		"seventeen fields":   `{"type":"response.create",` + numbered(16) + `}`,
	}
	for name, frame := range cases {
		t.Run(name, func(t *testing.T) {
			var tracker realtimeresponses.Tracker
			client(&tracker, frame)
			if !tracker.Pending() {
				t.Fatal("the response request was not counted")
			}
			if usage := provider(&tracker, true, done("resp_1", `{"input_tokens":1,"output_tokens":1}`)); usage == nil || tracker.Pending() {
				t.Fatalf("one terminal event gave usage %+v and pending %v, so the request was not counted exactly once", usage, tracker.Pending())
			}
		})
	}
}

func numbered(count int) string {
	fields := make([]string, count)
	for i := range fields {
		fields[i] = `"f` + string(rune('a'+i)) + `":"v"`
	}
	return strings.Join(fields, ",")
}

func TestClientFramesThatAskForNoResponseOweNothing(t *testing.T) {
	var tracker realtimeresponses.Tracker
	client(&tracker,
		`{"type":"input_audio_buffer.append","audio":"AAAA"}`,
		`{"type":"input_audio_buffer.commit"}`,
		`{"type":"session.update","session":{"voice":"alloy"}}`,
		`{"note":"no type"}`,
	)
	if tracker.Pending() {
		t.Fatal("a frame other than response.create owes a response")
	}
}

func TestClientFramesTheTrackerCannotReadLeaveTheSessionIncomplete(t *testing.T) {
	cases := map[string]func(*realtimeresponses.Tracker){
		"binary":         func(tr *realtimeresponses.Tracker) { tr.ClientFrame(websocket.MessageBinary, []byte(create)) },
		"oversized":      func(tr *realtimeresponses.Tracker) { client(tr, `{"type":"x","pad":"`+strings.Repeat("a", 1<<20)+`"}`) },
		"malformed":      func(tr *realtimeresponses.Tracker) { client(tr, `{"type":`) },
		"duplicate type": func(tr *realtimeresponses.Tracker) { client(tr, `{"type":"session.update","type":"response.create"}`) },
		"type alias":     func(tr *realtimeresponses.Tracker) { client(tr, `{"TYPE":"session.update","type":"response.create"}`) },
		"type not text":  func(tr *realtimeresponses.Tracker) { client(tr, `{"type":{"name":"response.create"}}`) },
		"marked by the caller": func(tr *realtimeresponses.Tracker) {
			tr.Untrackable()
		},
		"binary from the provider": func(tr *realtimeresponses.Tracker) {
			tr.ProviderFrame(websocket.MessageBinary, []byte(`{}`), false)
		},
	}
	for name, send := range cases {
		t.Run(name, func(t *testing.T) {
			var tracker realtimeresponses.Tracker
			send(&tracker)
			if !tracker.Pending() {
				t.Fatal("the session is complete after a frame nobody could read")
			}
			provider(&tracker, true, done("resp_1", `{"input_tokens":1,"output_tokens":1}`))
			if !tracker.Pending() {
				t.Fatal("a later terminal event cleared the uncertainty")
			}
		})
	}
}

func TestConcurrentResponsesFinishIndependently(t *testing.T) {
	usage := `{"input_tokens":1,"output_tokens":1}`
	var tracker realtimeresponses.Tracker
	client(&tracker, create, create)
	provider(&tracker, true, created("resp_a"), created("resp_b"), done("resp_a", usage))
	if !tracker.Pending() {
		t.Fatal("the second response is no longer owed after the first finished")
	}
	provider(&tracker, true, done("resp_b", usage))
	if tracker.Pending() {
		t.Fatal("both responses finished and one is still owed")
	}
}

func TestAResponseTheProviderStartsUnaskedIsOwedUntilItFinishes(t *testing.T) {
	usage := `{"input_tokens":1,"output_tokens":1}`
	var tracker realtimeresponses.Tracker
	provider(&tracker, true, `{"type":"response.output_text.delta","response_id":"resp_vad","delta":"hi"}`)
	if !tracker.Pending() {
		t.Fatal("a response the provider started on its own is not owed")
	}
	provider(&tracker, true, done("resp_vad", usage))
	if tracker.Pending() {
		t.Fatal("the response finished and is still owed")
	}
}

func TestAnUnidentifiedFragmentIsSettledOnlyByAnUnambiguousTerminalEvent(t *testing.T) {
	usage := `{"input_tokens":1,"output_tokens":1}`
	fragment := `{"type":"response.output_audio.delta","delta":"AAAA"}`

	var single realtimeresponses.Tracker
	client(&single, create)
	provider(&single, true, fragment)
	if !single.Pending() {
		t.Fatal("an unfinished fragment is not owed")
	}
	provider(&single, true, `{"type":"response.done","response":{"usage":`+usage+`}}`)
	if single.Pending() {
		t.Fatal("the only response finished and is still owed")
	}

	var concurrent realtimeresponses.Tracker
	client(&concurrent, create, create)
	provider(&concurrent, true, created("resp_a"), created("resp_b"), fragment, done("resp_a", usage), done("resp_b", usage))
	if !concurrent.Pending() {
		t.Fatal("a fragment that could belong to either response was treated as delivered")
	}
}

func TestATerminalEventThatMatchesNoKnownResponseLeavesTheSessionIncomplete(t *testing.T) {
	usage := `{"input_tokens":1,"output_tokens":1}`
	var tracker realtimeresponses.Tracker
	client(&tracker, create)
	provider(&tracker, true, created("resp_a"), done("resp_other", usage), done("resp_a", usage))
	if !tracker.Pending() {
		t.Fatal("a terminal event for an unknown response was accepted while another was running")
	}
}

func TestTrackingIsBoundedAndOverflowLeavesTheSessionIncomplete(t *testing.T) {
	usage := `{"input_tokens":1,"output_tokens":1}`
	const bound = 128

	var requested realtimeresponses.Tracker
	for range bound {
		client(&requested, create)
	}
	for range bound {
		provider(&requested, true, `{"type":"response.done","response":{"usage":`+usage+`}}`)
	}
	if requested.Pending() {
		t.Fatalf("%d requests answered by %d terminal events are still owed", bound, bound)
	}
	for range bound + 1 {
		client(&requested, create)
	}
	for range bound + 1 {
		provider(&requested, true, `{"type":"response.done","response":{"usage":`+usage+`}}`)
	}
	if !requested.Pending() {
		t.Fatal("more requests than the tracker can count were treated as all delivered")
	}

	var started realtimeresponses.Tracker
	for i := range bound + 1 {
		provider(&started, true, created("resp_"+strings.Repeat("x", i+1)))
	}
	for i := range bound + 1 {
		provider(&started, true, done("resp_"+strings.Repeat("x", i+1), usage))
	}
	if !started.Pending() {
		t.Fatal("more responses than the tracker can hold were treated as all delivered")
	}

	var long realtimeresponses.Tracker
	provider(&long, true, created(strings.Repeat("x", 513)), done(strings.Repeat("x", 513), usage))
	if !long.Pending() {
		t.Fatal("an identifier too long to track was treated as delivered")
	}
}
