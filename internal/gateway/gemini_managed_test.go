package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/routing"
)

func translateGeminiManagedFixture(t *testing.T, input string, configure ...func(*routing.Provider)) ([]byte, batchManifest, error) {
	t.Helper()
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	manifest := batchManifest{Adapter: "gemini_managed"}
	provider := routing.Provider{
		Kind: "gemini", VendorID: "google", Endpoint: "https://provider.example/v1beta",
		AuthMode: "api_key", ProfileID: "gemini-generation", ProfileRevision: "1",
	}
	for _, change := range configure {
		change(&provider)
	}
	go func() {
		done <- translateManagedBatchLines(writer, strings.NewReader(input), func(value string) ([]byte, bool) {
			return []byte("digest:" + value), true
		}, &pin{provider: provider, model: "gemini-target"}, &routing.Route{Slug: "route-slug"}, "gemini_managed", &manifest)
	}()
	var output bytes.Buffer
	_, readErr := io.Copy(&output, reader)
	_ = reader.Close()
	translateErr := <-done
	if readErr != nil && translateErr == nil {
		translateErr = readErr
	}
	return output.Bytes(), manifest, translateErr
}

func TestGeminiManagedBatchLineTranslationAndRefusals(t *testing.T) {
	input := `{"custom_id":"task-1","method":"POST","url":"/v1/chat/completions","body":{"model":"route-slug","messages":[{"role":"user","content":"hello"}]}}` + "\n"
	translated, manifest, err := translateGeminiManagedFixture(t, input)
	if err != nil {
		t.Fatal(err)
	}
	var line map[string]json.RawMessage
	if json.Unmarshal(bytes.TrimSpace(translated), &line) != nil || len(line) != 2 {
		t.Fatalf("invalid translated Gemini line: %s", translated)
	}
	var key string
	_ = json.Unmarshal(line["key"], &key)
	var request map[string]json.RawMessage
	if key != "task-1" || json.Unmarshal(line["request"], &request) != nil ||
		len(request["contents"]) == 0 || len(request["model"]) != 0 {
		t.Fatalf("translated line key=%q request=%s", key, line["request"])
	}
	if manifest.LineCount != 1 || manifest.Endpoint != "/v1/chat/completions" || manifest.LineOperation != "generation" || manifest.ManifestDigest == "" {
		t.Fatalf("translated manifest=%+v", manifest)
	}
	for _, bad := range []string{
		`{"custom_id":"task-1","method":"POST","url":"/v1/responses","body":{"model":"route-slug","input":"hello"}}`,
		`{"custom_id":"task-1","method":"POST","url":"/v1/chat/completions","body":{"model":"other","messages":[]}}`,
		`{"custom_id":"task-1","method":"GET","url":"/v1/chat/completions","body":{"model":"route-slug"}}`,
		`{"custom_id":"task-1","method":"POST","url":"/v1/chat/completions","body":{"model":"route-slug","stream":true}}`,
		`{"custom_id":"task-1","method":"POST","url":"/v1/chat/completions","body":{"model":"route-slug","messages":[]}}` + "\n" +
			`{"custom_id":"task-1","method":"POST","url":"/v1/chat/completions","body":{"model":"route-slug","messages":[]}}`,
	} {
		if _, _, err := translateGeminiManagedFixture(t, bad); err == nil {
			t.Fatalf("accepted unsupported managed Gemini input %s", bad)
		}
	}
	longID := strings.Repeat("x", 129)
	tooLong := `{"custom_id":"` + longID + `","method":"POST","url":"/v1/chat/completions","body":{"model":"route-slug","messages":[]}}`
	if _, _, err := translateGeminiManagedFixture(t, tooLong); err == nil {
		t.Fatal("accepted a custom_id longer than 128 bytes")
	}
}

