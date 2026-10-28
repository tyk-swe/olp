package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
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
	Code    int           `json:"code"`
	Message string        `json:"message"`
	Data    *rpcErrorData `json:"data,omitempty"`
}

type rpcErrorData struct {
	Code              string `json:"code"`
	Capability        string `json:"capability,omitempty"`
	Limit             int    `json:"limit,omitempty"`
	RetryAfterSeconds int64  `json:"retry_after_seconds,omitempty"`
	Status            string `json:"status,omitempty"`
	AttemptID         string `json:"attempt_id,omitempty"`
	DispatchID        string `json:"dispatch_id,omitempty"`
}

type rpcFailure struct {
	status  int
	code    int
	message string
	data    rpcErrorData
}

func failureRPC(status, code int, failure *apiError) rpcFailure {
	return rpcFailure{status: status, code: code, message: failure.msg, data: rpcErrorData{Code: failure.code}}
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
		writeRPCFailure(w, id, rpcFailure{status: http.StatusInternalServerError, code: rpcServerError, message: "The result could not be encoded.", data: rpcErrorData{Code: "encode_failed"}})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(rpcEnvelope{JSONRPC: "2.0", ID: idOrNull(id), Result: encoded})
}

func writeRPCError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string, data rpcErrorData) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(rpcEnvelope{JSONRPC: "2.0", ID: idOrNull(id), Error: &rpcWireError{Code: code, Message: message, Data: &data}})
}

func writeRPCFailure(w http.ResponseWriter, id json.RawMessage, failure rpcFailure) {
	writeRPCError(w, failure.status, id, failure.code, failure.message, failure.data)
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

func (s *Server) requireSession(ctx context.Context, r *http.Request, authority access.Authority, transport sessionTransport) (sessionRecord, sessionBinding, *rpcFailure) {
	binding := bindingOf(authority)
	if s.Sessions == nil {
		failure := failureRPC(http.StatusServiceUnavailable, rpcServerError, errSessionUnavailable)
		return sessionRecord{}, binding, &failure
	}
	sessionID := r.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		failure := failureRPC(http.StatusBadRequest, rpcInvalidRequest, errSessionRequired)
		return sessionRecord{}, binding, &failure
	}
	record, found, err := s.Sessions.accessSession(ctx, sessionID, binding)
	if err != nil {
		failure := failureRPC(http.StatusServiceUnavailable, rpcServerError, errSessionUnavailable)
		return sessionRecord{}, binding, &failure
	}
	if !found {
		failure := failureRPC(http.StatusNotFound, rpcInvalidRequest, errSessionNotFound)
		return sessionRecord{}, binding, &failure
	}
	if (record.protocol == mcpservers.LegacyProtocol) != (transport == sessionLegacy) {
		failure := failureRPC(http.StatusBadRequest, rpcInvalidRequest, errSessionProtocolMismatch)
		return sessionRecord{}, binding, &failure
	}
	if protocol := r.Header.Get("MCP-Protocol-Version"); protocol != "" && protocol != record.protocol {
		failure := failureRPC(http.StatusBadRequest, rpcInvalidRequest, errSessionProtocolMismatch)
		return sessionRecord{}, binding, &failure
	}
	return record, binding, nil
}

func (s *Server) serveStreamableGET(w http.ResponseWriter, r *http.Request) {
	if _, failure := s.authenticate(r); failure != nil {
		writeRPCFailure(w, nil, failureRPC(failure.status, rpcServerError, failure))
		return
	}
	w.Header().Set("Allow", "POST, DELETE")
	writeRPCFailure(w, nil, rpcFailure{status: http.StatusMethodNotAllowed, code: rpcServerError, message: "This endpoint does not offer a standalone event stream.", data: rpcErrorData{Code: "method_not_allowed"}})
}

