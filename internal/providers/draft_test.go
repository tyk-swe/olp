package providers

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/connectors/providerconfig"
	"github.com/tyk-swe/olp/internal/providers/connection"
)

func TestCheckActivationSlotValidatesSlotAndCredentialState(t *testing.T) {
	t.Parallel()

	apiKey := connection.Configuration{Identity: providerconfig.Identity[*string]{AuthMode: connectors.AuthAPIKey}}
	grant := connection.Configuration{Identity: providerconfig.Identity[*string]{
		AuthMode: connectors.AuthGrant, ProfileID: "profile", ProfileRevision: "build",
	}}
	noAuth := connection.Configuration{Identity: providerconfig.Identity[*string]{AuthMode: connectors.AuthNone}}

	for _, tc := range []struct {
		name     string
		cfg      connection.Configuration
		slots    []slotRow
		wantSlot string
		wantCode string
	}{
		{
			name:     "no enabled slots fails",
			cfg:      apiKey,
			slots:    []slotRow{{ID: "disabled", Default: true, Enabled: false, CredentialID: new("cred-1")}},
			wantCode: "credential_required",
		},
		{
			name:     "unauthenticated provider succeeds with enabled slot and no credential",
			cfg:      noAuth,
			slots:    []slotRow{{ID: "default", Default: true, Enabled: true}},
			wantSlot: "default",
		},
		{
			name:     "unauthenticated provider still requires an enabled slot",
			cfg:      noAuth,
			slots:    []slotRow{{ID: "disabled", Default: true, Enabled: false}},
			wantCode: "credential_required",
		},
		{
			name:     "missing api key credential fails",
			cfg:      apiKey,
			slots:    []slotRow{{ID: "default", Default: true, Enabled: true}},
			wantCode: "credential_required",
		},
		{
			name:     "revoked sole credential fails",
			cfg:      apiKey,
			slots:    []slotRow{{ID: "default", Default: true, Enabled: true, CredentialID: new("cred-1"), CredentialRevoked: true}},
			wantCode: "credential_revoked",
		},
		{
			name:     "lapsed sole grant fails",
			cfg:      grant,
			slots:    []slotRow{{ID: "default", Default: true, Enabled: true, CredentialID: new("cred-1"), CredentialPlugin: "build", CredentialProfile: "profile", CredentialLapsed: true}},
			wantCode: "credential_lapsed",
		},
		{
			name:     "mismatched credential fails",
			cfg:      grant,
			slots:    []slotRow{{ID: "default", Default: true, Enabled: true, CredentialID: new("cred-1")}},
			wantCode: "credential_mismatch",
		},
		{
			name: "healthy secondary slot is selected over revoked default",
			cfg:  apiKey,
			slots: []slotRow{
				{ID: "default", Default: true, Enabled: true, CredentialID: new("cred-revoked"), CredentialRevoked: true},
				{ID: "backup", Enabled: true, Position: 1, CredentialID: new("cred-ok")},
			},
			wantSlot: "backup",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			slot, err := checkActivationSlot(tc.cfg, tc.slots)
			if tc.wantCode != "" {
				problem, ok := errors.AsType[*httpkit.Problem](err)
				if !ok || problem.Status != 422 || problem.Code != tc.wantCode {
					t.Fatalf("checkActivationSlot error = %v, want 422 %s", err, tc.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("checkActivationSlot returned unexpected error: %v", err)
			}
			if slot == nil || slot.ID != tc.wantSlot {
				t.Fatalf("selected slot = %+v, want %q", slot, tc.wantSlot)
			}
		})
	}
}

func TestBuildRevisionModelsAndSlotsEnforceActivationRules(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	certified := []storedModel{
		{
			ID:            "m-1",
			UpstreamModel: "model-a",
			DisplayName:   "Model A",
			Enabled:       true,
			Capabilities:  []storedCapability{{Capability: connection.Capability{Operation: "generation", Surface: "openai", Mode: "unary"}, Source: "certified", CertifiedAt: &now}},
		},
	}

	t.Run("no enabled models rejected", func(t *testing.T) {
		t.Parallel()
		_, err := buildRevisionModels(nil)
		problem, ok := errors.AsType[*httpkit.Problem](err)
		if !ok || problem.Status != 422 || problem.Code != "no_enabled_models" {
			t.Fatalf("error = %v, want 422 no_enabled_models", err)
		}
	})

	t.Run("uncertified capability rejected", func(t *testing.T) {
		t.Parallel()
		models := []storedModel{{
			ID:            "m-1",
			UpstreamModel: "model-a",
			Enabled:       true,
			Capabilities:  []storedCapability{{Capability: connection.Capability{Operation: "generation", Surface: "openai", Mode: "unary"}, Source: "declared"}},
		}}
		_, err := buildRevisionModels(models)
		problem, ok := errors.AsType[*httpkit.Problem](err)
		if !ok || problem.Status != 422 || problem.Code != "certification_required" {
			t.Fatalf("error = %v, want 422 certification_required", err)
		}
	})

	t.Run("mixed grant principals rejected", func(t *testing.T) {
		t.Parallel()
		grantCfg := connection.Configuration{Identity: providerconfig.Identity[*string]{
			AuthMode: connectors.AuthGrant, ProfileID: "profile", ProfileRevision: "build",
		}}
		slots := []slotRow{
			{ID: "s-1", Name: "first", Default: true, Enabled: true, CredentialID: new("c-1"), CredentialPlugin: "build", CredentialProfile: "profile", CredentialPrincipal: "acct-1"},
			{ID: "s-2", Name: "second", Enabled: true, CredentialID: new("c-2"), CredentialPlugin: "build", CredentialProfile: "profile", CredentialPrincipal: "acct-2"},
		}
		_, _, err := buildRevisionSlots(grantCfg, certified, slots)
		problem, ok := errors.AsType[*httpkit.Problem](err)
		if !ok || problem.Status != 422 || problem.Code != "principal_mismatch" {
			t.Fatalf("error = %v, want 422 principal_mismatch", err)
		}
	})

	t.Run("unvalidated enabled slot rejected", func(t *testing.T) {
		t.Parallel()
		apiCfg := connection.Configuration{Identity: providerconfig.Identity[*string]{AuthMode: connectors.AuthAPIKey}}
		slots := []slotRow{{ID: "s-1", Name: "default", Default: true, Enabled: true, CredentialID: new("c-1"), CredentialVersion: new(2)}}
		_, _, err := buildRevisionSlots(apiCfg, certified, slots)
		problem, ok := errors.AsType[*httpkit.Problem](err)
		if !ok || problem.Status != 422 || problem.Code != "slot_validation_required" {
			t.Fatalf("error = %v, want 422 slot_validation_required", err)
		}
	})

	t.Run("validated slot publishes default credential version", func(t *testing.T) {
		t.Parallel()
		apiCfg := connection.Configuration{Identity: providerconfig.Identity[*string]{AuthMode: connectors.AuthAPIKey}}
		slot := slotRow{ID: "s-1", Name: "default", Default: true, Enabled: true, CredentialID: new("c-1"), CredentialVersion: new(3)}
		fp, err := slot.validationFingerprint(apiCfg, certified)
		if err != nil {
			t.Fatalf("validationFingerprint: %v", err)
		}
		slot.ValidatedFingerprint = &fp
		slot.ValidatedAt = &now
		published, version, err := buildRevisionSlots(apiCfg, certified, []slotRow{slot})
		if err != nil {
			t.Fatalf("buildRevisionSlots: %v", err)
		}
		if len(published) != 1 || version == nil || *version != 3 {
			t.Fatalf("published = (%+v, %v), want 1 slot with version 3", published, version)
		}
	})
}

func TestNormalizeSlotRestrictionsDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	original := SlotRestrictions{}
	normalized := normalizeSlotRestrictions(original)
	if original.AllowedAPIKeys != nil || original.AllowedModels != nil || original.AllowedRoutes != nil {
		t.Fatalf("normalizeSlotRestrictions mutated caller input: %+v", original)
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		t.Fatalf("marshal normalized restrictions: %v", err)
	}
	if want := `{"allowed_api_keys":[],"allowed_models":[],"allowed_routes":[]}`; string(raw) != want {
		t.Fatalf("marshaled restrictions = %s, want %s", raw, want)
	}
}
