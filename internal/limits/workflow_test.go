package limits

import "testing"

func workflowInt(value int64) *int64 { return &value }

func workflowCost(value string) *string { return &value }

func TestWorkflowBoundsValidateAllLimits(t *testing.T) {
	for _, test := range []struct {
		name   string
		bounds WorkflowBounds
		valid  bool
	}{
		{name: "unspecified bounds use defaults", valid: true},
		{name: "minimum limits and zero spend", bounds: WorkflowBounds{MaxIterations: workflowInt(1), MaxToolCalls: workflowInt(0), MaxDurationSeconds: workflowInt(1), MaxConcurrentRuns: workflowInt(1), MaxRunCost: workflowCost("0.00")}, valid: true},
		{name: "maximum limits and fractional spend", bounds: WorkflowBounds{MaxIterations: workflowInt(32), MaxToolCalls: workflowInt(128), MaxDurationSeconds: workflowInt(600), MaxConcurrentRuns: workflowInt(2147483647), MaxRunCost: workflowCost("12.50")}, valid: true},
		{name: "no model iterations", bounds: WorkflowBounds{MaxIterations: workflowInt(0)}},
		{name: "too many iterations", bounds: WorkflowBounds{MaxIterations: workflowInt(33)}},
		{name: "negative tool calls", bounds: WorkflowBounds{MaxToolCalls: workflowInt(-1)}},
		{name: "too many tool calls", bounds: WorkflowBounds{MaxToolCalls: workflowInt(129)}},
		{name: "zero duration", bounds: WorkflowBounds{MaxDurationSeconds: workflowInt(0)}},
		{name: "excess duration", bounds: WorkflowBounds{MaxDurationSeconds: workflowInt(601)}},
		{name: "zero concurrent runs", bounds: WorkflowBounds{MaxConcurrentRuns: workflowInt(0)}},
		{name: "excess concurrent runs", bounds: WorkflowBounds{MaxConcurrentRuns: workflowInt(2147483648)}},
		{name: "negative cost", bounds: WorkflowBounds{MaxRunCost: workflowCost("-1")}},
		{name: "malformed cost", bounds: WorkflowBounds{MaxRunCost: workflowCost("0.not-a-number")}},
		{name: "cost too precise", bounds: WorkflowBounds{MaxRunCost: workflowCost("0.0000000000000")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.bounds.Valid(); got != test.valid {
				t.Fatalf("validity = %v, want %v", got, test.valid)
			}
		})
	}
}

func TestWorkflowLimitsSelectTheMostRestrictiveConfiguredValue(t *testing.T) {
	if got := MinWorkflowInteger(8, nil, nil); got != 8 {
		t.Fatalf("unspecified iterations = %d, want default 8", got)
	}
	if got := MinWorkflowInteger(8, nil, workflowInt(12), workflowInt(3), workflowInt(5)); got != 3 {
		t.Fatalf("effective iterations = %d, want 3", got)
	}
	if got := MinWorkflowInteger(8, workflowInt(10)); got != 10 {
		t.Fatalf("configured iterations = %d, want 10", got)
	}
	if got := MinWorkflowCost(nil, nil); got != nil {
		t.Fatalf("unspecified cost = %q, want no cap", *got)
	}
	if got := MinWorkflowCost(nil, workflowCost("10.00"), workflowCost("9.9"), workflowCost("0.25")); got == nil || *got != "0.25" {
		t.Fatalf("effective cost = %v, want 0.25", got)
	}
	if got := MinWorkflowCost(workflowCost("0"), workflowCost("0.01")); got == nil || *got != "0" {
		t.Fatalf("zero cost cap = %v, want 0", got)
	}
}
