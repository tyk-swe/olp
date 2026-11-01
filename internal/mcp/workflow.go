package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
)

type WorkflowTool struct {
	ToolsetID  string
	RevisionID string
	Definition routing.MCPToolsetTool
}

type WorkflowError struct {
	Status  int
	Code    string
	Message string
}

func (e *WorkflowError) Error() string { return e.Message }

func workflowFailure(failure *apiError) error {
	return &WorkflowError{Status: failure.status, Code: failure.code, Message: failure.msg}
}

func (s *Server) Resolve(_ context.Context, authority access.Authority, release *runtime.Release, toolsets []string) ([]WorkflowTool, error) {
	if s == nil || s.Runtime == nil || authority.ProjectID == nil || release == nil || release.Snapshot == nil {
		return nil, workflowFailure(errToolsetNotFound)
	}
	var tools []WorkflowTool
	for _, id := range toolsets {
		resolved, failure := s.resolveToolset(authority, release.Snapshot, id)
		if failure != nil {
			return nil, workflowFailure(failure)
		}
		if len(resolved.drifted) != 0 || len(resolved.invalid) != 0 || len(resolved.missing) != 0 {
			return nil, workflowFailure(errToolDefinitionChanged)
		}
		for _, tool := range resolved.tools {
			if slices.ContainsFunc(tools, func(existing WorkflowTool) bool { return existing.Definition.Alias == tool.Alias }) {
				return nil, &WorkflowError{Status: http.StatusBadRequest, Code: "duplicate_tool_alias", Message: "Selected toolsets must not contain duplicate aliases."}
			}
			tools = append(tools, WorkflowTool{ToolsetID: id, RevisionID: resolved.revisionID, Definition: tool})
		}
	}
	return tools, nil
}

func (s *Server) AuthorizeWorkflowTool(authority access.Authority, tool WorkflowTool) error {
	if s == nil || s.Runtime == nil {
		return workflowFailure(fail(http.StatusServiceUnavailable, "authority_unavailable", "api_error", "Key authority is unavailable; retry shortly."))
	}
	if authority.ProjectID == nil {
		return workflowFailure(errPermissionDenied)
	}
	failure := s.currentToolAuthority(executeInput{authority: authority, resolved: resolvedToolset{id: tool.ToolsetID, revisionID: tool.RevisionID}, tool: tool.Definition})
	if failure != nil {
		return workflowFailure(failure)
	}
	return nil
}

func (s *Server) Call(ctx context.Context, authority access.Authority, release *runtime.Release, selected WorkflowTool, runID string, ordinal int, callID string, arguments json.RawMessage) (json.RawMessage, error) {
	return s.callWorkflow(ctx, authority, release, selected, runID, ordinal, callID, arguments, nil)
}

func (s *Server) CallApproved(ctx context.Context, authority access.Authority, release *runtime.Release, selected WorkflowTool, runID string, ordinal int, callID string, arguments json.RawMessage, approval agents.WorkflowApproval) (json.RawMessage, error) {
	return s.callWorkflow(ctx, authority, release, selected, runID, ordinal, callID, arguments, &approval)
}

func (s *Server) callWorkflow(ctx context.Context, authority access.Authority, release *runtime.Release, selected WorkflowTool, runID string, ordinal int, callID string, arguments json.RawMessage, approval *agents.WorkflowApproval) (json.RawMessage, error) {
	if s == nil || s.Runtime == nil || s.Ledger == nil || authority.ProjectID == nil || release == nil || release.Snapshot == nil {
		return nil, workflowFailure(errLedger)
	}
	resolved, failure := s.resolveToolset(authority, release.Snapshot, selected.ToolsetID)
	if failure != nil {
		return nil, workflowFailure(failure)
	}
	index := slices.IndexFunc(resolved.tools, func(tool routing.MCPToolsetTool) bool { return tool.Alias == selected.Definition.Alias })
	if resolved.revisionID != selected.RevisionID || index < 0 {
		return nil, workflowFailure(errToolDefinitionChanged)
	}
	tool := resolved.tools[index]
	if tool.Classification != "read_only" && approval == nil {
		return nil, &WorkflowError{Status: http.StatusForbidden, Code: "tool_approval_required", Message: "This tool requires approval before it can be called."}
	}
	if len(arguments) > maxCallBodyBytes || tool.ValidateArguments(arguments) != nil {
		return nil, &WorkflowError{Status: http.StatusBadRequest, Code: "invalid_tool_arguments", Message: "The model supplied arguments that do not match the tool schema."}
	}
	result := s.execute(ctx, executeInput{
		authority: authority, approval: approval, resolved: resolved, tool: tool, arguments: arguments,
		releaseID: releaseIDFor(release, release.Snapshot), release: release, snapshot: release.Snapshot,
		runID: runID, ordinal: ordinal, callID: callID,
	})
	if result.failure != nil {
		return nil, workflowFailure(result.failure)
	}
	if result.blocked != nil {
		return nil, workflowFailure(result.blocked)
	}
	if result.outcomeUnknown {
		return nil, &WorkflowError{Status: http.StatusBadGateway, Code: "tool_outcome_unknown", Message: result.unknownMessage}
	}
	if result.repeat != nil {
		if !result.repeatRetained {
			return nil, &WorkflowError{Status: http.StatusConflict, Code: "tool_outcome_unknown", Message: "The prior tool result is unavailable."}
		}
		return json.Marshal(result.repeatResult)
	}
	encoded, err := json.Marshal(result.result)
	if err != nil {
		return nil, fmt.Errorf("encode tool result: %w", err)
	}
	return encoded, nil
}
