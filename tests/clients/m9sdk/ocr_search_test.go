//go:build integration

package m9sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// post sends a raw JSON request to an inference path; no locked SDK covers
// these operations.
func post(t *testing.T, ctx context.Context, path string, body []byte, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, env(t, "OLP_CLIENTS_OPENAI_BASE_URL")+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+env(t, "OLP_CLIENTS_API_KEY"))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, data
}

func resetRecorded(t *testing.T, ctx context.Context) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, env(t, "OLP_CLIENTS_UPSTREAM_URL")+"/__recorded", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("recording reset: %d", resp.StatusCode)
	}
}

type recordedRequest struct {
	Dialect    string            `json:"dialect"`
	Path       string            `json:"path"`
	Model      string            `json:"model"`
	Headers    map[string]string `json:"headers"`
	Body       json.RawMessage   `json:"body"`
	Authorized bool              `json:"authorized"`
	Leaked     bool              `json:"leaked_client_credential"`
}

func recorded(t *testing.T, ctx context.Context, pathPrefix string) []recordedRequest {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, env(t, "OLP_CLIENTS_UPSTREAM_URL")+"/__recorded?path="+pathPrefix, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Requests []recordedRequest `json:"requests"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Requests
}

func TestM9SearchRefusalDispatchesNothing(t *testing.T) {
	_, ctx := client(t)
	resetRecorded(t, ctx)
	slug := env(t, "OLP_CLIENTS_MODEL_SEARCH_TAVILY")
	resp, data := post(t, ctx, "/search", []byte(`{"query":"q","search_recency_filter":"week"}`), map[string]string{"X-OLP-Route": slug})
	if resp.StatusCode < 400 {
		t.Fatalf("recency filter admitted: %d %s", resp.StatusCode, data)
	}
	if n := len(recorded(t, ctx, "/tavily")); n != 0 {
		t.Fatalf("refused request dispatched: %d records", n)
	}
}

func TestM9SearchRequiresRouteHeader(t *testing.T) {
	_, ctx := client(t)
	resp, data := post(t, ctx, "/search", []byte(`{"query":"q"}`), nil)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "missing_required_parameter") {
		t.Fatalf("missing route header: %d %s", resp.StatusCode, data)
	}
}

func TestM9AzureOCRProjectsPages(t *testing.T) {
	_, ctx := client(t)
	resetRecorded(t, ctx)
	slug := env(t, "OLP_CLIENTS_MODEL_AZURE_OCR")
	resp, data := post(t, ctx, "/ocr", []byte(`{"model":"`+slug+`","document":{"type":"document_url","document_url":"https://example.com/report.pdf"}}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("azure ocr: %d %s", resp.StatusCode, data)
	}
	var out struct {
		Model string `json:"model"`
		Pages []struct {
			Index    int    `json:"index"`
			Markdown string `json:"markdown"`
		} `json:"pages"`
		UsageInfo struct {
			PagesProcessed int `json:"pages_processed"`
		} `json:"usage_info"`
	}
	if json.Unmarshal(data, &out) != nil || out.Model != slug || len(out.Pages) != 1 || !strings.Contains(out.Pages[0].Markdown, "Fixture OCR text") || out.UsageInfo.PagesProcessed != 1 {
		t.Fatalf("projected: %s", data)
	}
	records := recorded(t, ctx, "/azure")
	submits, polls := 0, 0
	for _, rec := range records {
		if !rec.Authorized || rec.Leaked {
			t.Fatalf("record: %+v", rec)
		}
		switch {
		case strings.HasSuffix(rec.Path, ":analyze"):
			submits++
			if rec.Headers["ocp-apim-subscription-key"] != "[present]" {
				t.Fatalf("submit credential: %+v", rec.Headers)
			}
		case strings.Contains(rec.Path, "analyzeResults"):
			polls++
		}
	}
	if submits != 1 || polls != 1 {
		t.Fatalf("submits=%d polls=%d records=%+v", submits, polls, records)
	}
}

func TestM9AzureOCRRefusalDispatchesNothing(t *testing.T) {
	_, ctx := client(t)
	resetRecorded(t, ctx)
	slug := env(t, "OLP_CLIENTS_MODEL_AZURE_OCR")
	resp, data := post(t, ctx, "/ocr", []byte(`{"model":"`+slug+`","document":{"type":"document_url","document_url":"https://example.com/report.pdf"},"table_format":"html"}`), nil)
	if resp.StatusCode < 400 {
		t.Fatalf("html table format admitted: %d %s", resp.StatusCode, data)
	}
	if n := len(recorded(t, ctx, "/azure")); n != 0 {
		t.Fatalf("refused request dispatched: %d records", n)
	}
}
