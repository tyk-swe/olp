package routing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/antigravityauth"
	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/vendor"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/providerconfig"
)

func TestResearchAbsentPreservesLegacySnapshotDigest(t *testing.T) {
	// Canonical digest input persisted before research bindings existed.
	const legacy = `{"code_routes":{"coding":{"id":"route","project_id":"project","slug":"coding","pool_id":"pool","models":["model"],"enabled":true,"revision_id":"revision","revision":1,"etag":"etag","published_at":null}},"providers":{},"routes":{}}`
	var snapshot Snapshot
	if err := json.Unmarshal([]byte(legacy), &snapshot); err != nil {
		t.Fatal(err)
	}
	digest, err := snapshot.Digest()
	want := sha256.Sum256([]byte(legacy))
	if err != nil || digest != hex.EncodeToString(want[:]) {
		t.Fatalf("legacy release digest changed: %s %v", digest, err)
	}
}

func TestResearchSnapshotCloneOwnsBindingAndServices(t *testing.T) {
	tools := &code.ResearchTools{AccountID: "account", Services: []string{"search", "reader"}}
	input := &Snapshot{CodeRoutes: map[string]code.Route{"coding": {Tools: tools}}}
	frozen := input.Clone()
	before, err := frozen.Digest()
	if err != nil {
		t.Fatal(err)
	}
	tools.AccountID = "replacement"
	tools.Services[0] = "zread"
	after, err := frozen.Digest()
	if err != nil || after != before || frozen.CodeRoutes["coding"].Tools.AccountID != "account" || frozen.CodeRoutes["coding"].Tools.Services[0] != "search" {
		t.Fatalf("caller mutated frozen research binding: %+v %v", frozen.CodeRoutes["coding"].Tools, err)
	}
}

func TestPublishedCodexIdentityFreezesSeparatePolicy(t *testing.T) {
	input := &Snapshot{CodeRoutes: map[string]code.Route{"coding": {CodexCLIIdentity: true}}}
	frozen := input.Clone()
	adapter, _ := vendor.Lookup(code.AdapterCodex)
	if adapter.CompatibilityOriginator(frozen.CodeRoutes["coding"]) == "" {
		t.Fatal("publication dropped explicit identity policy")
	}
	input.CodeRoutes["coding"] = code.Route{MessagesToCodex: true, ChatToCodex: true}
	if adapter.CompatibilityOriginator(frozen.CodeRoutes["coding"]) == "" || adapter.CompatibilityOriginator(input.Clone().CodeRoutes["coding"]) != "" {
		t.Fatal("draft edit changed an existing publication or translation enabled identity")
	}
	enabled, err := frozen.Digest()
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := input.Clone().Digest()
	if err != nil || enabled == disabled {
		t.Fatal("new publication did not capture policy change", err)
	}
}

