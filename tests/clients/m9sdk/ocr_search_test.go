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
	Method     string            `json:"method"`
	Path       string            `json:"path"`
	Query      string            `json:"query"`
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

func TestM9BraveSearchUsesGetQueryTransport(t *testing.T) {
	_, ctx := client(t)
	resetRecorded(t, ctx)
	slug := env(t, "OLP_CLIENTS_MODEL_SEARCH_BRAVE")
	resp, data := post(t, ctx, "/search", []byte(`{"query":"q","max_results":2,"search_after_date_filter":"01/02/2026","search_before_date_filter":"03/04/2026"}`), map[string]string{"X-OLP-Route": slug})
	var out struct {
		ID      string `json:"id"`
		Results []struct {
			Title, URL, Snippet string
		} `json:"results"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(data, &out) != nil || !strings.HasPrefix(out.ID, "search_") || len(out.Results) != 1 || out.Results[0].Snippet != "Fixture description." {
		t.Fatalf("brave search: %d %s", resp.StatusCode, data)
	}
	records := recorded(t, ctx, "/brave")
	if len(records) != 1 || records[0].Method != http.MethodGet || records[0].Path != "/brave/res/v1/web/search" || !records[0].Authorized || records[0].Leaked {
		t.Fatalf("brave upstream records: %+v", records)
	}
	if !strings.Contains(records[0].Query, "count=2") || !strings.Contains(records[0].Query, "freshness=2026-01-02to2026-03-04") || len(records[0].Body) != 0 {
		t.Fatalf("brave upstream query: %q body=%s", records[0].Query, records[0].Body)
	}

	resetRecorded(t, ctx)
	resp, data = post(t, ctx, "/search", []byte(`{"query":"q","search_domain_filter":["a.com"]}`), map[string]string{"X-OLP-Route": slug})
	if resp.StatusCode < 400 {
		t.Fatalf("domain filter admitted: %d %s", resp.StatusCode, data)
	}
	if n := len(recorded(t, ctx, "/brave")); n != 0 {
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
