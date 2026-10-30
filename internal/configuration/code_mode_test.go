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

func TestCodeConfigurationDigestRetainsIndependentCodexIdentityPolicy(t *testing.T) {
	document := func(draft, published bool) configuration.Document {
		return configuration.Document{APIVersion: configuration.APIVersion, Code: &configuration.CodeConfiguration{Routes: []configuration.CodeRouteEntry{{ID: "route", Draft: configuration.CodeRoutePolicy{CodexCLIIdentity: draft, MessagesToCodex: true}, Published: &configuration.CodePublication{Policy: configuration.CodeRoutePolicy{CodexCLIIdentity: published, MessagesToCodex: true}}}}}}
	}
	digest := func(value configuration.Document) string {
		digest, err := configuration.Digest(&value)
		if err != nil {
			t.Fatal(err)
		}
		return digest
	}
	baseline := digest(document(false, false))
	for _, policy := range []struct{ draft, published bool }{{true, false}, {false, true}, {true, true}} {
		if digest(document(policy.draft, policy.published)) == baseline {
			t.Fatal("configuration lost an independently selected identity policy", policy)
		}
	}
	encoded, err := json.Marshal(document(false, true))
	if err != nil {
		t.Fatal(err)
	}
	var restored configuration.Document
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Code.Routes[0].Draft.CodexCLIIdentity || !restored.Code.Routes[0].Published.Policy.CodexCLIIdentity {
		t.Fatal("roundtrip changed the draft or published opt-in", restored.Code.Routes[0])
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

func TestCodeConfigurationDigestRetainsPaidFallbackAuthorization(t *testing.T) {
	artifact := func(slot string) configuration.Document {
		var document configuration.Document
		if err := json.Unmarshal([]byte(fmt.Sprintf(`{"api_version":"openllmproxy.dev/config/v2","code":{"routes":[{"id":"0192cf87-d4ab-7f2e-a8b1-c2d3e4f50601","project":"Coding","slug":"portable","draft":{"pool":"0192cf87-d4ab-7f2e-a8b1-c2d3e4f50602","models":["native"],"enabled":true,"initial_recovery_attempts":1,"paid_participation":true,"paid_fallback_targets":{"native":{"provider":"metered","slot":%q,"credential_ref":"paid"}}}}]}}`, slot)), &document); err != nil {
			t.Fatal(err)
		}
		return document
	}
	first, firstError := configuration.Digest(new(artifact("first")))
	if firstError != nil {
		t.Fatal(firstError)
	}
	second, secondError := configuration.Digest(new(artifact("second")))
	if secondError != nil {
		t.Fatal(secondError)
	}
	if first == second {
		t.Fatal("paid fallback authorization was discarded from configuration intent")
	}
}

func TestCodeConfigurationPaidFallbackRequiresRecovery(t *testing.T) {
	const pool = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50601"
	const route = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50602"
	artifact := func(recovery int) configuration.CodeConfiguration {
		return configuration.CodeConfiguration{Pools: []configuration.CodePoolEntry{{ID: pool, Project: "coding", Name: "shared", Kind: "shared", Accounts: map[string]int64{}, APIKeyRefs: []string{}}}, Routes: []configuration.CodeRouteEntry{{ID: route, Project: "coding", Slug: "coding", Draft: configuration.CodeRoutePolicy{Pool: pool, Models: []string{"native"}, Enabled: true, PaidParticipation: true, InitialRecoveryAttempts: recovery, PaidFallbackTargets: map[string]configuration.CodePaidTarget{"native": {Provider: "metered", Slot: "primary", CredentialRef: "paid"}}}}}}
	}
	if err := artifact(0).Validate(); err == nil {
		t.Fatal("fallback bypassed the required recovery authorization")
	}
	if err := artifact(1).Validate(); err != nil {
		t.Fatalf("authorized fallback was rejected %v", err)
	}
}
