package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/mcpservers"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
)

const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcServerError    = -32000
	rpcNotInitialized = -32002
)

type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcWireError   `json:"error,omitempty"`
}

type rpcWireError struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
}

type rpcMessage struct {
	id       json.RawMessage
	hasID    bool
	method   string
	hasCall  bool
	params   json.RawMessage
	response bool
}

func writeRPCResult(w http.ResponseWriter, status int, id json.RawMessage, result any) {
	encoded, err := json.Marshal(result)
	if err != nil {
		writeRPCError(w, http.StatusInternalServerError, id, rpcServerError, "The result could not be encoded.", map[string]any{"code": "encode_failed"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(rpcEnvelope{JSONRPC: "2.0", ID: idOrNull(id), Result: encoded})
}

func writeRPCError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string, data map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(rpcEnvelope{JSONRPC: "2.0", ID: idOrNull(id), Error: &rpcWireError{Code: code, Message: message, Data: data}})
}

func idOrNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func parseRPCMessage(raw []byte) (rpcMessage, int, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] == '[' {
		return rpcMessage{}, rpcInvalidRequest, false
	}
	if _, err := oif.ParseJSON(raw, oif.Limits{MaxBytes: maxCallBodyBytes, MaxDepth: 64, MaxNodes: 1 << 16}); err != nil {
		return rpcMessage{}, rpcParseError, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return rpcMessage{}, rpcParseError, false
	}
	var version string
	if json.Unmarshal(fields["jsonrpc"], &version) != nil || version != "2.0" {
		return rpcMessage{}, rpcInvalidRequest, false
	}
	message := rpcMessage{params: fields["params"]}
	if id, ok := fields["id"]; ok {
		if !validRPCID(id) {
			return rpcMessage{}, rpcInvalidRequest, false
		}
		message.id, message.hasID = id, true
	}
	if rawMethod, ok := fields["method"]; ok {
		if json.Unmarshal(rawMethod, &message.method) != nil || message.method == "" {
			return rpcMessage{}, rpcInvalidRequest, false
		}
		message.hasCall = true
	}
	_, hasResult := fields["result"]
	_, hasError := fields["error"]
	message.response = !message.hasCall && message.hasID && (hasResult || hasError)
	if !message.hasCall && !message.response {
		return rpcMessage{}, rpcInvalidRequest, false
	}
	return message, 0, true
}

func validRPCID(raw json.RawMessage) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return false
	}
	switch value.(type) {
	case string, json.Number:
		return true
	default:
		return false
	}
}

func acceptsStreamableHTTP(r *http.Request) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return false
	}
	accept := r.Header.Get("Accept")
	return accept == "" || strings.Contains(accept, "application/json") || strings.Contains(accept, "text/event-stream") || strings.Contains(accept, "*/*")
}

func (s *Server) serveStreamableGET(w http.ResponseWriter, r *http.Request) {
	if _, failure := s.authenticate(r); failure != nil {
		writeRPCError(w, failure.status, nil, rpcServerError, failure.msg, map[string]any{"code": failure.code})
		return
	}
	w.Header().Set("Allow", "POST, DELETE")
	writeRPCError(w, http.StatusMethodNotAllowed, nil, rpcServerError, "This endpoint does not offer a standalone event stream.", map[string]any{"code": "method_not_allowed"})
}

