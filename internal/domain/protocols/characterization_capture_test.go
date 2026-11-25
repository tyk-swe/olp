package protocols_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

type captured struct {
	Text          string            `json:"text,omitempty"`
	Calls         []json.RawMessage `json:"calls,omitempty"`
	Error         *failure          `json:"error,omitempty"`
	RejectedFrame int               `json:"rejected_frame,omitempty"`
}

func capturedOutcome(text string, calls []json.RawMessage, err error) captured {
	return captured{Text: text, Calls: calls, Error: classify(err)}
}

type captureModes struct {
	Both   captured `json:"output_and_tools"`
	Output captured `json:"output_only"`
	Tools  captured `json:"tools_only"`
}

func captureEach(run func(output, tools bool) captured) captureModes {
	return captureModes{Both: run(true, true), Output: run(true, false), Tools: run(false, true)}
}

func captureUnary(format wire.Format, body []byte, output, tools bool) captured {
	return capturedOutcome(protocols.CaptureResponse(format, body, output, tools))
}

func captureStream(format wire.Format, frames [][]byte, output, tools bool) captured {
	stream := protocols.NewCaptureStream(format, eventLimit, output, tools)
	for index, frame := range frames {
		if err := stream.Push(frame); err != nil {
			return captured{Error: classify(err), RejectedFrame: index + 1}
		}
	}
	return capturedOutcome(stream.Finish())
}

func serverSentFrames(body []byte) [][]byte {
	return bytes.SplitAfter(bytes.TrimSuffix(body, []byte("\n\n")), []byte("\n\n"))
}

func eventStreamFrames(body []byte) [][]byte {
	if len(body) < 4 {
		return nil
	}
	size := int(binary.BigEndian.Uint32(body))
	if size < 4 || size >= len(body) {
		return [][]byte{body}
	}
	return append([][]byte{body[:size]}, eventStreamFrames(body[size:])...)
}

func streamFrames(source wire.Format, body []byte) [][]byte {
	if source == wire.Bedrock {
		return eventStreamFrames(body)
	}
	return serverSentFrames(body)
}

func TestCaptureMatrix(t *testing.T) {
	t.Parallel()
	var cases []goldenCase
	for _, fixture := range responseFixtures {
		if fixture.format.Operation() != wire.OperationGeneration {
			continue
		}
		cases = append(cases, goldenCase{name: "unary " + string(fixture.format) + ", " + fixture.scenario, run: func(*testing.T) any {
			return captureEach(func(output, tools bool) captured {
				return captureUnary(fixture.format, []byte(fixture.body), output, tools)
			})
		}})
	}
	for _, source := range streamSources() {
		for _, scenario := range source.scenarios {
			cases = append(cases, goldenCase{name: "stream " + string(source.format) + ", " + scenario.name, run: func(*testing.T) any {
				frames := streamFrames(source.format, scenario.body)
				return captureEach(func(output, tools bool) captured {
					return captureStream(source.format, frames, output, tools)
				})
			}})
		}
	}
	checkGolden(t, "testdata/capture.golden.json", cases)
}
