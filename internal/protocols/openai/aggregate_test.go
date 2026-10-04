package openai

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// responsesEvents renders Responses events as an upstream streams them.
func responsesEvents(events ...string) string {
	var stream strings.Builder
	for _, event := range events {
		kind, _, _ := strings.Cut(strings.TrimPrefix(event, `{"type":"`), `"`)
		fmt.Fprintf(&stream, "event: %s\ndata: %s\n\n", kind, event)
	}
	return stream.String()
}

const (
	messageItem  = `{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello there","annotations":[]}]}`
	callItem     = `{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_1","name":"weather","arguments":"{\"city\":\"Paris\"}"}`
	streamUsage  = `{"input_tokens":7,"output_tokens":5,"total_tokens":12}`
	createdEvent = `{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","status":"in_progress","model":"acme-large","output":[]}}`
)

func terminalEvent(kind, status, output string) string {
	return `{"type":"` + kind + `","sequence_number":9,"response":{"id":"resp_1","object":"response","status":"` + status + `","model":"acme-large","output":` + output + `,"usage":` + streamUsage + `}}`
}

func itemDone(index int, item string) string {
	return fmt.Sprintf(`{"type":"response.output_item.done","sequence_number":%d,"output_index":%d,"item":%s}`, 4+index, index, item)
}

func TestAggregatedResponsesStreamIsTheTerminalResponse(t *testing.T) {
	stream := responsesEvents(createdEvent,
		`{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"in_progress","content":[]}}`,
		`{"type":"response.output_text.delta","sequence_number":2,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hello there"}`,
		itemDone(0, messageItem),
		terminalEvent("response.completed", "completed", "["+messageItem+"]"))
	c, err := Aggregate(FamilyResponses, strings.NewReader(stream), 4096, 4096)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"resp_1","object":"response","status":"completed","model":"acme-large","output":[` + messageItem + `],"usage":` + streamUsage + `}`
	if string(c.Body) != want || c.Usage == nil || c.Usage.TotalTokens != 12 {
		t.Fatalf("aggregated %s with usage %+v", c.Body, c.Usage)
	}
}

func TestAggregatedResponsesStreamFillsAnEmptyOutputWithTheCompletedItems(t *testing.T) {
	stream := responsesEvents(createdEvent,
		itemDone(1, callItem),
		itemDone(0, messageItem),
		terminalEvent("response.incomplete", "incomplete", "[]"))
	c, err := Aggregate(FamilyResponses, strings.NewReader(stream), 4096, 4096)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeResponse(c.Body, "team-model")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"resp_1","model":"team-model","object":"response","output":[` + messageItem + `,` + callItem + `],"status":"incomplete","usage":` + streamUsage + `}`
	if string(decoded.Body) != want || decoded.OutputText != "Hello there" || len(decoded.ToolCalls) != 1 || decoded.Usage.TotalTokens != 12 {
		t.Fatalf("aggregated %s", decoded.Body)
	}
}

// A stream that fails or stops short has no result, whatever it delivered
// first, and reports the usage it stated as a streaming caller would see it.
func TestAggregationFailsWithTheStream(t *testing.T) {
	failed := `{"type":"response.failed","sequence_number":9,"response":{"id":"resp_1","object":"response","status":"failed","model":"acme-large","output":[],"error":{"code":"server_error","message":"The model failed."},"usage":` + streamUsage + `}}`
	protocolError := func(err error) bool {
		_, ok := errors.AsType[*ProtocolError](err)
		return ok
	}
	for name, tc := range map[string]struct {
		stream string
		check  func(error) bool
		usage  bool
	}{
		"ends early": {responsesEvents(createdEvent, itemDone(0, messageItem)), func(err error) bool {
			protocol, ok := errors.AsType[*ProtocolError](err)
			return ok && protocol.Truncated
		}, false},
		"fails in band": {responsesEvents(createdEvent, itemDone(0, messageItem), failed), func(err error) bool {
			stated, ok := errors.AsType[*UpstreamError](err)
			return ok && stated.Code == "server_error"
		}, true},
		"completes an item twice": {responsesEvents(createdEvent, itemDone(0, messageItem), itemDone(0, messageItem), terminalEvent("response.completed", "completed", "[]")), protocolError, false},
		"item without its index":  {responsesEvents(createdEvent, `{"type":"response.output_item.done","sequence_number":1,"item":`+messageItem+`}`, terminalEvent("response.completed", "completed", "[]")), protocolError, false},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := Aggregate(FamilyResponses, strings.NewReader(tc.stream), 4096, 4096)
			if !tc.check(err) || c != nil && c.Body != nil || tc.usage != (c != nil && c.Usage != nil && c.Usage.TotalTokens == 12) {
				t.Fatalf("aggregated %+v: %v", c, err)
			}
		})
	}
}

func TestAggregationIsBounded(t *testing.T) {
	filled := responsesEvents(createdEvent, itemDone(0, messageItem), itemDone(1, callItem), terminalEvent("response.completed", "completed", "[]"))
	complete := responsesEvents(createdEvent, terminalEvent("response.completed", "completed", "["+messageItem+","+callItem+"]"))
	for name, stream := range map[string]string{"completed items": filled, "terminal response": complete} {
		t.Run(name, func(t *testing.T) {
			c, err := Aggregate(FamilyResponses, strings.NewReader(stream), 4096, 4096)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = Aggregate(FamilyResponses, strings.NewReader(stream), 4096, len(c.Body)); err != nil {
				t.Fatalf("a result at the bound: %v", err)
			}
			short, err := Aggregate(FamilyResponses, strings.NewReader(stream), 4096, len(c.Body)-1)
			if !errors.Is(err, ErrAggregateTooLarge) || short != nil && short.Body != nil {
				t.Fatalf("a result beyond the bound: %+v %v", short, err)
			}
		})
	}
	// Completed items count toward the bound as they arrive.
	items := responsesEvents(createdEvent, itemDone(0, messageItem), itemDone(1, callItem))
	if _, err := Aggregate(FamilyResponses, strings.NewReader(items), 4096, len(messageItem)+len(callItem)-1); !errors.Is(err, ErrAggregateTooLarge) {
		t.Fatalf("retained items beyond the bound: %v", err)
	}
}

func TestOnlyResponsesStreamsAggregate(t *testing.T) {
	if !Aggregates(FamilyResponses) {
		t.Fatal("Responses streams do not aggregate")
	}
	for _, family := range []Family{FamilyChat, FamilyAnthropic, FamilyGemini, FamilyBedrock} {
		if Aggregates(family) {
			t.Errorf("%s streams aggregate", family)
		}
		if _, err := Aggregate(family, strings.NewReader(""), 4096, 4096); err == nil {
			t.Errorf("aggregated a %s stream", family)
		}
	}
}

func TestAggregatedResponsesBoundsTinyItems(t *testing.T) {
	var stream strings.Builder
	stream.WriteString(responsesEvents(createdEvent))
	for i := 0; i <= maxAggregateItems; i++ {
		stream.WriteString(responsesEvents(itemDone(i, `{}`)))
	}
	c, err := Aggregate(FamilyResponses, strings.NewReader(stream.String()), 4096, 256<<20)
	if !errors.Is(err, ErrAggregateTooLarge) || c != nil && len(c.Body) != 0 {
		t.Fatalf("tiny item flood: completion=%+v error=%v", c, err)
	}
}
