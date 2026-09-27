package providers

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/runtime"
)

func TestPublishedSlotsUseOnlyCredentialsRequiredByAuthMode(t *testing.T) {
	for _, authMode := range []string{AuthNone, AuthAPIKey, AuthHeaders} {
		for _, enabled := range []bool{false, true} {
			for _, revoked := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/enabled=%t/revoked=%t", authMode, enabled, revoked), func(t *testing.T) {
					credential, version := "retained-credential", 1
					row := slotRow{
						ID: "slot", Default: true, Name: "default", Enabled: enabled, Weight: 1,
						CredentialID: &credential, CredentialVersion: &version, CredentialRevoked: revoked,
					}
					slot := row.published(authMode)
					if slot.Enabled != (enabled && (authMode == AuthNone || !revoked)) {
						t.Fatalf("incorrect published eligibility: %+v", slot)
					}
					if authMode == AuthNone {
						if slot.CredentialID != nil || slot.CredentialVersion != nil {
							t.Fatalf("no-auth slot still references a credential: %+v", slot)
						}
					} else if slot.CredentialID != row.CredentialID || slot.CredentialVersion != row.CredentialVersion {
						t.Fatalf("authenticated slot lost its credential: %+v", slot)
					}
					if row.CredentialID != &credential || row.CredentialVersion != &version || row.Enabled != enabled {
						t.Fatal("publication changed the draft slot")
					}
				})
			}
		}
	}
}

// Every slot of a provider revision observes one principal: activation
// refuses slots whose credential versions observe different principals,
// naming each slot's, and publishes the principal they share. A revoked
// version serves no more, so its principal counts for neither.
func TestActivationPublishesTheOnePrincipalItsSlotsObserve(t *testing.T) {
	grant := &Configuration{Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant}
	slot := func(name, principal string, revoked bool) slotRow {
		return slotRow{ID: name, Name: name, Enabled: true, Weight: 1, CredentialID: new(name + "-credential"), CredentialVersion: new(1),
			CredentialGrant: true, CredentialPrincipal: principal, CredentialRevoked: revoked}
	}
	pooled := []slotRow{slot("Default", "operator@example.com", false), slot("Backup", "operator@example.com", false), slot("Retired", "former@example.com", true), {ID: "Empty", Name: "Empty", Enabled: true, Weight: 1}}
	if err := onePrincipal(pooled, grant); err != nil {
		t.Fatal(err)
	}
	var published []runtime.RevisionSlot
	for i := range pooled {
		published = append(published, pooled[i].published(grant.AuthMode))
	}
	if principal := runtime.ObservedPrincipal(published); principal != "operator@example.com" || published[1].ObservedPrincipal != principal ||
		published[2].ObservedPrincipal != "" || published[2].Enabled || published[3].ObservedPrincipal != "" {
		t.Fatalf("published %+v", published)
	}

	mixed := []slotRow{slot("Default", "operator@example.com", false), slot("Backup", "other@example.com", false)}
	problem, ok := errors.AsType[*access.Problem](onePrincipal(mixed, grant))
	if !ok || problem.Status != 422 || problem.Code != "principal_mismatch" ||
		!strings.Contains(problem.Detail, "slot Default observes operator@example.com; slot Backup observes other@example.com") {
		t.Fatalf("mixed principals: %v", problem)
	}
	static := &Configuration{Kind: connectors.KindPlugin, AuthMode: connectors.AuthStaticCredential}
	if err := onePrincipal([]slotRow{{Name: "Default"}, {Name: "Backup"}}, static); err != nil {
		t.Fatal(err)
	}
}
