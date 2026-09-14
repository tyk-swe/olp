package providers

import (
	"testing"
	"time"
)

func TestCredentialValidationTracksUpstreamAccess(t *testing.T) {
	cfg := Configuration{Kind: "openai_compatible", AuthMode: AuthAPIKey, Endpoint: ptr("https://example.com/v1")}
	row := slotRow{CredentialID: ptr("original")}
	models := []storedModel{
		{UpstreamModel: "a", Enabled: true, Capabilities: []storedCapability{{Operation: "generation", Surface: "openai", Mode: "unary"}}},
		{UpstreamModel: "b", Enabled: true, Capabilities: []storedCapability{{Operation: "generation", Surface: "openai", Mode: "streaming"}}},
	}
	row.ValidatedAt = ptr(time.Now().UTC())
	row.ValidatedFingerprint = ptr(row.validationFingerprint(&cfg, models))
	if row.validationTime(&cfg, models) == nil {
		t.Fatal("matching validation was lost")
	}
	row.Restrictions.AllowedRoutes = []string{"route"}
	row.Priority, row.Weight = 3, 2
	if row.validationTime(&cfg, []storedModel{models[1], models[0]}) == nil {
		t.Fatal("routing preferences or ordering invalidated upstream access")
	}
	for _, mutate := range []func(*Configuration, *slotRow, []storedModel){
		func(c *Configuration, _ *slotRow, _ []storedModel) { c.Endpoint = ptr("https://other.example.com/v1") },
		func(_ *Configuration, s *slotRow, _ []storedModel) { s.CredentialID = ptr("rotated") },
		func(_ *Configuration, s *slotRow, _ []storedModel) { s.Restrictions.AllowedModels = []string{"a"} },
		func(_ *Configuration, _ *slotRow, m []storedModel) { m[1].Enabled = false },
		func(_ *Configuration, _ *slotRow, m []storedModel) { m[1].UpstreamModel = "new-model" },
		func(_ *Configuration, _ *slotRow, m []storedModel) {
			m[1].Capabilities = []storedCapability{{Operation: "generation", Surface: "openai", Mode: "unary"}}
		},
	} {
		changedCfg, changedSlot := cfg, row
		changedModels := append([]storedModel(nil), models...)
		mutate(&changedCfg, &changedSlot, changedModels)
		if changedSlot.validationTime(&changedCfg, changedModels) != nil {
			t.Fatal("changed upstream access retained validation")
		}
	}
}

func TestDefaultCertificationCannotCombineCredentialVersions(t *testing.T) {
	cfg := Configuration{Kind: "openai_compatible", AuthMode: AuthAPIKey, Endpoint: ptr("https://example.com/v1")}
	row := slotRow{Default: true, CredentialID: ptr("current")}
	at := time.Now().UTC()
	capability := storedCapability{Operation: "generation", Surface: "openai", Mode: "unary", Source: "certified", CertifiedAt: &at, CredentialFingerprint: row.credentialFingerprint(&cfg)}
	models := []storedModel{
		{UpstreamModel: "a", Enabled: true, Capabilities: []storedCapability{capability}},
		{UpstreamModel: "b", Enabled: true, Capabilities: []storedCapability{capability}},
	}
	if row.certificationTime(&cfg, models) == nil {
		t.Fatal("complete current certification did not validate the credential")
	}
	models[1].Capabilities[0].CredentialFingerprint = cfg.transportFingerprint() + ":old"
	if row.certificationTime(&cfg, models) != nil {
		t.Fatal("certifications from different credentials validated the slot")
	}
	models[1].Enabled = false
	if row.certificationTime(&cfg, models) == nil {
		t.Fatal("disabled model blocked validation of current access")
	}
}
