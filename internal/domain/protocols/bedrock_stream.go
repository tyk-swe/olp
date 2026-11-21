package protocols

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/awsframe"
	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

// ReadBedrockEvent checks the advertised length before the maintained AWS
// decoder allocates headers or payload, and lets that decoder verify both CRCs.
func ReadBedrockEvent(r io.Reader, limit int) (eventstream.Message, error) {
	var prelude [12]byte
	if _, e := io.ReadFull(r, prelude[:]); e != nil {
		return eventstream.Message{}, e
	}
	size := binary.BigEndian.Uint32(prelude[:4])
	if size > uint32(limit) {
		return eventstream.Message{}, wire.ErrEventTooLarge
	}
	if size < 16 {
		return eventstream.Message{}, protocolError("invalid AWS event length")
	}
	headerBytes := binary.BigEndian.Uint32(prelude[4:8])
	if headerBytes > size-16 {
		return eventstream.Message{}, protocolError("invalid AWS header length")
	}
	frame := make([]byte, int(size))
	copy(frame, prelude[:])
	if _, err := io.ReadFull(r, frame[12:]); err != nil {
		// A stream that ends after a whole prelude is truncated, not finished.
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return eventstream.Message{}, err
	}
	if err := awsframe.ValidateHeaders(frame[12 : 12+headerBytes]); err != nil {
		return eventstream.Message{}, err
	}
	return eventstream.NewDecoder().Decode(bytes.NewReader(frame), nil)
}

type bedrockStreamParser struct {
	c        *wire.Completion
	limit    int
	started  bool
	stopped  bool
	blocks   map[int64]*contentBlock
	retained int
}

func (p *bedrockStreamParser) apply(kind string, f Object) error {
	switch kind {
	case "messageStart":
		if p.started || str(f["role"]) != "assistant" {
			return protocolError("invalid Converse start")
		}
		p.started = true
	case "contentBlockStart":
		index, ok := count(f["contentBlockIndex"])
		if !p.started || p.stopped || !ok || p.blocks[index] != nil || len(p.blocks) >= max(1, p.limit/32) {
			return protocolError("invalid Converse block start")
		}
		p.blocks[index] = &contentBlock{}
	case "contentBlockDelta":
		return p.applyDelta(f)
	case "contentBlockStop":
		index, ok := count(f["contentBlockIndex"])
		block := p.blocks[index]
		if !ok || block == nil || p.stopped {
			return protocolError("invalid Converse block stop")
		}
		if block.args.Len() != 0 {
			if _, e := object([]byte(block.args.String())); e != nil {
				return protocolError("incomplete Converse tool arguments")
			}
		}
		p.retained -= block.args.Len()
		delete(p.blocks, index)
	case "messageStop":
		if !p.started || p.stopped || len(p.blocks) > 0 || str(f["stopReason"]) == "" {
			return protocolError("invalid Converse stop")
		}
		p.stopped = true
		p.c.FinishReason = bedrockFinish(str(f["stopReason"]))
	case "metadata":
		if !p.stopped {
			return protocolError("Converse metadata before stop")
		}
	default:
		if strings.HasSuffix(kind, "Exception") {
			return &wire.UpstreamError{Code: kind, Message: "Bedrock stream failed"}
		}
	}
	return nil
}

func (p *bedrockStreamParser) applyDelta(f Object) error {
	index, ok := count(f["contentBlockIndex"])
	if !p.started || p.stopped || !ok {
		return protocolError("invalid Converse delta")
	}
	block := p.blocks[index]
	if block == nil {
		if len(p.blocks) >= max(1, p.limit/32) {
			return protocolError("too many Converse blocks")
		}
		block = &contentBlock{}
		p.blocks[index] = block
	}
	delta, _ := optionalObject(f["delta"])
	call, e := optionalObject(delta["toolUse"])
	if e != nil {
		return e
	}
	if call != nil {
		part := str(call["input"])
		if p.retained+len(part) > p.limit {
			return wire.ErrEventTooLarge
		}
		p.retained += len(part)
		block.args.WriteString(part)
	}
	return nil
}

func bedrockHeader(message eventstream.Message, name string) string {
	if v := message.Headers.Get(name); v != nil {
		return v.String()
	}
	return ""
}

func encodeBedrockStreamFrame(native bool, message eventstream.Message, kind string, f Object) ([]byte, error) {
	if native {
		var encoded bytes.Buffer
		if err := eventstream.NewEncoder().Encode(&encoded, message); err != nil {
			return nil, err
		}
		return encoded.Bytes(), nil
	}
	f["type"] = raw(kind)
	return eventFrame(kind, f), nil
}

func streamBedrockEvents(r io.Reader, limit int, route string, emit wire.Emit, observe func(oif.Event) error, native bool) (*wire.Completion, error) {
	p := &bedrockStreamParser{c: &wire.Completion{ProviderModel: route}, limit: limit, blocks: map[int64]*contentBlock{}}
	sequence := uint64(0)
	for {
		message, e := ReadBedrockEvent(r, limit)
		if errors.Is(e, io.EOF) {
			if p.stopped {
				return p.c, nil
			}
			return p.c, &wire.ProtocolError{Detail: "Converse stream ended before messageStop", Truncated: true}
		}
		if e != nil {
			if errors.Is(e, wire.ErrEventTooLarge) {
				return p.c, e
			}
			if errors.Is(e, io.ErrUnexpectedEOF) {
				return p.c, &wire.ProtocolError{Detail: "truncated AWS event", Truncated: true}
			}
			return p.c, protocolError("invalid bounded AWS event stream")
		}
		if bedrockHeader(message, ":message-type") != "event" {
			return p.c, &wire.UpstreamError{Code: bedrockHeader(message, ":exception-type"), Message: "Bedrock stream failed"}
		}
		kind := bedrockHeader(message, ":event-type")
		event, e := openai.LiftEvent(wire.Bedrock, string(message.Payload), kind, sequence, limit)
		if e != nil {
			return p.c, e
		}
		sequence++
		f := event.Source().Fields()
		var accountingErr error
		if kind == "metadata" && p.stopped && f != nil {
			var observed *wire.Usage
			observed, accountingErr = bedrockUsage(f["usage"])
			if accountingErr == nil {
				p.c.Usage = observed
				p.c.UsageComplete = observed != nil
			}
		}
		if observe != nil {
			if e := observe(event); e != nil {
				return p.c, e
			}
		}
		if accountingErr != nil {
			return p.c, accountingErr
		}
		if f == nil {
			return p.c, protocolError("invalid AWS event payload")
		}
		if err := p.apply(kind, f); err != nil {
			return p.c, err
		}
		frame, err := encodeBedrockStreamFrame(native, message, kind, f)
		if err != nil {
			return p.c, err
		}
		if e := emit(frame); e != nil {
			return p.c, e
		}
	}
}

func (t *streamTranslator) bedrockFrame(f Object) error {
	index, _ := count(f["contentBlockIndex"])
	switch str(f["type"]) {
	case "messageStart":
		return t.start("")
	case "contentBlockStart":
		start, _ := optionalObject(f["start"])
		call, _ := optionalObject(start["toolUse"])
		if call != nil {
			return t.toolDelta(int(index), str(call["toolUseId"]), str(call["name"]), "")
		}
	case "contentBlockDelta":
		delta, _ := optionalObject(f["delta"])
		if e := t.textDelta(str(delta["text"])); e != nil {
			return e
		}
		call, _ := optionalObject(delta["toolUse"])
		if call != nil {
			return t.toolDelta(int(index), "", "", str(call["input"]))
		}
	}
	return nil
}
