package providers

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
)

func TestCredentialValidationTracksUpstreamAccess(t *testing.T) {
	cfg := Configuration{Kind: "openai_compatible", AuthMode: AuthAPIKey, Endpoint: new("https://example.com/v1")}
	row := slotRow{CredentialID: new("original")}
	models := []storedModel{
		{UpstreamModel: "a", Enabled: true, Capabilities: []storedCapability{{Operation: "generation", Surface: "openai", Mode: "unary"}}},
		{UpstreamModel: "b", Enabled: true, Capabilities: []storedCapability{{Operation: "generation", Surface: "openai", Mode: "streaming"}}},
	}
	row.ValidatedAt = new(time.Now().UTC())
	row.ValidatedFingerprint = new(row.validationFingerprint(&cfg, models))
	if row.validationTime(&cfg, models) == nil {
		t.Fatal("matching validation was lost")
	}
	row.Restrictions.AllowedRoutes = []string{"route"}
	row.Priority, row.Weight = 3, 2
	if row.validationTime(&cfg, []storedModel{models[1], models[0]}) == nil {
		t.Fatal("routing preferences or ordering invalidated upstream access")
	}
	for _, mutate := range []func(*Configuration, *slotRow, []storedModel){
		func(c *Configuration, _ *slotRow, _ []storedModel) { c.Endpoint = new("https://other.example.com/v1") },
		func(_ *Configuration, s *slotRow, _ []storedModel) { s.CredentialID = new("rotated") },
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

func TestBatchStoragePreservesLegacyTransportFingerprint(t *testing.T) {
	cfg := Configuration{Kind: KindOpenAI, AuthMode: AuthAPIKey}
	cfg.Normalize()
	// Pinned before batch_storage was introduced: unchanged providers retain
	// their stored credential-validation evidence across an upgrade.
	const legacy = "6c3bf07bc3b5a37b6b0d097b46a080fb"
	if got := cfg.transportFingerprint(); got != legacy {
		t.Fatalf("legacy transport fingerprint changed: %s", got)
	}
	cfg.BatchStorage = &connectors.BatchStorage{InputURIPrefix: "gs://bucket/input/", OutputURIPrefix: "gs://bucket/output/"}
	withStorage := cfg.transportFingerprint()
	cfg.BatchStorage.OutputURIPrefix = "gs://bucket/other/"
	if withStorage == legacy || cfg.transportFingerprint() == withStorage {
		t.Fatal("batch storage changes did not invalidate transport evidence")
	}
}
