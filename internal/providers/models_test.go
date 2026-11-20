package providers

import (
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/providerconfig"
	"github.com/tyk-swe/olp/internal/providers/connection"
)

func TestCertificationSelectsOnlyEligibleSlots(t *testing.T) {
	t.Parallel()

	const modelName = "target-model"
	model := storedModel{ID: "model-id", UpstreamModel: modelName, Enabled: true}
	apiKey := connection.Configuration{Identity: providerconfig.Identity[*string]{AuthMode: connectors.AuthAPIKey}}
	grant := connection.Configuration{Identity: providerconfig.Identity[*string]{
		AuthMode: connectors.AuthGrant, ProfileID: "profile", ProfileRevision: "build",
	}}
	noAuth := connection.Configuration{Identity: providerconfig.Identity[*string]{AuthMode: connectors.AuthNone}}
	allow := SlotRestrictions{AllowedModels: []string{modelName}}
	exclude := SlotRestrictions{AllowedModels: []string{"different-model", model.ID}}
	allowed := slotRow{ID: "allowed", Enabled: true, Priority: 5, Position: 3, CredentialID: new("credential-allowed"), Restrictions: allow}
	unrestricted := slotRow{ID: "unrestricted", Enabled: true, Priority: 1, Position: 2, CredentialID: new("credential-unrestricted")}
	deniedDefault := slotRow{ID: "denied-default", Enabled: true, Default: true, CredentialID: new("credential-default"), Restrictions: exclude}
	allowedDefault := slotRow{ID: "eligible-default", Enabled: true, Default: true, Priority: 9, CredentialID: new("credential-default"), Restrictions: allow}
	revoked := slotRow{ID: "revoked", Enabled: true, Default: true, CredentialID: new("credential-revoked"), CredentialRevoked: true}
	lapsed := slotRow{ID: "lapsed", Enabled: true, CredentialID: new("credential-lapsed"), CredentialPlugin: "build", CredentialProfile: "profile", CredentialLapsed: true}
	mismatched := slotRow{ID: "wrong-mode", Enabled: true, Default: true, CredentialID: new("credential-mismatch"), CredentialPlugin: "build", CredentialProfile: "profile"}
	for _, test := range []struct {
		name  string
		model storedModel
		cfg   connection.Configuration
		slots []slotRow
		want  string
	}{
		{"default excludes model", model, apiKey, []slotRow{deniedDefault, allowed}, allowed.ID},
		{"no slot allows model", model, apiKey, []slotRow{deniedDefault}, ""},
		{"unrestricted slot allows model", model, apiKey, []slotRow{deniedDefault, unrestricted}, unrestricted.ID},
		{"default wins among eligible slots", model, apiKey, []slotRow{unrestricted, allowedDefault}, allowedDefault.ID},
		{"priority wins before position", model, apiKey, []slotRow{allowed, {ID: "early", Enabled: true, Priority: 9, CredentialID: new("credential-early")}}, allowed.ID},
		{"position breaks priority tie", model, apiKey, []slotRow{allowed, {ID: "early", Enabled: true, Priority: 5, CredentialID: new("credential-early")}}, "early"},
		{"disabled model", storedModel{UpstreamModel: modelName}, apiKey, []slotRow{unrestricted}, ""},
		{"disabled slot", model, apiKey, []slotRow{deniedDefault, {ID: "disabled", CredentialID: new("credential-disabled"), Restrictions: allow}}, ""},
		{"missing credential", model, apiKey, []slotRow{deniedDefault, {ID: "missing", Enabled: true, Restrictions: allow}}, ""},
		{"revoked credential", model, apiKey, []slotRow{revoked, allowed}, allowed.ID},
		{"lapsed grant", model, grant, []slotRow{lapsed}, ""},
		{"mismatched credential", model, apiKey, []slotRow{mismatched, allowed}, allowed.ID},
		{"no credential needed", model, noAuth, []slotRow{deniedDefault, {ID: "no-auth", Enabled: true, Restrictions: allow}}, "no-auth"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := selectProbeSlot(test.slots, test.cfg, &test.model)
			if test.want == "" {
				if got != nil {
					t.Fatalf("selected slot %q, want no eligible slot", got.ID)
				}
				return
			}
			if got == nil {
				t.Fatalf("selected no slot, want %q", test.want)
			}
			if got.ID != test.want {
				t.Fatalf("selected slot %q, want %q", got.ID, test.want)
			}
		})
	}
}
