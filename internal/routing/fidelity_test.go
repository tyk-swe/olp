package routing

import (
	"encoding/json"
	"errors"
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
	for slug, route := range snapshot.Routes {
		route.Fidelity = RouteFidelity{Mode: FidelityStrict}
		snapshot.Routes[slug] = route
		err := snapshot.Compile()
		var incompatible *oif.Incompatibility
		if !errors.As(err, &incompatible) || incompatible.Code != "target_capability" || incompatible.Requirement != "explicit_profile" {
			t.Fatal("strict snapshot installed without explicit compiled profiles", err)
		}
		break
	}
}
