package attribution

import (
	"maps"
	"testing"
)

func TestPoliciesAccumulateWithoutOverridingPinsOrMutatingInputs(t *testing.T) {
	project := Policy{Required: []string{"team", "env"}, Defaults: map[string]string{"team": "core"}}
	key := Policy{Defaults: map[string]string{"env": "prod"}}
	caller := map[string]string{"task": "build"}
	got, err := Resolve(caller, project, key)
	if err != nil || !maps.Equal(got, map[string]string{"team": "core", "env": "prod", "task": "build"}) {
		t.Fatalf("resolved=%v err=%v", got, err)
	}
	got["team"] = "changed"
	if project.Defaults["team"] != "core" || len(caller) != 1 {
		t.Fatal("cached policy or caller map mutated")
	}
	for _, tc := range []struct {
		labels   map[string]string
		policies []Policy
		reason   string
	}{
		{nil, []Policy{{Required: []string{"team"}}}, "missing_attribution"},
		{map[string]string{"team": "other"}, []Policy{project, key}, "pinned_attribution_override"},
		{nil, []Policy{project, {Defaults: map[string]string{"team": "other"}}}, "conflicting_defaults"},
		{map[string]string{"a": "1", "b": "2", "c": "3"}, []Policy{project, key}, "too_many_keys"},
	} {
		_, err := Resolve(tc.labels, tc.policies...)
		if err == nil || err.(*Error).Reason != tc.reason {
			t.Fatalf("want %s: %v", tc.reason, err)
		}
	}
}

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