func (s *Server) serveStreamableDELETE(w http.ResponseWriter, r *http.Request) {
	authority, failure := s.authenticate(r)
	if failure != nil {
		writeRPCError(w, failure.status, nil, rpcServerError, failure.msg, map[string]any{"code": failure.code})
		return
	}
	sessionID := r.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		writeRPCError(w, http.StatusBadRequest, nil, rpcInvalidRequest, "Supply the session in the Mcp-Session-Id header.", map[string]any{"code": "session_required"})
		return
	}
	existing, ok := s.accessSession(sessionID, authority)
	if !ok {
		writeRPCError(w, http.StatusNotFound, nil, rpcInvalidRequest, "The session does not exist.", map[string]any{"code": "session_not_found"})
		return
	}
	if protocol := r.Header.Get("MCP-Protocol-Version"); protocol != "" && protocol != existing.protocol {
		writeRPCError(w, http.StatusBadRequest, nil, rpcInvalidRequest, "The protocol version does not match the session.", map[string]any{"code": "protocol_version_mismatch"})
		return
	}
	if !s.deleteSession(sessionID, authority) {
		writeRPCError(w, http.StatusNotFound, nil, rpcInvalidRequest, "The session does not exist.", map[string]any{"code": "session_not_found"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) serveStreamableHTTP(w http.ResponseWriter, r *http.Request) {
	if !acceptsStreamableHTTP(r) {
		writeRPCError(w, http.StatusNotAcceptable, nil, rpcInvalidRequest, "Send application/json and accept application/json or text/event-stream.", map[string]any{"code": "not_acceptable"})
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCallBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeRPCError(w, http.StatusRequestEntityTooLarge, nil, rpcInvalidRequest, "The message exceeds the 1 MiB bound.", map[string]any{"code": "request_too_large"})
			return
		}
		writeRPCError(w, http.StatusBadRequest, nil, rpcParseError, "The message could not be read.", map[string]any{"code": "parse_error"})
		return
	}
	message, code, ok := parseRPCMessage(raw)
	if !ok {
		writeRPCError(w, http.StatusBadRequest, nil, code, "Supply one JSON-RPC 2.0 message.", map[string]any{"code": "invalid_request"})
		return
	}
	authority, failure := s.authenticate(r)
	if failure != nil {
		writeRPCError(w, failure.status, message.id, rpcServerError, failure.msg, map[string]any{"code": failure.code})
		return
	}
	if message.hasCall && message.method == "initialize" {
		if !message.hasID {
			writeRPCError(w, http.StatusBadRequest, nil, rpcInvalidRequest, "Initialize requires a request ID.", map[string]any{"code": "invalid_request"})
			return
		}
		s.initializeSession(w, r, authority, message)
		return
	}
	sessionID := r.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		writeRPCError(w, http.StatusBadRequest, message.id, rpcInvalidRequest, "Supply the session in the Mcp-Session-Id header.", map[string]any{"code": "session_required"})
		return
	}
	existing, ok := s.accessSession(sessionID, authority)
	if !ok {
		writeRPCError(w, http.StatusNotFound, message.id, rpcInvalidRequest, "The session does not exist.", map[string]any{"code": "session_not_found"})
		return
	}
	if protocol := r.Header.Get("MCP-Protocol-Version"); protocol != "" && protocol != existing.protocol {
		writeRPCError(w, http.StatusBadRequest, message.id, rpcInvalidRequest, "The protocol version does not match the session.", map[string]any{"code": "protocol_version_mismatch"})
		return
	}
	if message.response {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if !message.hasID {
		if message.method == "notifications/initialized" {
			s.markSessionInitialized(sessionID, authority)
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	s.serveSessionRequest(w, r, authority, existing, message)
}

func (s *Server) sessionToolset(authority access.Authority, snapshot *routing.Snapshot, requested string) (resolvedToolset, *apiError) {
	if requested != "" {
		return s.resolveToolset(authority, snapshot, requested)
	}
	resolved := []resolvedToolset{}
	for _, toolsetID := range authority.Policy.AllowedToolsets {
		candidate, failure := s.resolveToolset(authority, snapshot, toolsetID)
		if failure == nil {
			resolved = append(resolved, candidate)
		} else if failure.status == http.StatusServiceUnavailable {
			return resolvedToolset{}, failure
		}
	}
	switch len(resolved) {
	case 1:
		return resolved[0], nil
	case 0:
		return resolvedToolset{}, errToolsetNotFound
	default:
		return resolvedToolset{}, fail(http.StatusBadRequest, "toolset_id_required", "invalid_request_error", "This key may use more than one toolset, so select one with the toolset_id query parameter.")
	}
}

type initializeRequest struct {
	ProtocolVersion string          `json:"protocolVersion"`
	Capabilities    json.RawMessage `json:"capabilities"`
	ClientInfo      json.RawMessage `json:"clientInfo"`
}

func negotiateProtocol(requested string) string {
	supported := mcpservers.SupportedProtocols()
	if slices.Contains(supported, requested) {
		return requested
	}
	return supported[0]
}

func (s *Server) initializeSession(w http.ResponseWriter, r *http.Request, authority access.Authority, message rpcMessage) {
	var params initializeRequest
	if len(message.params) == 0 || json.Unmarshal(message.params, &params) != nil || params.ProtocolVersion == "" {
		writeRPCError(w, http.StatusBadRequest, message.id, rpcInvalidParams, "Supply initialize parameters with a protocol version.", map[string]any{"code": "invalid_params"})
		return
	}
	release, snapshot, failure := s.snapshot()
	if failure != nil {
		writeRPCError(w, failure.status, message.id, rpcServerError, failure.msg, map[string]any{"code": failure.code})
		return
	}
	resolved, failure := s.sessionToolset(authority, snapshot, r.URL.Query().Get("toolset_id"))
	if failure != nil {
		writeRPCError(w, failure.status, message.id, rpcInvalidParams, failure.msg, map[string]any{"code": failure.code})
		return
	}
	sessionID, err := newSessionID()
	if err != nil {
		writeRPCError(w, http.StatusServiceUnavailable, message.id, rpcServerError, "A session could not be created, so nothing was dispatched.", map[string]any{"code": "session_unavailable"})
		return
	}
	now := s.now()
	candidate := session{
		id: sessionID, installationID: authority.InstallationID, projectID: *authority.ProjectID,
		principal: principalKey(authority), protocol: negotiateProtocol(params.ProtocolVersion),
		toolset: resolved, releaseID: releaseIDFor(release, snapshot), createdAt: now, lastActiveAt: now,
	}
	if !s.createSession(candidate) {
		writeRPCError(w, http.StatusTooManyRequests, message.id, rpcServerError, "This key already has the maximum number of active sessions.", map[string]any{"code": "session_limit_exceeded", "limit": maxSessionsPerKey})
		return
	}
	w.Header().Set("Mcp-Session-Id", sessionID)
	writeRPCResult(w, http.StatusOK, message.id, map[string]any{
		"protocolVersion": candidate.protocol,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": "openllmproxy-tools", "version": "1"},
	})
}

func unsupportedCapability(method string) (string, bool) {
	for _, prefix := range []struct {
		prefix     string
		capability string
	}{
		{"sampling/", "sampling"},
		{"elicitation/", "elicitation"},
		{"roots/", "roots"},
		{"resources/", "resources"},
		{"prompts/", "prompts"},
		{"completion/", "completion"},
		{"logging/", "logging"},
		{"tasks/", "tasks"},
	} {
		if strings.HasPrefix(method, prefix.prefix) {
			return prefix.capability, true
		}
	}
	return "", false
}

func (s *Server) serveSessionRequest(w http.ResponseWriter, r *http.Request, authority access.Authority, current session, message rpcMessage) {
	switch message.method {
	case "ping":
		writeRPCResult(w, http.StatusOK, message.id, map[string]any{})
	case "tools/list":
		if !current.initialized {
			writeRPCError(w, http.StatusOK, message.id, rpcNotInitialized, "Send notifications/initialized before using this session.", map[string]any{"code": "session_not_initialized"})
			return
		}
		s.sessionListTools(w, authority, current, message)
	case "tools/call":
		if !current.initialized {
			writeRPCError(w, http.StatusOK, message.id, rpcNotInitialized, "Send notifications/initialized before using this session.", map[string]any{"code": "session_not_initialized"})
			return
		}
		s.sessionCallTool(w, r, authority, current, message)
	default:
		if capability, ok := unsupportedCapability(message.method); ok {
			writeRPCError(w, http.StatusOK, message.id, rpcMethodNotFound, "The "+capability+" capability is not supported at this endpoint.", map[string]any{"code": "unsupported_capability", "capability": capability})
			return
		}
		writeRPCError(w, http.StatusOK, message.id, rpcMethodNotFound, "Method not found.", map[string]any{"code": "method_not_found"})
	}
}

func (s *Server) reauthorizeSession(authority access.Authority, current session) *apiError {
	if err := s.Runtime.AuthorizeAgentResource(authority, "tools", current.toolset.id, current.toolset.revisionID, *authority.ProjectID); err != nil {
		if errors.Is(err, runtime.ErrStaleAuthority) {
			return fail(http.StatusServiceUnavailable, "authority_unavailable", "api_error", "Key authority is unavailable; retry shortly.")
		}
		return errToolsetNotFound
	}
	return nil
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

func (s *Server) sessionListTools(w http.ResponseWriter, authority access.Authority, current session, message rpcMessage) {
	var params struct {
		Cursor string `json:"cursor"`
	}
	if len(message.params) > 0 && string(message.params) != "null" && json.Unmarshal(message.params, &params) != nil {
		writeRPCError(w, http.StatusOK, message.id, rpcInvalidParams, "Supply valid tools/list parameters.", map[string]any{"code": "invalid_params"})
		return
	}
	if params.Cursor != "" {
		writeRPCError(w, http.StatusOK, message.id, rpcInvalidParams, "The cursor was not issued by this endpoint.", map[string]any{"code": "invalid_cursor"})
		return
	}
	if failure := s.reauthorizeSession(authority, current); failure != nil {
		writeRPCError(w, http.StatusOK, message.id, rpcServerError, failure.msg, map[string]any{"code": failure.code})
		return
	}
	tools := []wireTool{}
	for _, tool := range current.toolset.tools {
		tools = append(tools, wireTool{Name: tool.Alias, Description: tool.Description, InputSchema: tool.InputSchema})
	}
	writeRPCResult(w, http.StatusOK, message.id, map[string]any{"tools": tools})
}

type sessionCallRequest struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type sessionCallResult struct {
	Content           []mcpservers.ToolContent `json:"content"`
	StructuredContent json.RawMessage          `json:"structuredContent,omitempty"`
	IsError           bool                     `json:"isError"`
	Meta              map[string]any           `json:"_meta"`
}

func (s *Server) sessionCallTool(w http.ResponseWriter, r *http.Request, authority access.Authority, current session, message rpcMessage) {
	var params sessionCallRequest
	if len(message.params) == 0 || json.Unmarshal(message.params, &params) != nil || params.Name == "" {
		writeRPCError(w, http.StatusOK, message.id, rpcInvalidParams, "Supply the tool name to call.", map[string]any{"code": "invalid_params"})
		return
	}
	index := slices.IndexFunc(current.toolset.tools, func(tool routing.MCPToolsetTool) bool { return tool.Alias == params.Name })
	if index < 0 {
		writeRPCError(w, http.StatusOK, message.id, rpcInvalidParams, errToolNotFound.msg, map[string]any{"code": errToolNotFound.code})
		return
	}
	tool := current.toolset.tools[index]
	arguments := params.Arguments
	if len(arguments) == 0 || string(arguments) == "null" {
		arguments = json.RawMessage(`{}`)
	}
	if err := tool.ValidateArguments(arguments); err != nil {
		writeRPCError(w, http.StatusOK, message.id, rpcInvalidParams, "The arguments do not match the pinned tool schema.", map[string]any{"code": "invalid_tool_arguments"})
		return
	}
	if s.Ledger == nil || s.Egress == nil || s.Auth == nil {
		writeRPCError(w, http.StatusOK, message.id, rpcServerError, errLedger.msg, map[string]any{"code": errLedger.code})
		return
	}
	outcome := s.execute(r.Context(), executeInput{authority: authority, resolved: current.toolset, tool: tool, arguments: arguments, releaseID: current.releaseID})
	switch {
	case outcome.failure != nil:
		data := map[string]any{"code": outcome.failure.code}
		if outcome.retryAfter > 0 {
			data["retry_after_seconds"] = int64((outcome.retryAfter + time.Second - 1) / time.Second)
		}
		writeRPCError(w, http.StatusOK, message.id, rpcServerError, outcome.failure.msg, data)
	case outcome.outcomeUnknown:
		writeRPCError(w, http.StatusOK, message.id, rpcServerError, outcome.unknownMessage, map[string]any{
			"code": "outcome_unknown", "status": string(StatusOutcomeUnknown),
			"attempt_id": outcome.reservation.AttemptID, "dispatch_id": outcome.reservation.DispatchID,
		})
	default:
		content := outcome.result.Content
		if content == nil {
			content = []mcpservers.ToolContent{}
		}
		writeRPCResult(w, http.StatusOK, message.id, sessionCallResult{
			Content: content, StructuredContent: outcome.result.StructuredContent, IsError: outcome.result.IsError,
			Meta: map[string]any{
				"olp.attempt_id": outcome.reservation.AttemptID, "olp.dispatch_id": outcome.reservation.DispatchID,
				"olp.toolset_id": current.toolset.id, "olp.toolset_revision_id": current.toolset.revisionID,
			},
		})
	}
}
