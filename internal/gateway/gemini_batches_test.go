package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/url"
	"strings"
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
	var upstream struct {
		Batch struct {
			Input struct {
				Requests struct {
					Requests []json.RawMessage `json:"requests"`
				} `json:"requests"`
			} `json:"inputConfig"`
		} `json:"batch"`
	}
	if json.Unmarshal(valid.Upstream, &upstream) != nil || len(upstream.Batch.Input.Requests.Requests) != 1 {
		t.Fatalf("invalid upstream inline request shape: %s", valid.Upstream)
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
	unmapped := []byte(`{"name":"batches/upstream","metadata":{"name":"batches/upstream"},"response":{"responsesFile":"files/secret"}}`)
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

func TestGeminiBatchMetadataExcludesContent(t *testing.T) {
	content := bytes.Repeat([]byte("private-model-content"), 1024)
	body, _ := json.Marshal(map[string]any{
		"name": "batches/upstream", "done": true,
		"metadata": map[string]any{
			"state": "BATCH_STATE_SUCCEEDED", "model": "models/provider-model",
			"inputConfig": map[string]any{"requests": map[string]any{"requests": string(content)}},
			"output":      map[string]any{"inlinedResponses": map[string]any{"inlinedResponses": []any{map[string]any{"response": string(content)}}}},
		},
	})
	metadata := geminiBatchMetadata(body, "provider-model", "files/local", "files/upstream", nil)
	if bytes.Contains(metadata, content) || bytes.Contains(metadata, []byte("inlinedResponses")) || len(metadata) > 1024 {
		t.Fatalf("Gemini metadata retained content (%d bytes)", len(metadata))
	}
	resource := &resources.Resource{ID: "gemini_batch_local", RouteSlug: "route", Metadata: metadata}
	local, upstream := geminiBatchInternalNames(resource)
	if local != "files/local" || upstream != "files/upstream" || geminiBatchState(metadata) != "BATCH_STATE_SUCCEEDED" {
		t.Fatalf("Gemini metadata lost lifecycle identity: %s", metadata)
	}
	projected, err := (&Server{}).geminiBatchResourceObject(context.Background(), resource, body)
	if err != nil || !bytes.Contains(projected, content) {
		t.Fatalf("transient inline output was lost: err=%v", err)
	}
}

func TestGeminiInlineBatchRefusesUnboundFileReferences(t *testing.T) {
	for _, request := range []string{
		`{"contents":[{"parts":[{"fileData":{"mimeType":"application/pdf","fileUri":"https://generativelanguage.googleapis.com/v1beta/files/foreign"}}]}]}`,
		`{"contents":[{"parts":[{"text":"read this"}]}],"cachedContent":"cachedContents/foreign"}`,
	} {
		body := []byte(`{"batch":{"inputConfig":{"requests":{"requests":[{"request":` + request + `,"metadata":{"key":"one"}}]}}}}`)
		if _, err := validateGeminiBatchRequest(body, "route"); err == nil || err.Code != "unsupported_parameter" {
			t.Fatalf("unowned file reference was not refused: %s err=%v", body, err)
		}
	}
}

func TestGeminiBatchProjectionPreservesOrdinaryReferenceText(t *testing.T) {
	body := []byte(`{"name":"batches/upstream","metadata":{"name":"batches/upstream","displayName":"files/daily","output":{"inlinedResponses":{"inlinedResponses":[{"metadata":{"key":"files/result-key"},"response":{"candidates":[{"content":{"parts":[{"text":"save to files/output.json or batches/archive"}]}}]}}]}}}}`)
	projected, err := projectGeminiBatchObject(body, "local", "route", "", "", nil)
	if err != nil || !bytes.Contains(projected, []byte("save to files/output.json or batches/archive")) {
		t.Fatalf("ordinary generated content was mistaken for a resource reference: %s err=%v", projected, err)
	}
	for _, body := range []string{"", "{}", "{ \n }"} {
		if !geminiBatchCancelAcknowledgement([]byte(body)) {
			t.Fatalf("valid empty cancellation acknowledgement refused: %q", body)
		}
	}
	if geminiBatchCancelAcknowledgement([]byte(`{"name":"batches/upstream"}`)) {
		t.Fatal("a job response was classified as an empty acknowledgement")
	}
}

func TestGeminiNativeFileQualificationRejectsUnownedReferences(t *testing.T) {
	digest := func(key string) ([]byte, bool) { sum := sha256.Sum256([]byte(key)); return sum[:], true }
	good := `{"key":"one","request":{"contents":[{"parts":[{"text":"hello"}]}],"tools":[{"functionDeclarations":[{"name":"read","parameters":{"type":"object","properties":{"fileUri":{"type":"string"}}}}]}]}}`
	manifest, err := qualifyGeminiBatchJSONL(strings.NewReader(good+"\n"), "route", digest)
	if err != nil || manifest.Adapter != "gemini_native" || manifest.LineCount != 1 || manifest.ManifestDigest == "" {
		t.Fatalf("valid native JSONL or opaque tool schema was refused: %+v %v", manifest, err)
	}
	for _, input := range []string{
		good + "\n" + good + "\n",
		`{"key":"one","request":{"contents":[{"parts":[{"fileData":{"fileUri":"files/foreign","mimeType":"application/pdf"}}]}]}}` + "\n",
		`{"key":"one","request":{"contents":[{"parts":[{"text":"hi"}]}],"cachedContent":"cachedContents/foreign"}}` + "\n",
	} {
		if _, err := qualifyGeminiBatchJSONL(strings.NewReader(input), "route", digest); err == nil {
			t.Fatalf("unqualified native JSONL was accepted: %s", input)
		}
	}
}
