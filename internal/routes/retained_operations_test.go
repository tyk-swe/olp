package routes

import "testing"

func TestRegisteredRetainedOperations(t *testing.T) {
	for _, operation := range []string{"fine_tuning", "vector_store", "retrieval", "conversation", "container", "eval", "skill", "context_cache", "agent_resource"} {
		if !registeredOperation(operation) {
			t.Fatalf("retained operation %s not registered for route publication", operation)
		}
	}
	if registeredOperation("not_an_operation") {
		t.Fatal("unknown operation registered")
	}
}
