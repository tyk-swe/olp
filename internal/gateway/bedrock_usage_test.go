package gateway

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
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
