package gateway

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/tyk-swe/olp/internal/protocols"
)

func bedrockEvent(t *testing.T, kind string, payload any) *eventstream.Message {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	h := eventstream.Headers{}
	h.Set(":message-type", eventstream.StringValue("event"))
	h.Set(":event-type", eventstream.StringValue(kind))
	return &eventstream.Message{Headers: h, Payload: raw}
}

func bedrockChunk(t *testing.T, inner string) *eventstream.Message {
	t.Helper()
	return bedrockEvent(t, "chunk", map[string]string{"bytes": base64.StdEncoding.EncodeToString([]byte(inner))})
}

func foldBedrockUsage(t *testing.T, messages ...*eventstream.Message) *wire.Usage {
	t.Helper()
	var usage *wire.Usage
	for _, message := range messages {
		observation, err := protocols.ObserveBedrockEvent(message, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		usage, err = observation.Usage.Merge(usage)
		if err != nil {
			t.Fatal(err)
		}
	}
	return usage
}

func TestBedrockTitanImageSettlesEstimate(t *testing.T) {
	image := []byte(`{"images":["aW1hZ2U="]}`)
	recorded := AttemptFact{Class: classSuccess, Committed: true}
	recorded.recordEvidence(true)
	for name, fact := range map[string]AttemptFact{
		"unmarked success": {Class: classSuccess, Committed: true},
		"recorded success": recorded,
	} {
		x := &call{estimate: 900, dispatched: true, facts: []AttemptFact{fact}}
		if e := (&Server{}).validateBedrockInvoke(x, "amazon.titan-image-generator-v2:0", image); e != nil {
			t.Fatalf("%s: validate: %+v", name, e)
		}
		if got := x.settledTokens(); got == nil || *got != 900 {
			t.Fatalf("%s: Titan image invoke settled %v tokens, want the 900-token estimate", name, got)
		}
	}
}

// bedrockTestUsage exercises the operation-owned projection, not a second
// gateway parser.
func bedrockTestUsage(message *eventstream.Message) (*wire.Usage, bool) {
	observation, err := protocols.ObserveBedrockEvent(message, 1<<20)
	if err != nil {
		return nil, false
	}
	return observation.Usage.Value, observation.Usage.Authoritative
}
