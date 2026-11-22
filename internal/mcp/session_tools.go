package mcp

import (
	"context"
	"encoding/json"
	"net/http"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/mcp/messages"
	"github.com/tyk-swe/olp/internal/mcp/sessions"
	"github.com/tyk-swe/olp/internal/routing"
)

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

func (s *Server) sessionListTools(w http.ResponseWriter, r *http.Request, authority keyauthority.Authority, current sessions.Record, message messages.Message) {
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

func (s *Server) sessionCallTool(w http.ResponseWriter, r *http.Request, authority keyauthority.Authority, current sessions.Record, message messages.Message) {
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
