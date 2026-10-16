package protocols

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func TestBedrockImageFormatsAndRefusals(t *testing.T) {
	for _, format := range []string{"png", "jpeg", "gif", "webp"} {
		t.Run(format, func(t *testing.T) {
			part := Part{MIME: "image/" + format, URL: "data:image/" + format + ";base64,aW1hZ2U="}
			body, err := encodeBedrock(&Generation{Messages: []Message{{Role: "user", Parts: []Part{part}}}}, "model", "generation")
			if err != nil || !bytes.Contains(raw(body), []byte(fmt.Sprintf(`"format":%q`, format))) {
				t.Fatalf("image format lost: %s %v", raw(body), err)
			}
		})
	}
	for _, tc := range []struct {
		name, role, mime, url, detail string
	}{
		{"remote URL", "user", "image/png", "https://example.com/image.png", ""},
		{"image detail", "user", "image/png", "data:image/png;base64,aW1hZ2U=", "high"},
		{"unsupported MIME", "user", "image/svg+xml", "data:image/svg+xml;base64,aW1hZ2U=", ""},
		{"invalid base64", "user", "image/png", "data:image/png;base64,invalid!", ""},
		{"system image", "system", "image/png", "data:image/png;base64,aW1hZ2U=", ""},
		{"tool result image", "tool", "image/png", "data:image/png;base64,aW1hZ2U=", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := Message{Role: tc.role, Parts: []Part{{Text: "result"}, {MIME: tc.mime, URL: tc.url, Detail: tc.detail}}}
			if tc.role == "tool" {
				message.ToolID = "tool-1"
			}
			for _, operation := range []string{"generation", "token_count"} {
				if _, err := encodeBedrock(&Generation{Messages: []Message{message}}, "model", operation); err == nil {
					t.Errorf("%s accepted unsupported %s", operation, tc.name)
				}
			}
		})
	}
}

func encodeBedrockMessages(t *testing.T, family wire.Format, body string) []map[string]json.RawMessage {
	t.Helper()
	request, err := Parse(family, []byte(body), "route")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _, err := Encode(request, "bedrock", "amazon-bedrock", "wire-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	var encodedWire struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(encoded, &encodedWire); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(encodedWire.Messages); i++ {
		if string(encodedWire.Messages[i]["role"]) == string(encodedWire.Messages[i-1]["role"]) {
			t.Fatalf("adjacent %s messages: %s", encodedWire.Messages[i]["role"], encoded)
		}
	}
	return encodedWire.Messages
}

func TestReadBedrockEventTruncatedAfterPrelude(t *testing.T) {
	encode := func(buffer *bytes.Buffer, event, payload string) {
		t.Helper()
		headers := eventstream.Headers{{Name: ":message-type", Value: eventstream.StringValue("event")}, {Name: ":event-type", Value: eventstream.StringValue(event)}}
		if err := eventstream.NewEncoder().Encode(buffer, eventstream.Message{Headers: headers, Payload: []byte(payload)}); err != nil {
			t.Fatal(err)
		}
	}
	var metadata bytes.Buffer
	encode(&metadata, "metadata", `{"usage":{"inputTokens":1,"outputTokens":2}}`)
	prelude := metadata.Bytes()[:12]
	if _, err := ReadBedrockEvent(bytes.NewReader(prelude), 4096); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v", err)
	}

	var stream bytes.Buffer
	encode(&stream, "messageStart", `{"role":"assistant"}`)
	encode(&stream, "messageStop", `{"stopReason":"end_turn"}`)
	stream.Write(prelude)
	_, err := Stream(wire.Bedrock, wire.Chat, bytes.NewReader(stream.Bytes()), 4096, "route", true, func([]byte) error { return nil })
	var protocolErr *wire.ProtocolError
	if !errors.As(err, &protocolErr) || !protocolErr.Truncated {
		t.Fatalf("stream err = %v", err)
	}
}
