package routing

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/tests/fixtures"
)

func fidelitySnapshot(t *testing.T) Snapshot {
	t.Helper()
	raw, err := fixtures.Files.ReadFile("routing/attempt-order.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Snapshot Snapshot `json:"snapshot"`
	}
	if err = json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	return corpus.Snapshot
}

func TestStrictSnapshotRequiresCompiledProfiles(t *testing.T) {
	snapshot := fidelitySnapshot(t)
	slugs := slices.Sorted(maps.Keys(snapshot.Routes))
	if len(slugs) == 0 {
		t.Fatal("routing fixture has no routes")
	}
	for _, slug := range slugs {
		candidate := snapshot.Clone()
		route := candidate.Routes[slug]
		route.Fidelity = RouteFidelity{Mode: FidelityStrict}
		candidate.Routes[slug] = route
		err := candidate.Compile()
		var incompatible *oif.Incompatibility
		if !errors.As(err, &incompatible) || incompatible.Code != "target_capability" || incompatible.Requirement != "explicit_profile" {
			t.Fatalf("strict snapshot for route %q installed without explicit compiled profiles: %v", slug, err)
		}
	}
}