func TestGeminiManagedStatusAndCounts(t *testing.T) {
	body := []byte(`{"name":"batches/fixture","metadata":{"state":"BATCH_STATE_SUCCEEDED","createTime":"2026-10-21T12:00:00Z","batchStats":{"requestCount":"4","successfulRequestCount":"3","failedRequestCount":"1"}}}`)
	manifest := batchManifest{Adapter: "gemini_managed", Endpoint: "/v1/chat/completions", LineOperation: "generation", LineCount: 4, ManifestDigest: "abc"}
	metadata, state, err := geminiManagedBatchMetadata(body, "target-model", "file_local", manifest, nil, json.RawMessage(`{"source":"client"}`))
	if err != nil || state != "completed" {
		t.Fatalf("Gemini status=%s err=%v", state, err)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(metadata, &fields) != nil {
		t.Fatalf("invalid managed metadata: %s", metadata)
	}
	var counts map[string]int64
	if json.Unmarshal(fields["request_counts"], &counts) != nil || counts["total"] != 4 || counts["completed"] != 3 || counts["failed"] != 1 {
		t.Fatalf("Gemini request counts=%s", fields["request_counts"])
	}
	for _, state := range []string{"JOB_STATE_FAILED", "BATCH_STATE_CANCELLED", "JOB_STATE_EXPIRED"} {
		object := []byte(`{"name":"batches/fixture","metadata":{"state":"` + state + `"}}`)
		if _, _, err := geminiManagedBatchMetadata(object, "target", "", manifest, nil, nil); err != nil {
			t.Fatalf("known state %s rejected: %v", state, err)
		}
	}
	unknown := []byte(`{"name":"batches/fixture","metadata":{"state":"JOB_STATE_UNKNOWN"}}`)
	if _, _, err := geminiManagedBatchMetadata(unknown, "target", "", manifest, nil, nil); err == nil {
		t.Fatal("accepted unknown Gemini managed state")
	}
	if got := geminiManagedRequestCounts([]byte(`{"metadata":{"batchStats":{"requestCount":"8"}}}`)); got["total"] != 8 || len(got) != 1 {
		t.Fatalf("request-count presence rules: %v", got)
	}
}

func TestGeminiManagedContentLineTranslation(t *testing.T) {
	usageLine := `{"key":"ok","response":{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}}`
	errorLine := `{"key":"bad","error":{"code":400,"message":"bad"}}`
	spool, err := media.NewSpool(t.TempDir(), media.MinCapacityBytes, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	server := &Server{cfg: Config{MaxResponseBytes: 1 << 20}, Media: &MediaDeps{Jobs: &media.Service{Transport: &media.Transport{Spool: spool}}}}
	source := func(body string) *media.Part {
		artifact, err := spool.Put(t.Context(), media.Upload{Filename: "results.jsonl", ContentType: "application/jsonl", MaximumLength: 1 << 20, Body: strings.NewReader(body)})
		if err != nil {
			t.Fatal(err)
		}
		return &media.Part{Handle: artifact.Handle, Filename: "results.jsonl", ContentType: "application/jsonl", Size: artifact.ContentLength, Limit: 1 << 20}
	}
	valid := source(usageLine + "\n" + errorLine + "\n")
	defer spool.Remove(valid.Handle)
	output, err := server.translateGeminiManagedContent(t.Context(), "route-slug", valid, "output")
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Remove(output.Handle)
	file, err := spool.Open(output.Handle)
	if err != nil {
		t.Fatal(err)
	}
	translated, err := io.ReadAll(file.File)
	_ = file.File.Close()
	if err != nil {
		t.Fatal(err)
	}
	var outputLine map[string]json.RawMessage
	if json.Unmarshal(bytes.TrimSpace(translated), &outputLine) != nil || string(outputLine["custom_id"]) != `"ok"` ||
		len(outputLine["error"]) == 0 || string(outputLine["error"]) != "null" || bytes.Contains(outputLine["response"], []byte(`"id"`)) {
		t.Fatalf("output content translation=%s", translated)
	}
	errorsPart, err := server.translateGeminiManagedContent(t.Context(), "route-slug", valid, "error")
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Remove(errorsPart.Handle)
	file, err = spool.Open(errorsPart.Handle)
	if err != nil {
		t.Fatal(err)
	}
	errorBody, err := io.ReadAll(file.File)
	_ = file.File.Close()
	if err != nil {
		t.Fatal(err)
	}
	var errorLineObject map[string]json.RawMessage
	if json.Unmarshal(bytes.TrimSpace(errorBody), &errorLineObject) != nil || string(errorLineObject["custom_id"]) != `"bad"` ||
		string(errorLineObject["response"]) != "null" || !bytes.Contains(errorLineObject["error"], []byte(`"code":"400"`)) {
		t.Fatalf("error content translation=%s", errorBody)
	}
	server.cfg.MaxResponseBytes = 16
	before := spool.UsedBytes()
	if _, err := server.translateGeminiManagedContent(t.Context(), "route-slug", valid, "output"); err == nil || spool.UsedBytes() != before {
		t.Fatalf("translated Gemini results ignored response cap or leaked spool bytes: %v", err)
	}
	server.cfg.MaxResponseBytes = 1 << 20
	for _, malformed := range []string{`{"response":{}}`, `{"key":"missing"}`, `{"key":"both","response":{},"error":{}}`} {
		part := source(malformed + "\n")
		if _, err := server.translateGeminiManagedContent(t.Context(), "route-slug", part, "output"); err == nil {
			t.Fatalf("accepted malformed Gemini result line %s", malformed)
		}
		_ = spool.Remove(part.Handle)
	}
}
