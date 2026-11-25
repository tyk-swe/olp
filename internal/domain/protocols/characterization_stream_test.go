package protocols_test

import (
	"bytes"
	"testing"
	"unicode/utf8"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

var streamTargets = []wire.Format{wire.Chat, wire.Completions, wire.Responses, wire.Anthropic, wire.Gemini, wire.GeminiStream, wire.Bedrock, wire.MistralFIM, wire.CohereChat}

type binaryFrame struct {
	Event   string `json:"event,omitempty"`
	Payload string `json:"payload,omitempty"`
	Digest  string `json:"digest"`
}

type streamOutcome struct {
	Frames     []any     `json:"frames"`
	Events     int       `json:"events"`
	Error      *failure  `json:"error,omitempty"`
	Completion *observed `json:"completion,omitempty"`
}

func recordFrame(frame []byte) any {
	if utf8.Valid(frame) {
		return string(frame)
	}
	message, err := protocols.ReadBedrockEvent(bytes.NewReader(frame), len(frame))
	if err != nil {
		return binaryFrame{Digest: digest(frame)}
	}
	kind := message.Headers.Get(":event-type")
	if kind == nil {
		return binaryFrame{Payload: string(message.Payload), Digest: digest(frame)}
	}
	return binaryFrame{Event: kind.String(), Payload: string(message.Payload), Digest: digest(frame)}
}

type streamRecorder struct {
	raw    bytes.Buffer
	frames []any
	events int
}

func (r *streamRecorder) emit(frame []byte) error {
	r.raw.Write(frame)
	r.frames = append(r.frames, recordFrame(bytes.Clone(frame)))
	return nil
}

func (r *streamRecorder) observe(oif.Event) error {
	r.events++
	return nil
}

func runStream(source, target wire.Format, body []byte, includeUsage bool) (streamOutcome, []byte) {
	recorder := &streamRecorder{frames: []any{}}
	completion, err := protocols.StreamWithEvents(source, target, bytes.NewReader(body), eventLimit, routeModel, includeUsage, recorder.emit, recorder.observe)
	return streamOutcome{Frames: recorder.frames, Events: recorder.events, Error: classify(err), Completion: observe(completion)}, recorder.raw.Bytes()
}

func TestStreamTranslationMatrix(t *testing.T) {
	t.Parallel()
	var cases []goldenCase
	for _, source := range streamSources() {
		for _, scenario := range source.scenarios {
			rows := newMatrix(preferred(source.format, streamTargets), func(target wire.Format) (any, bool) {
				outcome, _ := runStream(source.format, target, scenario.body, true)
				return outcome, outcome.Error == nil
			})
			for _, target := range rows.stored {
				cases = append(cases, goldenCase{name: string(source.format) + " to " + string(target) + ", " + scenario.name, run: func(*testing.T) any {
					return rows.row(target)
				}})
			}
		}
		for _, target := range []wire.Format{wire.Chat, wire.Completions} {
			scenario := source.scenarios[0]
			cases = append(cases, goldenCase{name: string(source.format) + " to " + string(target) + ", " + scenario.name + ", usage not requested", run: func(*testing.T) any {
				outcome, _ := runStream(source.format, target, scenario.body, false)
				return outcome
			}})
		}
	}
	checkGolden(t, "testdata/stream_matrix.golden.json", cases)
}

func TestStreamAggregatesTranslatedResponses(t *testing.T) {
	t.Parallel()
	var cases []goldenCase
	for _, scenario := range []streamScenario{{"tool call", responsesToolStream}, {"refusal", responsesRefusalStream}} {
		rows := newMatrix(preferred("", streamTargets), func(target wire.Format) (any, bool) {
			completion, err := protocols.AggregateTranslatedResponses(target, bytes.NewReader(scenario.body), eventLimit, routeModel, nil)
			return unaryOutcome(completion, err), err == nil
		})
		for _, target := range rows.stored {
			cases = append(cases, goldenCase{name: string(target) + ", " + scenario.name, run: func(*testing.T) any {
				return rows.row(target)
			}})
		}
	}
	checkGolden(t, "testdata/stream_aggregate.golden.json", cases)
}

type decoded struct {
	Body       any       `json:"body,omitempty"`
	Error      *failure  `json:"error,omitempty"`
	Completion *observed `json:"completion,omitempty"`
}

func unaryOutcome(completion *wire.Completion, err error) decoded {
	if completion == nil {
		return decoded{Error: classify(err)}
	}
	return decoded{Body: document(completion.Body), Error: classify(err), Completion: observe(completion)}
}
