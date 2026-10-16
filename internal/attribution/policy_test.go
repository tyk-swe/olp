package attribution

import (
	"testing"
)

func TestPolicyBoundsAndUnconfiguredFastPath(t *testing.T) {
	for _, p := range []Policy{
		{Required: []string{"team", "team"}}, {Required: []string{"@raw"}},
		{Defaults: map[string]string{"team": "free text"}},
		{Required: []string{"a", "b", "c", "d"}, Defaults: map[string]string{"e": "five"}},
	} {
		if p.Validate() == nil {
			t.Fatalf("invalid policy accepted: %#v", p)
		}
	}
	if err := (Policy{Required: []string{"team"}, Defaults: map[string]string{"team": "core"}}).Validate(); err != nil {
		t.Fatal(err)
	}
	if got := testing.AllocsPerRun(100, func() {
		if _, err := Resolve(nil, Policy{}, Policy{}); err != nil {
			panic(err)
		}
	}); got != 0 {
		t.Fatalf("unconfigured allocations: %g", got)
	}
}
