package management

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/mcp/messages"
	"github.com/tyk-swe/olp/internal/mcp/upstream"
	"github.com/tyk-swe/olp/internal/oif"
	client "github.com/tyk-swe/olp/management"
)

type MCP struct {
	access *access.Server
	api    http.Handler
}

func NewMCP(a *access.Server, api http.Handler) *MCP { return &MCP{access: a, api: api} }

func (server *MCP) Register(mux *http.ServeMux) {
	server.access.Route(mux, "POST /api/v1/mcp", server.serveMCP, access.MaxBody(4<<20), access.Deadline(60*time.Second))
}

func rpcResult[T any](id json.RawMessage, result T) (access.Reply, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(messages.Envelope{JSONRPC: "2.0", ID: messages.IDOrNull(id), Result: encoded}), nil
}

func rpcFailure(id json.RawMessage, code int, message string) access.Reply {
	return access.OK(messages.Envelope{JSONRPC: "2.0", ID: messages.IDOrNull(id), Error: &messages.Error{Code: code, Message: message}})
}

type managementTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations toolAnnotations `json:"annotations"`
}
type managementToolResult struct {
	Content           []upstream.ToolContent `json:"content"`
	StructuredContent json.RawMessage        `json:"structuredContent"`
	IsError           bool                   `json:"isError"`
}

type toolAnnotations struct {
	ReadOnly    bool `json:"readOnlyHint"`
	Destructive bool `json:"destructiveHint"`
	OpenWorld   bool `json:"openWorldHint"`
}

func (server *MCP) serveMCP(r *http.Request, principal access.Principal) (access.Reply, error) {
	var request messages.Request
	if err := access.DecodeUnique(r, &request, 4<<20); err != nil {
		return access.Reply{}, err
	}
	if request.JSONRPC != "2.0" || request.Method == "" {
		return rpcFailure(nil, -32600, "Invalid JSON-RPC request"), nil
	}
	if !request.Valid() {
		return rpcFailure(nil, -32600, "Invalid request ID"), nil
	}
	if len(request.ID) == 0 {
		if request.Method == "notifications/initialized" || request.Method == "notifications/cancelled" {
			return access.Reply{Status: 202}, nil
		}
		return rpcFailure(nil, -32600, "Requests require an ID"), nil
	}
	switch request.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return rpcFailure(request.ID, -32602, "Invalid initialize parameters"), nil
		}
		return rpcResult(request.ID, struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    struct {
				Tools struct {
					ListChanged bool `json:"listChanged"`
				} `json:"tools"`
			} `json:"capabilities"`
			ServerInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		}{ProtocolVersion: messages.NegotiateProtocol(params.ProtocolVersion), ServerInfo: struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}{Name: "openllmproxy-management", Version: "1"}})
	case "ping":
		return rpcResult(request.ID, struct{}{})
	case "tools/list":
		tools := []managementTool{}
		requirements, err := access.ContractRequirements()
		if err != nil {
			return access.Reply{}, err
		}
		for _, operation := range client.Operations() {
			if requirements[operation.Method+" "+operation.Path].Admits(principal) != nil {
				continue
			}
			readOnly := operation.ReadOnly
			tools = append(tools, managementTool{Name: operation.Name, Description: operation.Description, InputSchema: operation.InputSchema, Annotations: toolAnnotations{ReadOnly: readOnly, Destructive: !readOnly, OpenWorld: true}})
		}
		return rpcResult(request.ID, struct {
			Tools []managementTool `json:"tools"`
		}{Tools: tools})
	case "tools/call":
		var params struct {
			Name      string           `json:"name"`
			Arguments client.Arguments `json:"arguments"`
			Meta      json.RawMessage  `json:"_meta,omitempty"`
		}
		decoder := json.NewDecoder(bytes.NewReader(request.Params))
		decoder.DisallowUnknownFields()
		decoder.UseNumber()
		if err := decoder.Decode(&params); err != nil {
			return rpcFailure(request.ID, -32602, "Invalid tool arguments"), nil
		}
		operation, ok := client.Lookup(params.Name)
		if !ok {
			return rpcFailure(request.ID, -32602, "Unknown management tool"), nil
		}
		requirements, err := access.ContractRequirements()
		if err != nil {
			return access.Reply{}, err
		}
		if err = requirements[operation.Method+" "+operation.Path].Admits(principal); err != nil {
			return rpcFailure(request.ID, -32602, "Tool is outside the token's authority"), nil
		}
		inner, err := operation.Request(r.Context(), params.Arguments)
		if err != nil {
			return rpcFailure(request.ID, -32602, "Missing or invalid tool parameters"), nil
		}
		inner.Header.Set("Authorization", r.Header.Get("Authorization"))
		inner.RemoteAddr = r.RemoteAddr
		inner.Header.Set("X-Forwarded-For", r.Header.Get("X-Forwarded-For"))
		reply := &mcpWriter{header: http.Header{}, status: 200}
		server.api.ServeHTTP(reply, inner)
		body, failed := managementResponseBody(reply)
		result := maps.Collect(func(yield func(string, json.RawMessage) bool) {
			if !yield("status", json.RawMessage(strconv.Itoa(reply.status))) || !yield("body", body) {
				return
			}
			for key, value := range operation.ResponseMetadata(reply.header) {
				encoded, _ := json.Marshal(value)
				if !yield(key, encoded) {
					return
				}
			}
		})
		text, err := json.Marshal(result)
		if err != nil {
			return access.Reply{}, err
		}
		return rpcResult(request.ID, managementToolResult{
			Content: []upstream.ToolContent{{Type: "text", Text: string(text)}}, StructuredContent: text, IsError: failed,
		})
	default:
		return rpcFailure(request.ID, -32601, "Method not found"), nil
	}
}

