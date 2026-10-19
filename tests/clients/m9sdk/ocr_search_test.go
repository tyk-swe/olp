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

// checkRecording holds every recorded upstream request to credential safety:
// authorized by the upstream credential and never carrying the client's key.
func checkRecording(t *testing.T, records []recordedRequest) {
	t.Helper()
	for _, rec := range records {
		if !rec.Authorized || rec.Leaked {
			t.Fatalf("credential isolation failed: %+v", rec)
		}
	}
}

func TestM9NativeOCR(t *testing.T) {
	_, ctx := client(t)
	resetRecorded(t, ctx)
	slug := env(t, "OLP_CLIENTS_MODEL_MISTRAL_OCR")
	body := `{"model":"` + slug + `","document":{"type":"document_url","document_url":"https://fixture.example/report.pdf"}}`
	resp, data := post(t, ctx, "/ocr", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ocr: %d %s", resp.StatusCode, data)
	}
	var out struct {
		Model string `json:"model"`
		Pages []struct {
			Index    int    `json:"index"`
			Markdown string `json:"markdown"`
		} `json:"pages"`
		Usage struct {
			PagesProcessed int `json:"pages_processed"`
		} `json:"usage_info"`
	}
	if json.Unmarshal(data, &out) != nil || out.Model != slug || len(out.Pages) != 1 || out.Pages[0].Index != 0 || out.Pages[0].Markdown == "" || out.Usage.PagesProcessed != 1 {
		t.Fatalf("ocr result: %s", data)
	}
	records := recorded(t, ctx, "/mistral")
	if len(records) != 1 || records[0].Path != "/mistral/v1/ocr" {
		t.Fatalf("upstream recording: %+v", records)
	}
	var upstream map[string]any
	_ = json.Unmarshal(records[0].Body, &upstream)
	if upstream["model"] != env(t, "OLP_CLIENTS_UPSTREAM_MODEL_MISTRAL") {
		t.Fatalf("route binding did not rewrite the model: %s", records[0].Body)
	}
	checkRecording(t, records)
}

func TestM9Search(t *testing.T) {
	_, ctx := client(t)
	resetRecorded(t, ctx)
	for _, tc := range []struct {
		env, dialect, snippet string
		native                bool
	}{
		{"OLP_CLIENTS_MODEL_PERPLEXITY_SEARCH", "perplexity", "Fixture snippet.", true},
		{"OLP_CLIENTS_MODEL_SEARCH_TAVILY", "tavily", "Fixture content.", false},
		{"OLP_CLIENTS_MODEL_SEARCH_EXA", "exa", "Fixture highlight one.\n...\nFixture highlight two.", false},
		{"OLP_CLIENTS_MODEL_SEARCH_PARALLEL", "parallel", "Fixture excerpt.", false},
	} {
		slug := env(t, tc.env)
		resp, data := post(t, ctx, "/search", []byte(`{"query":"fixture query"}`), map[string]string{"X-OLP-Route": slug})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", tc.dialect, resp.StatusCode, data)
		}
		var out struct {
			ID      string `json:"id"`
			Results []struct {
				Title   string  `json:"title"`
				URL     string  `json:"url"`
				Snippet string  `json:"snippet"`
				Date    *string `json:"date"`
			} `json:"results"`
		}
		if json.Unmarshal(data, &out) != nil || len(out.Results) != 1 {
			t.Fatalf("%s: %s", tc.dialect, data)
		}
		row := out.Results[0]
		if row.URL != "https://fixture.example/result" || row.Snippet != tc.snippet {
			t.Fatalf("%s row: %+v", tc.dialect, row)
		}
		if tc.native {
			if !strings.HasPrefix(out.ID, "ppx_fx_") {
				t.Fatalf("native response identity: %s", data)
			}
		} else {
			if !strings.HasPrefix(out.ID, "search_") || strings.HasPrefix(out.ID, "search_fx_") {
				t.Fatalf("%s: upstream identity leaked: %q", tc.dialect, out.ID)
			}
		}
	}
	for dialect, prefix := range map[string]string{
		"perplexity": "/perplexity", "tavily": "/tavily", "exa": "/exa", "parallel": "/parallel",
	} {
		records := recorded(t, ctx, prefix)
		if len(records) != 1 {
			t.Fatalf("%s upstream calls: %+v", dialect, records)
		}
		rec := records[0]
		if rec.Path != prefix+"/search" && rec.Path != "/parallel/v1/search" {
			t.Fatalf("%s upstream path: %s", dialect, rec.Path)
		}
		var body map[string]any
		_ = json.Unmarshal(rec.Body, &body)
		if body["model"] != nil {
			t.Fatalf("%s: model field reached upstream: %s", dialect, rec.Body)
		}
		if dialect == "parallel" && (rec.Headers["x-api-key"] != "[present]" || rec.Headers["authorization"] != "") {
			t.Fatalf("parallel credential headers: %v", rec.Headers)
		}
	}
	checkRecording(t, recorded(t, ctx, "/"))
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
