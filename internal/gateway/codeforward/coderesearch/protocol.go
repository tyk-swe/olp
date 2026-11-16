// Package coderesearch bounds and filters the subscription research MCP surface.
// Native tool schemas, results, citations and errors remain opaque JSON.
package coderesearch

import (
	"encoding/json"
	"errors"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/oif"
)

const (
	MaxRequest  = 1 << 20
	MaxResponse = 8 << 20
	MaxEvent    = 1 << 20
)

var ErrProtocol = errors.New("invalid research MCP message")

func object(raw []byte, maxBytes int) (map[string]json.RawMessage, error) {
	if _, err := oif.ParseJSON(raw, oif.Limits{MaxBytes: maxBytes, MaxDepth: 64, MaxNodes: 1 << 17}); err != nil {
		return nil, ErrProtocol
	}
	var message map[string]json.RawMessage
	if json.Unmarshal(raw, &message) != nil || message == nil {
		return nil, ErrProtocol
	}
	return message, nil
}

// Request rejects batches, unsupported capabilities and calls outside the fixed
// service allowlist. Only initialization is rewritten, to decline server access
// to client sampling, roots and local files.
func Request(raw []byte, service string) ([]byte, string, error) {
	m, err := object(raw, MaxRequest)
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
		return nil, "", codemode.Refuse(403, "code_research_method_denied")
	}
	if method == "initialize" || method == "tools/call" {
		params, err := object(m["params"], MaxRequest)
		if err != nil {
			return nil, "", ErrProtocol
		}
		if method == "tools/call" {
			var name string
			if json.Unmarshal(params["name"], &name) != nil || !codemode.ResearchToolAllowed(service, name) {
				return nil, "", codemode.Refuse(403, "code_research_tool_denied")
			}
		} else {
			params["capabilities"] = json.RawMessage(`{}`)
			m["params"], _ = json.Marshal(params)
			raw, _ = json.Marshal(m)
		}
	}
	return raw, method, nil
}

// Response filters discovery while retaining each allowed tool's full native
// definition. GET streams can resume discovery, so recognize discovery results
// there as well. The transport handles server pings through PingResponse;
// other server requests and unadvertised notifications are dropped.
func Response(raw []byte, method, service string) ([]byte, error) {
	m, err := object(raw, MaxResponse)
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
	result, err := object(m["result"], MaxResponse)
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
			if codemode.ResearchToolAllowed(service, info.Name) {
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
