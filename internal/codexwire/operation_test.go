package codexwire

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/oif"
)

func TestClassifyWithCodexIdentityMatchesClassify(t *testing.T) {
	for _, test := range []struct {
		body      string
		headers   http.Header
		path      string
		websocket bool
	}{
		{`{"model":"gpt-5.4","previous_response_id":"resp_1","max_output_tokens":64}`, http.Header{"Thread-Id": {"root"}}, "responses", false},
		{`{"model":"gpt-5.4","input":[{"type":"compaction_trigger"}]}`, http.Header{"Session_id": {"root"}}, "responses", false},
		{`{"model":"gpt-5.4"}`, http.Header{"Session_id": {"root"}}, "responses/compact", false},
		{`{"type":"response.create","model":"gpt-5.4","generate":false}`, http.Header{"Session_id": {"root"}}, "responses", true},
		{`{"model":"gpt-5.4","conversation":"remote"}`, http.Header{"Thread-Id": {"root"}}, "responses", false},
		{`{"model":"bad model"}`, http.Header{"Thread-Id": {"root"}}, "responses", false},
		{`{"model":"gpt-5.4","background":true}`, http.Header{"X-Openai-Subagent": {"review"}}, "responses", false},
		{`{"model":"gpt-5.4"}`, http.Header{"X-Openai-Subagent": {"review"}}, "responses", false},
		{`{"model":"gpt-5.4","previous_response_id":"bad id"}`, http.Header{"Thread-Id": {"root"}}, "responses", false},
	} {
		want, wantErr := Classify([]byte(test.body), test.headers, test.path, test.websocket)
		got, gotErr := ClassifyWith([]byte(test.body), test.path, test.websocket, func(root oif.Value) (codemode.Identity, error) { return Identity(test.headers, root) })
		if !reflect.DeepEqual(got, want) || fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
			t.Fatalf("%s: got %+v %v, want %+v %v", test.body, got, gotErr, want, wantErr)
		}
	}
}