func TestPublishedMessagesPolicySelectsEligibleFrozenProviders(t *testing.T) {
	publishedAt := time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)
	snapshot := Snapshot{
		Generation: Generation{ID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607", Ordinal: 1},
		CodeRoutes: map[string]code.Route{
			"native":      {ID: "native-route", ProjectID: "project", Slug: "native", PoolID: "pool", Models: []string{"gpt-fixture-model", "minimax-fixture-model", "gemini-fixture-model"}, Enabled: true, RevisionID: "native-revision", Revision: 1, PublishedAt: &publishedAt, ModelRewriting: true, Aliases: map[string]string{"client-messages": "minimax-fixture-model"}},
			"antigravity": {ID: "antigravity-route", ProjectID: "project", Slug: "antigravity", PoolID: "pool", Models: []string{"gemini-fixture-model", "minimax-fixture-model"}, Enabled: true, RevisionID: "antigravity-revision", Revision: 1, PublishedAt: &publishedAt, ChatToAntigravity: true},
			"translated":  {ID: "translated-route", ProjectID: "project", Slug: "translated", PoolID: "pool", Models: []string{"gpt-fixture-model", "minimax-fixture-model", "gemini-fixture-model"}, Enabled: true, RevisionID: "translated-revision", Revision: 1, PublishedAt: &publishedAt, ChatToCodex: true, MessagesToCodex: true, ModelRewriting: true, Aliases: map[string]string{"client-stable": "gpt-fixture-model"}},
		},
		CodeConnections: map[string]Configuration{
			"native-revision:antigravity":      {Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: antigravityauth.ProfileID}},
			"translated-revision:antigravity":  {Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: antigravityauth.ProfileID}},
			"antigravity-revision:antigravity": {Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: antigravityauth.ProfileID}},
			"antigravity-revision:a-native":    {Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: codeplans.OpenCodeGoProfile}},
			"native-revision:a-native":         {Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: codeplans.OpenCodeGoProfile}},
			"native-revision:b-codex":          {Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: codexauth.ProfileID}},
			"translated-revision:a-native":     {Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: codeplans.OpenCodeGoProfile}},
			"translated-revision:b-codex":      {Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: codexauth.ProfileID}},
			"translated-revision:wrong-auth":   {Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "api_key", ProfileID: codexauth.ProfileID}},
			"other-revision:foreign-codex":     {Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: codexauth.ProfileID}},
		},
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var published Snapshot
	if err := json.Unmarshal(encoded, &published); err != nil {
		t.Fatal(err)
	}
	if err := published.Compile(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, slug, path, model string
		transport               code.Transport
		providers               []string
	}{
		{"chat-disabled-by-default", "native", "v1/chat/completions", "gpt-fixture-model", code.HTTP, nil},
		{"chat-published-opt-in", "translated", "v1/chat/completions", "gpt-fixture-model", code.HTTP, []string{"b-codex"}},
		{"chat-translation-is-http-only", "translated", "v1/chat/completions", "gpt-fixture-model", code.WebSocket, nil},
		{"native-default-refuses-cross-protocol", "native", "v1/messages", "gpt-fixture-model", code.HTTP, nil},
		{"exact-alias-selects-target-protocol", "native", "v1/messages", "client-messages", code.HTTP, []string{"a-native"}},
		{"exact-alias-reaches-native-target", "translated", "v1/messages", "client-stable", code.HTTP, []string{"b-codex"}},
		{"published-opt-in-reaches-codex", "translated", "v1/messages", "gpt-fixture-model", code.HTTP, []string{"b-codex"}},
		{"native-messages-remains-reachable", "native", "v1/messages", "minimax-fixture-model", code.HTTP, []string{"a-native"}},
		{"native-codex-responses-remains-reachable", "native", "responses", "gpt-fixture-model", code.HTTP, []string{"b-codex"}},
		{"native-responses-path-selects-matching-account", "native", "v1/responses", "gpt-fixture-model", code.HTTP, []string{"a-native"}},
		{"opt-in-preserves-native-responses", "translated", "responses", "gpt-fixture-model", code.HTTP, []string{"b-codex"}},
		{"messages-translation-is-http-only", "translated", "v1/messages", "gpt-fixture-model", code.WebSocket, nil},
		{"native-responses-keeps-websocket", "translated", "responses", "gpt-fixture-model", code.WebSocket, []string{"b-codex"}},
		{"antigravity-disabled-by-default", "native", "v1/chat/completions", "gemini-fixture-model", code.HTTP, []string{"a-native"}},
		{"codex-chat-opt-in-excludes-antigravity", "translated", "v1/chat/completions", "gemini-fixture-model", code.HTTP, []string{"a-native", "b-codex"}},
		{"published-antigravity-opt-in-reaches-chat", "antigravity", "v1/chat/completions", "gemini-fixture-model", code.HTTP, []string{"a-native", "antigravity"}},
		{"antigravity-opt-in-preserves-native-messages", "antigravity", "v1/messages", "minimax-fixture-model", code.HTTP, []string{"a-native"}},
		{"antigravity-refuses-websocket", "antigravity", "v1/chat/completions", "gemini-fixture-model", code.WebSocket, nil},
		{"unsupported-operation-remains-unserved", "translated", "unsupported-operation", "gpt-fixture-model", code.HTTP, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			route := published.CodeRoutes[test.slug]
			got := published.CodeModelProviders(route, test.path, test.model, test.transport)
			if !slices.Equal(got, test.providers) {
				t.Fatalf("published eligible providers %v, want %v", got, test.providers)
			}
		})
	}
}

func TestExactAliasSnapshotFreezesPublishedModelMapping(t *testing.T) {
	aliases := map[string]string{"team-code": "native-model"}
	pools := map[string][]string{"team-code": {"native-model", "native-next"}}
	input := &Snapshot{CodeRoutes: map[string]code.Route{"coding": {Models: []string{"native-model", "native-next", "native-other"}, Aliases: aliases, ModelRewriting: true, ModelPools: pools}}}
	frozen := input.Clone()
	before, err := frozen.Digest()
	if err != nil {
		t.Fatal(err)
	}
	aliases["team-code"] = "native-next"
	pools["team-code"][1] = "native-other"
	after, err := frozen.Digest()
	if err != nil || after != before {
		t.Fatalf("draft alias edit changed frozen publication %s, %s, %v", before, after, err)
	}
	resolved, err := frozen.CodeRoutes["coding"].ResolveModel(code.Operation{Model: "team-code"})
	if err != nil || resolved.Model != "native-model" {
		t.Fatalf("frozen alias no longer resolves to published target %+v, %v", resolved, err)
	}
	operations, err := frozen.CodeRoutes["coding"].RecoveryModels(code.Operation{Model: "team-code"}, nil)
	if err != nil || len(operations) != 2 || operations[1].Model != "native-next" {
		t.Fatalf("draft pool edit changed frozen recovery %+v, %v", operations, err)
	}
}
