package codemode

import "testing"

func TestUsageSubsetAndSafeIntegerValidation(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	for _, usage := range []Usage{{Total: n(-1)}, {Total: n(1 << 53)}, {Total: n(3), Input: n(2), Output: n(2)}, {Input: n(3), Cached: n(4)}, {Output: n(3), Reasoning: n(4)}} {
		if usage.Validate() == nil {
			t.Fatalf("invalid usage accepted: %+v", usage)
		}
	}
	if err := (Usage{Total: n(7), Input: n(3), Output: n(4), Cached: n(3), Reasoning: n(4)}).Validate(); err != nil {
		t.Fatal("subsets charged twice", err)
	}
	if err := (Usage{Input: n(2)}).Validate(); err != nil {
		t.Fatal("partial usage rejected", err)
	}
}

func TestIdentityRefusesUnsafeOrSelfParentingIdentifiers(t *testing.T) {
	for _, identity := range []Identity{{}, {Conversation: "a\nsecret"}, {Conversation: "a", Parent: "a"}, {Conversation: "a", Parent: " "}} {
		if identity.Validate() == nil {
			t.Fatalf("unsafe identity accepted: %+v", identity)
		}
	}
	if err := (Identity{Conversation: "thread-123", Parent: "root-123"}).Validate(); err != nil {
		t.Fatal(err)
	}
}
