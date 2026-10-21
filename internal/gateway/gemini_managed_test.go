package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

func TestGeminiManagedDownloadEnforcesResponseCap(t *testing.T) {
	h, x, p := pinnedExchangeFixture(t)
	x.family, x.actor = wire.File, "api_key_resource"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 17))
	}))
	defer upstream.Close()
	p.provider.Kind, p.provider.Endpoint, p.provider.VendorID = "gemini", upstream.URL+"/v1beta", "google"
	h.gateway.client, h.gateway.cfg.MaxResponseBytes = upstream.Client(), 16
	spool, err := media.NewSpool(t.TempDir(), media.MinCapacityBytes, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	h.gateway.Media = &MediaDeps{Jobs: &media.Service{Transport: &media.Transport{Spool: spool}}}
	response := httptest.NewRecorder()
	failure := h.gateway.geminiManagedFileContent(t.Context(), response, x,
		&resources.Resource{UpstreamID: "files/output", RouteSlug: routeSlug}, p)
	if failure == nil || failure.Code != "upstream_response_too_large" || response.Body.Len() != 0 || spool.UsedBytes() != 0 {
		t.Fatalf("oversized managed Gemini file escaped or leaked: %v bytes=%d spool=%d", failure, response.Body.Len(), spool.UsedBytes())
	}
}

func TestGeminiManagedUploadCompletesStartAttempt(t *testing.T) {
	h, x, p := pinnedExchangeFixture(t)
	x.family, x.actor = wire.File, "api_key_resource"
	var upstream *httptest.Server
	upstream = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Goog-Upload-Command") == "start" {
			w.Header().Set("X-Goog-Upload-URL", upstream.URL+"/upload/session")
			return
		}
		w.Header().Set("X-Goog-Upload-Status", "final")
		_, _ = io.WriteString(w, `{"file":{"name":"files/input"}}`)
	}))
	defer upstream.Close()
	p.provider.Kind, p.provider.Endpoint, p.provider.AuthMode = "gemini", upstream.URL+"/v1beta", "api_key"
	h.gateway.client = upstream.Client()
	spool, err := media.NewSpool(t.TempDir(), media.MinCapacityBytes, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	h.gateway.Media = &MediaDeps{Jobs: &media.Service{Transport: &media.Transport{Spool: spool}}}
	artifact, err := spool.Put(t.Context(), media.Upload{Filename: "input.jsonl", MaximumLength: 128, Body: strings.NewReader("{}\n")})
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Remove(artifact.Handle)
	_, _, failure := h.gateway.uploadGeminiManagedFile(t.Context(), x, p, &media.Part{Handle: artifact.Handle, Filename: "input.jsonl", Size: artifact.ContentLength})
	if failure != nil {
		t.Fatalf("managed upload failed: %+v", failure)
	}
	x.completePinned()
	if len(x.facts) != 2 || x.facts[0].Class != classSuccess || x.facts[1].Class != classSuccess {
		t.Fatalf("successful upload exchanges recorded failures: %+v", x.facts)
	}
}

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

func TestGeminiManagedAutomaticDefaults(t *testing.T) {
	input := `{"custom_id":"task","method":"POST","url":"/v1/chat/completions","body":{"model":"route-slug","messages":[{"role":"user","content":"hello"}]}}` + "\n"
	translated, _, err := translateGeminiManagedFixture(t, input, func(provider *routing.Provider) {
		provider.ProfileID, provider.ProfileRevision = "", ""
		provider.ParameterDefaults = map[string]json.RawMessage{"generationConfig": json.RawMessage(`{"temperature":0.25}`)}
	})
	if err != nil {
		t.Fatal(err)
	}
	var line struct {
		Request struct {
			GenerationConfig struct {
				Temperature float64 `json:"temperature"`
			} `json:"generationConfig"`
		} `json:"request"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(translated), &line); err != nil || line.Request.GenerationConfig.Temperature != 0.25 {
		t.Fatalf("Automatic Gemini defaults were not applied: %s err=%v", translated, err)
	}
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

func TestGeminiManagedMetadataStoresOnlyLifecycleFields(t *testing.T) {
	body := []byte(`{"name":"batches/private","done":true,"metadata":{"state":"BATCH_STATE_SUCCEEDED","createTime":"2026-10-21T12:00:00Z","batchStats":{"requestCount":"1","successfulRequestCount":"1"},"inputConfig":{"fileName":"files/input","requests":[{"prompt":"request-content-sentinel"}]},"output":{"responsesFile":"files/output","inlinedResponses":{"responses":[{"text":"output-content-sentinel"}]}}},"response":{"inlinedResponses":{"responses":[{"text":"response-content-sentinel"}]}},"error":{"code":3,"message":"error-content-sentinel","details":[{"data":"error-detail-sentinel"}]}}`)
	body = bytes.ReplaceAll(body, []byte("output-content-sentinel"), bytes.Repeat([]byte("output-content-sentinel"), 1024))
	manifest := batchManifest{Adapter: "gemini_managed", Endpoint: "/v1/chat/completions", LineOperation: "generation", LineCount: 1, ManifestDigest: "digest"}
	metadata, state, err := geminiManagedBatchMetadata(body, "model", "file_local", manifest, nil, nil)
	if err != nil || state != "completed" {
		t.Fatalf("metadata failed: %s %v", state, err)
	}
	if len(metadata) > 16<<10 {
		t.Fatalf("provider payload exceeded the lifecycle metadata bound: %d", len(metadata))
	}
	for _, sentinel := range []string{"request-content-sentinel", "output-content-sentinel", "response-content-sentinel", "error-content-sentinel", "error-detail-sentinel"} {
		if bytes.Contains(metadata, []byte(sentinel)) {
			t.Fatalf("provider content persisted in metadata: %s", sentinel)
		}
	}
	if !bytes.Contains(metadata, []byte(`"responsesFile":"files/output"`)) || !bytes.Contains(metadata, []byte(`"requestCount":"1"`)) {
		t.Fatalf("lifecycle/file/count evidence was dropped: %s", metadata)
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

func TestGeminiManagedResponseEnvelopeResultFile(t *testing.T) {
	body := []byte(`{"name":"batches/upstream","metadata":{"state":"BATCH_STATE_SUCCEEDED"},"response":{"responsesFile":"files/output"}}`)
	if name := geminiManagedResponsesFile(body); name != "files/output" {
		t.Fatalf("response-envelope result file missing: %q", name)
	}
}
