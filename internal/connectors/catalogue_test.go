package connectors

import (
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/vendors"
)

func providersOf(p Profile) []Config {
	candidates := []string{""}
	for _, contract := range vendors.All() {
		if kind, _ := vendors.Kind(contract.ID); Kind(kind) == p.Kind {
			candidates = append(candidates, contract.ID)
		}
	}
	var out []Config
	for _, vendor := range candidates {
		c := Config{Kind: p.Kind, ProfileID: p.ID, ProfileRevision: p.Revision, AuthMode: p.Authentication[0], VendorID: vendor}
		if c.ValidateProfile() == nil {
			out = append(out, c)
		}
	}
	return out
}

func TestEveryCatalogueProfileConfiguresAProviderThatServesItsOperations(t *testing.T) {
	t.Parallel()
	surfaces := []string{"openai", "anthropic", "gemini", "bedrock", "native"}
	modes := []string{"unary", "streaming", "async", "realtime", "webrtc"}
	for _, p := range Profiles() {
		t.Run(p.ID, func(t *testing.T) {
			t.Parallel()
			if found, err := LookupProfile(p.ID, p.Revision); err != nil || found.Label != p.Label {
				t.Fatalf("LookupProfile = %q, %v, want the catalogue's %q", found.Label, err, p.Label)
			}
			providers := providersOf(p)
			if len(providers) == 0 {
				t.Fatal("no provider of the profile's kind can select it")
			}
			for _, operation := range p.Operations {
				served := slices.ContainsFunc(providers, func(c Config) bool {
					return slices.ContainsFunc(surfaces, func(surface string) bool {
						return slices.ContainsFunc(modes, func(mode string) bool { return c.Supports(operation, surface, mode) })
					})
				})
				if !served {
					t.Errorf("the profile lists %s, which no provider that selects it serves", operation)
				}
			}
		})
	}
}

func TestProfilesHandsOutACatalogueCallersCannotChange(t *testing.T) {
	t.Parallel()
	first := Profiles()
	want := first[0].Operations[0]
	first[0].Operations[0] = "changed"
	first[0].OperationDialects["changed"] = "changed"
	again := Profiles()
	if got := again[0].Operations[0]; got != want {
		t.Errorf("operation = %q after a caller changed its copy, want %q", got, want)
	}
	if _, leaked := again[0].OperationDialects["changed"]; leaked {
		t.Error("a caller's change to its copy reached the catalogue")
	}
}

func TestNoCatalogueProfilePublishesADialectForAnOperationItDoesNotServe(t *testing.T) {
	t.Parallel()
	for _, p := range Profiles() {
		for operation := range p.OperationDialects {
			if !slices.Contains(p.Operations, operation) {
				t.Errorf("%s publishes a %s dialect and serves only %v", p.ID, operation, p.Operations)
			}
		}
	}
}

func TestASecondSpeakerCannotSilentlyUnpinAVendorOwnedProfile(t *testing.T) {
	t.Parallel()
	contracts := vendors.All()
	for _, p := range Profiles() {
		spoken := slices.ContainsFunc(contracts, func(c vendors.Contract) bool { return c.Speaks(p.Dialect) })
		owned := len(p.Operations) == 1 && slices.Contains([]string{"realtime", "decision", "ocr", "search"}, p.Operations[0])
		if owned && spoken && p.Vendor() == "" {
			t.Errorf("%s serves %s in a vendor's dialect and requires no vendor", p.ID, p.Operations[0])
		}
	}
}

func TestAPinnedProfileIsSelectedOnlyByItsVendor(t *testing.T) {
	t.Parallel()
	candidates := []string{""}
	for _, contract := range vendors.All() {
		candidates = append(candidates, contract.ID)
	}
	for _, p := range Profiles() {
		if p.Vendor() == "" {
			continue
		}
		if kind, _ := vendors.Kind(p.Vendor()); Kind(kind) != p.Kind {
			t.Errorf("%s requires %s, a vendor of kind %q, on kind %q", p.ID, p.Vendor(), kind, p.Kind)
		}
		for _, vendor := range candidates {
			c := Config{Kind: p.Kind, ProfileID: p.ID, ProfileRevision: p.Revision, AuthMode: p.Authentication[0], VendorID: vendor}
			if selected := c.ValidateProfile() == nil; selected != (vendor == p.Vendor()) {
				t.Errorf("%s selected by vendor %q = %t, and it requires %q", p.ID, vendor, selected, p.Vendor())
			}
		}
	}
}

func TestEveryVendorThatSpeaksCompletionsHasACompletionsProfile(t *testing.T) {
	t.Parallel()
	profiles := Profiles()
	for _, contract := range vendors.All() {
		profiled := slices.ContainsFunc(profiles, func(p Profile) bool {
			return p.Vendor() == contract.ID && p.Kind == Kind(contract.Connector) && p.OperationDialect("generation") == "openai-completions"
		})
		if profiled != contract.Speaks("openai-completions") {
			t.Errorf("%s speaks completions = %t, and a completions profile requires it = %t", contract.ID, !profiled, profiled)
		}
	}
}
