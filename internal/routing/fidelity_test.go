package routing

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"

	"github.com/tyk-swe/olp/internal/contentpolicy"
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
		route.ContentPolicy = &contentpolicy.Policy{Rules: []contentpolicy.Rule{{ID: "mask", Phase: contentpolicy.PhaseInput, Action: contentpolicy.ActionRedact, Pattern: "secret"}}}
		snapshot.Routes[slug] = route
		if err := snapshot.Validate(); !errors.Is(err, ErrFidelityPolicyConflict) {
			t.Fatal("strict policy conflict not checked first", err)
		}
		route.ContentPolicy.Rules[0].Action = contentpolicy.ActionBlock
		snapshot.Routes[slug] = route
		err := snapshot.Compile()
		var incompatible *oif.Incompatibility
		if !errors.As(err, &incompatible) || incompatible.Code != "target_capability" || incompatible.Requirement != "explicit_profile" {
			t.Fatal("strict snapshot installed without explicit compiled profiles", err)
		}
		route.Fidelity.Mode = FidelityTransformed
		route.ContentPolicy.Rules[0].Action = contentpolicy.ActionRedact
		snapshot.Routes[slug] = route
		if err := snapshot.Validate(); err != nil {
			t.Fatal("explicit transformed policy rejected", err)
		}
		break
	}
}
