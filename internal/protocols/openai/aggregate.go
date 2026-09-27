package openai

import (
	"encoding/json"
	"errors"
	"io"
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/oif"
)

// ErrAggregateTooLarge marks a stream whose non-streaming result exceeds the
// bound it is aggregated within.
var ErrAggregateTooLarge = errors.New("aggregated upstream stream exceeds the response size limit")

// aggregators reduce each family's event stream to its non-streaming result.
var aggregators = map[Family]func(r io.Reader, maxEventBytes, maxBytes int) (*Completion, error){
	FamilyResponses: aggregateResponses,
}

// Aggregates reports whether Aggregate reduces the family's streams.
func Aggregates(family Family) bool {
	_, ok := aggregators[family]
	return ok
}

// Aggregate reads an upstream's event stream to its end for a caller that did
// not ask to stream, and returns the family's non-streaming result as the
// completion's Body. The stream is validated as it is for a streaming caller.
// OIF keeps no accumulator, so the reducer retains at most maxBytes of the
// result and fails with ErrAggregateTooLarge beyond it. As with Stream, the
// completion summarizes what the stream reported, also on failure; a stream
// that fails or ends before its terminal event has no Body.
func Aggregate(family Family, r io.Reader, maxEventBytes, maxBytes int) (*Completion, error) {
	aggregate, ok := aggregators[family]
	if !ok {
		return nil, errors.New("no reducer aggregates this family's streams")
	}
	return aggregate(r, maxEventBytes, maxBytes)
}

// aggregateResponses reduces a Responses stream to the response its terminal
// event carries. An upstream may leave that response's output empty and
// deliver each item only in response.output_item.done, so an empty output
// becomes the items the stream completed, in output order.
func aggregateResponses(r io.Reader, maxEventBytes, maxBytes int) (*Completion, error) {
	var terminal json.RawMessage
	items := map[int64]json.RawMessage{}
	retained := 0
	c, err := StreamMetadataEvents(FamilyResponses, r, maxEventBytes, "", true, func([]byte) error { return nil }, func(event oif.Event) error {
		kind, _ := event.Source().Root().Lookup("type")
		switch text, _ := kind.Text(); text {
		case "response.output_item.done":
			fields := event.Source().Fields()
			index, indexed := int64Field(fields, "output_index")
			if _, err := object(fields["item"]); !indexed || err != nil {
				return &ProtocolError{Detail: "output item event has no output index or item"}
			}
			if _, done := items[index]; done {
				return &ProtocolError{Detail: "output item completed twice"}
			}
			if retained += len(fields["item"]); retained > maxBytes {
				return ErrAggregateTooLarge
			}
			items[index] = fields["item"]
		case "response.completed", "response.incomplete":
			terminal = event.Source().Fields()["response"]
		}
		return nil
	})
	if err != nil {
		return c, err
	}
	response, err := object(terminal)
	if err != nil {
		return c, &ProtocolError{Detail: "response is not an object"}
	}
	c.Body = terminal
	if output, _ := arrayField(response, "output"); len(output) == 0 && len(items) > 0 {
		completed := make([]json.RawMessage, 0, len(items))
		for _, index := range slices.Sorted(maps.Keys(items)) {
			completed = append(completed, items[index])
		}
		response["output"], _ = json.Marshal(completed)
		if c.Body, err = json.Marshal(response); err != nil {
			return c, err
		}
	}
	if len(c.Body) > maxBytes {
		c.Body = nil
		return c, ErrAggregateTooLarge
	}
	return c, nil
}
