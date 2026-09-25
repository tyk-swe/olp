package interaction

import (
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
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
			_, err := compiled.BindRequest(request(t, fixture.family, fixture.body), Context{})
			assertReason(t, err, "resource_affinity")
		})
	}
}

func TestNativeToolPayloadDoesNotBecomeResourceAuthority(t *testing.T) {
	compiled := template(t, configuration(t, "openai-chat"))
	source := request(t, openai.FamilyChat, `{"model":"route","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"caller_function","arguments":"{\"file_id\":\"caller-owned-value\"}"}}]},{"role":"tool","tool_call_id":"call-1","content":"done"},{"role":"user","content":"continue"}]}`)
	bind(t, compiled, source, Context{})
}

const managedFileID = "strict_file_0123456789abcdef0123456789abcdef"

func managedBinding(t *testing.T, compiled *Template, nativeID string) AssetBinding {
	t.Helper()
	return AssetBinding{
		LocalID: managedFileID, Kind: "strict_file", NativeID: nativeID, Purpose: "user_data",
		Serving: compiled.Serving(), Digest: "sha256:51", MediaType: "text/plain", Size: 51,
	}
}

// Verified bindings rewrite only the dialect-owned file positions the caller
// named. The provider sees the committed native identity; the caller's local
// ID never leaves the gateway and the source document stays untouched.
func TestResolvedFileBindingAdmitsManagedPositions(t *testing.T) {
	compiled := template(t, configuration(t, "openai-responses"))
	binding := managedBinding(t, compiled, "file-upstream-9")
	source := request(t, openai.FamilyResponses, `{"model":"route","store":false,"input":[`+
		`{"type":"message","role":"user","content":[{"type":"input_file","file_id":"`+managedFileID+`"}]},`+
		`{"type":"input_file","file_id":"`+managedFileID+`","filename":"notes.txt"},`+
		`{"type":"input_image","file_id":"`+managedFileID+`"},`+
		`{"type":"computer_call_output","call_id":"call-1","output":{"type":"computer_screenshot","file_id":"`+managedFileID+`"}},`+
		`{"type":"function_call_output","call_id":"call-2","output":[{"type":"input_file","file_id":"`+managedFileID+`"}]}`+
		`]}`)
	plan := bind(t, compiled, source, Context{ResolvedAssets: map[string]AssetBinding{managedFileID: binding}})
	effective := string(plan.Body())
	if strings.Contains(effective, managedFileID) {
		t.Fatalf("local file identity reached the provider body: %s", effective)
	}
	if count := strings.Count(effective, `"file_id":"file-upstream-9"`); count != 5 {
		t.Fatalf("resolved native identity bound at %d positions, want 5: %s", count, effective)
	}
	assets := plan.Assets()
	if len(assets) != 1 || assets[0].LocalID != managedFileID || assets[0].NativeID != "file-upstream-9" {
		t.Fatalf("plan assets: %+v", assets)
	}
	descriptor := plan.Prepared().Descriptor()
	if len(descriptor.Resources) != 1 || descriptor.Resources[0] != (oif.Resource{ID: managedFileID, Kind: "strict_file", Relation: "file_input"}) {
		t.Fatalf("descriptor resources: %+v", descriptor.Resources)
	}
	if len(descriptor.Assets) != 1 || descriptor.Assets[0].Digest != "sha256:51" || descriptor.Assets[0].Size != 51 {
		t.Fatalf("descriptor assets: %+v", descriptor.Assets)
	}
	bound := 0
	for _, disposition := range plan.Receipt().Dispositions {
		if disposition.Rule == "resolved_resource_binding" && disposition.Disposition == "bound" {
			bound++
		}
	}
	if bound != 5 {
		t.Fatalf("bound dispositions: %d", bound)
	}
	if source := string(source.OIF().Document().Bytes()); !strings.Contains(source, managedFileID) || strings.Contains(source, "file-upstream-9") {
		t.Fatalf("caller source mutated: %s", source)
	}
}

