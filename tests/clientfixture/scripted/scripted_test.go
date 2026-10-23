package scripted

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testCredential = "test-upstream-credential"
	testClientKey  = "olp_test_client_key"
)

// harness runs the fixture behind a real HTTP server.
type harness struct {
	t       *testing.T
	fixture *Fixture
	server  *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, Options{Credential: testCredential, ClientSecrets: []string{testClientKey}})
}

func newHarnessWith(t *testing.T, opts Options) *harness {
	t.Helper()
	f := New(opts)
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	return &harness{t: t, fixture: f, server: server}
}

type response struct {
	status  int
	headers http.Header
	body    []byte
}

func (r response) json() map[string]any {
	var out map[string]any
	if err := json.Unmarshal(r.body, &out); err != nil {
		panic("response is not a JSON object: " + string(r.body))
	}
	return out
}

// do sends a request with the credential header of the given vendor unless
// headers already carry one.
func (h *harness) do(method, path string, headers map[string]string, body any) response {
	h.t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			h.t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, h.server.URL+path, reader)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	return response{resp.StatusCode, resp.Header, data}
}

func (h *harness) openai(path string, body any) response {
	h.t.Helper()
	return h.do("POST", OpenAIPrefix+path, map[string]string{"Authorization": "Bearer " + testCredential}, body)
}

func (h *harness) anthropic(path string, body any) response {
	h.t.Helper()
	return h.do("POST", AnthropicPrefix+path, map[string]string{"X-Api-Key": testCredential, "Anthropic-Version": "2023-06-01"}, body)
}

func (h *harness) gemini(action string, body any) response {
	h.t.Helper()
	return h.do("POST", GeminiPrefix+"/models/"+GeminiModel+":"+action, map[string]string{"X-Goog-Api-Key": testCredential}, body)
}

func want(t *testing.T, r response, status int) {
	t.Helper()
	if r.status != status {
		t.Fatalf("status %d, want %d: %s", r.status, status, r.body)
	}
}

func path(v any, keys ...any) any {
	for _, k := range keys {
		switch key := k.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[key]
		case int:
			s, ok := v.([]any)
			if !ok || key >= len(s) {
				return nil
			}
			v = s[key]
		}
	}
	return v
}

// An empty secret is no secret: it must not flag every request.

func TestEveryVendorRateLimitNamesWhenToRetry(t *testing.T) {
	h := newHarness(t)
	limited := user(`[[olp:fail 429]]`)
	for name, r := range map[string]response{
		"openai":    h.openai("/chat/completions", map[string]any{"model": OpenAIModel, "messages": []any{limited}}),
		"anthropic": h.anthropic("/messages", map[string]any{"model": AnthropicModel, "max_tokens": 8, "messages": []any{limited}}),
		"gemini":    h.gemini("generateContent", map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": `[[olp:fail 429]]`}}}}}),
	} {
		want(t, r, http.StatusTooManyRequests)
		if got := r.headers.Get("Retry-After"); got != RetryAfterSeconds {
			t.Errorf("%s: Retry-After %q, want %q", name, got, RetryAfterSeconds)
		}
	}
	// Only a rate limit names a delay.
	r := h.openai("/chat/completions", map[string]any{"model": OpenAIModel, "messages": []any{user(`[[olp:fail 500]]`)}})
	want(t, r, http.StatusInternalServerError)
	if got := r.headers.Get("Retry-After"); got != "" {
		t.Errorf("a 500 carries Retry-After %q", got)
	}
}

// Identifiers restart, so a script replays identically.

// List while requests are in flight; a listing never sees a torn record.
