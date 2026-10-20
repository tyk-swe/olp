package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
)

func TestBatchLineOperation(t *testing.T) {
	for endpoint, want := range map[string]string{
		"/v1/chat/completions": "generation",
		"/v1/responses":        "generation",
		"/v1/completions":      "generation",
		"/v1/embeddings":       "embeddings",
	} {
		source := parseBatchTestObject(t, `{"endpoint":"`+endpoint+`"}`)
		if got, err := batchLineOperation(source); err != nil || got != want {
			t.Errorf("%s operation = %q, %v; want %q", endpoint, got, err, want)
		}
	}
	for _, body := range []string{`{}`, `{"endpoint":"/v1/moderations"}`, `{"endpoint":7}`} {
		source := parseBatchTestObject(t, body)
		if _, err := batchLineOperation(source); err == nil || err.Code != "unsupported_parameter" || err.Status != 400 {
			t.Errorf("unsupported endpoint %s was accepted: %v", body, err)
		}
	}
}

func TestBatchCompletionWindow(t *testing.T) {
	for _, test := range []struct {
		vendor string
		window string
		valid  bool
		want   time.Duration
	}{
		{"openai", "", true, 24 * time.Hour}, {"openai", "24h", true, 24 * time.Hour},
		{"openai", "1d", true, 24 * time.Hour}, {"openai", "48h", false, 0},
		{"groq", "24h", true, 24 * time.Hour}, {"groq", "7d", true, 7 * 24 * time.Hour},
		{"groq", "1h", false, 0}, {"groq", "8d", false, 0},
		{"together", "24h", true, 24 * time.Hour}, {"together", "2d", false, 0},
		{"mistral", "1h", true, time.Hour}, {"mistral", "7d", true, 7 * 24 * time.Hour},
		{"mistral", "30m", false, 0},
	} {
		body := `{}`
		if test.window != "" {
			body = `{"completion_window":"` + test.window + `"}`
		}
		got, err := batchCompletionWindow(parseBatchTestObject(t, body), test.vendor)
		if test.valid && (err != nil || got != test.want) {
			t.Errorf("%s %s completion window = %v, %v; want %v", test.vendor, test.window, got, err, test.want)
		}
		if !test.valid && (err == nil || err.Code != "invalid_value" || err.Status != 400) {
			t.Errorf("%s invalid completion window %s was accepted: %v", test.vendor, test.window, err)
		}
	}
}

