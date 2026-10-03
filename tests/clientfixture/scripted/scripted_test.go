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

func (h *harness) recorded(query string) []Record {
	h.t.Helper()
	r := h.do("GET", RecordedPath+query, nil, nil)
	var out struct {
		Requests []Record `json:"requests"`
		InFlight int      `json:"in_flight"`
	}
	if err := json.Unmarshal(r.body, &out); err != nil || out.InFlight != 0 {
		h.t.Fatalf("recording %s (in flight %d): %v", r.body, out.InFlight, err)
	}
	return out.Requests
}

func want(t *testing.T, r response, status int) {
	t.Helper()
	if r.status != status {
		t.Fatalf("status %d, want %d: %s", r.status, status, r.body)
	}
}

type frame struct{ event, data string }

// frames splits a server-sent event body into its frames.
func frames(t *testing.T, body []byte) []frame {
	t.Helper()
	var out []frame
	for _, raw := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var f frame
		for _, line := range strings.Split(raw, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				f.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				f.data = strings.TrimPrefix(line, "data: ")
			default:
				t.Fatalf("unexpected SSE line %q", line)
			}
		}
		out = append(out, f)
	}
	return out
}

func decodeFrames(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, f := range frames(t, body) {
		if f.data == "[DONE]" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(f.data), &m); err != nil {
			t.Fatalf("frame %q is not JSON: %v", f.data, err)
		}
		out = append(out, m)
	}
	return out
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

func TestEveryVendorRejectsAnotherCredentialAndRecordsIt(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		name, path string
		headers    map[string]string
		body       any
		message    func(map[string]any) any
	}{
		{"openai", OpenAIPrefix + "/chat/completions", map[string]string{"Authorization": "Bearer " + testClientKey}, map[string]any{"model": OpenAIModel}, func(m map[string]any) any { return path(m, "error", "type") }},
		{"anthropic", AnthropicPrefix + "/messages", map[string]string{"X-Api-Key": testClientKey, "Anthropic-Version": "2023-06-01"}, map[string]any{"model": AnthropicModel}, func(m map[string]any) any { return path(m, "error", "type") }},
		{"gemini", GeminiPrefix + "/models/" + GeminiModel + ":generateContent", map[string]string{"X-Goog-Api-Key": testClientKey}, map[string]any{}, func(m map[string]any) any { return path(m, "error", "status") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := h.do("POST", tc.path, tc.headers, tc.body)
			want(t, r, http.StatusUnauthorized)
			if got := tc.message(r.json()); got != "authentication_error" && got != "UNAUTHENTICATED" {
				t.Fatalf("error %v: %s", got, r.body)
			}
		})
	}
	records := h.recorded("")
	if len(records) != 3 {
		t.Fatalf("recorded %d requests, want 3", len(records))
	}
	for _, rec := range records {
		if rec.Authorized || !rec.LeakedClientCredential || rec.Status != http.StatusUnauthorized || !rec.Complete {
			t.Errorf("record %+v: want an unauthorized request that leaked the client credential", rec)
		}
		for name, value := range rec.Headers {
			if strings.Contains(value, testClientKey) || strings.Contains(value, testCredential) {
				t.Errorf("header %s keeps a credential value %q", name, value)
			}
		}
	}
}

// A caller credential is a leak wherever the upstream received it, and whether
// or not the upstream accepted the request: in a header the vendor does not
// read, in the query string, as Gemini takes a key, and in the body.
func TestRecordingFlagsACallerCredentialWhereverItAppears(t *testing.T) {
	generate := GeminiPrefix + "/models/" + GeminiModel + ":generateContent"
	upstream := map[string]string{"X-Goog-Api-Key": testCredential}
	for name, send := range map[string]func(h *harness) response{
		"header": func(h *harness) response {
			return h.do("POST", generate, map[string]string{"X-Goog-Api-Key": testCredential, "X-Forwarded-Authorization": "Bearer " + testClientKey}, map[string]any{"contents": geminiPrompt("hi")})
		},
		"query string": func(h *harness) response {
			return h.do("POST", generate+"?key="+testClientKey, upstream, map[string]any{"contents": geminiPrompt("hi")})
		},
		"query string beside another parameter": func(h *harness) response {
			return h.do("POST", generate+"?alt=sse&key="+testClientKey, upstream, map[string]any{"contents": geminiPrompt("hi")})
		},
		"body": func(h *harness) response {
			return h.do("POST", generate, upstream, map[string]any{"contents": geminiPrompt("my key is " + testClientKey)})
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			want(t, send(h), http.StatusOK)
			records := h.recorded("")
			if len(records) != 1 || !records[0].Authorized || !records[0].LeakedClientCredential {
				t.Fatalf("records %+v: want one authorized request that leaked the client credential", records)
			}
		})
	}

	t.Run("nothing leaked", func(t *testing.T) {
		h := newHarness(t)
		want(t, h.do("POST", generate+"?alt=sse&key="+testCredential, upstream, map[string]any{"contents": geminiPrompt("hi")}), http.StatusOK)
		if records := h.recorded(""); len(records) != 1 || records[0].LeakedClientCredential {
			t.Fatalf("records %+v: want one request that leaked nothing", records)
		}
	})

	// An empty secret is no secret: it must not flag every request.
	t.Run("an empty secret", func(t *testing.T) {
		h := newHarnessWith(t, Options{Credential: testCredential, ClientSecrets: []string{""}})
		want(t, h.do("POST", generate, upstream, map[string]any{"contents": geminiPrompt("hi")}), http.StatusOK)
		if records := h.recorded(""); len(records) != 1 || records[0].LeakedClientCredential {
			t.Fatalf("records %+v: want one request that leaked nothing", records)
		}
	})
}

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

