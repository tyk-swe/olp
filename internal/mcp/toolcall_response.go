package mcp

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/tyk-swe/olp/internal/mcp/accounting"
	"github.com/tyk-swe/olp/internal/mcp/upstream"
	"github.com/tyk-swe/olp/internal/routing"
)

func writeError(w http.ResponseWriter, err *apiError) {
	writeJSON(w, err.status, toolErrorResponse{Error: toolCallError{Message: err.msg, Type: err.kind, Code: err.code}})
}

func writeJSON[T any](w http.ResponseWriter, status int, body T) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeOutcomeUnknown(w http.ResponseWriter, attemptID, dispatchID, message string) {
	writeJSON(w, http.StatusBadGateway, toolCallRefusal{
		AttemptID: attemptID, DispatchID: dispatchID, Status: accounting.StatusOutcomeUnknown,
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
	ToolsetID         string                 `json:"toolset_id"`
	ToolsetRevisionID string                 `json:"toolset_revision_id"`
	Name              string                 `json:"name"`
	Content           []upstream.ToolContent `json:"content"`
	StructuredContent json.RawMessage        `json:"structured_content"`
	IsError           bool                   `json:"is_error"`
	AttemptID         string                 `json:"attempt_id"`
	DispatchID        string                 `json:"dispatch_id"`
}

func writeToolResult(w http.ResponseWriter, resolved resolvedToolset, tool routing.MCPToolsetTool, result upstream.ToolResult, attemptID, dispatchID string) {
	writeJSON(w, http.StatusOK, toolCallResponse{
		ToolsetID: resolved.id, ToolsetRevisionID: resolved.revisionID, Name: tool.Alias,
		Content: result.Content, StructuredContent: result.StructuredContent, IsError: result.IsError,
		AttemptID: attemptID, DispatchID: dispatchID,
	})
}

type toolCallRefusal struct {
	AttemptID  string            `json:"attempt_id"`
	DispatchID string            `json:"dispatch_id"`
	Status     accounting.Status `json:"status"`
	Error      toolCallError     `json:"error"`
}

func writeGuardrailRefusal(w http.ResponseWriter, attemptID, dispatchID string, refusal *apiError) {
	writeJSON(w, refusal.status, toolCallRefusal{
		AttemptID: attemptID, DispatchID: dispatchID, Status: accounting.StatusGuardrailBlocked,
		Error: toolCallError{Message: refusal.msg, Type: refusal.kind, Code: refusal.code},
	})
}

func writeRepeat(w http.ResponseWriter, outcome ReplayUnavailable) {
	repeat := outcome.Call
	kind := outcome.Reason
	if kind == repeatGuardrailBlocked {
		writeGuardrailRefusal(w, repeat.AttemptID, repeat.DispatchID, errGuardrailBlocked)
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
	Content           []upstream.ToolContent `json:"content"`
	StructuredContent json.RawMessage        `json:"structuredContent,omitempty"`
	IsError           bool                   `json:"isError"`
	Meta              callMeta               `json:"_meta"`
}

func writeSessionToolResult(w http.ResponseWriter, id json.RawMessage, resolved resolvedToolset, result upstream.ToolResult, attemptID, dispatchID string) {
	content := func() []upstream.ToolContent {
		if result.Content == nil {
			return []upstream.ToolContent{}
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

func writeSessionRepeat(w http.ResponseWriter, id json.RawMessage, outcome ReplayUnavailable) {
	repeat := outcome.Call
	kind := outcome.Reason
	if kind == repeatGuardrailBlocked {
		writeRPCFailure(w, id, rpcFailure{
			status: http.StatusOK, code: rpcServerError, message: errGuardrailBlocked.msg,
			data: rpcErrorData{Code: errGuardrailBlocked.code, Status: string(accounting.StatusGuardrailBlocked), AttemptID: repeat.AttemptID, DispatchID: repeat.DispatchID},
		})
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

func apiFailure(failure *Failure) *apiError {
	if failure.Guardrail != nil {
		return fail(failure.Guardrail.Status, failure.Code, "invalid_request_error", failure.Message)
	}
	switch failure.Code {
	case "permission_denied", "user_grant_required", "delegation_unavailable":
		return fail(http.StatusForbidden, failure.Code, "permission_error", failure.Message)
	case "budget_exceeded", "tool_rate_limited", "task_limit_exceeded":
		return fail(http.StatusTooManyRequests, failure.Code, "rate_limit_error", failure.Message)
	case "call_id_required", "invalid_call_id", "guardrail_blocked":
		return fail(http.StatusBadRequest, failure.Code, "invalid_request_error", failure.Message)
	case "tool_unpriced":
		return fail(http.StatusUnprocessableEntity, failure.Code, "invalid_request_error", failure.Message)
	case "tool_definition_changed":
		return fail(http.StatusConflict, failure.Code, "api_error", failure.Message)
	case "upstream_unavailable":
		return fail(http.StatusBadGateway, failure.Code, "api_error", failure.Message)
	case "guardrail_unavailable":
		return fail(http.StatusInternalServerError, failure.Code, "api_error", failure.Message)
	default:
		return fail(http.StatusServiceUnavailable, failure.Code, "api_error", failure.Message)
	}
}

func writeExecution(w http.ResponseWriter, resolved resolvedToolset, tool routing.MCPToolsetTool, execution Execution) {
	switch outcome := execution.Outcome.(type) {
	case Completed:
		writeToolResult(w, resolved, tool, outcome.Result, outcome.Receipt.AttemptID, outcome.Receipt.DispatchID)
	case Replayed:
		writeToolResult(w, resolved, tool, outcome.Result, outcome.Receipt.AttemptID, outcome.Receipt.DispatchID)
	case ReplayUnavailable:
		writeRepeat(w, outcome)
	case Refused:
		if outcome.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.FormatInt(int64((outcome.RetryAfter+time.Second-1)/time.Second), 10))
		}
		writeError(w, apiFailure(outcome.Failure))
	case Blocked:
		writeGuardrailRefusal(w, outcome.Receipt.AttemptID, outcome.Receipt.DispatchID, apiFailure(outcome.Failure))
	case Uncertain:
		writeOutcomeUnknown(w, outcome.Receipt.AttemptID, outcome.Receipt.DispatchID, outcome.Message)
	}
}

func writeSessionExecution(w http.ResponseWriter, id json.RawMessage, resolved resolvedToolset, execution Execution) {
	switch outcome := execution.Outcome.(type) {
	case Completed:
		writeSessionToolResult(w, id, resolved, outcome.Result, outcome.Receipt.AttemptID, outcome.Receipt.DispatchID)
	case Replayed:
		writeSessionToolResult(w, id, resolved, outcome.Result, outcome.Receipt.AttemptID, outcome.Receipt.DispatchID)
	case ReplayUnavailable:
		writeSessionRepeat(w, id, outcome)
	case Refused:
		failure := apiFailure(outcome.Failure)
		writeRPCFailure(w, id, rpcFailure{status: http.StatusOK, code: rpcServerError, message: failure.msg,
			data: rpcErrorData{Code: failure.code, RetryAfterSeconds: int64((outcome.RetryAfter + time.Second - 1) / time.Second)}})
	case Blocked:
		failure := apiFailure(outcome.Failure)
		writeRPCFailure(w, id, rpcFailure{status: http.StatusOK, code: rpcServerError, message: failure.msg,
			data: rpcErrorData{Code: failure.code, Status: string(accounting.StatusGuardrailBlocked), AttemptID: outcome.Receipt.AttemptID, DispatchID: outcome.Receipt.DispatchID}})
	case Uncertain:
		writeRPCFailure(w, id, rpcFailure{status: http.StatusOK, code: rpcServerError, message: outcome.Message,
			data: rpcErrorData{Code: "outcome_unknown", Status: string(accounting.StatusOutcomeUnknown), AttemptID: outcome.Receipt.AttemptID, DispatchID: outcome.Receipt.DispatchID}})
	}
}
