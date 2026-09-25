package runtime

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/tests/fixtures"
)

func historicalFidelitySnapshot(t *testing.T) Snapshot {
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
func TestHistoricalRouteFidelityKeepsRecordedDigest(t *testing.T) {
	snapshot := historicalFidelitySnapshot(t)
	// Captured with snapshot.go/publish.go from pre-fidelity checkpoint 1030e6ef,
	// using the unchanged independently checked-in routing source fixture.
	const previousDigest = "db556c7bfd9209dc7edd834abe3aca58129406d82dd0354ca25b153d26ef93a3"
	digest, err := snapshot.Digest()
	if err != nil || digest != previousDigest {
		t.Fatalf("historical digest changed: %s %v", digest, err)
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"fidelity"`) {
		t.Fatal("omitted legacy fidelity entered serialized snapshots")
	}
	var restored Snapshot
	if err = json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if _, err = NewRelease("legacy-fixture", 1, &restored, nil); err != nil {
		t.Fatal("historical snapshot no longer installs", err)
	}
}
func TestStrictSnapshotCannotInstallLegacyExecution(t *testing.T) {
	snapshot := historicalFidelitySnapshot(t)
	for slug, route := range snapshot.Routes {
		route.Fidelity = &RouteFidelity{Mode: FidelityStrict}
		route.ContentPolicy = &contentpolicy.Policy{Rules: []contentpolicy.Rule{{ID: "mask", Phase: contentpolicy.PhaseInput, Action: contentpolicy.ActionRedact, Pattern: "secret"}}}
		snapshot.Routes[slug] = route
		if err := snapshot.Validate(); !errors.Is(err, ErrFidelityPolicyConflict) {
			t.Fatal("strict policy conflict not checked first", err)
		}
		route.ContentPolicy.Rules[0].Action = contentpolicy.ActionBlock
		snapshot.Routes[slug] = route
		_, err := NewRelease("strict-fixture", 1, &snapshot, nil)
		var incompatible *interaction.Error
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
func TestExplicitFidelityDefaultsAndInvalidContracts(t *testing.T) {
	f, err := DecodeFidelity(nil)
	if err != nil || f != nil || FidelityMode(f) != FidelityLegacy {
		t.Fatal("omission changed historical behavior")
	}
	f, err = DecodeFidelity([]byte(`{}`))
	if err != nil || f.Mode != FidelityStrict {
		t.Fatal("explicit empty contract did not default strict")
	}
	for _, raw := range []string{`null`, `[]`, `{"mode":null}`, `{"mode":""}`, `{"mode":"STRICT"}`, `{"mode":"native"}`, `{"mode":"strict","mode":"legacy"}`, `{"Mode":"legacy"}`, `{"mode":"strict","other":true}`} {
		if _, err := DecodeFidelity([]byte(raw)); err == nil {
			t.Fatalf("ambiguous fidelity accepted: %s", raw)
		}
	}
}

// Draft validation compiles the same links release installation does, so a
// strict realtime target installation would refuse is refused before publish.
func TestDraftCompilationRefusesUninstallableStrictRealtime(t *testing.T) {
	providerID := uuid.NewString()
	provider := Provider{ID: providerID, Name: "realtime", Kind: "openai", Enabled: true, RevisionID: uuid.NewString(),
		Capabilities: []Capability{{Model: "gpt-realtime", Operation: "realtime", Surface: "openai", Mode: "realtime"}}}
	route := Route{ID: uuid.NewString(), Slug: "rt", Operations: []string{"realtime"}, OverallTimeout: 1000, MaxAttempts: 1,
		Fidelity: &RouteFidelity{Mode: FidelityStrict},
		Targets:  []Target{{ID: uuid.NewString(), ProviderID: providerID, ProviderModel: "gpt-realtime", Weight: 1, Timeout: 1000, RoutingID: uuid.NewString()}}}
	route.RoutingID = route.ID
	snapshot := &Snapshot{Generation: Generation{ID: uuid.NewString(), Ordinal: 1, ActivatedAt: time.Now()},
		Providers: map[string]Provider{providerID: provider}, Routes: map[string]Route{route.Slug: route}}
	installErr := snapshot.Validate()
	if installErr == nil {
		t.Fatal("installation accepted a strict realtime target without a versioned profile")
	}
	if err := snapshot.CompileRouteExecution(route); err == nil {
		t.Fatalf("draft compilation accepted a route installation refuses: %v", installErr)
	}
}
