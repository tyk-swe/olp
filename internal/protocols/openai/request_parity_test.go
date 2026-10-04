package openai

import (
	"encoding/json"
	"math/rand/v2"
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
)

// validateFields reads the conversation of a request from its parsed document
// when it has one and from the copies in fields when it has not, which is what a
// request with a provider's defaults merged into it is validated by. The two are
// held to the same answer for any envelope, the same error to the code, message
// and parameter or none, and the same members read off it.

type envelopeGen struct{ r *rand.Rand }

func (g envelopeGen) intn(n int) int { return g.r.IntN(n) }

func (g envelopeGen) pick(options ...any) any { return options[g.intn(len(options))] }

func (g envelopeGen) wild(depth int) any {
	switch g.intn(8) {
	case 0:
		return g.pick("text", "", "user", "message", "input_file", "item_reference", "line\n\"quoted\"")
	case 1:
		return g.pick(0, 1, 2.5, -1)
	case 2, 3:
		return nil
	case 4, 5:
		if depth <= 0 {
			return true
		}
		items := make([]any, g.intn(3))
		for i := range items {
			items[i] = g.wild(depth - 1)
		}
		return items
	}
	if depth <= 0 {
		return false
	}
	out := map[string]any{}
	for range g.intn(4) {
		out[g.pick("type", "role", "content", "id", "file_id", "output", "text").(string)] = g.wild(depth - 1)
	}
	return out
}

func (g envelopeGen) part() any {
	if g.intn(6) == 0 {
		return g.wild(2)
	}
	part := map[string]any{"type": g.pick("text", "input_text", "input_file", "input_image", "computer_screenshot", "image_url", "output_text")}
	if g.intn(2) == 0 {
		part["file_id"] = g.pick("file_1", nil, 3, "")
	}
	if g.intn(2) == 0 {
		part["text"] = g.pick("hello", "line\n\"two\"")
	}
	return part
}

func (g envelopeGen) parts() any {
	switch g.intn(8) {
	case 0:
		return g.pick("plain text", "")
	case 1:
		return g.wild(2)
	case 2:
		return g.part()
	}
	parts := make([]any, g.intn(4))
	for i := range parts {
		parts[i] = g.part()
	}
	return parts
}

func (g envelopeGen) message() any {
	if g.intn(8) == 0 {
		return g.wild(2)
	}
	message := map[string]any{}
	if g.intn(8) != 0 {
		message["role"] = g.pick("user", "assistant", "", nil, 7, "system")
	}
	if g.intn(8) != 0 {
		message["content"] = g.parts()
	}
	return message
}

func (g envelopeGen) item() any {
	if g.intn(8) == 0 {
		return g.wild(2)
	}
	item := map[string]any{}
	if g.intn(4) != 0 {
		item["type"] = g.pick("message", "item_reference", "function_call_output", "custom_tool_call_output", "computer_call_output", "function_call", nil, 3, "")
	}
	if g.intn(4) == 0 {
		item["id"] = g.pick("msg_1", nil, "")
	}
	if g.intn(2) == 0 {
		item["content"] = g.parts()
	}
	if g.intn(2) == 0 {
		item["output"] = g.parts()
	}
	if g.intn(4) == 0 {
		item["file_id"] = g.pick("file_1", nil)
		item["type"] = g.pick("input_file", "input_image", "computer_screenshot", "message")
	}
	return item
}

func (g envelopeGen) conversation(item func() any) any {
	switch g.intn(10) {
	case 0:
		return g.pick("plain prompt", "", nil, 3)
	case 1:
		return g.wild(2)
	case 2:
		return []any{}
	}
	items := make([]any, 1+g.intn(4))
	for i := range items {
		items[i] = item()
	}
	return items
}

func (g envelopeGen) fields() map[string]json.RawMessage {
	raw := map[string]any{}
	if g.intn(12) != 0 {
		raw["model"] = g.pick("route", "Not A Slug", nil, 3)
	}
	if g.intn(2) == 0 {
		raw["messages"] = g.conversation(g.message)
	}
	if g.intn(2) == 0 {
		raw["input"] = g.conversation(g.item)
	}
	if g.intn(6) == 0 {
		raw["stream"] = g.pick(true, false, "yes", nil)
	}
	if g.intn(6) == 0 {
		raw["max_tokens"] = g.pick(5, -1, "x", nil)
	}
	if g.intn(8) == 0 {
		raw["max_completion_tokens"] = g.pick(5, nil)
	}
	if g.intn(8) == 0 {
		raw["previous_response_id"] = g.pick("resp_1", 4)
	}
	if g.intn(8) == 0 {
		raw["tools"] = g.pick([]any{map[string]any{}}, []any{1}, "x", nil)
	}
	fields := make(map[string]json.RawMessage, len(raw))
	for name, v := range raw {
		fields[name], _ = json.Marshal(v)
	}
	return fields
}

