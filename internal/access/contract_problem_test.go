package access

import (
	"errors"
	"fmt"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
)

func TestContractProblemTellsTheAuthorWhichDeclarationServesTheTarget(t *testing.T) {
	for requirement, guidance := range map[string]string{
		"explicit_profile":         "Refused. Declare the route transformed to use a provider without a profile.",
		"operation_contract":       "Refused. Declare the route transformed to translate for this target.",
		"native_media_contract":    "Refused. Declare the route transformed to translate for this target.",
		"native_batch_contract":    "Refused. Declare the route transformed to translate for this target.",
		"native_realtime_contract": "Refused. Declare the route transformed to translate for this target.",
		"bounded_buffering":        "Refused.",
	} {
		// Publication wraps a route's refusal with the route and operation.
		err := fmt.Errorf("route %q realtime: %w", "route", &oif.Incompatibility{Code: "target_capability", Requirement: requirement, Message: "Refused."})
		var problem *Problem
		if !errors.As(ContractProblem(err), &problem) || problem.Status != 422 || problem.Code != "target_capability" || problem.Detail != guidance {
			t.Fatalf("%s refusal = %+v", requirement, problem)
		}
	}
	if ContractProblem(errors.New("route is malformed")) != nil {
		t.Fatal("an error without an unmet obligation became a strict refusal")
	}
}