// Every other provider asset shape keeps the historical refusal: missing or
// mismatched bindings, non-admitted positions, and foreign dialects.
func TestResolvedFileBindingRefusals(t *testing.T) {
	compiled := template(t, configuration(t, "openai-responses"))
	for _, fixture := range []struct {
		name     string
		body     string
		bindings map[string]AssetBinding
	}{
		{"no binding", `{"model":"route","store":false,"input":[{"type":"message","role":"user","content":[{"type":"input_file","file_id":"` + managedFileID + `"}]}]}`, nil},
		{"wrong purpose", `{"model":"route","store":false,"input":[{"type":"input_file","file_id":"` + managedFileID + `"}]}`, map[string]AssetBinding{
			managedFileID: {LocalID: managedFileID, Kind: "strict_file", NativeID: "file-upstream-9", Purpose: "batch", Serving: compiled.Serving()},
		}},
		{"different serving", `{"model":"route","store":false,"input":[{"type":"input_file","file_id":"` + managedFileID + `"}]}`, map[string]AssetBinding{
			managedFileID: {LocalID: managedFileID, Kind: "strict_file", NativeID: "file-upstream-9", Purpose: "user_data", Serving: ServingIdentity{ProviderID: "other"}},
		}},
		{"empty native identity", `{"model":"route","store":false,"input":[{"type":"input_file","file_id":"` + managedFileID + `"}]}`, map[string]AssetBinding{
			managedFileID: {LocalID: managedFileID, Kind: "strict_file", Purpose: "user_data", Serving: compiled.Serving()},
		}},
		{"unqualified part kind", `{"model":"route","store":false,"input":[{"type":"message","role":"user","content":[{"type":"file","file_id":"` + managedFileID + `"}]}]}`, map[string]AssetBinding{
			managedFileID: managedBinding(t, compiled, "file-upstream-9"),
		}},
		{"non-string reference", `{"model":"route","store":false,"input":[{"type":"input_file","file_id":7}]}`, nil},
		{"foreign provider identity", `{"model":"route","store":false,"input":[{"type":"input_file","file_id":"file-provider-owned"}]}`, nil},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			document, err := oif.ParseJSON([]byte(fixture.body), oif.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			source := openai.NewSourceEnvelope(openai.FamilyResponses, "route", false, document)
			_, bindErr := compiled.BindRequest(source, Context{ResolvedAssets: fixture.bindings})
			assertReason(t, bindErr, "resource_affinity")
		})
	}
}

// A binding never widens into a dialect whose file contract is not the
// reviewed Responses contract, and never rewrites the caller's tool payloads.
func TestResolvedFileBindingStaysDialectOwned(t *testing.T) {
	chat := template(t, configuration(t, "openai-chat"))
	binding := AssetBinding{LocalID: managedFileID, Kind: "strict_file", NativeID: "file-upstream-9", Purpose: "user_data", Serving: chat.Serving()}
	document, err := oif.ParseJSON([]byte(`{"model":"route","messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"`+managedFileID+`"}}]}]}`), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	source := openai.NewSourceEnvelope(openai.FamilyChat, "route", false, document)
	if _, err := chat.BindRequest(source, Context{ResolvedAssets: map[string]AssetBinding{managedFileID: binding}}); err == nil {
		t.Fatal("chat-wire file position accepted a resolved binding")
	}
}

// FileReferences surfaces only caller strings at dialect-owned positions for
// the gateway's resource-authority scan; opaque payload members stay private.
func TestFileReferencesVisitsOnlyDialectPositions(t *testing.T) {
	document, err := oif.ParseJSON([]byte(`{"model":"route","input":[`+
		`{"type":"input_file","file_id":"strict_file_a"},`+
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"file_id is plain text here"},{"type":"input_image","file_id":"strict_file_b"}]},`+
		`{"type":"function_call","call_id":"c","name":"f","arguments":"{\"file_id\":\"payload-only\"}"},`+
		`{"type":"mystery","file_id":"not-a-position"}`+
		`]}`), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	responses, ok := protocols.DialectForFamily(openai.FamilyResponses)
	if !ok {
		t.Fatal("responses dialect not registered")
	}
	refs := FileReferences(document, responses)
	if len(refs) != 2 || refs[0] != "strict_file_a" || refs[1] != "strict_file_b" {
		t.Fatalf("file references: %v", refs)
	}
	// The chat walk treats input items as message-shaped content carriers:
	// item-level file_id is not a chat position, but a nested input_image
	// part still names a dialect-owned file position.
	chat, ok := protocols.DialectForFamily(openai.FamilyChat)
	if !ok {
		t.Fatal("chat dialect not registered")
	}
	if refs := FileReferences(document, chat); len(refs) != 1 || refs[0] != "strict_file_b" {
		t.Fatalf("chat references: %v", refs)
	}
}
