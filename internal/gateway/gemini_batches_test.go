package gateway

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/resources"
)

func TestGeminiBatchRequestValidationAndFileRewrite(t *testing.T) {
	inline := []byte(`{"batch":{"display_name":"nightly","input_config":{"requests":{"requests":[{"request":{"contents":[{"parts":[{"text":"hello"}]}]},"metadata":{"key":"request-1"}}]}}}}`)
	valid, err := validateGeminiBatchRequest(inline, "team-model")
	if err != nil || valid.Expected == nil || *valid.Expected != 1 || len(valid.Keys) != 1 || valid.Keys[0] != "request-1" {
		t.Fatalf("valid inline Gemini batch: %+v, %v", valid, err)
	}
	for _, raw := range []string{
		`{"batch":{"output_config":{},"input_config":{"requests":{"requests":[]}}}}`,
		`{"batch":{"input_config":{"requests":{"requests":[{"request":{"contents":[]},"metadata":{"key":""}}]}}}}`,
		`{"batch":{"input_config":{"requests":{"requests":[{"request":{"contents":[]},"metadata":{"key":"same"}},{"request":{"contents":[]},"metadata":{"key":"same"}}]}}}}`,
		`{"batch":{"input_config":{"requests":{"requests":[{"request":{"contents":[]},"metadata":{"key":"ok","extra":true}}]}}}}`,
		`{"batch":{"input_config":{"requests":{"requests":[{"request":{"contents":[]},"metadata":{"key":"ok"}}]}}},"unknown":true}`,
	} {
		if _, err := validateGeminiBatchRequest([]byte(raw), "team-model"); err == nil {
			t.Fatalf("accepted invalid Gemini batch request %s", raw)
		}
	}
	localFile := "gemini_file_0123456789abcdef0123456789abcdef"
	fileInput, err := validateGeminiBatchRequest([]byte(`{"batch":{"inputConfig":{"fileName":"files/`+localFile+`"}}}`), "team-model")
	if err != nil || fileInput.Expected != nil || fileInput.LocalFileID != localFile {
		t.Fatalf("valid file Gemini batch: %+v, %v", fileInput, err)
	}
	rewritten, rewriteErr := rewriteGeminiBatchInputFile(fileInput.Upstream, "files/upstream-file")
	if rewriteErr != nil || geminiBatchInputName(rewritten) != "files/upstream-file" {
		t.Fatalf("rewritten Gemini file input: %s, %v", rewritten, rewriteErr)
	}
}

func TestProjectGeminiBatchObjectAndRejectUnmappedIdentifiers(t *testing.T) {
	body := []byte(`{"name":"batches/upstream-batch","metadata":{"name":"batches/upstream-batch","model":"models/provider-model","inputConfig":{"fileName":"files/input-upstream"},"output":{"responsesFile":"files/output-upstream"}},"other":{"count":9}}`)
	projected, err := projectGeminiBatchObject(body, "gemini_batch_local", "route-slug", "files/gemini_file_input", "files/input-upstream",
		map[string]string{"files/output-upstream": "files/gemini_file_output"})
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if json.Unmarshal(projected, &object) != nil {
		t.Fatalf("invalid projected Gemini operation: %s", projected)
	}
	metadata := object["metadata"].(map[string]any)
	input := metadata["inputConfig"].(map[string]any)
	output := metadata["output"].(map[string]any)
	if object["name"] != "batches/gemini_batch_local" || metadata["name"] != "batches/gemini_batch_local" ||
		metadata["model"] != "models/route-slug" || input["fileName"] != "files/gemini_file_input" ||
		output["responsesFile"] != "files/gemini_file_output" || object["other"].(map[string]any)["count"] != float64(9) {
		t.Fatalf("Gemini batch projection=%s", projected)
	}
	unmapped := []byte(`{"name":"batches/upstream","metadata":{"name":"batches/upstream"},"unexpected":"files/secret"}`)
	if _, err := projectGeminiBatchObject(unmapped, "local", "route", "", "", nil); err == nil {
		t.Fatal("passed through an unmapped Gemini file identifier")
	}
}

func TestGeminiBatchPageCursor(t *testing.T) {
	id := resources.LocalID(resources.KindGeminiBatch, uuid.New())
	if size, token, err := geminiBatchPageCursor(url.Values{"pageSize": {"100"}, "pageToken": {id}}); err != nil || size != 100 || token != id {
		t.Fatalf("cursor=%d %q err=%v", size, token, err)
	}
	for _, query := range []url.Values{{"pageSize": {"101"}}, {"pageSize": {"0"}}, {"pageToken": {"invalid"}}} {
		if _, _, err := geminiBatchPageCursor(query); err == nil {
			t.Fatalf("accepted invalid page cursor %v", query)
		}
	}
}
