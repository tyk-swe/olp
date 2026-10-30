package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
)

func WorkflowRequestRefusal(request *wire.Request) string {
	if request == nil {
		return ""
	}
	if request.Family != wire.Chat && request.Family != wire.Responses && request.Family != wire.Anthropic && request.Family.Surface() != "gemini" {
		return "tool_execution_unsupported"
	}
	if raw := request.Field("n"); request.Family == wire.Chat && len(raw) != 0 {
		var choices int
		if json.Unmarshal(raw, &choices) != nil || choices != 1 {
			return "tool_execution_choices_unsupported"
		}
	}
	if request.Family.Surface() != "gemini" {
		return ""
	}
	for _, field := range []string{"generationConfig", "generation_config"} {
		raw := request.Field(field)
		if len(raw) == 0 {
			continue
		}
		var config struct {
			CandidateCount      *int `json:"candidateCount"`
			CandidateCountSnake *int `json:"candidate_count"`
		}
		if json.Unmarshal(raw, &config) != nil || config.CandidateCount != nil && *config.CandidateCount != 1 || config.CandidateCountSnake != nil && *config.CandidateCountSnake != 1 {
			return "tool_execution_choices_unsupported"
		}
	}
	return ""
}

var ErrWorkflowToolsetsInvalid = errors.New("invalid workflow toolsets")
var ErrWorkflowToolsetsDenied = errors.New("workflow toolsets not permitted")

func SelectWorkflowToolsets(allowed []string, raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return slices.Clone(allowed), nil
	}
	var selected []string
	if err := json.Unmarshal(raw, &selected); err != nil || selected == nil {
		return nil, ErrWorkflowToolsetsInvalid
	}
	for i, id := range selected {
		if !slices.Contains(allowed, id) || slices.Index(selected, id) != i {
			return nil, ErrWorkflowToolsetsDenied
		}
	}
	return selected, nil
}

type WorkflowResolver interface {
	Resolve(context.Context, access.Authority, *runtime.Release, []string) ([]WorkflowTool, error)
}

type discoveredToolset struct {
	permitted bool
	tools     []WorkflowTool
}

func DescribeWorkflow(ctx context.Context, resolver WorkflowResolver, authority access.Authority, release *runtime.Release, route, serving routing.Route, surface string, caller *limits.WorkflowBounds, available bool, now time.Time) *routing.WorkflowCapability {
	if route.ToolExecution != "gateway" || route.Fidelity.Strict() || !authority.Allows("inference", route.Slug, route.ProjectID, now) || resolver == nil || release == nil || release.Snapshot == nil {
		return nil
	}
	sets := slices.Collect(func(yield func(discoveredToolset) bool) {
		for _, id := range route.Toolsets {
			if route.ProjectID == nil || !authority.AllowsResource("tools", id, *route.ProjectID, now) {
				if !yield(discoveredToolset{}) {
					return
				}
				continue
			}
			tools, err := resolver.Resolve(ctx, authority, release, []string{id})
			if !yield(discoveredToolset{permitted: err == nil, tools: tools}) {
				return
			}
		}
	})
	allowed := defaultToolsetsAllowed(ctx, resolver, authority, release, route.Toolsets, sets)
	tools := append([]routing.WorkflowCapabilityTool{}, slices.Collect(func(yield func(routing.WorkflowCapabilityTool) bool) {
		for _, set := range sets {
			if !set.permitted {
				continue
			}
			for _, tool := range set.tools {
				classification := tool.Definition.Classification
				if classification == "" {
					classification = "approval_required"
				}
				if !yield(routing.WorkflowCapabilityTool{ToolsetID: tool.ToolsetID, RevisionID: tool.RevisionID, Alias: tool.Definition.Alias, Name: tool.Definition.Name, Classification: classification, UnitPrice: tool.Definition.UnitPrice}) {
					return
				}
			}
		}
	})...)
	modes := append([]string{}, slices.Collect(func(yield func(string) bool) {
		for _, mode := range []string{"unary", "streaming"} {
			if slices.Contains(serving.Operations, "generation") && slices.ContainsFunc(serving.Targets, func(target routing.Target) bool {
				provider, ok := release.Snapshot.Providers[target.ProviderID]
				return target.Shadow == nil && ok && provider.Enabled && provider.SupportsToolCalls(target.ProviderModel, surface, mode)
			}) && !yield(mode) {
				return
			}
		}
	})...)
	return &routing.WorkflowCapability{Enabled: available && len(modes) > 0, DefaultToolsetsAllowed: allowed, Limits: limits.ResolveWorkflowBounds(nil, route.RunLimits, authority.Policy.RunLimits, caller), CertifiedModes: modes, Tools: tools}
}

func defaultToolsetsAllowed(ctx context.Context, resolver WorkflowResolver, authority access.Authority, release *runtime.Release, selected []string, sets []discoveredToolset) bool {
	if slices.ContainsFunc(sets, func(set discoveredToolset) bool { return !set.permitted }) {
		return false
	}
	_, err := resolver.Resolve(ctx, authority, release, selected)
	return err == nil
}
