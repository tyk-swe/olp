package mcp

import (
	"encoding/json"
	"net/http"

	"github.com/tyk-swe/olp/internal/mcpservers"
	"github.com/tyk-swe/olp/internal/routing"
)

func writeError(w http.ResponseWriter, err *apiError) {
	writeJSON(w, err.status, toolErrorResponse{Error: toolCallError{Message: err.msg, Type: err.kind, Code: err.code}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeOutcomeUnknown(w http.ResponseWriter, attemptID, dispatchID, message string) {
	writeJSON(w, http.StatusBadGateway, toolCallRefusal{
		AttemptID: attemptID, DispatchID: dispatchID, Status: StatusOutcomeUnknown,
		Error: toolCallError{Message: message, Type: "api_error", Code: "outcome_unknown"},
	})
}

var errGuardrailBlocked = fail(http.StatusBadRequest, "guardrail_blocked", "invalid_request_error", "The tool call could not satisfy the configured guardrails.")

type toolCallError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

type toolErrorResponse struct {
	Error toolCallError `json:"error"`
}

type toolsResponse struct {
	Tools []listedTool `json:"tools"`
}

type toolCallResponse struct {
	ToolsetID         string                   `json:"toolset_id"`
	ToolsetRevisionID string                   `json:"toolset_revision_id"`
	Name              string                   `json:"name"`
	Content           []mcpservers.ToolContent `json:"content"`
	StructuredContent json.RawMessage          `json:"structured_content"`
	IsError           bool                     `json:"is_error"`
	AttemptID         string                   `json:"attempt_id"`
	DispatchID        string                   `json:"dispatch_id"`
}

func writeToolResult(w http.ResponseWriter, resolved resolvedToolset, tool routing.MCPToolsetTool, result mcpservers.ToolResult, attemptID, dispatchID string) {
	writeJSON(w, http.StatusOK, toolCallResponse{
		ToolsetID: resolved.id, ToolsetRevisionID: resolved.revisionID, Name: tool.Alias,
		Content: result.Content, StructuredContent: result.StructuredContent, IsError: result.IsError,
		AttemptID: attemptID, DispatchID: dispatchID,
	})
}

type toolCallRefusal struct {
	AttemptID  string        `json:"attempt_id"`
	DispatchID string        `json:"dispatch_id"`
	Status     Status        `json:"status"`
	Error      toolCallError `json:"error"`
}

func writeGuardrailRefusal(w http.ResponseWriter, attemptID, dispatchID string, refusal *apiError) {
	writeJSON(w, refusal.status, toolCallRefusal{
		AttemptID: attemptID, DispatchID: dispatchID, Status: StatusGuardrailBlocked,
		Error: toolCallError{Message: refusal.msg, Type: refusal.kind, Code: refusal.code},
	})
}

func writeRepeat(w http.ResponseWriter, resolved resolvedToolset, tool routing.MCPToolsetTool, outcome execution) {
	repeat := outcome.repeat
	kind := repeatOutcome(repeat, outcome.repeatRetained)
	if kind == repeatGuardrailBlocked {
		writeGuardrailRefusal(w, repeat.AttemptID, repeat.DispatchID, errGuardrailBlocked)
		return
	}
	if kind == repeatRetainedResult {
		writeToolResult(w, resolved, tool, outcome.repeatResult, repeat.AttemptID, repeat.DispatchID)
		return
	}
	code, message := repeatFailure(kind)
	if kind == repeatOutcomeUnknown {
		writeOutcomeUnknown(w, repeat.AttemptID, repeat.DispatchID, message)
		return
	}
	writeJSON(w, http.StatusConflict, toolCallRefusal{
		AttemptID: repeat.AttemptID, DispatchID: repeat.DispatchID, Status: repeat.Status,
		Error: toolCallError{Message: message, Type: "invalid_request_error", Code: code},
	})
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

func writeSessionToolResult(w http.ResponseWriter, id json.RawMessage, resolved resolvedToolset, result mcpservers.ToolResult, attemptID, dispatchID string) {
	content := func() []mcpservers.ToolContent {
		if result.Content == nil {
			return []mcpservers.ToolContent{}
		}
		return result.Content
	}()
	writeRPCResult(w, http.StatusOK, id, sessionCallResult{
		Content: content, StructuredContent: result.StructuredContent, IsError: result.IsError,
		Meta: callMeta{
			AttemptID: attemptID, DispatchID: dispatchID,
			ToolsetID: resolved.id, ToolsetRevisionID: resolved.revisionID,
		},
	})
}

func writeSessionRepeat(w http.ResponseWriter, id json.RawMessage, resolved resolvedToolset, outcome execution) {
	repeat := outcome.repeat
	kind := repeatOutcome(repeat, outcome.repeatRetained)
	if kind == repeatGuardrailBlocked {
		writeRPCFailure(w, id, rpcFailure{
			status: http.StatusOK, code: rpcServerError, message: errGuardrailBlocked.msg,
			data: rpcErrorData{Code: errGuardrailBlocked.code, Status: string(StatusGuardrailBlocked), AttemptID: repeat.AttemptID, DispatchID: repeat.DispatchID},
		})
		return
	}
	if kind == repeatRetainedResult {
		writeSessionToolResult(w, id, resolved, outcome.repeatResult, repeat.AttemptID, repeat.DispatchID)
		return
	}
	code, message := repeatFailure(kind)
	data := rpcErrorData{Code: code, Status: string(repeat.Status), AttemptID: repeat.AttemptID, DispatchID: repeat.DispatchID}
	if kind == repeatConflict {
		writeRPCFailure(w, id, rpcFailure{status: http.StatusOK, code: rpcInvalidParams, message: message, data: data})
		return
	}
	writeRPCFailure(w, id, rpcFailure{status: http.StatusOK, code: rpcServerError, message: message, data: data})
}