func TestManagedBatchProjection(t *testing.T) {
	for adapter, statuses := range managedBatchStatusProjection {
		for source, want := range statuses {
			body := map[string]any{"id": "job-status", "status": source, "endpoint": "/v1/chat/completions", "created_at": 1792497600}
			if adapter == "together" {
				body["input_file_id"] = "file-in"
			} else {
				body["input_files"] = []string{"file-in"}
			}
			encoded, _ := json.Marshal(body)
			projected, _, err := projectManagedBatch(encoded, adapter, "24h")
			if err != nil {
				t.Fatalf("%s status %s: %v", adapter, source, err)
			}
			var value map[string]any
			if json.Unmarshal(projected, &value) != nil || value["status"] != want {
				t.Fatalf("%s status %s projected as %v", adapter, source, value)
			}
		}
	}
	tests := []struct {
		name    string
		adapter string
		body    string
		status  string
		total   int
		wantErr bool
	}{
		{"together", "together", `{"job":{"id":"job-t","status":"IN_PROGRESS","endpoint":"/v1/chat/completions","input_file_id":"file-in","output_file_id":"file-out","created_at":"2026-10-20T12:00:00Z"}}`, "in_progress", 0, false},
		{"mistral", "mistral", `{"id":"job-m","status":"SUCCESS","endpoint":"/v1/embeddings","input_files":["file-in"],"output_file":"file-out","total_requests":"8","succeeded_requests":"6","failed_requests":"2","created_at":1792497600}`, "completed", 8, false},
		{"unknown", "together", `{"job":{"id":"job-t","status":"UNKNOWN","endpoint":"/v1/chat/completions","input_file_id":"file-in"}}`, "", 0, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			projected, id, err := projectManagedBatch([]byte(test.body), test.adapter, "24h")
			if test.wantErr {
				if err == nil {
					t.Fatal("unknown provider status was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]any
			if err = json.Unmarshal(projected, &object); err != nil {
				t.Fatal(err)
			}
			if object["id"] != id || object["status"] != test.status || object["completion_window"] != "24h" {
				t.Fatalf("projection = %v", object)
			}
			if test.total > 0 {
				counts, ok := object["request_counts"].(map[string]any)
				if !ok || counts["total"] != float64(test.total) {
					t.Fatalf("Mistral request_counts = %v", object["request_counts"])
				}
			}
		})
	}
}

func TestManagedBatchRequestBodies(t *testing.T) {
	manifest := batchManifest{Endpoint: "/v1/chat/completions", LineCount: 3}
	source := parseBatchTestObject(t, `{"input_file_id":"file-local","endpoint":"/v1/chat/completions","completion_window":"24h","metadata":{"owner":"team"}}`)
	together, expected, err := managedBatchRequestBody(source, "together", "file-remote", "target-model", manifest)
	if err != nil || expected == nil || *expected != 3 {
		t.Fatalf("Together batch request: expected=%v err=%v", expected, err)
	}
	var togetherFields map[string]json.RawMessage
	_ = json.Unmarshal(together, &togetherFields)
	if string(togetherFields["completion_window"]) != `"24h"` || string(togetherFields["input_file_id"]) != `"file-remote"` {
		t.Fatalf("Together batch request = %s", together)
	}
	mistralSource := parseBatchTestObject(t, `{"input_file_id":"file-local","endpoint":"/v1/chat/completions","completion_window":"7d","metadata":{"owner":"team"}}`)
	mistral, expected, err := managedBatchRequestBody(mistralSource, "mistral", "file-remote", "target-model", manifest)
	if err != nil || expected == nil || *expected != 3 {
		t.Fatalf("Mistral batch request: expected=%v err=%v", expected, err)
	}
	var mistralFields map[string]json.RawMessage
	_ = json.Unmarshal(mistral, &mistralFields)
	if string(mistralFields["timeout_hours"]) != "168" || string(mistralFields["model"]) != `"target-model"` ||
		string(mistralFields["metadata"]) != `{"owner":"team"}` {
		t.Fatalf("Mistral batch request = %s", mistral)
	}
	badMetadata := parseBatchTestObject(t, `{"endpoint":"/v1/chat/completions","metadata":{"owner":7}}`)
	if _, _, err = managedBatchRequestBody(badMetadata, "mistral", "file-remote", "target-model", manifest); err == nil || err.Code != "unsupported_parameter" {
		t.Fatalf("non-string Mistral metadata was accepted: %v", err)
	}
	mismatched := parseBatchTestObject(t, `{"endpoint":"/v1/embeddings"}`)
	if _, _, err = managedBatchRequestBody(mismatched, "together", "file-remote", "target-model", manifest); err == nil || err.Code != "unsupported_parameter" {
		t.Fatalf("batch endpoint/file mismatch was accepted: %v", err)
	}
}

func TestManagedBatchLineEndpointAndIdentityLimits(t *testing.T) {
	if operation, family, ok := managedBatchEndpoint("together", "/v1/chat/completions"); !ok || operation != "generation" || family != wire.Chat {
		t.Fatalf("Together chat endpoint = %s/%s/%v", operation, family, ok)
	}
	if _, _, ok := managedBatchEndpoint("together", "/v1/embeddings"); ok {
		t.Fatal("Together accepted embeddings")
	}
	if operation, family, ok := managedBatchEndpoint("mistral", "/v1/embeddings"); !ok || operation != "embeddings" || family != wire.Embeddings {
		t.Fatalf("Mistral embeddings endpoint = %s/%s/%v", operation, family, ok)
	}
	if managedCustomIDLimit("together") != 64 || managedCustomIDLimit("mistral") != 128 {
		t.Fatal("managed custom_id byte limits are incorrect")
	}
}

func TestManagedBatchLineValidationRefusals(t *testing.T) {
	chat := func(customID, method, endpoint, model string) string {
		body, _ := json.Marshal(map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "x"}}})
		line, _ := json.Marshal(map[string]any{"custom_id": customID, "method": method, "url": endpoint, "body": json.RawMessage(body)})
		return string(line) + "\n"
	}
	for _, test := range []struct {
		name, line, adapter string
	}{
		{"malformed-json", "{\n", "together"},
		{"unknown-line-field", `{"custom_id":"x","method":"POST","url":"/v1/chat/completions","body":{"model":"route"},"extra":true}` + "\n", "together"},
		{"empty-custom-id", chat("", "POST", "/v1/chat/completions", "route"), "together"},
		{"long-custom-id", chat(strings.Repeat("x", 65), "POST", "/v1/chat/completions", "route"), "together"},
		{"wrong-method", chat("id", "GET", "/v1/chat/completions", "route"), "together"},
		{"unsupported-endpoint", chat("id", "POST", "/v1/moderations", "route"), "together"},
		{"wrong-model", chat("id", "POST", "/v1/chat/completions", "other"), "together"},
		{"mistral-long-custom-id", chat(strings.Repeat("x", 129), "POST", "/v1/chat/completions", "route"), "mistral"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader, writer := io.Pipe()
			var p *pin
			var route *routing.Route
			if test.name == "wrong-model" {
				p = &pin{model: "target"}
				route = &routing.Route{Slug: "route"}
			}
			err := translateManagedBatchLines(writer, strings.NewReader(test.line), nil, p, route, test.adapter, &batchManifest{})
			_ = reader.Close()
			failure, ok := err.(*Error)
			if !ok || failure.Code != "unsupported_parameter" || failure.Status != 400 {
				t.Fatalf("line refusal = %v", err)
			}
		})
	}
}

