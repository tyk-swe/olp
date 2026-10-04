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

// A credential slot's version fits a provider that authenticates with it: a
// pasted credential a provider that takes a static one, and a grant only a
// provider pinning the plugin build that enrolled it, whatever the build's
// name.
func TestCredentialVersionsFitOnlyProvidersThatAuthenticateWithThem(t *testing.T) {
	enrolling, other := strings.Repeat("a", 64), strings.Repeat("b", 64)
	grant := &Configuration{Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileRevision: enrolling, ProfileID: "profile"}
	static := &Configuration{Kind: connectors.KindPlugin, AuthMode: connectors.AuthStaticCredential, ProfileRevision: enrolling, ProfileID: "profile"}
	for _, tc := range []struct {
		cfg     *Configuration
		plugin  string
		refusal string
	}{
		{cfg: grant, plugin: enrolling},
		{cfg: static},
		{cfg: grant, plugin: other, refusal: "re-enroll the slot's grant"},
		{cfg: grant, refusal: "Enroll a grant"},
		{cfg: static, plugin: enrolling, refusal: "Rotate its credential"},
	} {
		row := slotRow{Name: "Default", CredentialID: new("credential"), CredentialPlugin: tc.plugin, CredentialProfile: "profile"}
		err := row.credentialFits(tc.cfg)
		problem, refused := errors.AsType[*access.Problem](err)
		if (err != nil) != (tc.refusal != "") || refused && (problem.Code != "credential_mismatch" || !strings.Contains(problem.Detail, tc.refusal)) {
			t.Errorf("a %s provider and a version enrolled through %q: %v", tc.cfg.AuthMode, tc.plugin, err)
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
			CredentialPlugin: "plugin-digest", CredentialPrincipal: principal, CredentialRevoked: revoked}
	}
	// A revoked version, or one whose grant lapsed, serves no more and
	// observes no principal; gateways skip a lapsed one as ineligible.
	lapsed := slot("Lapsed", "former@example.com", false)
	lapsed.CredentialLapsed = true
	pooled := []slotRow{slot("Default", "operator@example.com", false), slot("Backup", "operator@example.com", false), slot("Retired", "former@example.com", true), {ID: "Empty", Name: "Empty", Enabled: true, Weight: 1}, lapsed}
	if err := onePrincipal(pooled, grant); err != nil {
		t.Fatal(err)
	}
	var published []runtime.RevisionSlot
	for i := range pooled {
		published = append(published, pooled[i].published(grant.AuthMode))
	}
	if principal := runtime.ObservedPrincipal(published); principal != "operator@example.com" || published[1].ObservedPrincipal != principal ||
		published[2].ObservedPrincipal != "" || published[2].Enabled || published[3].ObservedPrincipal != "" || published[4].ObservedPrincipal != "" {
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

func TestGrantCredentialCannotCrossProfilesWithinTheSameBuild(t *testing.T) {
	cfg := &Configuration{Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileRevision: strings.Repeat("a", 64), ProfileID: "receiving-profile"}
	for _, profile := range []string{"enrolling-profile", "", "receiving-profile"} {
		row := slotRow{Name: "default", CredentialID: new("credential"), CredentialPlugin: cfg.ProfileRevision, CredentialProfile: profile}
		err := row.credentialFits(cfg)
		if (err == nil) != (profile == cfg.ProfileID) {
			t.Fatalf("profile %q: %v", profile, err)
		}
	}
}
