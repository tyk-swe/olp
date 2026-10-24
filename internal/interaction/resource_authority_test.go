package interaction

import (
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func TestVerifiedGenerationResourceAuthority(t *testing.T) {
	for _, tc := range []struct {
		profile  string
		family   wire.Format
		body     string
		bindings map[string]string
	}{
		{"openai-responses", wire.Responses, `{"model":"route","input":[{"role":"user","content":[{"type":"input_file","file_id":"file-local"},{"type":"input_file","file_id":"file-two-local"}]}],"store":false,"conversation":{"id":"conv-local"},"tools":[{"type":"file_search","vector_store_ids":["store-local"]}]}`, map[string]string{"/input/0/content/0/file_id": "file-native", "/input/0/content/1/file_id": "file-two-native", "/conversation/id": "conv-native", "/tools/0/vector_store_ids/0": "store-native"}},
		{"gemini-generation", wire.Gemini, `{"contents":[{"parts":[{"text":"hello"}]}],"cachedContent":"cache-local"}`, map[string]string{"/cachedContent": "cachedContents/native"}},
		{"openai-responses", wire.Responses, `{"model":"route","input":"hello","store":false,"tools":[{"type":"file_search","vector_store_ids":["store-local"]}]}`, map[string]string{"/tools/0/vector_store_ids/0": "store-native"}},
	} {
		t.Run(tc.profile, func(t *testing.T) {
			config := configuration(t, tc.profile)
			compiled := template(t, config)
			source := request(t, tc.family, tc.body)
			var changes []oif.Change
			for path, id := range tc.bindings {
				changes = append(changes, oif.Change{Pointer: path, Value: `"` + id + `"`, Reason: "verified test authority"})
			}
			bound, err := source.WithResourceBindings(changes...)
			if err != nil {
				t.Fatal(err)
			}
			serving := config.Provider.ServingIdentity(config.ProviderID, config.RevisionID, config.Model)
			context := Context{AllowProviderState: true, RequiredServing: &serving, ResolvedResources: tc.bindings, RetainedResponses: tc.family == wire.Responses}
			if _, err := compiled.Bind(bound, context); err != nil {
				t.Fatalf("verified resource was refused: %v", err)
			}
			context.ResolvedResources = nil
			if _, err := compiled.Bind(bound, context); err == nil {
				t.Fatal("unverified references were accepted")
			}
			context.ResolvedResources = tc.bindings
			context.RequiredServing = nil
			_, err = compiled.Bind(bound, context)
			assertReason(t, err, "resource_affinity")
		})
	}
}

func TestVerifiedFirstAssetDoesNotAuthorizeLaterAsset(t *testing.T) {
	config := configuration(t, "openai-responses")
	compiled := template(t, config)
	source := request(t, wire.Responses, `{"model":"route","input":[{"role":"user","content":[{"type":"input_file","file_id":"first"},{"type":"input_file","file_id":"unowned"}]}],"store":false}`)
	serving := config.Provider.ServingIdentity(config.ProviderID, config.RevisionID, config.Model)
	_, err := compiled.Bind(source, Context{AllowProviderState: true, RequiredServing: &serving, ResolvedResources: map[string]string{"/input/0/content/0/file_id": "first"}})
	assertReason(t, err, "resource_affinity")
}
