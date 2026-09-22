package interaction

import (
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func TestNativeAssetIDsRequireOwnedServingAuthority(t *testing.T) {
	for _, fixture := range []struct {
		profile string
		family  openai.Family
		body    string
	}{
		{"openai-chat", openai.FamilyChat, `{"model":"route","messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"private-file"}}]}]}`},
		{"anthropic-messages", openai.FamilyAnthropic, `{"model":"route","max_tokens":32,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"file","file_id":"private-file"}}]}]}`},
		{"gemini-generation", openai.FamilyGemini, `{"contents":[{"parts":[{"fileData":{"mimeType":"text/plain","fileUri":"https://generativelanguage.googleapis.com/v1beta/files/private-file"}}]}]}`},
		{"gemini-generation", openai.FamilyGemini, `{"contents":[{"parts":[{"fileData":{"mimeType":"text/plain","fileUri":"https://GENERATIVELANGUAGE.googleapis.com./v1beta/files/private-file"}}]}]}`},
	} {
		t.Run(fixture.profile, func(t *testing.T) {
			compiled := template(t, configuration(t, fixture.profile))
			_, err := compiled.Bind(request(t, fixture.family, fixture.body), Context{})
			assertReason(t, err, "resource_affinity")
		})
	}
}

func TestNativeToolPayloadDoesNotBecomeResourceAuthority(t *testing.T) {
	compiled := template(t, configuration(t, "openai-chat"))
	source := request(t, openai.FamilyChat, `{"model":"route","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"caller_function","arguments":"{\"file_id\":\"caller-owned-value\"}"}}]},{"role":"tool","tool_call_id":"call-1","content":"done"},{"role":"user","content":"continue"}]}`)
	bind(t, compiled, source, Context{})
}
