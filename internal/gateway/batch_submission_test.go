package gateway

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/oif"
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
	if got, err := batchCompletionWindow(parseBatchTestObject(t, `{}`)); err != nil || got != 24*time.Hour {
		t.Fatalf("default completion window = %v, %v", got, err)
	}
	if got, err := batchCompletionWindow(parseBatchTestObject(t, `{"completion_window":"24h"}`)); err != nil || got != 24*time.Hour {
		t.Fatalf("24h completion window = %v, %v", got, err)
	}
	for _, body := range []string{`{"completion_window":7}`, `{"completion_window":"forever"}`, `{"completion_window":"48h"}`} {
		if _, err := batchCompletionWindow(parseBatchTestObject(t, body)); err == nil || err.Code != "invalid_value" || err.Status != 400 {
			t.Errorf("invalid completion window %s was accepted: %v", body, err)
		}
	}
}

func parseBatchTestObject(t *testing.T, body string) oif.Document {
	t.Helper()
	document, err := oif.ParseJSON([]byte(body), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return document
}
