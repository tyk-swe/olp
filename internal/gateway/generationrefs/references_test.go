package generationrefs_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/generationrefs"
	"github.com/tyk-swe/olp/internal/resources"
)

func document(t *testing.T, raw string) oif.Document {
	t.Helper()
	parsed, err := oif.ParseJSON([]byte(raw), oif.Limits{MaxBytes: 1 << 16})
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	return parsed
}

func named(refs []generationrefs.Reference) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, ref.Pointer+" "+ref.Kind+" "+ref.ID)
	}
	return out
}

func owned(kind, upstream string) (string, error) {
	if !strings.HasPrefix(upstream, "provider-") {
		return "", resources.ErrNotFound
	}
	return kind + ":" + strings.TrimPrefix(upstream, "provider-"), nil
}

func same(t *testing.T, got []byte, want string) {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("result %s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatalf("expectation %s: %v", want, err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestARequestNamesItsProviderResourcesOnlyAtTheDocumentedPositions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		family wire.Format
		body   string
		want   []string
	}{
		{"a chat request names none", wire.Chat, `{"conversation":"c1","messages":[{"role":"user","content":[{"type":"input_file","file_id":"f1"}]}]}`, []string{}},
		{"a conversation by name", wire.Responses, `{"conversation":"c1"}`, []string{"/conversation conversation c1"}},
		{"a conversation by object", wire.Responses, `{"conversation":{"id":"c1"}}`, []string{"/conversation/id conversation c1"}},
		{"no conversation", wire.Responses, `{"conversation":null,"input":"hello"}`, []string{}},
		{"files in a message", wire.Responses, `{"input":[{"role":"user","content":[{"type":"input_text","text":"file_id f0"},{"type":"input_file","file_id":"f1"},{"type":"input_image","file_id":"f2"}]}]}`,
			[]string{"/input/0/content/1/file_id file f1", "/input/0/content/2/file_id file f2"}},
		{"a file as a typed message", wire.Responses, `{"input":[{"type":"message","content":[{"type":"input_file","file_id":"f1"}]}]}`, []string{"/input/0/content/0/file_id file f1"}},
		{"a file part without a file", wire.Responses, `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`, []string{}},
		{"a file in a tool result", wire.Responses, `{"input":[{"type":"function_call_output","call_id":"1","output":[{"type":"input_file","file_id":"f1"}]},{"type":"computer_call_output","call_id":"2","output":{"type":"computer_screenshot","file_id":"f2"}},{"type":"custom_tool_call_output","call_id":"3","output":"file_id f3"}]}`,
			[]string{"/input/0/output/0/file_id file f1", "/input/1/output/file_id file f2"}},
		{"a file an unrelated item only mentions", wire.Responses, `{"input":[{"type":"function_call","call_id":"1","name":"read","arguments":"{\"file_id\":\"f1\"}","content":[{"type":"input_file","file_id":"f1"}]}]}`, []string{}},
		{"searched stores", wire.Responses, `{"tools":[{"type":"function","name":"read"},{"type":"file_search","vector_store_ids":["v1","v2"]}]}`,
			[]string{"/tools/1/vector_store_ids/0 vector_store v1", "/tools/1/vector_store_ids/1 vector_store v2"}},
		{"a context cache", wire.Gemini, `{"cachedContent":"cachedContents/c1","contents":[{"parts":[{"text":"hello"}]}]}`, []string{"/cachedContent context_cache cachedContents/c1"}},
		{"a context cache on a stream", wire.GeminiStream, `{"cachedContent":"c1"}`, []string{"/cachedContent context_cache c1"}},
		{"a file the caller hosts", wire.Gemini, `{"contents":[{"parts":[{"fileData":{"fileUri":"https://example.com/a.png"}}]}]}`, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refs, err := generationrefs.InRequest(document(t, tc.body), tc.family)
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if got := named(refs); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestARequestIsRefusedForReferencesNoOwnershipContractCovers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		family wire.Format
		body   string
	}{
		{"a conversation without an identifier", wire.Responses, `{"conversation":{}}`},
		{"a conversation that is not a name", wire.Responses, `{"conversation":7}`},
		{"an empty file identifier", wire.Responses, `{"input":[{"role":"user","content":[{"type":"input_file","file_id":""}]}]}`},
		{"a file identifier that is not text", wire.Responses, `{"input":[{"role":"user","content":[{"type":"input_file","file_id":7}]}]}`},
		{"an item reference", wire.Responses, `{"input":[{"type":"item_reference","id":"i1"}]}`},
		{"an untyped item with an identifier", wire.Responses, `{"input":[{"id":"i1"}]}`},
		{"a container call in the input", wire.Responses, `{"input":[{"type":"code_interpreter_call","id":"x"}]}`},
		{"a container tool", wire.Responses, `{"tools":[{"type":"code_interpreter"}]}`},
		{"a file search without stores", wire.Responses, `{"tools":[{"type":"file_search"}]}`},
		{"a file search with no store named", wire.Responses, `{"tools":[{"type":"file_search","vector_store_ids":[]}]}`},
		{"a store that is not a name", wire.Responses, `{"tools":[{"type":"file_search","vector_store_ids":[7]}]}`},
		{"a provider file by name", wire.Gemini, `{"contents":[{"parts":[{"fileData":{"fileUri":"files/abc"}}]}]}`},
		{"a provider file by rooted name", wire.Gemini, `{"contents":[{"parts":[{"fileData":{"fileUri":"/files/abc"}}]}]}`},
		{"a provider file by address", wire.Gemini, `{"contents":[{"parts":[{"fileData":{"fileUri":"https://GenerativeLanguage.googleapis.com./v1beta/files/abc"}}]}]}`},
		{"a provider bucket", wire.Gemini, `{"contents":[{"parts":[{"fileData":{"fileUri":"GS://bucket/a"}}]}]}`},
		{"another provider bucket", wire.Gemini, `{"contents":[{"parts":[{"fileData":{"fileUri":"s3://bucket/a"}}]}]}`},
		{"a provider file in the system instruction", wire.Gemini, `{"systemInstruction":{"parts":[{"fileData":{"fileUri":"files/abc"}}]}}`},
		{"a context cache that is not a name", wire.Gemini, `{"cachedContent":{"name":"c1"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if refs, err := generationrefs.InRequest(document(t, tc.body), tc.family); err == nil {
				t.Fatalf("accepted with %q", named(refs))
			}
		})
	}
}

func TestAResultShowsTheCallersOwnIdentifiersWhereverTheProviderNamedAResource(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"a whole response",
			`{"id":"provider-r","conversation":"provider-c","tools":[{"type":"function","name":"provider-x"},{"type":"file_search","vector_store_ids":["provider-v"]}],"output":[{"type":"file_search_call","results":[{"file_id":"provider-f","vector_store_id":"provider-v","text":"provider-t"}]},{"type":"message","content":[{"type":"output_text","text":"provider-t","annotations":[{"type":"file_citation","file_id":"provider-f"},{"type":"url_citation","file_id":"provider-u"}]}]},{"type":"function_call","arguments":"{\"file_id\":\"provider-a\"}"}]}`,
			`{"id":"provider-r","conversation":"conversation:c","tools":[{"type":"function","name":"provider-x"},{"type":"file_search","vector_store_ids":["vector_store:v"]}],"output":[{"type":"file_search_call","results":[{"file_id":"file:f","vector_store_id":"vector_store:v","text":"provider-t"}]},{"type":"message","content":[{"type":"output_text","text":"provider-t","annotations":[{"type":"file_citation","file_id":"file:f"},{"type":"url_citation","file_id":"provider-u"}]}]},{"type":"function_call","arguments":"{\"file_id\":\"provider-a\"}"}]}`},
		{"a conversation object", `{"conversation":{"id":"provider-c"}}`, `{"conversation":{"id":"conversation:c"}}`},
		{"a stream event carrying the response", `{"type":"response.completed","response":{"conversation":"provider-c","output":[]}}`, `{"type":"response.completed","response":{"conversation":"conversation:c","output":[]}}`},
		{"a stream event carrying an item", `{"type":"response.output_item.done","item":{"type":"file_search_call","results":[{"file_id":"provider-f"}]}}`, `{"type":"response.output_item.done","item":{"type":"file_search_call","results":[{"file_id":"file:f"}]}}`},
		{"a stream event carrying a part", `{"type":"response.content_part.done","part":{"type":"output_text","annotations":[{"type":"file_citation","file_id":"provider-f"}]}}`, `{"type":"response.content_part.done","part":{"type":"output_text","annotations":[{"type":"file_citation","file_id":"file:f"}]}}`},
		{"a stream event carrying an annotation", `{"type":"response.output_text.annotation.added","annotation":{"type":"file_citation","file_id":"provider-f"}}`, `{"type":"response.output_text.annotation.added","annotation":{"type":"file_citation","file_id":"file:f"}}`},
		{"nothing named", `{"output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],"conversation":null}`, `{"output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],"conversation":null}`},
		{"a citation without a file", `{"annotation":{"type":"file_citation","file_id":null}}`, `{"annotation":{"type":"file_citation","file_id":null}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projected, err := generationrefs.ProjectResult(document(t, tc.body), owned)
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			same(t, projected.Bytes(), tc.want)
		})
	}
}

