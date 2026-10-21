//go:build integration

package integration_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/usage"
)

func attributionCall(t *testing.T, h *accessHarness, model, key string, headers []string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.HTTP.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	for _, header := range headers {
		req.Header.Add(usage.AttributionHeader, header)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}