func (s *Server) serveStreamableDELETE(w http.ResponseWriter, r *http.Request) {
	authority, failure := s.authenticate(r)
	if failure != nil {
		writeRPCFailure(w, nil, failureRPC(failure.status, rpcServerError, failure))
		return
	}
	record, binding, refused := s.requireSession(r.Context(), r, authority, sessionStreamable)
	if refused != nil {
		writeRPCFailure(w, nil, *refused)
		return
	}
	deleted, err := s.Sessions.deleteSession(r.Context(), record.id, binding)
	if err != nil {
		writeRPCFailure(w, nil, failureRPC(http.StatusServiceUnavailable, rpcServerError, errSessionUnavailable))
		return
	}
	if !deleted {
		writeRPCFailure(w, nil, failureRPC(http.StatusNotFound, rpcInvalidRequest, errSessionNotFound))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) serveStreamableHTTP(w http.ResponseWriter, r *http.Request) {
	s.serveSessionHTTP(w, r, sessionStreamable)
}

func (s *Server) serveSessionHTTP(w http.ResponseWriter, r *http.Request, transport sessionTransport) {
	if !acceptsStreamableHTTP(r) {
		writeRPCFailure(w, nil, rpcFailure{status: http.StatusNotAcceptable, code: rpcInvalidRequest, message: "Send application/json and accept application/json or text/event-stream.", data: rpcErrorData{Code: "not_acceptable"}})
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCallBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeRPCFailure(w, nil, rpcFailure{status: http.StatusRequestEntityTooLarge, code: rpcInvalidRequest, message: "The message exceeds the 1 MiB bound.", data: rpcErrorData{Code: "request_too_large"}})
			return
		}
		writeRPCFailure(w, nil, rpcFailure{status: http.StatusBadRequest, code: rpcParseError, message: "The message could not be read.", data: rpcErrorData{Code: "parse_error"}})
		return
	}
	if transport == sessionLegacy {
		_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	}
	message, code, ok := parseRPCMessage(raw)
	if !ok {
		writeRPCFailure(w, nil, rpcFailure{status: http.StatusBadRequest, code: code, message: "Supply one JSON-RPC 2.0 message.", data: rpcErrorData{Code: "invalid_request"}})
		return
	}
	authority, failure := s.authenticate(r)
	if failure != nil {
		writeRPCFailure(w, message.id, failureRPC(failure.status, rpcServerError, failure))
		return
	}
	if message.hasCall && message.method == "initialize" {
		if !message.hasID {
			writeRPCFailure(w, nil, rpcFailure{status: http.StatusBadRequest, code: rpcInvalidRequest, message: "Initialize requires a request ID.", data: rpcErrorData{Code: "invalid_request"}})
			return
		}
		if transport == sessionLegacy {
			s.initializeLegacySession(w, r, authority, message)
			return
		}
		s.initializeSession(w, r, authority, message)
		return
	}
	record, binding, refused := s.requireSession(r.Context(), r, authority, transport)
	if refused != nil {
		writeRPCFailure(w, message.id, *refused)
		return
	}
	if message.response {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if !message.hasID {
		if message.method == "notifications/initialized" && (transport != sessionLegacy || s.legacyNegotiated(record.id)) {
			_ = s.Sessions.markSessionInitialized(r.Context(), record.id, binding)
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	s.serveSessionRequest(w, r, authority, record, message)
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
	ProtocolVersion string `json:"protocolVersion"`
}

type toolsCapability struct {
	ListChanged bool `json:"listChanged"`
}

type initializeCapabilities struct {
	Tools toolsCapability `json:"tools"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeResult struct {
	ProtocolVersion string                 `json:"protocolVersion"`
	Capabilities    initializeCapabilities `json:"capabilities"`
	ServerInfo      serverInfo             `json:"serverInfo"`
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
		writeRPCFailure(w, message.id, rpcFailure{status: http.StatusBadRequest, code: rpcInvalidParams, message: "Supply initialize parameters with a protocol version.", data: rpcErrorData{Code: "invalid_params"}})
		return
	}
	candidate, refused := s.createSessionRecord(r, authority, negotiateProtocol(params.ProtocolVersion))
	if refused != nil {
		writeRPCFailure(w, message.id, *refused)
		return
	}
	w.Header().Set("Mcp-Session-Id", candidate.id)
	writeInitializeResult(w, message.id, candidate)
}

func writeInitializeResult(w http.ResponseWriter, id json.RawMessage, record sessionRecord) {
	writeRPCResult(w, http.StatusOK, id, initializeResult{
		ProtocolVersion: record.protocol,
		Capabilities:    initializeCapabilities{Tools: toolsCapability{ListChanged: false}},
		ServerInfo:      serverInfo{Name: "openllmproxy-tools", Version: "1"},
	})
}

func (s *Server) createSessionRecord(r *http.Request, authority access.Authority, protocol string) (sessionRecord, *rpcFailure) {
	release, snapshot, failure := s.snapshot()
	if failure != nil {
		refused := failureRPC(failure.status, rpcServerError, failure)
		return sessionRecord{}, &refused
	}
	resolved, failure := s.sessionToolset(authority, snapshot, r.URL.Query().Get("toolset_id"))
	if failure != nil {
		if failure == errToolDefinitionChanged {
			refused := failureRPC(http.StatusOK, rpcServerError, failure)
			return sessionRecord{}, &refused
		}
		refused := failureRPC(failure.status, rpcInvalidParams, failure)
		return sessionRecord{}, &refused
	}
	if s.Sessions == nil {
		refused := failureRPC(http.StatusServiceUnavailable, rpcServerError, errSessionCreateUnavailable)
		return sessionRecord{}, &refused
	}
	certifications := s.certifyToolsetServers(r.Context(), authority, resolved)
	for _, state := range certifications {
		if state == certificationInvalid {
			refused := failureRPC(errCatalogUnavailable.status, rpcServerError, errCatalogUnavailable)
			return sessionRecord{}, &refused
		}
	}
	sessionID, err := newSessionID()
	if err != nil {
		refused := failureRPC(http.StatusServiceUnavailable, rpcServerError, errSessionCreateUnavailable)
		return sessionRecord{}, &refused
	}
	candidate := sessionRecord{
		id: sessionID, binding: bindingOf(authority), protocol: protocol,
		toolsetID: resolved.id, toolsetRevisionID: resolved.revisionID,
		releaseID: releaseIDFor(release, snapshot), certifications: certifications,
	}
	created, err := s.Sessions.createSession(r.Context(), candidate)
	if err != nil {
		refused := failureRPC(http.StatusServiceUnavailable, rpcServerError, errSessionCreateUnavailable)
		return sessionRecord{}, &refused
	}
	if !created {
		refused := rpcFailure{status: http.StatusTooManyRequests, code: rpcServerError, message: errSessionLimit.msg, data: rpcErrorData{Code: errSessionLimit.code, Limit: maxSessionsPerKey}}
		return sessionRecord{}, &refused
	}
	return candidate, nil
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

type pingResult struct{}

func (s *Server) serveSessionRequest(w http.ResponseWriter, r *http.Request, authority access.Authority, current sessionRecord, message rpcMessage) {
	if !current.initialized {
		writeRPCFailure(w, message.id, failureRPC(http.StatusOK, rpcNotInitialized, errSessionNotInitialized))
		return
	}
	switch message.method {
	case "ping":
		resolved, _, _, failure := s.pinnedToolset(current)
		if failure != nil {
			s.writeSessionFailure(w, r, current, message.id, failure)
			return
		}
		if failure = s.reauthorizeSession(authority, current, resolved); failure != nil {
			s.writeSessionFailure(w, r, current, message.id, failure)
			return
		}
		writeRPCResult(w, http.StatusOK, message.id, pingResult{})
	case "tools/list":
		s.sessionListTools(w, r, authority, current, message)
	case "tools/call":
		s.sessionCallTool(w, r, authority, current, message)
	default:
		if capability, ok := unsupportedCapability(message.method); ok {
			writeRPCFailure(w, message.id, rpcFailure{status: http.StatusOK, code: rpcMethodNotFound, message: "The " + capability + " capability is not supported at this endpoint.", data: rpcErrorData{Code: "unsupported_capability", Capability: capability}})
			return
		}
		writeRPCFailure(w, message.id, rpcFailure{status: http.StatusOK, code: rpcMethodNotFound, message: "Method not found.", data: rpcErrorData{Code: "method_not_found"}})
	}
}

func (s *Server) reauthorizeSession(authority access.Authority, current sessionRecord, resolved resolvedToolset) *apiError {
	if err := s.Runtime.AuthorizeAgentResource(authority, "tools", current.toolsetID, current.toolsetRevisionID, *authority.ProjectID); err != nil {
		if errors.Is(err, runtime.ErrStaleAuthority) {
			return fail(http.StatusServiceUnavailable, "authority_unavailable", "api_error", "Key authority is unavailable; retry shortly.")
		}
		if s.driftScoped(authority, resolved) {
			if len(resolved.tools) == 0 {
				return errToolDefinitionChanged
			}
			return nil
		}
		return errToolsetNotFound
	}
	return nil
}

func (s *Server) writeSessionFailure(w http.ResponseWriter, r *http.Request, current sessionRecord, id json.RawMessage, failure *apiError) {
	if failure == errToolsetNotFound {
		s.endSession(r.Context(), current)
	}
	writeRPCFailure(w, id, failureRPC(http.StatusOK, rpcServerError, failure))
}

func (s *Server) pinnedToolset(current sessionRecord) (resolvedToolset, *runtime.Release, *routing.Snapshot, *apiError) {
	release, snapshot, failure := s.snapshot()
	if failure != nil {
		return resolvedToolset{}, nil, nil, failure
	}
	pinned := slices.ContainsFunc(snapshot.AgentPublications, func(revision agentcontract.Revision) bool {
		return revision.ID == current.toolsetRevisionID && revision.PublicationID == current.toolsetID &&
			revision.ProjectID == current.binding.projectID && revision.Kind == "toolset"
	})
	if !pinned {
		return resolvedToolset{}, nil, nil, errToolsetNotFound
	}
	resolution, ok := snapshot.MCPToolset(current.toolsetRevisionID)
	if !ok {
		return resolvedToolset{}, nil, nil, errToolsetNotFound
	}
	return toolsetFromResolution(current.toolsetID, current.toolsetRevisionID, resolution), release, snapshot, nil
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type listToolsResult struct {
	Tools []wireTool `json:"tools"`
}

func (s *Server) endSession(ctx context.Context, current sessionRecord) {
	if s.Sessions == nil {
		return
	}
	_, _ = s.Sessions.deleteSession(ctx, current.id, current.binding)
}

func (s *Server) sessionListTools(w http.ResponseWriter, r *http.Request, authority access.Authority, current sessionRecord, message rpcMessage) {
	var params struct {
		Cursor string `json:"cursor"`
	}
	if len(message.params) > 0 && string(message.params) != "null" && json.Unmarshal(message.params, &params) != nil {
		writeRPCFailure(w, message.id, rpcFailure{status: http.StatusOK, code: rpcInvalidParams, message: "Supply valid tools/list parameters.", data: rpcErrorData{Code: "invalid_params"}})
		return
	}
	if params.Cursor != "" {
		writeRPCFailure(w, message.id, rpcFailure{status: http.StatusOK, code: rpcInvalidParams, message: "The cursor was not issued by this endpoint.", data: rpcErrorData{Code: "invalid_cursor"}})
		return
	}
	resolved, _, _, failure := s.pinnedToolset(current)
	if failure != nil {
		s.writeSessionFailure(w, r, current, message.id, failure)
		return
	}
	if failure = s.reauthorizeSession(authority, current, resolved); failure != nil {
		s.writeSessionFailure(w, r, current, message.id, failure)
		return
	}
	if failure = resolved.listingFailure(); failure != nil {
		writeRPCFailure(w, message.id, failureRPC(http.StatusOK, rpcServerError, failure))
		return
	}
	tools := []wireTool{}
	for _, tool := range resolved.tools {
		tools = append(tools, wireTool{Name: tool.Alias, Description: tool.Description, InputSchema: tool.InputSchema})
	}
	writeRPCResult(w, http.StatusOK, message.id, listToolsResult{Tools: tools})
}

type sessionCallRequest struct {
	Name      string           `json:"name"`
	Arguments json.RawMessage  `json:"arguments"`
	Meta      *callRequestMeta `json:"_meta"`
}

type callRequestMeta struct {
	CallID *string `json:"olp.call_id"`
}

func (r sessionCallRequest) callID() *string {
	if r.Meta == nil {
		return nil
	}
	return r.Meta.CallID
}

type callMeta struct {
	AttemptID         string `json:"olp.attempt_id"`
	DispatchID        string `json:"olp.dispatch_id"`
	ToolsetID         string `json:"olp.toolset_id"`
	ToolsetRevisionID string `json:"olp.toolset_revision_id"`
}

type sessionCallResult struct {
	Content           []mcpservers.ToolContent `json:"content"`
	StructuredContent json.RawMessage          `json:"structuredContent,omitempty"`
	IsError           bool                     `json:"isError"`
	Meta              callMeta                 `json:"_meta"`
}

func (s *Server) sessionCallTool(w http.ResponseWriter, r *http.Request, authority access.Authority, current sessionRecord, message rpcMessage) {
	var params sessionCallRequest
	if len(message.params) == 0 || json.Unmarshal(message.params, &params) != nil || params.Name == "" {
		writeRPCFailure(w, message.id, rpcFailure{status: http.StatusOK, code: rpcInvalidParams, message: "Supply the tool name to call.", data: rpcErrorData{Code: "invalid_params"}})
		return
	}
	if failure := callIDFailure(params.callID()); failure != nil {
		writeRPCFailure(w, message.id, rpcFailure{status: http.StatusOK, code: rpcInvalidParams, message: failure.msg, data: rpcErrorData{Code: failure.code}})
		return
	}
	resolved, release, snapshot, failure := s.pinnedToolset(current)
	if failure != nil {
		s.writeSessionFailure(w, r, current, message.id, failure)
		return
	}
	if failure = s.reauthorizeSession(authority, current, resolved); failure != nil {
		s.writeSessionFailure(w, r, current, message.id, failure)
		return
	}
	tool, found := resolved.lookup(params.Name)
	switch found {
	case toolFound:
	case toolDrifted:
		writeRPCFailure(w, message.id, failureRPC(http.StatusOK, rpcServerError, errToolDefinitionChanged))
		return
	case toolInvalidSchema:
		writeRPCFailure(w, message.id, failureRPC(http.StatusOK, rpcServerError, errToolSchemaInvalid))
		return
	case toolServerMissing:
		s.writeSessionFailure(w, r, current, message.id, errToolsetNotFound)
		return
	default:
		writeRPCFailure(w, message.id, failureRPC(http.StatusOK, rpcInvalidParams, errToolNotFound))
		return
	}
	certification := current.certification(tool.ServerID)
	if certification == certificationDrifted {
		writeRPCFailure(w, message.id, failureRPC(http.StatusOK, rpcServerError, errToolDefinitionChanged))
		return
	}
	arguments := params.Arguments
	if len(arguments) == 0 || string(arguments) == "null" {
		arguments = json.RawMessage(`{}`)
	}
	if err := tool.ValidateArguments(arguments); err != nil {
		writeRPCFailure(w, message.id, rpcFailure{status: http.StatusOK, code: rpcInvalidParams, message: "The arguments do not match the pinned tool schema.", data: rpcErrorData{Code: "invalid_tool_arguments"}})
		return
	}
	if s.Ledger == nil || s.Egress == nil || s.Auth == nil {
		writeRPCFailure(w, message.id, failureRPC(http.StatusOK, rpcServerError, errLedger))
		return
	}
	outcome := s.execute(r.Context(), executeInput{authority: authority, resolved: resolved, tool: tool, arguments: arguments, releaseID: current.releaseID, callID: *params.callID(), release: release, snapshot: snapshot})
	if outcome.failure == errPermissionDenied && s.driftScoped(authority, resolved) {
		writeRPCFailure(w, message.id, failureRPC(http.StatusOK, rpcServerError, errToolDefinitionChanged))
		return
	}
	if outcome.repeat != nil {
		s.writeSessionRepeat(w, message.id, resolved, outcome)
		return
	}
	s.recordCallCertification(r.Context(), current, tool, certification, outcome)
	switch {
	case outcome.blocked != nil:
		refused := failureRPC(http.StatusOK, rpcServerError, outcome.blocked)
		refused.data.Status = string(StatusGuardrailBlocked)
		refused.data.AttemptID = outcome.reservation.AttemptID
		refused.data.DispatchID = outcome.reservation.DispatchID
		writeRPCFailure(w, message.id, refused)
	case outcome.failure != nil:
		refused := failureRPC(http.StatusOK, rpcServerError, outcome.failure)
		if outcome.retryAfter > 0 {
			refused.data.RetryAfterSeconds = int64((outcome.retryAfter + time.Second - 1) / time.Second)
		}
		writeRPCFailure(w, message.id, refused)
	case outcome.outcomeUnknown:
		writeRPCFailure(w, message.id, rpcFailure{status: http.StatusOK, code: rpcServerError, message: outcome.unknownMessage, data: rpcErrorData{
			Code: "outcome_unknown", Status: string(StatusOutcomeUnknown),
			AttemptID: outcome.reservation.AttemptID, DispatchID: outcome.reservation.DispatchID,
		}})
	default:
		content := outcome.result.Content
		if content == nil {
			content = []mcpservers.ToolContent{}
		}
		writeRPCResult(w, http.StatusOK, message.id, sessionCallResult{
			Content: content, StructuredContent: outcome.result.StructuredContent, IsError: outcome.result.IsError,
			Meta: callMeta{
				AttemptID: outcome.reservation.AttemptID, DispatchID: outcome.reservation.DispatchID,
				ToolsetID: resolved.id, ToolsetRevisionID: resolved.revisionID,
			},
		})
	}
}

func (s *Server) writeSessionRepeat(w http.ResponseWriter, id json.RawMessage, resolved resolvedToolset, outcome execution) {
	repeat := outcome.repeat
	kind := repeatOutcome(repeat, outcome.repeatRetained)
	if kind == repeatGuardrailBlocked {
		refused := failureRPC(http.StatusOK, rpcServerError, errGuardrailBlocked)
		refused.data.Status = string(StatusGuardrailBlocked)
		refused.data.AttemptID = repeat.AttemptID
		refused.data.DispatchID = repeat.DispatchID
		writeRPCFailure(w, id, refused)
		return
	}
	if kind == repeatRetainedResult {
		content := outcome.repeatResult.Content
		if content == nil {
			content = []mcpservers.ToolContent{}
		}
		writeRPCResult(w, http.StatusOK, id, sessionCallResult{
			Content: content, StructuredContent: outcome.repeatResult.StructuredContent, IsError: outcome.repeatResult.IsError,
			Meta: callMeta{
				AttemptID: repeat.AttemptID, DispatchID: repeat.DispatchID,
				ToolsetID: resolved.id, ToolsetRevisionID: resolved.revisionID,
			},
		})
		return
	}
	code, message := repeatFailure(kind)
	rpcCode := rpcServerError
	if kind == repeatConflict {
		rpcCode = rpcInvalidParams
	}
	data := rpcErrorData{Code: code, Status: string(repeat.Status), AttemptID: repeat.AttemptID, DispatchID: repeat.DispatchID}
	writeRPCFailure(w, id, rpcFailure{status: http.StatusOK, code: rpcCode, message: message, data: data})
}

func (s *Server) recordCallCertification(ctx context.Context, current sessionRecord, tool routing.MCPToolsetTool, before certificationState, outcome execution) {
	if s.Sessions == nil {
		return
	}
	if outcome.failure != nil {
		switch {
		case outcome.certification == certificationDrifted:
			_, _ = s.Sessions.setSessionCertification(ctx, current.id, current.binding, tool.ServerID, certificationDrifted)
		case outcome.failure == errPermissionDenied:
			s.endSession(ctx, current)
		}
		return
	}
	switch {
	case outcome.listChanged:
		_, _ = s.Sessions.setSessionCertification(ctx, current.id, current.binding, tool.ServerID, certificationInvalid)
	case before == certificationInvalid:
		_, _ = s.Sessions.setSessionCertification(ctx, current.id, current.binding, tool.ServerID, certificationCertified)
	}
}
