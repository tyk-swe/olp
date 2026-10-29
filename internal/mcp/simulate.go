package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/routing"
)

type toolCallLimits struct {
	ServerCallsPerMinute int64 `json:"server_calls_per_minute"`
	ServerMaxConcurrency int64 `json:"server_max_concurrency"`
	KeyCallsPerMinute    int64 `json:"key_calls_per_minute"`
	KeyMaxConcurrency    int64 `json:"key_max_concurrency"`
	TimeoutSeconds       int64 `json:"timeout_seconds"`
	RequestBytes         int64 `json:"request_bytes"`
	ResultBytes          int64 `json:"result_bytes"`
}

func tighterToolLimit(server int64, key *int64) *int64 {
	if key != nil && *key < server {
		return key
	}
	return &server
}

func describeTool(authority access.Authority, resolved resolvedToolset, tool routing.MCPToolsetTool) listedTool {
	effective := routing.EffectiveToolLimits(tool.CallsPerMinute, tool.MaxConcurrency, tool.PerKeyCallsPerMinute, tool.PerKeyMaxConcurrency)
	return listedTool{
		ToolsetID: resolved.id, ToolsetRevisionID: resolved.revisionID,
		Name: tool.Alias, Description: tool.Description, InputSchema: tool.InputSchema,
		UnitPrice: cloneUnitPrice(tool.UnitPrice), PricingState: pricingStateFor(tool.UnitPrice),
		BudgetRequired: authority.ToolDebitBoundaries(toolRateLeaseTTL).CostBudgeted,
		Limits: toolCallLimits{
			ServerCallsPerMinute: effective.CallsPerMinute, ServerMaxConcurrency: effective.MaxConcurrency,
			KeyCallsPerMinute: *tighterToolLimit(effective.PerKeyCallsPerMinute, authority.Policy.RequestsPerMinute),
			KeyMaxConcurrency: *tighterToolLimit(effective.PerKeyMaxConcurrency, authority.Policy.MaxConcurrency),
			TimeoutSeconds:    int64(defaultCallTimeout / time.Second), RequestBytes: maxCallBodyBytes, ResultBytes: maxCallBodyBytes,
		},
	}
}

type preparedCall struct {
	resolved  resolvedToolset
	tool      routing.MCPToolsetTool
	arguments json.RawMessage
}

func decodeCall(w http.ResponseWriter, r *http.Request) (callRequest, *apiError) {
	var request callRequest
	raw, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCallBodyBytes))
	if readErr != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(readErr, &tooLarge) {
			return callRequest{}, fail(http.StatusRequestEntityTooLarge, "request_too_large", "invalid_request_error", "The call body exceeds the 1 MiB bound.")
		}
		return callRequest{}, fail(http.StatusBadRequest, "invalid_request", "invalid_request_error", "Supply a tool call JSON object.")
	}
	if _, parseErr := oif.ParseJSON(raw, oif.Limits{MaxBytes: maxCallBodyBytes, MaxDepth: 64, MaxNodes: 1 << 16}); parseErr != nil {
		return callRequest{}, fail(http.StatusBadRequest, "invalid_request", "invalid_request_error", "Supply a tool call JSON object.")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return callRequest{}, fail(http.StatusRequestEntityTooLarge, "request_too_large", "invalid_request_error", "The call body exceeds the 1 MiB bound.")
		}
		return callRequest{}, fail(http.StatusBadRequest, "invalid_request", "invalid_request_error", "Supply a tool call JSON object.")
	}
	if request.executionSelection.supplied() {
		return callRequest{}, fail(http.StatusBadRequest, "invalid_request", "invalid_request_error", "The registered server determines transport and executable settings.")
	}
	return request, nil
}

func normalizedArguments(arguments json.RawMessage) json.RawMessage {
	if len(arguments) == 0 || string(arguments) == "null" {
		return json.RawMessage(`{}`)
	}
	return arguments
}

func (s *Server) prepareCall(authority access.Authority, snapshot *routing.Snapshot, request callRequest) (preparedCall, *apiError) {
	resolved, failure := s.resolveToolset(authority, snapshot, request.ToolsetID)
	if failure != nil {
		return preparedCall{}, failure
	}
	tool, found := resolved.lookup(request.Name)
	switch found {
	case toolDrifted:
		return preparedCall{}, errToolDefinitionChanged
	case toolInvalidSchema:
		return preparedCall{}, errToolSchemaInvalid
	case toolFound:
	default:
		return preparedCall{}, errToolNotFound
	}
	arguments := normalizedArguments(request.Arguments)
	if err := tool.ValidateArguments(arguments); err != nil {
		return preparedCall{}, fail(http.StatusUnprocessableEntity, "invalid_tool_arguments", "invalid_request_error", "The arguments do not match the pinned tool schema.")
	}
	return preparedCall{resolved: resolved, tool: tool, arguments: arguments}, nil
}

type simulatedToolCall struct {
	ConfigurationValid bool       `json:"configuration_valid"`
	Tool               listedTool `json:"tool"`
	RuntimeChecks      []string   `json:"runtime_checks"`
}

func (s *Server) simulateTool(w http.ResponseWriter, r *http.Request) {
	authority, failure := s.authenticate(r)
	if failure != nil {
		writeError(w, failure)
		return
	}
	_, snapshot, snapshotFailure := s.snapshot()
	if snapshotFailure != nil {
		writeError(w, snapshotFailure)
		return
	}
	request, decodeFailure := decodeCall(w, r)
	if decodeFailure != nil {
		writeError(w, decodeFailure)
		return
	}
	prepared, preparationFailure := s.prepareCall(authority, snapshot, request)
	if preparationFailure != nil {
		writeError(w, preparationFailure)
		return
	}
	boundaries := authority.ToolDebitBoundaries(toolRateLeaseTTL)
	if !boundaries.Resolved {
		writeError(w, errLedger)
		return
	}
	if prepared.tool.UnitPrice == nil && boundaries.CostBudgeted {
		writeError(w, fail(http.StatusUnprocessableEntity, "tool_unpriced", "invalid_request_error", "The tool has no price and an enforced budget needs a finite bound, so the call cannot be dispatched."))
		return
	}
	writeJSON(w, http.StatusOK, simulatedToolCall{
		ConfigurationValid: true, Tool: describeTool(authority, prepared.resolved, prepared.tool),
		RuntimeChecks: []string{"live_catalog", "credentials", "guardrails", "budget_balance", "rate", "concurrency"},
	})
}
