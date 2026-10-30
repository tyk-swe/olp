package configuration_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/tyk-swe/olp/internal/configuration"
)

func TestCodeConfigurationDigestRetainsPublishedAndDraftPolicies(t *testing.T) {
	decode := func(raw string) *configuration.Document {
		t.Helper()
		var doc configuration.Document
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			t.Fatal(err)
		}
		return &doc
	}
	original := decode(`{"api_version":"openllmproxy.dev/config/v2","code":{"routes":[{"id":"0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607","project":"team","slug":"coding","draft":{"pool":"pool","models":["next"],"enabled":true},"published":{"policy":{"pool":"pool","models":["native"],"enabled":true,"model_rewriting":true,"aliases":{"stable":"native"},"initial_recovery_attempts":2,"messages_to_codex":true},"connections":{}}}]}}`)
	changed := decode(`{"api_version":"openllmproxy.dev/config/v2","code":{"routes":[{"id":"0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607","project":"team","slug":"coding","draft":{"pool":"pool","models":["next"],"enabled":true},"published":{"policy":{"pool":"pool","models":["native"],"enabled":true,"model_rewriting":true,"aliases":{"stable":"native"},"initial_recovery_attempts":0,"messages_to_codex":true},"connections":{}}}]}}`)
	before, err := configuration.Digest(original)
	if err != nil {
		t.Fatal(err)
	}
	after, err := configuration.Digest(changed)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("published recovery policy was discarded from the configuration intent")
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	roundtrip, err := configuration.Digest(decode(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	if before != roundtrip {
		t.Fatal("configuration roundtrip changed its effective intent")
	}
}

func TestCodeConfigurationRefusesCrossProjectAndUnresolvedReferences(t *testing.T) {
	const account = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50601"
	const pool = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50602"
	const route = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50603"
	valid := configuration.CodeConfiguration{Accounts: []configuration.CodeAccountEntry{{ID: account, Project: "team", Provider: "subscription", CredentialRef: "enrollment", Name: "account", Enabled: true, Models: []string{"native"}}}, Pools: []configuration.CodePoolEntry{{ID: pool, Project: "team", Name: "pool", Kind: "shared", Accounts: map[string]int64{account: 2}, APIKeyRefs: []string{"key"}}}, Routes: []configuration.CodeRouteEntry{{ID: route, Project: "team", Slug: "coding", Draft: configuration.CodeRoutePolicy{Pool: pool, Models: []string{"native"}, Enabled: true}}}}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{"other", ""} {
		t.Run("pool-project-"+project, func(t *testing.T) {
			bad := valid
			bad.Pools = []configuration.CodePoolEntry{{ID: pool, Project: project, Name: "pool", Kind: "shared", Accounts: map[string]int64{account: 2}, APIKeyRefs: []string{"key"}}}
			if err := bad.Validate(); err == nil {
				t.Fatal("account reference crossed its project")
			}
		})
	}
	bad := valid
	bad.Routes = []configuration.CodeRouteEntry{{ID: route, Project: "team", Slug: "coding", Draft: configuration.CodeRoutePolicy{Pool: account, Models: []string{"native"}, Enabled: true}}}
	if err := bad.Validate(); err == nil {
		t.Fatal("unresolved pool reference accepted")
	}
}

func TestCodeConfigurationDigestRetainsPaidAuthorization(t *testing.T) {
	digest := func(enabled bool) string {
		t.Helper()
		raw := fmt.Sprintf(`{"api_version":"%s","code":{"routes":[{"id":"00000000-0000-4000-8000-000000000001","project":"source","slug":"coding","draft":{"pool":"00000000-0000-4000-8000-000000000002","models":["native"],"paid_participation":%t,"paid_targets":{"native":{"provider":"metered","slot":"primary","credential_ref":"paid-primary"}}}}]}}`, configuration.APIVersion, enabled)
		var doc configuration.Document
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			t.Fatal(err)
		}
		value, err := configuration.Digest(&doc)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	if digest(true) == digest(false) {
		t.Fatal("paid monetary authorization was discarded from configuration intent")
	}
}

func TestCodeConfigurationPreservesUnassignedPools(t *testing.T) {
	code := configuration.CodeConfiguration{Pools: []configuration.CodePoolEntry{{ID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50601", Project: "team", Name: "unassigned", Kind: "shared", Accounts: map[string]int64{}, APIKeyRefs: []string{}}}}
	if err := code.Validate(); err != nil {
		t.Fatalf("unassigned pool policy was rejected %v", err)
	}
}

func TestCodeConfigurationDigestRetainsChatCompatibility(t *testing.T) {
	digest := func(enabled bool) string {
		t.Helper()
		var document configuration.Document
		if err := json.Unmarshal([]byte(fmt.Sprintf(`{"api_version":"openllmproxy.dev/config/v2","code":{"routes":[{"id":"0192cf87-d4ab-7f2e-a8b1-c2d3e4f50601","project":"team","slug":"coding","draft":{"pool":"0192cf87-d4ab-7f2e-a8b1-c2d3e4f50602","models":["native"],"enabled":true,"chat_to_antigravity":%t}}]}}`, enabled)), &document); err != nil {
			t.Fatal(err)
		}
		value, err := configuration.Digest(&document)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	if digest(true) == digest(false) {
		t.Fatal("explicit chat compatibility was discarded from the configuration intent")
	}
}
