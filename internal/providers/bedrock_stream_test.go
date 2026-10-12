package providers

import (
	"bytes"
	"encoding/base64"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

func TestBedrockCertificationRequiresNativeTerminal(t *testing.T) {
	for _, test := range []struct {
		name, kind, payload string
		failure, valid      bool
	}{
		{"converse terminal", "messageStop", `{"stopReason":"end_turn"}`, false, true},
		{"invoke terminal", "chunk", `{"bytes":"` + base64.StdEncoding.EncodeToString([]byte(`{"type":"message_stop"}`)) + `"}`, false, true},
		{"missing terminal", "contentBlockDelta", `{"delta":{"text":"partial"}}`, false, false},
		{"invalid terminal", "messageStop", `{"stopReason":null}`, false, false},
		{"native exception", "modelStreamErrorException", `{"message":"failed"}`, true, false},
		{"malformed payload", "messageStop", `{"stopReason":"end_turn",`, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var body bytes.Buffer
			headers := eventstream.Headers{}
			messageType := "event"
			if test.failure {
				messageType = "exception"
			}
			headers.Set(":message-type", eventstream.StringValue(messageType))
			headers.Set(":event-type", eventstream.StringValue(test.kind))
			if err := eventstream.NewEncoder().Encode(&body, eventstream.Message{Headers: headers, Payload: []byte(test.payload)}); err != nil {
				t.Fatal(err)
			}
			if err := readBedrockProbeStream(body.Bytes(), 64<<10); (err == nil) != test.valid {
				t.Fatalf("native certification error = %v, valid = %v", err, test.valid)
			}
		})
	}
	if err := readBedrockProbeStream(nil, 64<<10); err == nil {
		t.Fatal("empty stream certified")
	}
}
