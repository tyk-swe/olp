package limits

import (
	"bytes"
	"encoding/json"
	"math/big"
	"strings"
)

type WorkflowBounds struct {
	MaxIterations      *int64  `json:"max_iterations,omitempty"`
	MaxToolCalls       *int64  `json:"max_tool_calls,omitempty"`
	MaxDurationSeconds *int64  `json:"max_duration_seconds,omitempty"`
	MaxConcurrentRuns  *int64  `json:"max_concurrent_runs,omitempty"`
	MaxRunCost         *string `json:"max_run_cost,omitempty"`
}

func ParseWorkflowBounds(raw json.RawMessage) (WorkflowBounds, bool) {
	if len(raw) == 0 {
		return WorkflowBounds{}, true
	}
	var bounds WorkflowBounds
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || decoder.Decode(&bounds) != nil || !bounds.Valid() {
		return WorkflowBounds{}, false
	}
	return bounds, true
}

func (b WorkflowBounds) Valid() bool {
	return validWorkflowInteger(b.MaxIterations, 1, 32) &&
		validWorkflowInteger(b.MaxToolCalls, 0, 128) &&
		validWorkflowInteger(b.MaxDurationSeconds, 1, 600) &&
		validWorkflowInteger(b.MaxConcurrentRuns, 1, 2147483647) &&
		(b.MaxRunCost == nil || validWorkflowCost(*b.MaxRunCost))
}

func validWorkflowCost(value string) bool {
	if ValidCostLimit(value) {
		return true
	}
	integer, fraction, decimal := strings.Cut(value, ".")
	return len(integer) > 0 && len(integer) <= 12 && strings.Trim(integer, "0") == "" &&
		(!decimal || (len(fraction) > 0 && len(fraction) <= 12 && strings.Trim(fraction, "0") == ""))
}

func validWorkflowInteger(value *int64, minimum, maximum int64) bool {
	return value == nil || (*value >= minimum && *value <= maximum)
}

func MinWorkflowInteger(fallback int64, values ...*int64) int64 {
	selected := minWorkflowInteger(values)
	if selected == nil {
		return fallback
	}
	return *selected
}

func minWorkflowInteger(values []*int64) *int64 {
	if len(values) == 0 {
		return nil
	}
	remaining := minWorkflowInteger(values[1:])
	if values[0] == nil || remaining != nil && *remaining < *values[0] {
		return remaining
	}
	return values[0]
}

func MinWorkflowCost(values ...*string) *string {
	if len(values) == 0 {
		return nil
	}
	remaining := MinWorkflowCost(values[1:]...)
	if values[0] == nil {
		return remaining
	}
	if remaining == nil {
		return values[0]
	}
	candidate, _ := new(big.Rat).SetString(*values[0])
	current, _ := new(big.Rat).SetString(*remaining)
	if candidate.Cmp(current) < 0 {
		return values[0]
	}
	return remaining
}

func ResolveWorkflowBounds(prior, route, key, caller *WorkflowBounds) WorkflowBounds {
	previous, operator, principal, requested := workflowBoundsOrZero(prior), workflowBoundsOrZero(route), workflowBoundsOrZero(key), workflowBoundsOrZero(caller)
	iterations := MinWorkflowInteger(8, previous.MaxIterations, operator.MaxIterations, principal.MaxIterations, requested.MaxIterations)
	tools := MinWorkflowInteger(32, previous.MaxToolCalls, operator.MaxToolCalls, principal.MaxToolCalls, requested.MaxToolCalls)
	duration := MinWorkflowInteger(120, previous.MaxDurationSeconds, operator.MaxDurationSeconds, principal.MaxDurationSeconds, requested.MaxDurationSeconds)
	concurrency := MinWorkflowInteger(0, previous.MaxConcurrentRuns, operator.MaxConcurrentRuns, principal.MaxConcurrentRuns, requested.MaxConcurrentRuns)
	return WorkflowBounds{MaxIterations: &iterations, MaxToolCalls: &tools, MaxDurationSeconds: &duration, MaxConcurrentRuns: workflowConcurrency(concurrency), MaxRunCost: MinWorkflowCost(previous.MaxRunCost, operator.MaxRunCost, principal.MaxRunCost, requested.MaxRunCost)}
}

func workflowBoundsOrZero(bounds *WorkflowBounds) WorkflowBounds {
	if bounds == nil {
		return WorkflowBounds{}
	}
	return *bounds
}

func workflowConcurrency(value int64) *int64 {
	if value == 0 {
		return nil
	}
	return &value
}
