package gateway

import (
	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

func bedrockMetadata(payload string) *eventstream.Message {
	h := eventstream.Headers{}
	h.Set(":message-type", eventstream.StringValue("event"))
	h.Set(":event-type", eventstream.StringValue("metadata"))
	return &eventstream.Message{Headers: h, Payload: []byte(payload)}
}
