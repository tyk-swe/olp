package interaction

import (
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func TestNativeAssetIDsRequireOwnedServingAuthority(t *testing.T) {
	for _, fixture := range []struct {
		profile string
		family  wire.Format
		body    string
	}{
		{"openai-chat", wire.Chat, `{"model":"route","messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"private-file"}}]}]}`},
		{"anthropic-messages", wire.Anthropic, `{"model":"route","max_tokens":32,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"file","file_id":"private-file"}}]}]}`},
		{"gemini-generation", wire.Gemini, `{"contents":[{"parts":[{"fileData":{"mimeType":"text/plain","fileUri":"https://generativelanguage.googleapis.com/v1beta/files/private-file"}}]}]}`},
		{"gemini-generation", wire.Gemini, `{"contents":[{"parts":[{"fileData":{"mimeType":"text/plain","fileUri":"https://GENERATIVELANGUAGE.googleapis.com./v1beta/files/private-file"}}]}]}`},
	} {
		t.Run(fixture.profile, func(t *testing.T) {
			compiled := template(t, configuration(t, fixture.profile))
			_, err := compiled.Bind(request(t, fixture.family, fixture.body), Context{})
			assertReason(t, err, "resource_affinity")
		})
	}
}
