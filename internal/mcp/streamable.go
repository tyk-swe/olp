package mcp

import (
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
	"github.com/tyk-swe/olp/internal/mcp/messages"
	"github.com/tyk-swe/olp/internal/mcp/sessions"
	"github.com/tyk-swe/olp/internal/mcp/upstream"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
)

const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcServerError    = -32000
	rpcNotInitialized = -32002
)

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

func writeRPCResult[T any](w http.ResponseWriter, status int, id json.RawMessage, result T) {
	encoded, err := json.Marshal(result)
	if err != nil {
		writeRPCFailure(w, id, rpcFailure{status: http.StatusInternalServerError, code: rpcServerError, message: "The result could not be encoded.", data: rpcErrorData{Code: "encode_failed"}})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(messages.Envelope{JSONRPC: "2.0", ID: messages.IDOrNull(id), Result: encoded})
}

func writeRPCError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string, data rpcErrorData) {
	encoded, _ := json.Marshal(data)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(messages.Envelope{JSONRPC: "2.0", ID: messages.IDOrNull(id), Error: &messages.Error{Code: code, Message: message, Data: encoded}})
}

func writeRPCFailure(w http.ResponseWriter, id json.RawMessage, failure rpcFailure) {
	writeRPCError(w, failure.status, id, failure.code, failure.message, failure.data)
}

func parseRPCMessage(raw []byte) (messages.Message, int, bool) {
	message, code := messages.Parse(raw, oif.Limits{MaxBytes: maxCallBodyBytes, MaxDepth: 64, MaxNodes: 1 << 16})
	if code != 0 {
		return messages.Message{}, code, false
	}
	var selection executionSelection
	if json.Unmarshal(raw, &selection) != nil || selection.supplied() {
		return messages.Message{}, rpcInvalidRequest, false
	}
	return message, 0, true
}

func acceptsStreamableHTTP(r *http.Request) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return false
	}
	accept := r.Header.Get("Accept")
	return accept == "" || strings.Contains(accept, "application/json") || strings.Contains(accept, "text/event-stream") || strings.Contains(accept, "*/*")
}

func (s *Server) requireSession(ctx context.Context, r *http.Request, authority access.Authority, transport sessionTransport) (sessions.Record, sessions.Binding, *rpcFailure) {
	binding := sessions.BindingOf(authority)
	if s.sessions == nil {
		failure := failureRPC(http.StatusServiceUnavailable, rpcServerError, errSessionUnavailable)
		return sessions.Record{}, binding, &failure
	}
	sessionID := r.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		failure := failureRPC(http.StatusBadRequest, rpcInvalidRequest, errSessionRequired)
		return sessions.Record{}, binding, &failure
	}
	record, found, err := s.sessions.Access(ctx, sessionID, binding)
	if err != nil {
		failure := failureRPC(http.StatusServiceUnavailable, rpcServerError, errSessionUnavailable)
		return sessions.Record{}, binding, &failure
	}
	if !found {
		failure := failureRPC(http.StatusNotFound, rpcInvalidRequest, errSessionNotFound)
		return sessions.Record{}, binding, &failure
	}
	if (record.Protocol == upstream.LegacyProtocol) != (transport == sessionLegacy) {
		failure := failureRPC(http.StatusBadRequest, rpcInvalidRequest, errSessionProtocolMismatch)
		return sessions.Record{}, binding, &failure
	}
	if protocol := r.Header.Get("MCP-Protocol-Version"); protocol != "" && protocol != record.Protocol {
		failure := failureRPC(http.StatusBadRequest, rpcInvalidRequest, errSessionProtocolMismatch)
		return sessions.Record{}, binding, &failure
	}
	return record, binding, nil
}

func (s *Server) serveStreamableGET(w http.ResponseWriter, r *http.Request) {
	if _, failure := s.reauthenticateSession(r); failure != nil {
		writeRPCFailure(w, nil, failureRPC(failure.status, rpcServerError, failure))
		return
	}
	w.Header().Set("Allow", "POST, DELETE")
	writeRPCFailure(w, nil, rpcFailure{status: http.StatusMethodNotAllowed, code: rpcServerError, message: "This endpoint does not offer a standalone event stream.", data: rpcErrorData{Code: "method_not_allowed"}})
}

