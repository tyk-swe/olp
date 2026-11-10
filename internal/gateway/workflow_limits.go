package gateway

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

func (x *call) workflowModelCost(attempt routing.Attempt) *Error {
	if x.workflow == nil || x.generation.runLimits.MaxRunCost == nil {
		return nil
	}
	if attempt.Price == nil || x.snapshot() == nil {
		return workflowUnboundedCost()
	}
	provider, ok := x.snapshot().FindProvider(attempt.ProviderID)
	if !ok {
		return workflowUnboundedCost()
	}
	metadata, valid := provider.Metadata(attempt.UpstreamModel)
	if !valid || metadata.ContextLength == nil || *metadata.ContextLength < 1 {
		return workflowUnboundedCost()
	}
	var output *int64
	if x.unary != nil {
		output = metadata.MaxOutputTokens
	} else {
		record := x.targetPreparation(&provider, attempt.TargetID, attempt.UpstreamModel)
		if record.hasOutput {
			output = record.output.output
		} else if record.provider.demand != nil {
			output = record.provider.demand.MaxOutputTokens
		}
	}
	if output == nil || *output < 1 {
		return workflowUnboundedCost()
	}
	bound, priced := attempt.Price.WorkflowModelCostBound(*metadata.ContextLength, *output, attempt.Discount)
	if !priced {
		return workflowUnboundedCost()
	}
	return x.workflowCheckCost(bound)
}

func (x *call) workflowToolCost(tool WorkflowTool) (usage.Cost, bool, *Error) {
	if tool.Definition.UnitPrice == nil {
		if x.generation.runLimits.MaxRunCost == nil {
			return usage.Cost{}, false, nil
		}
		return usage.Cost{}, false, workflowUnboundedCost()
	}
	bound, ok := usage.ParseCost(*tool.Definition.UnitPrice)
	if !ok {
		if x.generation.runLimits.MaxRunCost == nil {
			return usage.Cost{}, false, nil
		}
		return usage.Cost{}, false, workflowUnboundedCost()
	}
	if x.generation.runLimits.MaxRunCost == nil {
		return bound, true, nil
	}
	return bound, true, x.workflowCheckCost(bound)
}

func (x *call) workflowCheckCost(next usage.Cost) *Error {
	limit, ok := usage.ParseCost(*x.generation.runLimits.MaxRunCost)
	if !ok || x.workflow.unknownCost {
		return workflowUnboundedCost()
	}
	if x.workflow.priorCost.Add(x.settledCost()).Add(x.workflow.toolCost).Add(next).Cmp(limit) > 0 {
		return invalidRequest("run_cost_limit", "The next workflow step exceeds the run cost limit.", nil)
	}
	return nil
}

func workflowUnboundedCost() *Error {
	return invalidRequest("run_cost_unbounded", "The workflow step cannot be bounded under the run cost limit.", nil)
}

func (s *Server) reserveWorkflowRun(ctx context.Context, x *call) (*admission.Lease, *Error) {
	if x.workflow == nil {
		return nil, nil
	}
	if !s.deps.Admission.ready() {
		if x.generation.runLimits.MaxConcurrentRuns == nil {
			return nil, nil
		}
		return nil, limitsUnavailable()
	}
	reserveCtx, cancel := context.WithTimeout(ctx, reserveTimeout)
	defer cancel()
	lease, err := s.deps.Admission.limiter.Reserve(reserveCtx, admission.Request{
		Lookup:   "run_" + strings.ReplaceAll(x.keyID, "-", ""),
		LeaseTTL: time.Duration(*x.generation.runLimits.MaxDurationSeconds)*time.Second + time.Minute,
		Rate:     admission.RateLimits{MaxConcurrency: x.generation.runLimits.MaxConcurrentRuns, TrackConcurrency: true},
	})
	if err == nil {
		return lease, nil
	}
	var exceeded *limits.ExceededError
	if errors.As(err, &exceeded) {
		return nil, rateLimited(exceeded.Dimension, exceeded.RetryAfter, exceeded.Estimate)
	}
	return nil, s.deps.Admission.outage(x.keyID, false, err)
}
