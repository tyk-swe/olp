package research

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/oif"
)

const (
	MaxRequest  = 1 << 20
	MaxResponse = 8 << 20
	MaxEvent    = 1 << 20
)

var ErrProtocol = errors.New("invalid research MCP message")

func parseObject(raw []byte, maxBytes int) (map[string]json.RawMessage, error) {
	if _, err := oif.ParseJSON(raw, oif.Limits{MaxBytes: maxBytes, MaxDepth: 64, MaxNodes: 1 << 17}); err != nil {
		return nil, ErrProtocol
	}
	var message map[string]json.RawMessage
	if json.Unmarshal(raw, &message) != nil || message == nil {
		return nil, ErrProtocol
	}
	return message, nil
}

func Request(raw []byte, service string) ([]byte, string, error) {
	m, err := parseObject(raw, MaxRequest)
	if err != nil || string(m["jsonrpc"]) != `"2.0"` {
		return nil, "", ErrProtocol
	}
	if len(m["result"]) != 0 || len(m["error"]) != 0 {
		return nil, "", ErrProtocol
	}
	var method string
	if json.Unmarshal(m["method"], &method) != nil {
		return nil, "", ErrProtocol
	}
	id, request := m["id"]
	if request && !validID(id) {
		return nil, "", ErrProtocol
	}
	switch method {
	case "initialize", "tools/list", "tools/call", "ping":
		if !request {
			return nil, "", ErrProtocol
		}
	case "notifications/initialized", "notifications/cancelled", "notifications/progress":
		if request {
			return nil, "", ErrProtocol
		}
	default:
		return nil, "", code.Refuse(403, "code_research_method_denied")
	}
	if method == "initialize" || method == "tools/call" {
		params, err := parseObject(m["params"], MaxRequest)
		if err != nil {
			return nil, "", ErrProtocol
		}
		if method == "tools/call" {
			var name string
			if json.Unmarshal(params["name"], &name) != nil || !code.ResearchToolAllowed(service, name) {
				return nil, "", code.Refuse(403, "code_research_tool_denied")
			}
		} else {
			params["capabilities"] = json.RawMessage(`{}`)
			m["params"], _ = json.Marshal(params)
			raw, _ = json.Marshal(m)
		}
	}
	return raw, method, nil
}

func Response(raw []byte, method, service string) ([]byte, error) {
	m, err := parseObject(raw, MaxResponse)
	if err != nil || string(m["jsonrpc"]) != `"2.0"` {
		return nil, ErrProtocol
	}
	if v, ok := m["method"]; ok {
		var notification string
		if json.Unmarshal(v, &notification) != nil {
			return nil, ErrProtocol
		}
		if _, request := m["id"]; request {
			return nil, nil
		}
		switch notification {
		case "notifications/progress", "notifications/message", "notifications/tools/list_changed", "notifications/cancelled":
			return raw, nil
		default:
			return nil, nil
		}
	}
	if _, ok := m["error"]; ok {
		return raw, nil
	}
	result, err := parseObject(m["result"], MaxResponse)
	if err != nil {
		return nil, err
	}
	_, hasContent := result["content"]
	_, hasTools := result["tools"]
	_, hasServer := result["serverInfo"]
	changed := false
	if method == "tools/list" || method == "" && hasTools && !hasContent {
		var tools []json.RawMessage
		if json.Unmarshal(result["tools"], &tools) != nil || tools == nil || len(tools) > 128 {
			return nil, ErrProtocol
		}
		allowed := make([]json.RawMessage, 0, len(tools))
		for _, tool := range tools {
			var info struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(tool, &info) != nil {
				return nil, ErrProtocol
			}
			if code.ResearchToolAllowed(service, info.Name) {
				allowed = append(allowed, tool)
			}
		}
		result["tools"], _ = json.Marshal(allowed)
		changed = true
	}
	if method == "initialize" || method == "" && hasServer && !hasContent {
		var capabilities map[string]json.RawMessage
		if json.Unmarshal(result["capabilities"], &capabilities) != nil {
			return nil, ErrProtocol
		}
		tools := capabilities["tools"]
		capabilities = map[string]json.RawMessage{}
		if len(tools) != 0 {
			capabilities["tools"] = tools
		}
		result["capabilities"], _ = json.Marshal(capabilities)
		changed = true
	}
	if !changed {
		return raw, nil
	}
	m["result"], _ = json.Marshal(result)
	return json.Marshal(m)
}

func ValidSession(value string) bool {
	if len(value) < 1 || len(value) > 256 {
		return false
	}
	for i := range len(value) {
		if value[i] < 0x21 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

func MessageID(raw []byte) json.RawMessage {
	var message struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(raw, &message) != nil {
		return nil
	}
	return message.ID
}

func Initialized(raw []byte, requestID json.RawMessage) bool {
	m, err := parseObject(raw, MaxResponse)
	if err != nil || string(m["jsonrpc"]) != `"2.0"` || len(m["method"]) != 0 || len(m["error"]) != 0 || !sameID(m["id"], requestID) {
		return false
	}
	result, err := parseObject(m["result"], MaxResponse)
	if err != nil {
		return false
	}
	var protocol string
	if json.Unmarshal(result["protocolVersion"], &protocol) != nil || protocol == "" {
		return false
	}
	if _, err := parseObject(result["capabilities"], MaxResponse); err != nil {
		return false
	}
	var info struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	return json.Unmarshal(result["serverInfo"], &info) == nil && info.Name != "" && info.Version != ""
}

func PingResponse(raw []byte) ([]byte, error) {
	m, err := parseObject(raw, MaxResponse)
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
		if _, err := parseObject(params, MaxResponse); err != nil {
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