func TestManagedBatchLineTranslationAndDuplicateIdentity(t *testing.T) {
	provider := routing.Provider{
		Kind: "openai_compatible", VendorID: "together", ProfileID: "together-files-batches",
		ProfileRevision: "1", Endpoint: "https://api.together.ai/v1", AuthMode: "api_key",
	}
	p := &pin{provider: provider, model: "target-model"}
	route := &routing.Route{Slug: "route"}
	digest := func(customID string) ([]byte, bool) {
		return []byte("digest:" + customID), true
	}
	line, _ := json.Marshal(map[string]any{
		"custom_id": "line-1", "method": "POST", "url": "/v1/chat/completions",
		"body": map[string]any{"model": "route", "messages": []any{map[string]any{"role": "user", "content": "hello"}}},
	})
	translated, manifest, err := runManagedTranslation(string(line)+"\n", digest, p, route, "together")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.LineCount != 1 || manifest.Endpoint != "/v1/chat/completions" || manifest.LineOperation != "generation" ||
		manifest.ManifestDigest == "" || !json.Valid(translated) {
		t.Fatalf("translated manifest = %+v, body=%s", manifest, translated)
	}
	var projected map[string]json.RawMessage
	if json.Unmarshal(bytes.TrimSpace(translated), &projected) != nil || len(projected) != 2 {
		t.Fatalf("translated line has unexpected fields: %s", translated)
	}
	var body map[string]json.RawMessage
	_ = json.Unmarshal(projected["body"], &body)
	if string(body["model"]) != `"target-model"` {
		t.Fatalf("Together model = %s, want target-model", body["model"])
	}
	mistralProvider := routing.Provider{
		Kind: "openai_compatible", VendorID: "mistral", ProfileID: "mistral-files-batches",
		ProfileRevision: "1", Endpoint: "https://api.mistral.ai/v1", AuthMode: "api_key",
	}
	mistralPin := &pin{provider: mistralProvider, model: "target-model"}
	embeddingLine, _ := json.Marshal(map[string]any{
		"custom_id": "embedding-1", "method": "POST", "url": "/v1/embeddings",
		"body": map[string]any{"model": "route", "input": "hello"},
	})
	embedding, manifest, err := runManagedTranslation(string(embeddingLine)+"\n", digest, mistralPin, route, "mistral")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.LineOperation != "embeddings" {
		t.Fatalf("Mistral embedding manifest = %+v", manifest)
	}
	var embeddingLineResult map[string]json.RawMessage
	if json.Unmarshal(bytes.TrimSpace(embedding), &embeddingLineResult) != nil {
		t.Fatalf("Mistral translated line is invalid: %s", embedding)
	}
	body = nil
	_ = json.Unmarshal(embeddingLineResult["body"], &body)
	if _, present := body["model"]; present {
		t.Fatalf("Mistral per-line body retained a model: %s", embedding)
	}
	duplicate := string(line) + "\n" + string(line) + "\n"
	_, _, err = runManagedTranslation(duplicate, digest, p, route, "together")
	failure, ok := err.(*Error)
	if !ok || failure.Code != "unsupported_parameter" || !strings.Contains(failure.Message, "line 2") {
		t.Fatalf("duplicate custom_id refusal = %v", err)
	}
}

