package messages

import (
	"bytes"
	"encoding/json"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"github.com/tyk-swe/olp/internal/oif"
)

type Kind uint8

const (
	RequestMessage Kind = iota + 1
	Notification
	Response
)

type Message struct {
	Kind   Kind
	ID     json.RawMessage
	Method string
	Params json.RawMessage
	Result json.RawMessage
	Error  json.RawMessage
}

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (r Request) Valid() bool {
	return r.JSONRPC == "2.0" && r.Method != "" && (len(r.ID) == 0 || ValidID(r.ID))
}

type Envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func IDOrNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func ValidID(raw json.RawMessage) bool {
	value := bytes.TrimSpace(raw)
	if len(value) == 0 || !json.Valid(value) {
		return false
	}
	return value[0] == '"' || value[0] == '-' || value[0] >= '0' && value[0] <= '9'
}

func Parse(raw []byte, limits oif.Limits) (Message, int) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] == '[' {
		return Message{}, jsonrpc.CodeInvalidRequest
	}
	document, err := oif.ParseJSON(raw, limits)
	if err != nil {
		return Message{}, jsonrpc.CodeParseError
	}
	root := document.Root()
	version, _ := root.Lookup("jsonrpc")
	if text, ok := version.Text(); !ok || text != "2.0" {
		return Message{}, jsonrpc.CodeInvalidRequest
	}
	id, hasID := root.Lookup("id")
	if hasID && !ValidID(id.Bytes()) {
		return Message{}, jsonrpc.CodeInvalidRequest
	}
	method, hasMethod := root.Lookup("method")
	params, _ := root.Lookup("params")
	if hasMethod {
		name, ok := method.Text()
		if !ok || name == "" {
			return Message{}, jsonrpc.CodeInvalidRequest
		}
		kind := func() Kind {
			if hasID {
				return RequestMessage
			}
			return Notification
		}()
		return Message{Kind: kind, ID: id.Bytes(), Method: name, Params: params.Bytes()}, 0
	}
	result, hasResult := root.Lookup("result")
	failure, hasError := root.Lookup("error")
	if !hasID || !hasResult && !hasError {
		return Message{}, jsonrpc.CodeInvalidRequest
	}
	return Message{Kind: Response, ID: id.Bytes(), Result: result.Bytes(), Error: failure.Bytes()}, 0
}

func SupportedProtocols() []string {
	return []string{"2025-11-25", "2025-06-18", "2025-03-26"}
}

func NegotiateProtocol(requested string) string {
	supported := SupportedProtocols()
	if slices.Contains(supported, requested) {
		return requested
	}
	return supported[0]
}
