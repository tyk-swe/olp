package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEncodeValidatesMergedDefaults(t *testing.T) {
	for _, tc := range []struct {
		family   Family
		defaults string
	}{
		{FamilyResponses, `{"previous_response_id":"resp_other_account"}`},
		{FamilyResponses, `{"conversation":"conv_other_account"}`},
		{FamilyResponses, `{"background":true}`},
		{FamilyResponses, `{"max_output_tokens":"10"}`},
		{FamilyChat, `{"max_tokens":1,"max_completion_tokens":2}`},
		{FamilyChat, `{"stream_options":{"include_usage":"true"}}`},
		{FamilyChat, `{"tools":[null]}`},
	} {
		t.Run(string(tc.family)+tc.defaults, func(t *testing.T) {
			input := `"messages":[{"role":"user","content":"hi"}],"stream":true`
			if tc.family == FamilyResponses {
				input = `"input":"hi"`
			}
			r, err := Parse(tc.family, []byte(`{"model":"r",`+input+`}`))
			if err != nil {
				t.Fatal(err)
			}
			var defaults map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tc.defaults), &defaults); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Encode("org/upstream:model", defaults); err == nil {
				t.Fatal("invalid merged request reached the upstream encoder")
			}
		})
	}
	r, err := Parse(FamilyResponses, []byte(`{"model":"r","input":"hi","temperature":0.7}`))
	if err != nil {
		t.Fatal(err)
	}
	body, err := r.Encode("org/upstream:model", map[string]json.RawMessage{
		"temperature": json.RawMessage(`0.1`), "vendor_extension": json.RawMessage(`{"keep":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["model"]) != `"org/upstream:model"` || string(fields["temperature"]) != "0.7" || string(fields["vendor_extension"]) != `{"keep":true}` {
		t.Fatalf("valid fields changed: %s", body)
	}
}

func TestUnaryResponsesRequireTerminalSuccess(t *testing.T) {
	for _, status := range []string{`"failed"`, `"queued"`, `"in_progress"`, `"cancelled"`, `null`, `{}`, `""`} {
		if _, err := DecodeResponse([]byte(`{"output":[],"status":`+status+`}`), "r"); err == nil {
			t.Errorf("accepted status %s", status)
		}
	}
	if _, err := DecodeResponse([]byte(`{"output":[],"status":"completed","error":{}}`), "r"); err == nil {
		t.Error("accepted a non-null error without a message")
	}
	for _, status := range []string{"completed", "incomplete"} {
		if _, err := DecodeResponse([]byte(`{"output":[],"status":"`+status+`"}`), "r"); err != nil {
			t.Errorf("rejected terminal %s: %v", status, err)
		}
	}
}

func TestResponsesStreamRejectsInvalidEventsBeforeCommit(t *testing.T) {
	for _, event := range []string{
		`{"type":"response.output_text.delta\ndata: [DONE]"}`,
		`{"type":"response.output_text.delta\rdata: [DONE]"}`,
		`{"type":"response.output_text.delta\u0000"}`,
		`{"type":"response.completed"}`,
		`{"type":"response.completed","response":null}`,
		`{"type":"response.completed","response":{"output":[],"status":"in_progress"}}`,
		`{"type":"response.completed","response":{"output":[],"status":"incomplete"}}`,
		`{"type":"response.completed","response":{"output":[],"error":{}}}`,
	} {
		emitted := 0
		_, err := Stream(FamilyResponses, strings.NewReader("data: "+event+"\n\n"), 4096, "r", true, func([]byte) error { emitted++; return nil })
		if err == nil || emitted != 0 {
			t.Errorf("invalid event committed: %s: frames=%d err=%v", event, emitted, err)
		}
	}
}
