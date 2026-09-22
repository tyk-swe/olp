package connectors

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

// EventStream reports the hosting framing independently from the payload dialect.
func (c Config) EventStream() bool {
	return c.Hosting() == "bedrock-converse" || c.Hosting() == "bedrock-anthropic-invoke"
}

// StreamPayload removes only the qualified hosting envelope, retaining the native
// Anthropic event document verbatim. Every event is bounded before allocating it;
// there are no goroutines, unbounded queues, fabricated terminal events or retries.
func (c Config) StreamPayload(reader io.Reader, maxEventBytes int) io.Reader {
	if c.Hosting() != "bedrock-anthropic-invoke" {
		return reader
	}
	return &anthropicInvokeReader{reader: reader, limit: maxEventBytes}
}

type anthropicInvokeReader struct {
	reader io.Reader
	limit  int
	frame  *bytes.Reader
}

func (r *anthropicInvokeReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.frame != nil && r.frame.Len() > 0 {
		return r.frame.Read(p)
	}
	var prelude [12]byte
	if _, err := io.ReadFull(r.reader, prelude[:]); err != nil {
		return 0, err
	}
	total := binary.BigEndian.Uint32(prelude[:4])
	if total < 16 || uint64(total) > uint64(r.limit) {
		return 0, errors.New("Bedrock event exceeds the configured event limit")
	}
	message, err := eventstream.NewDecoder().Decode(io.MultiReader(bytes.NewReader(prelude[:]), io.LimitReader(r.reader, int64(total)-12)), nil)
	if err != nil {
		return 0, errors.New("malformed Bedrock event framing")
	}
	messageType, eventType := "", ""
	for _, header := range message.Headers {
		if header.Name == ":message-type" {
			messageType = header.Value.String()
		}
		if header.Name == ":event-type" {
			eventType = header.Value.String()
		}
	}
	if messageType != "event" || eventType != "chunk" {
		return 0, errors.New("unexpected Bedrock event or upstream exception")
	}
	var envelope struct {
		Bytes string `json:"bytes"`
	}
	decoder := json.NewDecoder(bytes.NewReader(message.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil || envelope.Bytes == "" {
		return 0, errors.New("invalid Bedrock chunk envelope")
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(envelope.Bytes)
	if err != nil || len(payload) > r.limit {
		return 0, errors.New("invalid Bedrock chunk payload")
	}
	var native struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(payload, &native) != nil || native.Type == "" || len(native.Type) > 128 || strings.ContainsAny(native.Type, "\r\n\x00") {
		return 0, errors.New("native Bedrock chunk has no valid event identity")
	}
	// Anthropic payload objects cannot contain literal newlines inside strings;
	// JSON whitespace outside strings is compacted only for safe SSE framing.
	var compact bytes.Buffer
	if json.Compact(&compact, payload) != nil {
		return 0, errors.New("invalid native Bedrock event")
	}
	frame := []byte("event: " + native.Type + "\ndata: ")
	frame = append(frame, compact.Bytes()...)
	frame = append(frame, '\n', '\n')
	if len(frame) > r.limit {
		return 0, errors.New("unwrapped event exceeds the configured event limit")
	}
	r.frame = bytes.NewReader(frame)
	return r.frame.Read(p)
}
