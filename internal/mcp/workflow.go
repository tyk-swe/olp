package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agents/runs"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
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

func (s *Server) Resolve(_ context.Context, authority access.Authority, release *release.Release, toolsets []string) ([]WorkflowTool, error) {
	if s == nil || s.runtime == nil || authority.ProjectID == nil || release == nil || release.Snapshot == nil {
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
	if s == nil || s.runtime == nil {
		return workflowFailure(fail(http.StatusServiceUnavailable, "authority_unavailable", "api_error", "Key authority is unavailable; retry shortly."))
	}
	if authority.ProjectID == nil {
		return workflowFailure(errPermissionDenied)
	}
	failure := s.executor.currentToolAuthority(CallRequest{Authority: authority, ToolsetID: tool.ToolsetID, ToolsetRevisionID: tool.RevisionID, Tool: tool.Definition})
	if failure != nil {
		return workflowFailure(apiFailure(failure))
	}
	return nil
}

func (s *Server) Call(ctx context.Context, authority access.Authority, release *release.Release, selected WorkflowTool, runID string, ordinal int, callID string, arguments json.RawMessage) (json.RawMessage, error) {
	return s.callWorkflow(ctx, authority, release, selected, runID, ordinal, callID, arguments, nil)
}

func (s *Server) CallApproved(ctx context.Context, authority access.Authority, release *release.Release, selected WorkflowTool, runID string, ordinal int, callID string, arguments json.RawMessage, approval runs.WorkflowApproval) (json.RawMessage, error) {
	return s.callWorkflow(ctx, authority, release, selected, runID, ordinal, callID, arguments, &approval)
}

func (s *Server) callWorkflow(ctx context.Context, authority access.Authority, release *release.Release, selected WorkflowTool, runID string, ordinal int, callID string, arguments json.RawMessage, approval *runs.WorkflowApproval) (json.RawMessage, error) {
	if s == nil || s.runtime == nil || !s.executor.available() || authority.ProjectID == nil || release == nil || release.Snapshot == nil {
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
	result := s.executor.Execute(ctx, CallRequest{
		Authority: authority, Approval: approval, ToolsetID: resolved.id, ToolsetRevisionID: resolved.revisionID, Tool: tool, Arguments: arguments,
		ReleaseID: releaseIDFor(release, release.Snapshot), Release: release, Policies: release.Snapshot,
		RunID: runID, Ordinal: ordinal, CallID: callID,
	})
	switch outcome := result.Outcome.(type) {
	case Refused:
		return nil, workflowFailure(apiFailure(outcome.Failure))
	case Blocked:
		return nil, workflowFailure(apiFailure(outcome.Failure))
	case Uncertain:
		return nil, &WorkflowError{Status: http.StatusBadGateway, Code: "tool_outcome_unknown", Message: outcome.Message}
	case ReplayUnavailable:
		return nil, &WorkflowError{Status: http.StatusConflict, Code: "tool_outcome_unknown", Message: "The prior tool result is unavailable."}
	case Replayed:
		return json.Marshal(outcome.Result)
	case Completed:
		return json.Marshal(outcome.Result)
	default:
		return nil, workflowFailure(errLedger)
	}
}
