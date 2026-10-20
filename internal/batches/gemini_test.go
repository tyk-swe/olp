package batches

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
)

func geminiBatchFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "tests", "fixtures", "vendors", "gemini", name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestGeminiReaderPollInlineAndFileResults(t *testing.T) {
	inline := geminiBatchFixture(t, "batch.succeeded.inline.json")
	fileJob := geminiBatchFixture(t, "batch.succeeded.file.json")
	running := geminiBatchFixture(t, "batch.running.json")
	listing := geminiBatchFixture(t, "batch.list.response.json")
	results := geminiBatchFixture(t, "batch.results.jsonl")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1beta/batches/batch-0123456789":
			_, _ = w.Write(inline)
		case r.URL.Path == "/v1beta/batches/batch-file-0123456789":
			_, _ = w.Write(fileJob)
		case r.URL.Path == "/v1beta/batches/running":
			_, _ = w.Write(running)
		case r.URL.Path == "/v1beta/batches":
			_, _ = w.Write(listing)
		case r.URL.Path == "/download/v1beta/files/output-gemini-file:download":
			_, _ = w.Write(results)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	policy := &egress.Policy{
		AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		PlainHTTPHosts:  []string{"127.0.0.1"},
	}
	reader := &GeminiReader{Auth: connectors.NewAuth(policy), Egress: policy}
	target := &PinnedTarget{
		Config: connectors.Config{Kind: "gemini", AuthMode: "api_key", Endpoint: server.URL + "/v1beta"},
		Secret: []byte("fixture-key"), Client: policy.Client(time.Second), Model: "gemini-2.5-flash",
	}
	settlement := Settlement{UpstreamID: geminiStringPointer("batch-0123456789")}
	snapshot, err := reader.Poll(context.Background(), settlement, target)
	if err != nil || !snapshot.Terminal || snapshot.Expected == nil || *snapshot.Expected != 2 {
		t.Fatalf("inline poll: %+v, %v", snapshot, err)
	}
	var inlineResults []Result
	cursor, done, err := reader.ReadResults(context.Background(), target, snapshot, Cursor{}, func(results []Result, _ Cursor) error {
		inlineResults = append(inlineResults, results...)
		return nil
	})
	if err != nil || !done || cursor.Phase != "done" || len(inlineResults) != 2 {
		t.Fatalf("inline results: %+v done=%v results=%+v err=%v", cursor, done, inlineResults, err)
	}
	success := inlineResults[0]
	if success.Kind != ResultSucceeded || success.Usage.InputTokens == nil || *success.Usage.InputTokens != 10 ||
		success.Usage.OutputTokens == nil || *success.Usage.OutputTokens != 6 ||
		success.Usage.CachedInputTokens == nil || *success.Usage.CachedInputTokens != 3 ||
		success.Usage.ToolCalls["google_search_query"] != 2 || inlineResults[1].Kind != ResultErrored {
		t.Fatalf("Gemini inline accounting results: %+v", inlineResults)
	}
	fileSettlement := Settlement{UpstreamID: geminiStringPointer("batch-file-0123456789")}
	fileSnapshot, err := reader.Poll(context.Background(), fileSettlement, target)
	if err != nil || fileSnapshot.OutputFileID != "files/output-gemini-file" {
		t.Fatalf("file poll: %+v, %v", fileSnapshot, err)
	}
	var fileResults []Result
	_, done, err = reader.ReadResults(context.Background(), target, fileSnapshot, Cursor{}, func(results []Result, _ Cursor) error {
		fileResults = append(fileResults, results...)
		return nil
	})
	if err != nil || !done || len(fileResults) != 2 || fileResults[0].Kind != ResultSucceeded || fileResults[1].Kind != ResultErrored {
		t.Fatalf("file results: %+v done=%v err=%v", fileResults, done, err)
	}
	reconcileSettlement := Settlement{
		CreatedAt: time.Date(2026, 10, 21, 11, 59, 0, 0, time.UTC),
		Cursor:    json.RawMessage(`{"submission":{"gemini_file":"files/input-gemini-file"}}`),
	}
	reconciled, err := reader.Reconcile(context.Background(), reconcileSettlement, target, "files/input-gemini-file", "")
	if err != nil || len(reconciled.Matches) != 1 || reconciled.Matches[0].ID != "batch-file-0123456789" {
		t.Fatalf("file reconciliation: %+v, %v", reconciled, err)
	}
	inlineReconcile, err := reader.Reconcile(context.Background(), Settlement{Cursor: json.RawMessage(`{"submission":{"gemini_inline":true}}`)}, target, "", "")
	if err != nil || len(inlineReconcile.Matches) != 0 {
		t.Fatalf("inline reconciliation: %+v, %v", inlineReconcile, err)
	}
	if _, err := parseGeminiResult([]byte(`{"response":{}}`), "model"); err != ErrCursorConflict {
		t.Fatalf("missing result key error=%v", err)
	}
	worker := &Worker{}
	worker.setup()
	if worker.Readers["gemini"] == nil {
		t.Fatal("Gemini settlement reader is not registered")
	}
}

func geminiStringPointer(value string) *string { return &value }