func TestAResultIsRefusedWhenItNamesAResourceTheCallerDoesNotOwnOrAContainer(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       error
	}{
		{"an unknown conversation", `{"conversation":"elsewhere"}`, resources.ErrNotFound},
		{"an unknown searched store", `{"tools":[{"type":"file_search","vector_store_ids":["elsewhere"]}]}`, resources.ErrNotFound},
		{"an unknown cited file", `{"output":[{"type":"message","content":[{"annotations":[{"type":"file_citation","file_id":"elsewhere"}]}]}]}`, resources.ErrNotFound},
		{"an unknown search result store", `{"output":[{"type":"file_search_call","results":[{"file_id":"provider-f","vector_store_id":"elsewhere"}]}]}`, resources.ErrNotFound},
		{"a conversation that is not a name", `{"conversation":7}`, resources.ErrContract},
		{"an empty cited file", `{"annotation":{"type":"file_citation","file_id":""}}`, resources.ErrContract},
		{"a container call", `{"item":{"type":"code_interpreter_call"}}`, resources.ErrContract},
		{"a container citation", `{"part":{"annotations":[{"type":"container_file_citation","file_id":"provider-f"}]}}`, resources.ErrContract},
		{"a container call inside the response of an event", `{"response":{"output":[{"type":"code_interpreter_call"}]}}`, resources.ErrContract},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := generationrefs.ProjectResult(document(t, tc.body), owned); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestAResourceResolvedToNoIdentifierIsTreatedAsNotOwned(t *testing.T) {
	unnamed := func(string, string) (string, error) { return "", nil }
	if _, err := generationrefs.ProjectResult(document(t, `{"conversation":"provider-c"}`), unnamed); !errors.Is(err, resources.ErrNotFound) {
		t.Fatalf("result: got %v", err)
	}
	items := document(t, `{"data":[{"type":"message","content":[{"type":"input_file","file_id":"provider-f"}]}]}`)
	refs, err := generationrefs.InInputItems(items)
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if _, err := generationrefs.ProjectInputItems(items, refs, unnamed); !errors.Is(err, resources.ErrNotFound) {
		t.Fatalf("input items: got %v", err)
	}
}

func TestStoredInputItemsShowTheCallersOwnFileIdentifiers(t *testing.T) {
	items := document(t, `{"object":"list","data":[{"type":"message","content":[{"type":"input_text","text":"provider-t"},{"type":"input_file","file_id":"provider-f"}]},{"type":"function_call_output","output":[{"type":"input_image","file_id":"provider-g"}]}]}`)
	refs, err := generationrefs.InInputItems(items)
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	projected, err := generationrefs.ProjectInputItems(items, refs, owned)
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	same(t, projected.Bytes(), `{"object":"list","data":[{"type":"message","content":[{"type":"input_text","text":"provider-t"},{"type":"input_file","file_id":"file:f"}]},{"type":"function_call_output","output":[{"type":"input_image","file_id":"file:g"}]}]}`)

	foreign := document(t, `{"data":[{"type":"message","content":[{"type":"input_file","file_id":"elsewhere"}]}]}`)
	refs, err = generationrefs.InInputItems(foreign)
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if _, err := generationrefs.ProjectInputItems(foreign, refs, owned); !errors.Is(err, resources.ErrNotFound) {
		t.Fatalf("a file the caller does not own: got %v", err)
	}
	for name, body := range map[string]string{"a list without items": `{"object":"list"}`, "items that are not a list": `{"data":{}}`, "a reply that is not an object": `[]`} {
		if _, err := generationrefs.InInputItems(document(t, body)); !errors.Is(err, resources.ErrContract) {
			t.Fatalf("%s: got %v", name, err)
		}
	}
	if _, err := generationrefs.InInputItems(document(t, `{"data":[{"type":"item_reference","id":"i1"}]}`)); err == nil {
		t.Fatal("an item reference in stored input was accepted")
	}
}
