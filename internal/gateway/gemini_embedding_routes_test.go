package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// geminiError is the Gemini error envelope.
type geminiError struct {
	Code    int    `json:"code"`
	Status  string `json:"status"`
	Message string `json:"message"`
	Details []struct {
		Reason string `json:"reason"`
	} `json:"details"`
}

// geminiEmbeddingError posts a native Gemini embedding request and returns the
// status and Gemini error envelope that answered it.
func geminiEmbeddingError(t *testing.T, h *harness, model, action string) (int, geminiError) {
	t.Helper()
	body := `{"content":{"parts":[{"text":"hi"}]}}`
	if action == "batchEmbedContents" {
		body = `{"requests":[{"content":{"parts":[{"text":"hi"}]}}]}`
	}
	resp := h.do(t.Context(), http.MethodPost, "/gemini/v1beta/models/"+model+":"+action, fullKey, []byte(body), nil)
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var envelope struct {
		Error geminiError `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("status=%d body=%s", resp.StatusCode, data)
	}
	return resp.StatusCode, envelope.Error
}

func TestGeminiNativeEmbeddingsOnAnUnknownRouteAreNotFound(t *testing.T) {
	for _, action := range []string{"embedContent", "batchEmbedContents"} {
		t.Run(action, func(t *testing.T) {
			h := strictHarness(t, strictEmbeddings)
			status, got := geminiEmbeddingError(t, h, "no-such-route", action)
			if status != http.StatusNotFound || got.Code != http.StatusNotFound || got.Status != "NOT_FOUND" ||
				len(got.Details) != 1 || got.Details[0].Reason != "route_not_found" {
				t.Fatalf("status=%d error=%+v", status, got)
			}
		})
	}
}

func TestGeminiNativeEmbeddingsNeedAStrictRoute(t *testing.T) {
	for _, action := range []string{"embedContent", "batchEmbedContents"} {
		t.Run(action, func(t *testing.T) {
			h := newHarness(t, Config{})
			status, got := geminiEmbeddingError(t, h, routeSlug, action)
			if status != http.StatusBadRequest || got.Status != "INVALID_ARGUMENT" || !strings.Contains(got.Message, "strict route") {
				t.Fatalf("status=%d error=%+v", status, got)
			}
		})
	}
}