// In-memory dispatch has no socket reads to deadline. The enclosing request
// context bounds handler work, and this writer bounds every response.
type mcpWriter struct {
	header    http.Header
	status    int
	body      bytes.Buffer
	overflow  bool
	committed bool
}

func (writer *mcpWriter) Header() http.Header { return writer.header }
func (writer *mcpWriter) WriteHeader(status int) {
	if !writer.committed {
		writer.status, writer.committed = status, true
	}
}
func (writer *mcpWriter) Write(data []byte) (int, error) {
	writer.committed = true
	if writer.body.Len()+len(data) > client.MaxResponseBytes {
		writer.overflow = true
		return 0, errors.New("MCP response exceeds limit")
	}
	return writer.body.Write(data)
}
func (*mcpWriter) SetReadDeadline(time.Time) error  { return nil }
func (*mcpWriter) SetWriteDeadline(time.Time) error { return nil }
func (*mcpWriter) Flush()                           {}

func managementResponseBody(reply *mcpWriter) (json.RawMessage, bool) {
	if reply.status < 200 || reply.status >= 300 || reply.overflow {
		encoded, _ := json.Marshal(struct {
			Status int    `json:"status"`
			Error  string `json:"error"`
		}{Status: reply.status, Error: "management_operation_failed"})
		return encoded, true
	}
	if reply.body.Len() == 0 {
		return json.RawMessage("null"), false
	}
	document, err := oif.ParseJSON(reply.body.Bytes(), oif.Limits{MaxBytes: client.MaxResponseBytes, MaxDepth: 128, MaxNodes: 1 << 20})
	if err != nil {
		return json.RawMessage(`{"error":"unsupported_response"}`), true
	}
	return redactMCP(document.Root(), responseData), false
}

type redactionMode uint8

const (
	responseData redactionMode = iota
	schemaValue
	schemaMembers
)

func secretField(key string) bool {
	lower := strings.ToLower(key)
	return lower == "secret" || lower == "token" || lower == "password" || lower == "csrf_token" || lower == "recovery_codes" || lower == "secret_bindings" || strings.HasSuffix(lower, "_secret") || strings.HasSuffix(lower, "_password") || strings.HasSuffix(lower, "_token") || strings.Contains(lower, "private_key")
}

func childRedaction(mode redactionMode, member oif.Member) redactionMode {
	if mode == schemaMembers {
		return schemaValue
	}
	if mode == responseData {
		kind, _ := member.Value.Lookup("type")
		if text, ok := kind.Text(); member.Name == "input_schema" && ok && text == "object" {
			return schemaValue
		}
		return responseData
	}
	switch member.Name {
	case "properties", "patternProperties", "$defs", "definitions", "dependentSchemas", "dependencies":
		return schemaMembers
	case "additionalProperties", "unevaluatedProperties", "propertyNames", "items", "additionalItems", "unevaluatedItems", "contains", "not", "if", "then", "else", "allOf", "anyOf", "oneOf", "prefixItems", "contentSchema":
		return schemaValue
	default:
		return responseData
	}
}

func redactMCP(value oif.Value, mode redactionMode) json.RawMessage {
	switch value.Kind() {
	case oif.Object:
		members := maps.Collect(func(yield func(string, json.RawMessage) bool) {
			for _, member := range value.Members() {
				if mode != schemaMembers && secretField(member.Name) {
					continue
				}
				if !yield(member.Name, redactMCP(member.Value, childRedaction(mode, member))) {
					return
				}
			}
		})
		encoded, _ := json.Marshal(members)
		return encoded
	case oif.Array:
		children := slices.Collect(func(yield func(json.RawMessage) bool) {
			for _, child := range value.Elements() {
				if !yield(redactMCP(child, mode)) {
					return
				}
			}
		})
		if len(children) == 0 {
			return json.RawMessage("[]")
		}
		encoded, _ := json.Marshal(children)
		return encoded
	default:
		return value.Bytes()
	}
}
