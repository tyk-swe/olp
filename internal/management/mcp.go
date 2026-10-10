package management

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	client "github.com/tyk-swe/olp/sdk/management"
)

// MCP exposes the token-admitted contract as a stateless Streamable HTTP MCP
// server. Each tools/call dispatches to the ordinary management handler, which
// authenticates again and retains its own validation, transactions and audit.
type MCP struct {
	Access *access.Server
	API    http.Handler
}

func (server *MCP) Register(mux *http.ServeMux) {
	server.Access.Route(mux, "POST /api/v1/mcp", server.serveMCP, access.MaxBody(4<<20), access.Deadline(60*time.Second))
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func rpcResult(id json.RawMessage, result any) access.Reply {
	return access.OK(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func rpcFailure(id json.RawMessage, code int, message string) access.Reply {
	return access.OK(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

func (server *MCP) serveMCP(r *http.Request, principal access.Principal) (access.Reply, error) {
	var request rpcRequest
	if err := access.DecodeUnique(r, &request, 4<<20); err != nil {
		return access.Reply{}, err
	}
	if request.JSONRPC != "2.0" || request.Method == "" {
		return rpcFailure(nil, -32600, "Invalid JSON-RPC request"), nil
	}
	if len(request.ID) == 0 {
		if request.Method == "notifications/initialized" || request.Method == "notifications/cancelled" {
			return access.Reply{Status: 202}, nil
		}
		return rpcFailure(nil, -32600, "Requests require an ID"), nil
	}
	var id any
	if err := json.Unmarshal(request.ID, &id); err != nil {
		return rpcFailure(nil, -32600, "Invalid request ID"), nil
	}
	switch id.(type) {
	case string, float64:
	default:
		return rpcFailure(nil, -32600, "Invalid request ID"), nil
	}
	switch request.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return rpcFailure(request.ID, -32602, "Invalid initialize parameters"), nil
		}
		version := "2025-11-25"
		if params.ProtocolVersion == "2025-03-26" || params.ProtocolVersion == "2025-06-18" {
			version = params.ProtocolVersion
		}
		return rpcResult(request.ID, map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{"listChanged": false}}, "serverInfo": map[string]any{"name": "openllmproxy-management", "version": "1"}}), nil
	case "ping":
		return rpcResult(request.ID, map[string]any{}), nil
	case "tools/list":
		tools := []any{}
		requirements, err := access.ContractRequirements()
		if err != nil {
			return access.Reply{}, err
		}
		for _, operation := range client.Operations() {
			if requirements[operation.Method+" "+operation.Path].Admits(principal) != nil {
				continue
			}
			readOnly := operation.Method == "GET" || operation.Name == "plan_configuration" || strings.HasPrefix(operation.Name, "simulate_") || operation.Name == "lookup_end_user"
			tools = append(tools, map[string]any{"name": operation.Name, "description": operation.Description, "inputSchema": operation.InputSchema, "annotations": map[string]any{"readOnlyHint": readOnly, "destructiveHint": !readOnly, "openWorldHint": true}})
		}
		return rpcResult(request.ID, map[string]any{"tools": tools}), nil
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
		server.API.ServeHTTP(reply, inner)
		failed := reply.status < 200 || reply.status >= 300 || reply.overflow
		var body any
		if failed {
			body = map[string]any{"status": reply.status, "error": "management_operation_failed"}
		} else if reply.body.Len() != 0 {
			if body, err = decodeMCPBody(reply.body.Bytes()); err != nil {
				body = map[string]any{"error": "unsupported_response"}
				failed = true
			}
		}
		redactMCP(body)
		result := map[string]any{"status": reply.status, "body": body}
		if etag := reply.header.Get("ETag"); etag != "" {
			result["etag"] = etag
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return access.Reply{}, err
		}
		return rpcResult(request.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": string(encoded)}}, "structuredContent": result, "isError": failed}), nil
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

// Preserve exact schema numbers and counters while inspecting nested secrets.
// float64 conversion can change a certified schema without changing its digest.
func decodeMCPBody(data []byte) (any, error) {
	if !json.Valid(data) {
		return nil, errors.New("invalid management JSON response")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	err := decoder.Decode(&value)
	return value, err
}

func redactMCP(value any) {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			name := strings.ToLower(key)
			if mcpSecretName(name) {
				delete(v, key)
			} else if schema, ok := child.(map[string]any); name == "input_schema" && ok && schema["type"] == "object" {
				redactMCPSchema(schema)
			} else {
				redactMCP(child)
			}
		}
	case []any:
		for _, child := range v {
			redactMCP(child)
		}
	}
}

var _ io.Writer = (*mcpWriter)(nil)

func mcpSecretName(name string) bool {
	return name == "secret" || name == "token" || name == "password" || name == "csrf_token" || name == "recovery_codes" || name == "secret_bindings" || strings.HasSuffix(name, "_secret") || strings.HasSuffix(name, "_password") || strings.HasSuffix(name, "_token") || strings.Contains(name, "private_key")
}

// Schema property and definition names describe inputs, rather than contain
// credential values. Preserve those names, but redact data in examples,
// defaults and extensions with the ordinary response rules.
func redactMCPSchema(value any) {
	switch schema := value.(type) {
	case map[string]any:
		for name, child := range schema {
			if mcpSecretName(strings.ToLower(name)) {
				delete(schema, name)
				continue
			}
			switch name {
			case "properties", "patternProperties", "$defs", "definitions", "dependentSchemas", "dependencies":
				if definitions, ok := child.(map[string]any); ok {
					for _, definition := range definitions {
						redactMCPSchema(definition)
					}
				} else {
					redactMCP(child)
				}
			case "additionalProperties", "unevaluatedProperties", "propertyNames", "items", "additionalItems", "unevaluatedItems", "contains", "not", "if", "then", "else", "allOf", "anyOf", "oneOf", "prefixItems", "contentSchema":
				redactMCPSchema(child)
			default:
				redactMCP(child)
			}
		}
	case []any:
		for _, child := range schema {
			redactMCPSchema(child)
		}
	}
}
