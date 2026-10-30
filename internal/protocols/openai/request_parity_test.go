package openai

import (
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/oif"
)

// validateFields reads the conversation of a request from its parsed document
// when it has one and from the copies in fields when it has not, which is what a
// request with a provider's defaults merged into it is validated by. The two are
// held to the same answer for any envelope, the same error to the code, message
// and parameter or none, and the same members read off it.

// legacyParseDocument is parseDocument as it validated an envelope when all of
// its fields, the conversation among them, were copied out of the document as
// json.RawMessage and read from the copies.
func legacyParseDocument(family wire.Format, doc oif.Document) (*wire.Request, error) {
	fields := doc.Fields()
	if fields == nil {
		return nil, &wire.RequestError{Code: "invalid_json", Message: "The request body must be one JSON object."}
	}
	r, err := wire.NewRequest(family, "", false, doc)
	if err != nil {
		return nil, err
	}
	if err := validateFields(r, fields, oif.Value{}, ""); err != nil {
		return nil, err
	}
	r = r.WithValidation("openai")
	return r, nil
}

// sameValidation fails the test if the two readings of one envelope differ, and
// says whether it was accepted.
func sameValidation(t testing.TB, family wire.Format, doc oif.Document) bool {
	t.Helper()
	got, gotErr := parseDocument(family, doc, "")
	want, wantErr := legacyParseDocument(family, doc)
	if !reflect.DeepEqual(gotErr, wantErr) || (got == nil) != (want == nil) {
		t.Fatalf("%s\nrequest %.1500s\ndocument: %v\nfields:   %v", family, doc.Raw(), gotErr, wantErr)
	}
	if got != nil && (got.Family != want.Family || got.Route != want.Route || got.Stream != want.Stream || got.IncludeUsage != want.IncludeUsage || !got.ValidatedBy("openai")) {
		t.Fatalf("%s\nrequest %.1500s\ndocument: %+v\nfields:   %+v", family, doc.Raw(), got, want)
	}
	return got != nil
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
		for _, family := range []wire.Format{wire.Chat, wire.Responses, wire.InputTokens, wire.Embeddings} {
			sameValidation(t, family, doc)
		}
	})
}
