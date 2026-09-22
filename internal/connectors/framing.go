package connectors

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"

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
	// Inspect original header entries before the SDK decoder's map-like Set
	// operation can collapse duplicate pseudo-headers.
	headerBytes := binary.BigEndian.Uint32(prelude[4:8])
	if headerBytes > total-16 {
		return 0, errors.New("malformed Bedrock event headers")
	}
	frameBytes := make([]byte, int(total))
	copy(frameBytes, prelude[:])
	if _, err := io.ReadFull(r.reader, frameBytes[12:]); err != nil {
		return 0, err
	}
	if err := validateBedrockHeaderMembers(frameBytes[12 : 12+headerBytes]); err != nil {
		return 0, err
	}
	message, err := eventstream.NewDecoder().Decode(bytes.NewReader(frameBytes), nil)
	if err != nil {
		return 0, errors.New("malformed Bedrock event framing")
	}
	messageType, eventType := "", ""
	seenHeaders := map[string]bool{}
	for _, header := range message.Headers {
		if header.Name == ":message-type" || header.Name == ":event-type" {
			if seenHeaders[header.Name] {
				return 0, errors.New("ambiguous Bedrock event header")
			}
			seenHeaders[header.Name] = true
		}
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
	if _, err := oif.ParseJSON(message.Payload, oif.Limits{MaxBytes: r.limit}); err != nil {
		return 0, errors.New("invalid or ambiguous Bedrock chunk envelope")
	}
	var envelope struct {
		Bytes string `json:"bytes"`
	}
	decoder := json.NewDecoder(bytes.NewReader(message.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil || envelope.Bytes == "" || decoder.Decode(new(any)) != io.EOF {
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

func validateBedrockHeaderMembers(data []byte) error {
	seen := map[string]bool{}
	invalid := errors.New("malformed or ambiguous Bedrock event headers")
	for len(data) > 0 {
		length := int(data[0])
		data = data[1:]
		if length == 0 || len(data) < length+1 {
			return invalid
		}
		name := string(data[:length])
		kind := data[length]
		data = data[length+1:]
		if name == ":message-type" || name == ":event-type" {
			if seen[name] || kind != 7 {
				return invalid
			}
			seen[name] = true
		}
		width := 0
		switch kind {
		case 0, 1:
		case 2:
			width = 1
		case 3:
			width = 2
		case 4:
			width = 4
		case 5, 8:
			width = 8
		case 9:
			width = 16
		case 6, 7:
			if len(data) < 2 {
				return invalid
			}
			width = 2 + int(binary.BigEndian.Uint16(data[:2]))
		default:
			return invalid
		}
		if len(data) < width {
			return invalid
		}
		data = data[width:]
	}
	return nil
}
