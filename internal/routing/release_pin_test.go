package routing

import (
	"encoding/json"
	"testing"
)

func TestHistoricalReleasePinRejectsSiblingCredentialAndMissingIdentity(t *testing.T) {
	first, second := "credential-1", "credential-2"
	provider := Provider{ID: "provider", RevisionID: "revision", ActiveCredential: &second, Slots: []Slot{{ID: "first", CredentialID: &first}, {ID: "second", CredentialID: &second}}}
	route := Route{Slug: "route", RevisionID: "route-revision", Targets: []Target{{ProviderID: "provider", ProviderModel: "model"}}}
	source, err := json.Marshal(Snapshot{Providers: map[string]Provider{provider.ID: provider}, Routes: map[string]Route{route.Slug: route}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, revision, slug, slot string
		credential                 *string
		valid                      bool
	}{
		{"exact selected pin", "revision", "route", "first", &first, true},
		{"sibling and default credential", "revision", "route", "first", &second, false},
		{"missing credential", "revision", "route", "first", nil, false},
		{"missing slot", "revision", "route", "missing", &first, false},
		{"wrong provider revision", "other", "route", "first", &first, false},
		{"missing route", "revision", "missing", "first", &first, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pin, err := DecodeReleasePin(source, ReleasePinIdentity{ProviderID: "provider", ProviderRevisionID: tc.revision, RouteSlug: tc.slug, Model: "model", SlotID: tc.slot, CredentialID: tc.credential})
			if (err == nil) != tc.valid {
				t.Fatalf("pin=%+v err=%v", pin, err)
			}
			if tc.valid && (pin.Provider.RevisionID != "revision" || pin.Slot.ID != "first" || *pin.Slot.CredentialID != first) {
				t.Fatalf("identity changed: %+v", pin)
			}
		})
	}
	for _, model := range []string{"", "another-model"} {
		if _, err := DecodeReleasePin(source, ReleasePinIdentity{ProviderID: "provider", ProviderRevisionID: "revision", RouteSlug: "route", Model: model, SlotID: "first", CredentialID: &first}); err == nil {
			t.Fatalf("route admitted unrelated model %q", model)
		}
	}
}

func TestHistoricalSlotDoesNotApplyNewWorkEnabledPolicy(t *testing.T) {
	credential := "retained-version"
	provider := Provider{Slots: []Slot{{ID: "disabled-now", Enabled: false, CredentialID: &credential}}}
	if _, ok := PinnedSlot(&provider, "disabled-now", &credential); !ok {
		t.Fatal("new-work eligibility displaced historical identity")
	}
	provider.Slots[0].CredentialID = nil
	if _, ok := PinnedSlot(&provider, "disabled-now", nil); !ok {
		t.Fatal("credentialless retained identity rejected")
	}
}
