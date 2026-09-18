package openai

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestResponsesRejectAccountScopedInputReferences(t *testing.T) {
	for _, tc := range []struct {
		name, input, param string
	}{
		{"item", `[{"type":"item_reference","id":"msg_private"}]`, "input[0]"},
		{"implicit item", `[{"id":"msg_private"}]`, "input[0]"},
		{"null item type", `[{"type":null,"id":"msg_private"}]`, "input[0]"},
		{"ambiguous item", `[{"id":"msg_private","role":"user","content":"hi"}]`, "input[0]"},
		{"later item", `[{"role":"user","content":"hi"},{"type":"item_reference","id":"msg_private"}]`, "input[1]"},
		{"file", `[{"role":"user","content":[{"type":"input_text","text":"read this"},{"type":"input_file","file_id":"file_private"}]}]`, "input[0].content[1].file_id"},
		{"image", `[{"type":"message","role":"user","content":[{"type":"input_image","file_id":"file_private"}]}]`, "input[0].content[0].file_id"},
		{"function output", `[{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_file","file_id":"file_private"}]}]`, "input[0].output[0].file_id"},
		{"custom tool output", `[{"type":"custom_tool_call_output","call_id":"call_1","output":[{"type":"input_image","file_id":"file_private"}]}]`, "input[0].output[0].file_id"},
		{"computer output", `[{"type":"computer_call_output","call_id":"call_1","output":{"type":"computer_screenshot","file_id":"file_private"}}]`, "input[0].output.file_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(FamilyResponses, []byte(`{"model":"r","input":`+tc.input+`}`))
			var rejected *RequestError
			if !errors.As(err, &rejected) || rejected.Code != "unsupported_stateful_reference" || rejected.Param != tc.param {
				t.Fatalf("reference was not rejected at %s: %v", tc.param, err)
			}
		})
	}
}

// Scalar text containing reference-like words is covered by the exact message
// envelope assertion in TestResponsesTextUsesMessageEnvelopeWithoutLosingNativeExtensions.
func TestResponsesPreserveInlineInputsAndToolResults(t *testing.T) {
	for _, input := range []string{
		`[{"type":"message","id":"msg_inline","role":"assistant","content":[{"type":"output_text","text":"previous answer","annotations":[]}]},{"role":"user","content":"continue"}]`,
		`[{"role":"user","content":[{"type":"input_file","filename":"notes.txt","file_data":"data:text/plain;base64,aGk="},{"type":"input_file","file_url":"https://example.com/notes.pdf"},{"type":"input_image","image_url":"data:image/png;base64,aGk=","file_id":null}]}]`,
		`[{"type":"function_call","id":"fc_inline","call_id":"call_1","name":"read","arguments":"{}"},{"type":"function_call_output","id":"fco_inline","call_id":"call_1","output":"{\"type\":\"item_reference\",\"id\":\"data\"}"}]`,
		`[{"type":"custom_tool_call_output","call_id":"call_1","output":[{"type":"input_file","file_data":"aGk="}]}]`,
		`[{"type":"computer_call_output","call_id":"call_1","output":{"type":"computer_screenshot","image_url":"https://example.com/screen.png"}}]`,
		`[{"role":"user","content":"hi","vendor_extension":{"type":"input_file","file_id":"application-data"}}]`,
	} {
		request, err := Parse(FamilyResponses, []byte(`{"model":"r","input":`+input+`}`))
		if err != nil {
			t.Fatalf("inline input rejected: %s: %v", input, err)
		}
		encoded, err := request.Encode("upstream", nil)
		if err != nil {
			t.Fatal(err)
		}
		var want any
		var got map[string]any
		if err := json.Unmarshal([]byte(input), &want); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got["input"], want) {
			t.Fatalf("inline input changed: %s", encoded)
		}
	}
}
