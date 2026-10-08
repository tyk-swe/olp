package openapi

import (
	"encoding/json"
	"testing"
)

// Writes that require an Idempotency-Key and an If-Match must say so, or a
// client generated from the contract cannot call them.
func TestGuardedWritesDeclareTheirPreconditions(t *testing.T) {
	type parameter struct {
		In       string `json:"in"`
		Name     string `json:"name"`
		Required bool   `json:"required"`
	}
	type operation struct {
		OperationID string                     `json:"operationId"`
		Parameters  []parameter                `json:"parameters"`
		Responses   map[string]json.RawMessage `json:"responses"`
	}
	var document struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(Document, &document); err != nil {
		t.Fatal(err)
	}
	for _, guarded := range []struct{ path, method string }{
		{"/api/v1/routing-policies/{scope}/{id}", "put"},
		{"/api/v1/providers/{provider_id}/credential-slots/{slot_id}", "put"},
	} {
		raw, ok := document.Paths[guarded.path][guarded.method]
		if !ok {
			t.Fatalf("%s %s is missing", guarded.method, guarded.path)
		}
		var op operation
		if err := json.Unmarshal(raw, &op); err != nil {
			t.Fatal(err)
		}
		if inherited := document.Paths[guarded.path]["parameters"]; len(inherited) > 0 {
			var parameters []parameter
			if err := json.Unmarshal(inherited, &parameters); err != nil {
				t.Fatal(err)
			}
			op.Parameters = append(parameters, op.Parameters...)
		}

		for _, header := range []string{"If-Match", "Idempotency-Key"} {
			found := false
			for _, p := range op.Parameters {
				found = found || p.In == "header" && p.Name == header && p.Required
			}
			if !found {
				t.Errorf("%s does not require the %s header", op.OperationID, header)
			}
		}
		for _, status := range []string{"400", "409", "412", "428"} {
			if _, ok := op.Responses[status]; !ok {
				t.Errorf("%s does not document a %s response", op.OperationID, status)
			}
		}
	}
}
