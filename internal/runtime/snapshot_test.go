package runtime

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func quotaLimit(value int64) *int64 { return &value }

func quotaSnapshot(provider Provider) *Snapshot {
	return &Snapshot{
		Generation: Generation{ID: uuid.NewString(), Ordinal: 1, ActivatedAt: time.Now().UTC()},
		Providers:  map[string]Provider{provider.ID: provider},
		Routes:     map[string]Route{},
	}
}

// A release recorded before connection quotas existed must still install, so
// a provider that publishes no quota and no vendor must keep the serving
// shape - and therefore the digest - it had then.
func TestConnectionQuotaOnlyReachesTheSnapshotWhenConfigured(t *testing.T) {
	provider := Provider{ID: uuid.NewString(), Name: "vendor pool", Kind: "openai_compatible", Enabled: true, RevisionID: uuid.NewString(), Capabilities: []Capability{}}
	encoded, err := json.Marshal(provider)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "vendor_id") || strings.Contains(string(encoded), "limits") {
		t.Fatalf("an unconfigured provider changed the serving shape: %s", encoded)
	}
	plain := quotaSnapshot(provider)
	if err = plain.Validate(); err != nil {
		t.Fatal(err)
	}
	before, err := plain.Digest()
	if err != nil {
		t.Fatal(err)
	}

	provider.VendorID = "openai"
	provider.Limits = &Limits{RequestsPerMinute: quotaLimit(60), TokensPerMinute: quotaLimit(120000), MaxConcurrency: quotaLimit(4)}
	quoted := quotaSnapshot(provider)
	if err = quoted.Validate(); err != nil {
		t.Fatal(err)
	}
	after, err := quoted.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("the published quota did not reach the digest")
	}

	stored, err := json.Marshal(quoted)
	if err != nil {
		t.Fatal(err)
	}
	installed := &Snapshot{}
	if err = json.Unmarshal(stored, installed); err != nil {
		t.Fatal(err)
	}
	served := installed.Providers[provider.ID]
	if served.VendorID != "openai" || served.Limits == nil {
		t.Fatalf("quota lost in the serving snapshot: %+v", served)
	}
	if *served.Limits.RequestsPerMinute != 60 || *served.Limits.TokensPerMinute != 120000 || *served.Limits.MaxConcurrency != 4 {
		t.Fatalf("quota changed in the serving snapshot: %+v", *served.Limits)
	}
	reinstalled, err := installed.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if reinstalled != after {
		t.Fatalf("digest %s changed to %s across a round trip", after, reinstalled)
	}
}

func TestPublishedLimitsDropsQuotasThatBoundNothing(t *testing.T) {
	if publishedLimits(nil) != nil {
		t.Fatal("a missing quota published a quota")
	}
	if publishedLimits(&Limits{}) != nil {
		t.Fatal("an empty quota published a quota")
	}
	configured := &Limits{MaxConcurrency: quotaLimit(2)}
	if publishedLimits(configured) != configured {
		t.Fatal("a configured quota was dropped")
	}
}