func runManagedTranslation(body string, digest func(string) ([]byte, bool), p *pin, route *routing.Route, adapter string) ([]byte, batchManifest, error) {
	reader, writer := io.Pipe()
	manifest := batchManifest{Adapter: adapter}
	result := make(chan error, 1)
	go func() {
		result <- translateManagedBatchLines(writer, strings.NewReader(body), digest, p, route, adapter, &manifest)
	}()
	translated, readErr := io.ReadAll(reader)
	err := <-result
	_ = reader.Close()
	if err != nil {
		return translated, manifest, err
	}
	return translated, manifest, readErr
}

func parseBatchTestObject(t *testing.T, body string) oif.Document {
	t.Helper()
	document, err := oif.ParseJSON([]byte(body), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func TestStrictAndFireworksBatchRefusals(t *testing.T) {
	if err := strictBatchTargetRefusal(&pin{provider: routing.Provider{Kind: "openai_compatible"}}); err == nil || err.Type != "invalid_request_error" || err.Code != "target_capability" || err.Status != 422 ||
		err.Message != "Strict routes serve files and batches only for OpenAI and Azure." {
		t.Fatalf("strict managed batch refusal = %v", err)
	}
	if err := strictBatchTargetRefusal(&pin{provider: routing.Provider{Kind: "azure_openai"}}); err != nil {
		t.Fatalf("Azure strict batch was refused: %v", err)
	}
	x := &call{request: request{release: &runtime.Release{Snapshot: &routing.Snapshot{
		Providers: map[string]routing.Provider{"fireworks": {ID: "fireworks", Kind: "openai_compatible", VendorID: "fireworks"}},
	}}}}
	route := &routing.Route{Targets: []routing.Target{{ProviderID: "fireworks"}}}
	if err := fireworksBatchRefusal(x, route); err == nil || err.Type != "invalid_request_error" || err.Code != "target_capability" || err.Status != 422 ||
		err.Message != "Fireworks documents no OpenAI-compatible files/batches API; its dataset batch jobs return results only through provider-returned signed URLs, which the egress contract forbids." {
		t.Fatalf("Fireworks batch refusal = %v", err)
	}
}

func TestManagedFileProjectionDoesNotInventFields(t *testing.T) {
	resource := &resources.Resource{
		UUID: uuid.New(), Kind: resources.KindFile,
		Metadata: managedFileMetadata([]byte(`{}`), []byte(`{"bytes":12,"created_at":1792497600,"filename":"batch.jsonl","purpose":"batch-api","status":"processed"}`), "together", batchManifest{}, "input"),
	}
	body, err := (&Server{}).fileObject(context.Background(), resource)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		t.Fatal("managed file projection is not JSON")
	}
	if string(fields["purpose"]) != `"batch"` || string(fields["object"]) != `"file"` {
		t.Fatalf("managed file projection = %s", body)
	}
	if _, fabricated := fields["status"]; fabricated {
		t.Fatalf("managed file projection fabricated a status: %s", body)
	}
}
