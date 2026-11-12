package gateway

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage/rates"
)

func (x *call) workflowModelCost(attempt routing.Attempt) *fault.Error {
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
		output = x.preparation().attemptEvidence(attempt).output
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

func (x *call) workflowToolCost(tool WorkflowTool) (rates.Cost, bool, *fault.Error) {
	if tool.Definition.UnitPrice == nil {
		if x.generation.runLimits.MaxRunCost == nil {
			return rates.Cost{}, false, nil
		}
		return rates.Cost{}, false, workflowUnboundedCost()
	}
	bound, ok := rates.ParseCost(*tool.Definition.UnitPrice)
	if !ok {
		if x.generation.runLimits.MaxRunCost == nil {
			return rates.Cost{}, false, nil
		}
		return rates.Cost{}, false, workflowUnboundedCost()
	}
	if x.generation.runLimits.MaxRunCost == nil {
		return bound, true, nil
	}
	return bound, true, x.workflowCheckCost(bound)
}

func (x *call) workflowCheckCost(next rates.Cost) *fault.Error {
	limit, ok := rates.ParseCost(*x.generation.runLimits.MaxRunCost)
	if !ok || x.workflow.unknownCost {
		return workflowUnboundedCost()
	}
	if x.workflow.priorCost.Add(x.settledCost()).Add(x.workflow.toolCost).Add(next).Cmp(limit) > 0 {
		return fault.InvalidRequest("run_cost_limit", "The next workflow step exceeds the run cost limit.", nil)
	}
	return nil
}

func workflowUnboundedCost() *fault.Error {
	return fault.InvalidRequest("run_cost_unbounded", "The workflow step cannot be bounded under the run cost limit.", nil)
}

func (s *Server) reserveWorkflowRun(ctx context.Context, x *call) (*admission.Lease, *fault.Error) {
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