func (s *Server) serveStreamableDELETE(w http.ResponseWriter, r *http.Request) {
	authority, failure := s.authenticateSession(r)
	if failure != nil {
		writeRPCFailure(w, nil, failureRPC(failure.status, rpcServerError, failure))
		return
	}
	record, binding, refused := s.requireSession(r.Context(), r, authority, sessionStreamable)
	if refused != nil {
		writeRPCFailure(w, nil, *refused)
		return
	}
	deleted, err := s.sessions.Delete(r.Context(), record.ID, binding)
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
	authority, failure := s.authenticateSessionMessage(r, message, transport)
	if failure != nil {
		writeRPCFailure(w, message.ID, failureRPC(failure.status, rpcServerError, failure))
		return
	}
	if (message.Kind != messages.Response) && message.Method == "initialize" {
		if len(message.ID) == 0 {
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
		writeRPCFailure(w, message.ID, *refused)
		return
	}
	if message.Kind == messages.Response {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if len(message.ID) == 0 {
		if message.Method == "notifications/initialized" && (transport != sessionLegacy || s.legacyNegotiated(record.ID)) {
			_ = s.sessions.Initialize(r.Context(), record.ID, binding)
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	s.serveSessionRequest(w, r, authority, record, message)
}

func (s *Server) authenticateSessionMessage(r *http.Request, message messages.Message, transport sessionTransport) (access.Authority, *apiError) {
	if transport == sessionStreamable && (message.Kind != messages.Response) && message.Method == "initialize" {
		return s.authenticate(r)
	}
	return s.authenticateSession(r)
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

func (s *Server) initializeSession(w http.ResponseWriter, r *http.Request, authority access.Authority, message messages.Message) {
	var params initializeRequest
	if len(message.Params) == 0 || json.Unmarshal(message.Params, &params) != nil || params.ProtocolVersion == "" {
		writeRPCFailure(w, message.ID, rpcFailure{status: http.StatusBadRequest, code: rpcInvalidParams, message: "Supply initialize parameters with a protocol version.", data: rpcErrorData{Code: "invalid_params"}})
		return
	}
	candidate, refused := s.createSessionRecord(r, authority, messages.NegotiateProtocol(params.ProtocolVersion))
	if refused != nil {
		writeRPCFailure(w, message.ID, *refused)
		return
	}
	w.Header().Set("Mcp-Session-Id", candidate.ID)
	writeInitializeResult(w, message.ID, candidate)
}

func writeInitializeResult(w http.ResponseWriter, id json.RawMessage, record sessions.Record) {
	writeRPCResult(w, http.StatusOK, id, initializeResult{
		ProtocolVersion: record.Protocol,
		Capabilities:    initializeCapabilities{Tools: toolsCapability{ListChanged: false}},
		ServerInfo:      serverInfo{Name: "openllmproxy-tools", Version: "1"},
	})
}

func (s *Server) createSessionRecord(r *http.Request, authority access.Authority, protocol string) (sessions.Record, *rpcFailure) {
	release, snapshot, failure := s.snapshot()
	if failure != nil {
		refused := failureRPC(failure.status, rpcServerError, failure)
		return sessions.Record{}, &refused
	}
	resolved, failure := s.sessionToolset(authority, snapshot, r.URL.Query().Get("toolset_id"))
	if failure != nil {
		if failure == errToolDefinitionChanged {
			refused := failureRPC(http.StatusOK, rpcServerError, failure)
			return sessions.Record{}, &refused
		}
		refused := failureRPC(failure.status, rpcInvalidParams, failure)
		return sessions.Record{}, &refused
	}
	if s.sessions == nil {
		refused := failureRPC(http.StatusServiceUnavailable, rpcServerError, errSessionCreateUnavailable)
		return sessions.Record{}, &refused
	}
	certifications := s.certifyToolsetServers(r.Context(), authority, resolved)
	for _, state := range certifications {
		if state == sessions.Invalid {
			refused := failureRPC(errCatalogUnavailable.status, rpcServerError, errCatalogUnavailable)
			return sessions.Record{}, &refused
		}
	}
	sessionID, err := sessions.NewID()
	if err != nil {
		refused := failureRPC(http.StatusServiceUnavailable, rpcServerError, errSessionCreateUnavailable)
		return sessions.Record{}, &refused
	}
	candidate := sessions.Record{
		ID: sessionID, Binding: sessions.BindingOf(authority), Protocol: protocol,
		ToolsetID: resolved.id, ToolsetRevisionID: resolved.revisionID,
		ReleaseID: releaseIDFor(release, snapshot), Certifications: certifications,
	}
	created, err := s.sessions.Create(r.Context(), candidate)
	if err != nil {
		refused := failureRPC(http.StatusServiceUnavailable, rpcServerError, errSessionCreateUnavailable)
		return sessions.Record{}, &refused
	}
	if !created {
		refused := rpcFailure{status: http.StatusTooManyRequests, code: rpcServerError, message: errSessionLimit.msg, data: rpcErrorData{Code: errSessionLimit.code, Limit: sessions.MaxPerKey}}
		return sessions.Record{}, &refused
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

func (s *Server) serveSessionRequest(w http.ResponseWriter, r *http.Request, authority access.Authority, current sessions.Record, message messages.Message) {
	if !current.Initialized {
		writeRPCFailure(w, message.ID, failureRPC(http.StatusOK, rpcNotInitialized, errSessionNotInitialized))
		return
	}
	switch message.Method {
	case "ping":
		resolved, _, _, failure := s.pinnedToolset(current)
		if failure != nil {
			s.writeSessionFailure(w, r, current, message.ID, failure)
			return
		}
		if failure = s.reauthorizeSession(authority, current, resolved); failure != nil {
			s.writeSessionFailure(w, r, current, message.ID, failure)
			return
		}
		writeRPCResult(w, http.StatusOK, message.ID, pingResult{})
	case "tools/list":
		s.sessionListTools(w, r, authority, current, message)
	case "tools/call":
		s.sessionCallTool(w, r, authority, current, message)
	default:
		if capability, ok := unsupportedCapability(message.Method); ok {
			writeRPCFailure(w, message.ID, rpcFailure{status: http.StatusOK, code: rpcMethodNotFound, message: "The " + capability + " capability is not supported at this endpoint.", data: rpcErrorData{Code: "unsupported_capability", Capability: capability}})
			return
		}
		writeRPCFailure(w, message.ID, rpcFailure{status: http.StatusOK, code: rpcMethodNotFound, message: "Method not found.", data: rpcErrorData{Code: "method_not_found"}})
	}
}

func (s *Server) reauthorizeSession(authority access.Authority, current sessions.Record, resolved resolvedToolset) *apiError {
	if err := s.runtime.AuthorizeAgentResource(authority, "tools", current.ToolsetID, current.ToolsetRevisionID, *authority.ProjectID); err != nil {
		if errors.Is(err, release.ErrStaleAuthority) {
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

func (s *Server) writeSessionFailure(w http.ResponseWriter, r *http.Request, current sessions.Record, id json.RawMessage, failure *apiError) {
	if failure == errToolsetNotFound {
		s.endSession(r.Context(), current)
	}
	writeRPCFailure(w, id, failureRPC(http.StatusOK, rpcServerError, failure))
}

func (s *Server) pinnedToolset(current sessions.Record) (resolvedToolset, *release.Release, *routing.Snapshot, *apiError) {
	release, snapshot, failure := s.snapshot()
	if failure != nil {
		return resolvedToolset{}, nil, nil, failure
	}
	pinned := slices.ContainsFunc(snapshot.AgentPublications, func(revision agentcontract.Revision) bool {
		return revision.ID == current.ToolsetRevisionID && revision.PublicationID == current.ToolsetID &&
			revision.ProjectID == current.Binding.ProjectID() && revision.Kind == "toolset"
	})
	if !pinned {
		return resolvedToolset{}, nil, nil, errToolsetNotFound
	}
	resolution, ok := snapshot.MCPToolset(current.ToolsetRevisionID)
	if !ok {
		return resolvedToolset{}, nil, nil, errToolsetNotFound
	}
	return toolsetFromResolution(current.ToolsetID, current.ToolsetRevisionID, resolution), release, snapshot, nil
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type listToolsResult struct {
	Tools []wireTool `json:"tools"`
}

func (s *Server) endSession(ctx context.Context, current sessions.Record) {
	if s.sessions == nil {
		return
	}
	_, _ = s.sessions.Delete(ctx, current.ID, current.Binding)
}

func (s *Server) sessionListTools(w http.ResponseWriter, r *http.Request, authority access.Authority, current sessions.Record, message messages.Message) {
	var params struct {
		Cursor string `json:"cursor"`
	}
	if len(message.Params) > 0 && string(message.Params) != "null" && json.Unmarshal(message.Params, &params) != nil {
		writeRPCFailure(w, message.ID, rpcFailure{status: http.StatusOK, code: rpcInvalidParams, message: "Supply valid tools/list parameters.", data: rpcErrorData{Code: "invalid_params"}})
		return
	}
	if params.Cursor != "" {
		writeRPCFailure(w, message.ID, rpcFailure{status: http.StatusOK, code: rpcInvalidParams, message: "The cursor was not issued by this endpoint.", data: rpcErrorData{Code: "invalid_cursor"}})
		return
	}
	resolved, _, _, failure := s.pinnedToolset(current)
	if failure != nil {
		s.writeSessionFailure(w, r, current, message.ID, failure)
		return
	}
	if failure = s.reauthorizeSession(authority, current, resolved); failure != nil {
		s.writeSessionFailure(w, r, current, message.ID, failure)
		return
	}
	if failure = resolved.listingFailure(); failure != nil {
		writeRPCFailure(w, message.ID, failureRPC(http.StatusOK, rpcServerError, failure))
		return
	}
	tools := []wireTool{}
	for _, tool := range resolved.tools {
		tools = append(tools, wireTool{Name: tool.Alias, Description: tool.Description, InputSchema: tool.InputSchema})
	}
	writeRPCResult(w, http.StatusOK, message.ID, listToolsResult{Tools: tools})
}

type sessionCallRequest struct {
	executionSelection
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

func (s *Server) sessionCallTool(w http.ResponseWriter, r *http.Request, authority access.Authority, current sessions.Record, message messages.Message) {
	var params sessionCallRequest
	if len(message.Params) == 0 || json.Unmarshal(message.Params, &params) != nil || params.Name == "" {
		writeRPCFailure(w, message.ID, rpcFailure{status: http.StatusOK, code: rpcInvalidParams, message: "Supply the tool name to call.", data: rpcErrorData{Code: "invalid_params"}})
		return
	}
	if params.executionSelection.supplied() {
		writeRPCFailure(w, message.ID, rpcFailure{status: http.StatusOK, code: rpcInvalidParams, message: "The registered server determines transport and executable settings.", data: rpcErrorData{Code: "invalid_params"}})
		return
	}
	if failure := callIDFailure(params.callID()); failure != nil {
		writeRPCFailure(w, message.ID, rpcFailure{status: http.StatusOK, code: rpcInvalidParams, message: failure.msg, data: rpcErrorData{Code: failure.code}})
		return
	}
	resolved, release, snapshot, failure := s.pinnedToolset(current)
	if failure != nil {
		s.writeSessionFailure(w, r, current, message.ID, failure)
		return
	}
	if failure = s.reauthorizeSession(authority, current, resolved); failure != nil {
		s.writeSessionFailure(w, r, current, message.ID, failure)
		return
	}
	tool, found := resolved.lookup(params.Name)
	if found == toolServerMissing {
		s.writeSessionFailure(w, r, current, message.ID, errToolsetNotFound)
		return
	}
	if failure := toolLookupFailure(found); failure != nil {
		code := rpcInvalidParams
		if found == toolDrifted || found == toolInvalidSchema {
			code = rpcServerError
		}
		writeRPCFailure(w, message.ID, failureRPC(http.StatusOK, code, failure))
		return
	}
	certification := current.Certification(tool.ServerID)
	if certification == sessions.Drifted {
		writeRPCFailure(w, message.ID, failureRPC(http.StatusOK, rpcServerError, errToolDefinitionChanged))
		return
	}
	arguments := normalizedArguments(params.Arguments)
	if err := tool.ValidateArguments(arguments); err != nil {
		writeRPCFailure(w, message.ID, rpcFailure{status: http.StatusOK, code: rpcInvalidParams, message: "The arguments do not match the pinned tool schema.", data: rpcErrorData{Code: "invalid_tool_arguments"}})
		return
	}
	if !s.executor.available() {
		writeRPCFailure(w, message.ID, failureRPC(http.StatusOK, rpcServerError, errLedger))
		return
	}
	outcome := s.executor.Execute(r.Context(), CallRequest{Authority: authority, ToolsetID: resolved.id, ToolsetRevisionID: resolved.revisionID, Tool: tool, Arguments: arguments, ReleaseID: current.ReleaseID, CallID: *params.callID(), Release: release, Policies: snapshot})
	if refused, ok := outcome.Outcome.(Refused); ok && refused.Failure.Code == "permission_denied" && s.driftScoped(authority, resolved) {
		writeRPCFailure(w, message.ID, failureRPC(http.StatusOK, rpcServerError, errToolDefinitionChanged))
		return
	}
	s.recordCallCertification(r.Context(), current, tool, certification, outcome)
	writeSessionExecution(w, message.ID, resolved, outcome)
}

func (s *Server) recordCallCertification(ctx context.Context, current sessions.Record, tool routing.MCPToolsetTool, before sessions.Certification, execution Execution) {
	if s.sessions == nil {
		return
	}
	if refused, ok := execution.Outcome.(Refused); ok {
		if execution.Certification == sessions.Drifted {
			_, _ = s.sessions.Certify(ctx, current.ID, current.Binding, tool.ServerID, sessions.Drifted)
		} else if refused.Failure.Code == "permission_denied" {
			s.endSession(ctx, current)
		}
		return
	}
	if execution.ListChanged {
		_, _ = s.sessions.Certify(ctx, current.ID, current.Binding, tool.ServerID, sessions.Invalid)
		return
	}
	if before == sessions.Invalid && execution.Certification == sessions.Certified {
		_, _ = s.sessions.Certify(ctx, current.ID, current.Binding, tool.ServerID, sessions.Certified)
	}
}
