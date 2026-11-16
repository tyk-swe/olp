package coderesearch

import (
	"bytes"
	"encoding/json"
)

// MessageID extracts an ID from a request already validated by Request.
func MessageID(raw []byte) json.RawMessage {
	var message struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(raw, &message) != nil {
		return nil
	}
	return message.ID
}

// Initialized accepts only a successful InitializeResult for this request.
// Notifications, JSON-RPC errors and malformed or mismatched replies cannot
// establish durable session state.
func Initialized(raw []byte, requestID json.RawMessage) bool {
	m, err := object(raw, MaxResponse)
	if err != nil || string(m["jsonrpc"]) != `"2.0"` || len(m["method"]) != 0 || len(m["error"]) != 0 || !sameID(m["id"], requestID) {
		return false
	}
	result, err := object(m["result"], MaxResponse)
	if err != nil {
		return false
	}
	var protocol string
	if json.Unmarshal(result["protocolVersion"], &protocol) != nil || protocol == "" {
		return false
	}
	if _, err := object(result["capabilities"], MaxResponse); err != nil {
		return false
	}
	var info struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	return json.Unmarshal(result["serverInfo"], &info) == nil && info.Name != "" && info.Version != ""
}

// PingResponse handles only server liveness requests. It does not enable any
// other server callback or accept arbitrary client-supplied RPC responses.
func PingResponse(raw []byte) ([]byte, error) {
	m, err := object(raw, MaxResponse)
	if err != nil {
		return nil, err
	}
	var method string
	if json.Unmarshal(m["method"], &method) != nil || method != "ping" {
		return nil, nil
	}
	if string(m["jsonrpc"]) != `"2.0"` || !validID(m["id"]) || len(m["result"]) != 0 || len(m["error"]) != 0 {
		return nil, ErrProtocol
	}
	if params, ok := m["params"]; ok {
		if _, err := object(params, MaxResponse); err != nil {
			return nil, err
		}
	}
	return json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": struct{}{}})
}

func validID(raw json.RawMessage) bool {
	value, ok := decodeID(raw)
	return ok && value != nil
}

func sameID(left, right json.RawMessage) bool {
	a, aOK := decodeID(left)
	b, bOK := decodeID(right)
	return aOK && bOK && a == b
}

func decodeID(raw json.RawMessage) (any, bool) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return nil, false
	}
	switch value.(type) {
	case string, json.Number:
		return value, true
	default:
		return nil, false
	}
}
