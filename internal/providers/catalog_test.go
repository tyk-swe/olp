package providers

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/signing"
)

func testCatalog(t *testing.T) *catalog.Signed {
	t.Helper()
	observed := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	provenance := catalog.Provenance{SourceURL: "https://example.test/models", ObservedAt: observed}
	limit, output, three := int64(200000), int64(64000), catalog.Decimal("3")
	retires, deprecated := catalog.Date("2026-11-30"), catalog.Date("2026-09-30")
	document, err := catalog.Encode(&catalog.Catalog{APIVersion: catalog.APIVersion, PublishedAt: observed, Currency: "USD", Vendors: []catalog.Vendor{{ID: "anthropic", Name: "Anthropic", Models: []catalog.Model{{
		ID: "claude-sonnet-4-5-20250929", Aliases: []string{"claude-sonnet-4-5"}, CanonicalModel: "anthropic/claude-sonnet-4.5", ContextLength: &limit, MaxOutputTokens: &output,
		InputModalities: []string{"image", "text"}, OutputModalities: []string{"text"}, Capabilities: catalog.Capabilities{Tools: new(true)},
		Lifecycle: &catalog.Lifecycle{DeprecatedAt: &deprecated, RetiresAt: &retires, Replacement: "claude-sonnet-5-5", Provenance: provenance},
		Prices:    []catalog.Price{{Operation: "generation", InputPerMillion: &three, Unrepresentable: []catalog.Unrepresentable{}, Provenance: provenance}}, Provenance: provenance,
	}}}}})
	if err != nil {
		t.Fatal(err)
	}
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	signature, err := signing.Sign(nil, "test-key", private, document)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := catalog.Load(document, signature, signing.MustKeyring(signing.Key{ID: "test-key", Public: public}))
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func TestCatalogSuggestionsMatchByAliasAndKeepOperatorFacts(t *testing.T) {
	signed := testCatalog(t)
	cfg := Configuration{Kind: KindAnthropic, AuthMode: AuthAPIKey}
	cfg.Normalize()
	cfg.Options.Models = map[string]json.RawMessage{"claude-sonnet-4-5": json.RawMessage(`{"context_length":100000,"deployment":"team-sonnet","region":"us"}`)}
	models := []storedModel{{ID: "m1", UpstreamModel: "claude-sonnet-4-5"}, {ID: "m2", UpstreamModel: "unlisted"}}
	suggestions, err := suggestionsFor(signed, &cfg, models)
	if err != nil {
		t.Fatal(err)
	}
	if len(suggestions) != 1 {
		t.Fatalf("suggestions = %+v", suggestions)
	}
	suggestion := suggestions[0]
	if suggestion.MatchedBy != "alias" || suggestion.CatalogModel != "claude-sonnet-4-5-20250929" || suggestion.Conflict != nil || suggestion.Lifecycle == nil || *suggestion.Lifecycle.RetiresAt != "2026-11-30" {
		t.Fatalf("suggestion = %+v", suggestion)
	}
	for _, change := range []string{"context_length", "max_output_tokens", "canonical_model", "input_modalities"} {
		if !slices.Contains(suggestion.Changes, change) {
			t.Fatalf("changes %v lack %s", suggestion.Changes, change)
		}
	}
	merged, _, err := applyCatalogFacts(cfg.Options.Models["claude-sonnet-4-5"], suggestion.Facts)
	if err != nil {
		t.Fatal(err)
	}
	var facts map[string]any
	_ = json.Unmarshal(merged, &facts)
	if facts["deployment"] != "team-sonnet" || facts["region"] != "us" || facts["context_length"] != float64(200000) || facts["source"] != signed.Source() || facts["observed_at"] != "2026-10-05T12:00:00Z" {
		t.Fatalf("merged facts = %v", facts)
	}
	if _, changes, _ := applyCatalogFacts(merged, suggestion.Facts); len(changes) != 0 {
		t.Fatalf("accepted facts still differ: %v", changes)
	}
}

// TestCatalogNeverPosesAsPrivacyEvidence refuses to overwrite the source that
// attests a privacy declaration.
func TestCatalogNeverPosesAsPrivacyEvidence(t *testing.T) {
	signed := testCatalog(t)
	cfg := Configuration{Kind: KindAnthropic, AuthMode: AuthAPIKey}
	cfg.Normalize()
	cfg.Options.Models = map[string]json.RawMessage{"claude-sonnet-4-5-20250929": json.RawMessage(`{"zero_data_retention":true,"source":"https://contract.example.test","observed_at":"2026-09-01T00:00:00Z"}`)}
	suggestions, err := suggestionsFor(signed, &cfg, []storedModel{{ID: "m1", UpstreamModel: "claude-sonnet-4-5-20250929"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(suggestions) != 1 || suggestions[0].Conflict == nil || *suggestions[0].Conflict != "privacy_evidence" || suggestions[0].MatchedBy != "id" {
		t.Fatalf("suggestions = %+v", suggestions)
	}
}

func TestCatalogSuggestionsMatchAStoredCanonicalModel(t *testing.T) {
	signed := testCatalog(t)
	cfg := Configuration{Kind: KindAnthropic, AuthMode: AuthAPIKey}
	cfg.Normalize()
	cfg.Options.Models = map[string]json.RawMessage{"house-sonnet": json.RawMessage(`{"canonical_model":"anthropic/claude-sonnet-4.5"}`)}
	suggestions, _ := suggestionsFor(signed, &cfg, []storedModel{{ID: "m1", UpstreamModel: "house-sonnet"}})
	if len(suggestions) != 1 || suggestions[0].MatchedBy != "canonical_model" {
		t.Fatalf("suggestions = %+v", suggestions)
	}
	if lifecycle := signed.Lifecycle("anthropic", "house-sonnet", canonicalModel(cfg.Options.Models["house-sonnet"])); lifecycle == nil || *lifecycle.Replacement != "claude-sonnet-5-5" {
		t.Fatalf("lifecycle = %+v", lifecycle)
	}
	if signed.Lifecycle("google", "house-sonnet", canonicalModel(nil)) != nil {
		t.Fatal("a lifecycle matched under another vendor")
	}
}
