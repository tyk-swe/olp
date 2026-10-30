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
	return describeWorkflow(ctx, resolver, authority, release, route, caller, available, now, func(mode string) bool {
		return workflowServingRouteAllowed(authority, serving, now) && workflowTargetSupports(release.Snapshot, serving, surface, mode, nil)
	})
}

func DescribeReachableWorkflow(ctx context.Context, resolver WorkflowResolver, authority access.Authority, release *runtime.Release, route routing.Route, surface string, available bool, now time.Time) *routing.WorkflowCapability {
	return describeWorkflow(ctx, resolver, authority, release, route, nil, available, now, func(mode string) bool {
		supported, _ := workflowReachableMode(release.Snapshot, authority, route, surface, mode, nil, now)
		return supported
	})
}

func describeWorkflow(ctx context.Context, resolver WorkflowResolver, authority access.Authority, release *runtime.Release, route routing.Route, caller *limits.WorkflowBounds, available bool, now time.Time, supports func(string) bool) *routing.WorkflowCapability {
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
				if !yield(routing.WorkflowCapabilityTool{ToolsetID: tool.ToolsetID, RevisionID: tool.RevisionID, Alias: tool.Definition.Alias, Name: tool.Definition.Name, Classification: workflowClassification(tool.Definition.Classification), UnitPrice: tool.Definition.UnitPrice}) {
					return
				}
			}
		}
	})...)
	modes := append([]string{}, slices.Collect(func(yield func(string) bool) {
		for _, mode := range []string{"unary", "streaming"} {
			if supports(mode) && !yield(mode) {
				return
			}
		}
	})...)
	return &routing.WorkflowCapability{Enabled: available && len(modes) > 0, DefaultToolsetsAllowed: allowed, Limits: limits.ResolveWorkflowBounds(nil, route.RunLimits, authority.Policy.RunLimits, caller), CertifiedModes: modes, Tools: tools}
}

func workflowClassification(classification string) string {
	if classification == "" {
		return "approval_required"
	}
	return classification
}

func workflowServingRouteAllowed(authority access.Authority, route routing.Route, now time.Time) bool {
	return !route.Fidelity.Strict() && slices.Contains(route.Operations, "generation") && authority.Allows("inference", route.Slug, route.ProjectID, now)
}

func workflowTargetSupports(snapshot *routing.Snapshot, route routing.Route, surface, mode string, tags []string) bool {
	return slices.ContainsFunc(route.Targets, func(target routing.Target) bool {
		provider, ok := snapshot.Providers[target.ProviderID]
		return workflowTargetMatches(target, tags) && ok && provider.Enabled && provider.SupportsToolCalls(target.ProviderModel, surface, mode) && provider.Connector().Supports("generation", surface, mode)
	})
}

func workflowTargetMatches(target routing.Target, tags []string) bool {
	return target.Shadow == nil && (len(tags) == 0 || slices.ContainsFunc(tags, func(tag string) bool { return slices.Contains(target.Tags, tag) }))
}

func workflowTargetCanExceedContext(snapshot *routing.Snapshot, route routing.Route, surface, mode string, tags []string) bool {
	return slices.ContainsFunc(route.Targets, func(target routing.Target) bool {
		provider, ok := snapshot.Providers[target.ProviderID]
		if !workflowTargetMatches(target, tags) || !ok || !provider.Enabled || !provider.Supports(target.ProviderModel, "generation", surface, mode) {
			return false
		}
		var metadata routing.ModelMetadata
		_ = json.Unmarshal(provider.Models[target.ProviderModel], &metadata)
		return metadata.ContextLength != nil || metadata.MaxOutputTokens != nil
	})
}

func workflowReachableMode(snapshot *routing.Snapshot, authority access.Authority, route routing.Route, surface, mode string, path []string, now time.Time) (eligible, mayFallback bool) {
	if slices.Contains(path, route.Slug) || !workflowServingRouteAllowed(authority, route, now) {
		return false, false
	}
	nextPath := append(slices.Clone(path), route.Slug)
	supported, canFallback := workflowSelectorsSupport(snapshot, authority, route, route.Selectors, surface, mode, nextPath, now)
	if supported || !canFallback {
		return supported, canFallback
	}
	return slices.ContainsFunc(route.Fallbacks, func(fallback routing.Fallback) bool {
		if !slices.Contains(fallback.On, routing.FallbackContextWindow) {
			return false
		}
		next, ok := snapshot.Routes[fallback.Route]
		if !ok {
			return false
		}
		supported, _ := workflowReachableMode(snapshot, authority, next, surface, mode, nextPath, now)
		return supported
	}), true
}

func workflowSelectorsSupport(snapshot *routing.Snapshot, authority access.Authority, route routing.Route, selectors []routing.Selector, surface, mode string, path []string, now time.Time) (eligible, mayFallback bool) {
	if len(selectors) == 0 {
		return workflowTargetSupports(snapshot, route, surface, mode, nil), workflowTargetCanExceedContext(snapshot, route, surface, mode, nil)
	}
	selector := selectors[0]
	if !workflowPredicateCanMatch(selector.When, mode) {
		return workflowSelectorsSupport(snapshot, authority, route, selectors[1:], surface, mode, path, now)
	}
	if selector.Route != "" {
		next, ok := snapshot.Routes[selector.Route]
		if !ok || !slices.Contains(next.Operations, "generation") || !authority.Allows("inference", next.Slug, next.ProjectID, now) {
			return workflowSelectorsSupport(snapshot, authority, route, selectors[1:], surface, mode, path, now)
		}
		supported, canFallback := workflowReachableMode(snapshot, authority, next, surface, mode, path, now)
		if supported || workflowPredicateAlwaysMatches(selector.When) {
			return supported, canFallback
		}
		laterSupported, laterCanFallback := workflowSelectorsSupport(snapshot, authority, route, selectors[1:], surface, mode, path, now)
		return laterSupported, canFallback || laterCanFallback
	} else if workflowTargetSupports(snapshot, route, surface, mode, selector.Tags) {
		return true, false
	}
	if workflowPredicateAlwaysMatches(selector.When) {
		return false, workflowTargetCanExceedContext(snapshot, route, surface, mode, selector.Tags)
	}
	supported, canFallback := workflowSelectorsSupport(snapshot, authority, route, selectors[1:], surface, mode, path, now)
	return supported, canFallback || workflowTargetCanExceedContext(snapshot, route, surface, mode, selector.Tags)
}

func workflowPredicateCanMatch(predicate routing.Predicate, mode string) bool {
	return (len(predicate.Operations) == 0 || slices.Contains(predicate.Operations, "generation")) && (predicate.Streaming == nil || *predicate.Streaming == (mode == "streaming"))
}

func workflowPredicateAlwaysMatches(predicate routing.Predicate) bool {
	return predicate.MinInputTokens == nil && predicate.MaxInputTokens == nil && predicate.MinOutputTokens == nil && predicate.MaxOutputTokens == nil && predicate.Tools == nil && len(predicate.Modalities) == 0 && predicate.StructuredOutput == nil && len(predicate.ReasoningEffort) == 0 && predicate.Classifier == nil && predicate.Plugin == nil
}

func defaultToolsetsAllowed(ctx context.Context, resolver WorkflowResolver, authority access.Authority, release *runtime.Release, selected []string, sets []discoveredToolset) bool {
	if slices.ContainsFunc(sets, func(set discoveredToolset) bool { return !set.permitted }) {
		return false
	}
	_, err := resolver.Resolve(ctx, authority, release, selected)
	return err == nil
}
