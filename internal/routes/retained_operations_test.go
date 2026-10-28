package routes

import (
	"errors"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/retainedapi"
)

func TestSimulationAdmitsDeclaredRetainedOperations(t *testing.T) {
	declared := []struct{ operation, surface string }{{"retrieval", "openai"}}
	for _, family := range retainedapi.Families() {
		declared = append(declared, struct{ operation, surface string }{family.Operation, family.Surface})
	}
	for _, tuple := range declared {
		if code := problemCode(validTuple(tuple.operation, tuple.surface, "unary")); code == "operation_unavailable" {
			t.Errorf("declared retained operation %s is unavailable to route simulation", tuple.operation)
		}
	}
	if code := problemCode(validTuple("not_an_operation", "openai", "unary")); code != "operation_unavailable" {
		t.Fatalf("undeclared operation reached route simulation past the availability gate with code %q", code)
	}
}

func problemCode(err error) string {
	var problem *access.Problem
	if errors.As(err, &problem) {
		return problem.Code
	}
	if err != nil {
		return err.Error()
	}
	return ""
}