func TestRecordingCapturesTheRequestAndFilters(t *testing.T) {
	h := newHarness(t)
	want(t, h.openai("/chat/completions", map[string]any{"model": OpenAIModel, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}), 200)
	want(t, h.anthropic("/messages/count_tokens", map[string]any{"model": AnthropicModel, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}), 200)
	r := h.do("POST", AnthropicPrefix+"/messages", map[string]string{"X-Api-Key": testCredential, "Anthropic-Version": "2023-06-01", "Anthropic-Beta": "prompt-caching-2024-07-31,context-1m-2025-08-07"},
		map[string]any{"model": AnthropicModel, "max_tokens": 5, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	want(t, r, 200)

	all := h.recorded("")
	if len(all) != 3 || all[0].Seq != 1 || all[2].Seq != 3 {
		t.Fatalf("recording %+v", all)
	}
	chat := all[0]
	if chat.Dialect != "openai.chat" || chat.Path != OpenAIPrefix+"/chat/completions" || chat.Model != OpenAIModel || chat.Method != "POST" ||
		!chat.Authorized || chat.Status != 200 || !chat.Complete || chat.Script != "text" || chat.Stream || chat.LeakedClientCredential {
		t.Fatalf("chat record %+v", chat)
	}
	if chat.Headers["authorization"] != "[present]" || chat.Headers["content-type"] != "application/json" {
		t.Fatalf("headers %v", chat.Headers)
	}
	if got := all[2].Headers["anthropic-beta"]; got != "prompt-caching-2024-07-31,context-1m-2025-08-07" {
		t.Fatalf("anthropic-beta recorded as %q", got)
	}
	var body struct{ Model string }
	if json.Unmarshal(chat.Body, &body) != nil || body.Model != OpenAIModel {
		t.Fatalf("body %s", chat.Body)
	}

	for query, count := range map[string]int{
		"?dialect=anthropic":            2,
		"?dialect=anthropic.messages":   1,
		"?path=" + OpenAIPrefix:         1,
		"?model=" + AnthropicModel:      2,
		"?model=nope":                   0,
		"?script=count_tokens":          1,
		"?dialect=anthropic&model=nope": 0,
	} {
		if got := len(h.recorded(query)); got != count {
			t.Errorf("filter %s matched %d, want %d", query, got, count)
		}
	}
}

func TestResetForgetsRecordingsStoredResponsesAndCaches(t *testing.T) {
	h := newHarness(t)
	created := h.openai("/responses", map[string]any{"model": OpenAIModel, "input": "hi"})
	want(t, created, 200)
	id := created.json()["id"].(string)
	cached := map[string]any{"model": AnthropicModel, "max_tokens": 5, "system": []any{map[string]any{"type": "text", "text": "stable prefix", "cache_control": map[string]any{"type": "ephemeral"}}}, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	want(t, h.anthropic("/messages", cached), 200)

	want(t, h.do("DELETE", RecordedPath, nil, nil), http.StatusNoContent)
	if n := len(h.recorded("")); n != 0 {
		t.Fatalf("%d records survived the reset", n)
	}
	want(t, h.do("GET", OpenAIPrefix+"/responses/"+id, map[string]string{"Authorization": "Bearer " + testCredential}, nil), http.StatusNotFound)
	usage := h.anthropic("/messages", cached).json()["usage"].(map[string]any)
	if usage["cache_creation_input_tokens"].(float64) == 0 || usage["cache_read_input_tokens"].(float64) != 0 {
		t.Fatalf("the cache survived the reset: %v", usage)
	}
	// Identifiers restart, so a script replays identically.
	if again := h.openai("/responses", map[string]any{"model": OpenAIModel, "input": "hi"}).json()["id"]; again != id {
		t.Fatalf("response id %v after reset, want %v", again, id)
	}
}

func TestOversizedBodiesAreRefused(t *testing.T) {
	h := newHarness(t)
	big := strings.Repeat("x", maxBodyBytes+1)
	want(t, h.openai("/chat/completions", `{"model":"`+big+`"}`), http.StatusRequestEntityTooLarge)
}

func TestConcurrentRequestsAndListingsAreSafeAndAllRecorded(t *testing.T) {
	h := newHarness(t)
	const workers = 24
	done := make(chan struct{})
	for i := 0; i < workers; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			body := map[string]any{"model": OpenAIModel, "stream": true, "messages": []any{user("hi")}}
			if h.openai("/chat/completions", body).status != 200 || h.anthropic("/messages", map[string]any{"model": AnthropicModel, "max_tokens": 4, "messages": []any{user("hi")}}).status != 200 {
				t.Error("a concurrent request failed")
			}
		}()
	}
	// List while requests are in flight; a listing never sees a torn record.
	for i := 0; i < 50; i++ {
		r := h.do("GET", RecordedPath, nil, nil)
		want(t, r, 200)
	}
	for i := 0; i < workers; i++ {
		<-done
	}
	records := h.recorded("")
	if len(records) != 2*workers {
		t.Fatalf("recorded %d requests, want %d", len(records), 2*workers)
	}
	seen := map[int]bool{}
	for _, rec := range records {
		if seen[rec.Seq] || !rec.Complete || rec.Status != 200 || rec.Script == "" {
			t.Fatalf("record %+v", rec)
		}
		seen[rec.Seq] = true
	}
}
