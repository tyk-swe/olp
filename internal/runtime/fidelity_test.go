package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/oif"
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

func TestSnapshotsCarryExplicitRouteFidelity(t *testing.T) {
	snapshot := fidelitySnapshot(t)
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"fidelity":{"mode":"transformed"}`) {
		t.Fatal("serialized snapshot omitted the route fidelity", string(data))
	}
	var restored Snapshot
	if err = json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if _, err = NewRelease("transformed-fixture", 1, &restored, nil); err != nil {
		t.Fatal("transformed snapshot did not install", err)
	}
	for slug, route := range restored.Routes {
		route.Fidelity = RouteFidelity{}
		restored.Routes[slug] = route
	}
	if err = restored.Validate(); err == nil || !strings.Contains(err.Error(), "fidelity.mode must be strict or transformed") {
		t.Fatal("snapshot without a route fidelity installed", err)
	}
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

func TestOmittedFidelityIsStrictAndLegacyIsRejected(t *testing.T) {
	for _, raw := range []string{``, `null`, ` null `, `{}`, `{"mode":"strict"}`} {
		f, err := DecodeFidelity([]byte(raw))
		if err != nil || f.Mode != FidelityStrict || !f.Strict() {
			t.Fatalf("%q did not declare a strict route: %+v %v", raw, f, err)
		}
	}
	f, err := DecodeFidelity([]byte(`{"mode":"transformed"}`))
	if err != nil || f.Mode != FidelityTransformed || f.Strict() {
		t.Fatal("transformed declaration was not preserved", f, err)
	}
	for _, raw := range []string{`{"mode":"legacy"}`, `[]`, `"strict"`, `{"mode":null}`, `{"mode":""}`, `{"mode":"STRICT"}`, `{"mode":"native"}`, `{"mode":"strict","mode":"legacy"}`, `{"Mode":"legacy"}`, `{"mode":"strict","other":true}`} {
		if _, err := DecodeFidelity([]byte(raw)); err == nil {
			t.Fatalf("ambiguous fidelity accepted: %s", raw)
		}
	}
	if err := ValidateRouteFidelity(RouteFidelity{Mode: "legacy"}, nil); err == nil {
		t.Fatal("legacy fidelity validated")
	}
}

func TestUnsetOrUnknownFidelityFailsClosedAsStrict(t *testing.T) {
	for _, f := range []RouteFidelity{{}, {Mode: "legacy"}, {Mode: "STRICT"}} {
		if !f.Strict() {
			t.Fatalf("fidelity %+v relaxed the strict promise", f)
		}
	}
	if (RouteFidelity{Mode: FidelityTransformed}).Strict() {
		t.Fatal("an explicit transformed route kept the strict promise")
	}
}

func TestStrictRefusalTellsTheAuthorWhichDeclarationServesTheTarget(t *testing.T) {
	for requirement, guidance := range map[string]string{
		"explicit_profile":         "Refused. Declare the route transformed to use a provider without a profile.",
		"operation_contract":       "Refused. Declare the route transformed to translate for this target.",
		"native_media_contract":    "Refused. Declare the route transformed to translate for this target.",
		"native_batch_contract":    "Refused. Declare the route transformed to translate for this target.",
		"native_realtime_contract": "Refused. Declare the route transformed to translate for this target.",
		"bounded_buffering":        "Refused.",
	} {
		// Publication wraps a route's refusal with the route and operation.
		err := fmt.Errorf("route %q realtime: %w", "route", &oif.Incompatibility{Code: "target_capability", Requirement: requirement, Message: "Refused."})
		var problem *access.Problem
		if !errors.As(StrictRefusal(err), &problem) || problem.Status != 422 || problem.Code != "target_capability" || problem.Detail != guidance {
			t.Fatalf("%s refusal = %+v", requirement, problem)
		}
	}
	if StrictRefusal(errors.New("route is malformed")) != nil {
		t.Fatal("an error without an unmet obligation became a strict refusal")
	}
}
