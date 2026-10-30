package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/routing"
)

type paidPublicationTx struct {
	pgx.Tx
	revisions []routing.ProviderRevision
}

func (tx paidPublicationTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return &paidPublicationRows{revisions: tx.revisions}, nil
}

type paidPublicationRows struct {
	pgx.Rows
	revisions []routing.ProviderRevision
	position  int
}

func (r *paidPublicationRows) Next() bool {
	if r.position >= len(r.revisions) {
		return false
	}
	r.position++
	return true
}
func (r *paidPublicationRows) Close()     {}
func (r *paidPublicationRows) Err() error { return nil }
func (r *paidPublicationRows) Scan(dest ...any) error {
	revision := r.revisions[r.position-1]
	values := []any{revision.ID, revision.State, revision.RevisionID, revision.Name, revision.Configuration, revision.Models, revision.Slots, revision.ProjectID, revision.InstalledPlugin}
	if len(dest) != len(values) {
		return fmt.Errorf("unexpected fixture row width %d", len(dest))
	}
	for i, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(encoded, dest[i]); err != nil {
			return err
		}
	}
	return nil
}

type paidPublicationFixture struct {
	route    codemode.Route
	revision routing.ProviderRevision
	slot     routing.Slot
}

func newPaidPublicationFixture(t *testing.T, source string, enabled bool) paidPublicationFixture {
	t.Helper()
	project, providerID, slotID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	slot := routing.Slot{ID: slotID, Name: "metered credential", Enabled: enabled, CredentialID: new(uuid.NewString()), CredentialVersion: new(2), AllowedModels: []string{"native-model"}, AllowedRoutes: []string{"coding"}, AllowedAPIKeys: []string{"authorized-key"}, MaxConcurrency: new(int64(3)), Supply: routing.Supply{MonthlyCostLimit: new("4.00")}}
	models, err := json.Marshal([]routing.RevisionModel{{ID: uuid.NewString(), UpstreamModel: "native-model", Capabilities: []routing.RevisionCapability{{Operation: "generation", Surface: "openai", Mode: "unary", Source: "certified"}}}})
	if err != nil {
		t.Fatal(err)
	}
	slots, err := json.Marshal([]routing.RevisionSlot{{Slot: slot, Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	configuration := fmt.Appendf(nil, `{"kind":"openai","auth_mode":"api_key","endpoint":"https://metered.example/v1","profile_id":"openai-responses","profile_revision":%q,"credential_source":%q,"options":{"vendor_id":"openai","limits":{"requests_per_minute":17,"daily_cost_limit":"1.20"}}}`, connectors.ProfileRevision, source)
	return paidPublicationFixture{
		route:    codemode.Route{ID: uuid.NewString(), ProjectID: project, Slug: "coding", PoolID: uuid.NewString(), Enabled: true, Models: []string{"native-model"}, PaidParticipation: true, PaidTargets: map[string]codemode.PaidTarget{"native-model": {ProviderID: providerID, SlotID: slotID}}},
		revision: routing.ProviderRevision{ID: providerID, RevisionID: uuid.NewString(), Name: "metered", State: "active", ProjectID: &project, Configuration: configuration, Models: models, Slots: slots}, slot: slot,
	}
}

func TestCodePaidPublicationFreezesNativeAuthorityAndLimits(t *testing.T) {
	fixture := newPaidPublicationFixture(t, "", true)
	encoded, err := codePaidPublication(t.Context(), paidPublicationTx{revisions: []routing.ProviderRevision{fixture.revision}}, fixture.route)
	if err != nil {
		t.Fatal(err)
	}
	var published map[string]routing.Provider
	if err := json.Unmarshal(encoded, &published); err != nil {
		t.Fatal(err)
	}
	target := published[fixture.revision.ID]
	if len(published) != 1 || target.RevisionID != fixture.revision.RevisionID || target.ProjectID == nil || *target.ProjectID != fixture.route.ProjectID || target.Endpoint != "https://metered.example/v1" || target.VendorID != "openai" {
		t.Fatalf("published target lost frozen provider authority %+v", target)
	}
	if protocol, ok := routing.CodePaidProtocol(target, "native-model"); !ok || protocol != codemode.ProtocolResponses {
		t.Fatalf("published target lost native capability %q %v", protocol, ok)
	}
	if target.Limits.RequestsPerMinute == nil || *target.Limits.RequestsPerMinute != 17 || target.Limits.DailyCostLimit == nil || *target.Limits.DailyCostLimit != "1.20" {
		t.Fatalf("published target lost connection limits %+v", target.Limits)
	}
	if len(target.Slots) != 1 {
		t.Fatalf("published target lost selected credential slot %+v", target.Slots)
	}
	slot := target.Slots[0]
	if slot.ID != fixture.slot.ID || slot.CredentialID == nil || *slot.CredentialID != *fixture.slot.CredentialID || slot.CredentialVersion == nil || *slot.CredentialVersion != *fixture.slot.CredentialVersion || slot.MaxConcurrency == nil || *slot.MaxConcurrency != 3 || slot.MonthlyCostLimit == nil || *slot.MonthlyCostLimit != "4.00" {
		t.Fatalf("published target lost slot authority or limits %+v", slot)
	}
	if !slot.Allows("native-model", "coding", "authorized-key") || slot.Allows("native-model", "coding", "unassigned-key") {
		t.Fatal("published slot lost API-key restriction")
	}
}

func TestCodePaidPublicationRefusesUnavailableTargets(t *testing.T) {
	for _, scenario := range []struct {
		name, source     string
		enabled, missing bool
	}{
		{name: "provider unavailable", enabled: true, missing: true},
		{name: "credential slot disabled"},
		{name: "caller credentials", source: "caller", enabled: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newPaidPublicationFixture(t, scenario.source, scenario.enabled)
			revisions := func() []routing.ProviderRevision {
				if scenario.missing {
					return nil
				}
				return []routing.ProviderRevision{fixture.revision}
			}()
			encoded, err := codePaidPublication(t.Context(), paidPublicationTx{revisions: revisions}, fixture.route)
			var problem *access.Problem
			if !errors.As(err, &problem) || problem.Status != http.StatusUnprocessableEntity || problem.Field != "paid_targets" || len(encoded) != 0 {
				t.Fatalf("unusable paid target was not refused %s %v", encoded, err)
			}
		})
	}
}
