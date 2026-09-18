package openai

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestResponsesTextUsesMessageEnvelopeWithoutLosingNativeExtensions(t *testing.T) {
	for _, family := range []Family{FamilyResponses, FamilyInputTokens} {
		r, err := Parse(family, []byte(`{"model":"route","input":"item_reference file_id file_private","vendor":{"opaque":"preserve"}}`))
		if err != nil {
			t.Fatal(err)
		}
		body, err := r.Encode("upstream", nil)
		if err != nil {
			t.Fatal(err)
		}
		var actual, expected any
		if err := json.Unmarshal(body, &actual); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(`{"model":"upstream","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"item_reference file_id file_private"}]}],"vendor":{"opaque":"preserve"}}`), &expected); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(actual, expected) {
			t.Fatalf("%s lost normalized meaning or extensions: %s", family, body)
		}
		if string(r.Field("input")) != `"item_reference file_id file_private"` {
			t.Fatal("upstream encoding mutated the admitted request")
		}
	}
}