// tameFields is an envelope a client could send, that has at most one thing wrong
// with it, in the place the validation reads from the document.
func (g envelopeGen) tameFields() map[string]json.RawMessage {
	text := func() any { return g.pick("hello", "line\n\"two\"", "") }
	messages := make([]any, 1+g.intn(3))
	for i := range messages {
		messages[i] = map[string]any{"role": g.pick("user", "assistant", "system"), "content": g.pick("hi", []any{map[string]any{"type": "text", "text": text()}})}
	}
	items := make([]any, 1+g.intn(3))
	for i := range items {
		item := map[string]any{"type": g.pick("message", "function_call_output", "computer_call_output", "function_call"), "content": []any{map[string]any{"type": g.pick("input_text", "input_image"), "image_url": "https://example.test/a.png"}}}
		if item["type"] != "message" {
			item["output"] = g.pick("done", []any{map[string]any{"type": "input_file", "file_data": "data:text/plain;base64,YQ=="}})
		}
		items[i] = item
	}
	raw := map[string]any{"model": "route", "messages": messages, "input": g.pick("prompt", items)}
	// The members the conversation is not read from are read as they were, and
	// each is right or wrong.
	for name, values := range map[string][]any{
		"max_tokens": {5, -1, "x", nil}, "max_completion_tokens": {5, nil}, "max_output_tokens": {5, 0}, "temperature": {0.5, "hot", nil},
		"seed": {1, "x"}, "n": {1, 0}, "stream": {true, false, "yes"}, "top_logprobs": {3, 30},
		"stream_options": {map[string]any{"include_usage": true}, "x", map[string]any{"include_usage": "y"}},
		"tools":          {[]any{map[string]any{"type": "function"}}, []any{1}, "x"}, "response_format": {map[string]any{"type": "text"}, 3},
		"previous_response_id": {"resp_1", 4}, "instructions": {"be brief", 4}, "background": {true, "x"},
	} {
		if g.intn(5) == 0 {
			raw[name] = values[g.intn(len(values))]
		}
	}
	switch g.intn(6) {
	case 0:
		raw["messages"] = append(messages, g.message())
	case 1:
		raw["input"] = append(items, g.item())
	case 2:
		raw["input"] = items
		items[g.intn(len(items))].(map[string]any)["file_id"] = g.pick("file_1", nil)
		items[0].(map[string]any)["type"] = g.pick("input_file", "message")
	case 3:
		raw["input"] = items
		items[0].(map[string]any)["content"] = []any{g.part()}
	}
	fields := make(map[string]json.RawMessage, len(raw))
	for name, v := range raw {
		fields[name], _ = json.Marshal(v)
	}
	return fields
}

// legacyParseDocument is parseDocument as it validated an envelope when all of
// its fields, the conversation among them, were copied out of the document as
// json.RawMessage and read from the copies.
func legacyParseDocument(family Family, doc oif.Document) (*Request, error) {
	fields := doc.Fields()
	if fields == nil {
		return nil, &RequestError{Code: "invalid_json", Message: "The request body must be one JSON object."}
	}
	r := NewSourceEnvelope(family, "", false, doc)
	if err := r.validateFields(fields, oif.Value{}); err != nil {
		return nil, err
	}
	r.source = r.source.WithDescriptor(Descriptor(family, r.Stream))
	r.validatedDocument, r.validatedFamily = doc, family
	return r, nil
}

// sameValidation fails the test if the two readings of one envelope differ, and
// says whether it was accepted.
func sameValidation(t testing.TB, family Family, doc oif.Document) bool {
	t.Helper()
	got, gotErr := parseDocument(family, doc)
	want, wantErr := legacyParseDocument(family, doc)
	if !reflect.DeepEqual(gotErr, wantErr) || (got == nil) != (want == nil) {
		t.Fatalf("%s\nrequest %.1500s\ndocument: %v\nfields:   %v", family, doc.Raw(), gotErr, wantErr)
	}
	if got != nil && (got.Family != want.Family || got.Route != want.Route || got.Stream != want.Stream || got.IncludeUsage != want.IncludeUsage || got.validatedDocument != doc) {
		t.Fatalf("%s\nrequest %.1500s\ndocument: %+v\nfields:   %+v", family, doc.Raw(), got, want)
	}
	return got != nil
}

func TestTheConversationIsValidatedAsTheFieldsAreValidated(t *testing.T) {
	cases := 600
	if testing.Short() {
		cases = 100
	}
	var accepted, refused int
	for _, family := range []Family{FamilyChat, FamilyResponses, FamilyInputTokens, FamilyEmbeddings, FamilyModeration} {
		g := envelopeGen{rand.New(rand.NewPCG(53, uint64(len(family))))}
		for range cases {
			fields := g.fields()
			if g.intn(2) == 0 {
				fields = g.tameFields()
			}
			encoded, _ := json.Marshal(fields)
			doc, err := oif.ParseJSON(encoded, oif.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			if sameValidation(t, family, doc) {
				accepted++
			} else {
				refused++
			}
		}
	}
	// The comparison is only worth something if both answers are given.
	if accepted < cases/2 || refused < cases {
		t.Errorf("%d envelopes were accepted and %d refused: the generator is not exercising the validation", accepted, refused)
	}
}

func FuzzTheConversationIsValidatedAsTheFieldsAreValidated(f *testing.F) {
	f.Add([]byte(`{"model":"route","messages":[{"role":"user","content":"hi"},{"role":"assistant"}]}`))
	f.Add([]byte(`{"model":"route","messages":[{"role":"user"},[]]}`))
	f.Add([]byte(`{"model":"route","input":[{"type":"message","content":[{"type":"input_file","file_id":"f"}]},"x"],"stream":true}`))
	f.Add([]byte(`{"model":"route","input":[{"type":"computer_call_output","output":{"type":"computer_screenshot","file_id":null}},{"id":"i"}]}`))
	f.Add([]byte(`{"model":"route","input":"text","max_output_tokens":5,"tools":[{}]}`))
	f.Add([]byte(`{"model":"route","messages":[{"role":"user","content":"x"}],"stream":true,"stream_options":{"include_usage":true}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		doc, err := oif.ParseJSON(data, oif.Limits{MaxBytes: 16 << 10, MaxNodes: 1024, MaxDepth: 32})
		if err != nil {
			return
		}
		for _, family := range []Family{FamilyChat, FamilyResponses, FamilyInputTokens, FamilyEmbeddings} {
			sameValidation(t, family, doc)
		}
	})
}
