package gateway

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/tests/fixtures"
)

const (
	routeSlug  = "team-chat"
	fullKey    = "olp_full_key"
	readKey    = "olp_read_key"
	otherKey   = "olp_other_key"
	secretA    = "secret-a"
	secretB    = "secret-b"
	modelA     = "model-a"
	modelB     = "model-b"
	answerText = "hello from upstream"
)

type fakeRuntime struct {
	mu      sync.Mutex
	release *runtime.Release
	keys    map[string]access.Authority
	stale   bool
	revoked map[string]bool
}

func (f *fakeRuntime) Release() *runtime.Release { f.mu.Lock(); defer f.mu.Unlock(); return f.release }

func (f *fakeRuntime) Authenticate(secret string) (access.Authority, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stale {
		return access.Authority{}, runtime.ErrStaleAuthority
	}
	a, ok := f.keys[secret]
	if !ok {
		return access.Authority{}, runtime.ErrInvalidKey
	}
	return a, nil
}

func (f *fakeRuntime) Revoked(id string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.revoked[id] }

type capture struct {
	mu   sync.Mutex
	envs []Envelope
}

func (c *capture) Terminal(e Envelope) { c.mu.Lock(); defer c.mu.Unlock(); c.envs = append(c.envs, e) }

func (c *capture) last(t *testing.T) Envelope {
	t.Helper()
	// Reading a known-length HTTP response can finish before the serving
	// goroutine records its terminal event. Await that event explicitly.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		if len(c.envs) != 0 {
			event := c.envs[len(c.envs)-1]
			c.mu.Unlock()
			return event
		}
		c.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no envelope emitted within one second")
	return Envelope{}
}

// mock is the upstream: one handler per provider prefix, swappable per test.
type mock struct {
	mu       sync.Mutex
	handlers map[string]http.HandlerFunc
	calls    map[string]int
}

func (m *mock) set(provider string, h http.HandlerFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers[provider] = h
}

func (m *mock) count(provider string) int { m.mu.Lock(); defer m.mu.Unlock(); return m.calls[provider] }

func (m *mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	provider := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)[0]
	m.mu.Lock()
	h := m.handlers[provider]
	m.calls[provider]++
	m.mu.Unlock()
	if h == nil {
		http.Error(w, "unexpected provider", http.StatusTeapot)
		return
	}
	h(w, r)
}

func completion(model, text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`, model, text)
	}
}

func status(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		io.WriteString(w, body)
	}
}

type harness struct {
	t        *testing.T
	rt       *fakeRuntime
	mock     *mock
	sink     *capture
	gateway  *Server
	server   *httptest.Server
	keyID    string
	slotA    string
	credA    string
	upstream *httptest.Server
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	m := &mock{handlers: map[string]http.HandlerFunc{}, calls: map[string]int{}}
	m.set("a", completion(modelA, answerText))
	m.set("b", completion(modelB, answerText))
	upstream := httptest.NewServer(m)
	t.Cleanup(upstream.Close)
	credA, credB := uuid.NewString(), uuid.NewString()
	version := 1
	slotA := uuid.NewString()
	provider := func(name, secretID, model, slot string) runtime.Provider {
		id := uuid.NewString()
		return runtime.Provider{
			ID: id, Name: name, Kind: "openai_compatible", Enabled: true, ActiveCredential: &secretID, RevisionID: uuid.NewString(),
			Capabilities: []runtime.Capability{{Model: model, Operation: "generation", Surface: "openai", Mode: "unary"}, {Model: model, Operation: "generation", Surface: "openai", Mode: "streaming"}},
			Endpoint:     upstream.URL + "/" + name + "/v1", AuthMode: "api_key",
			Slots: []runtime.Slot{{ID: slot, Name: "default", Enabled: true, Weight: 1, CredentialID: &secretID, CredentialVersion: &version}},
		}
	}
	a := provider("a", credA, modelA, slotA)
	b := provider("b", credB, modelB, uuid.NewString())
	routeID := uuid.NewString()
	snapshot := &runtime.Snapshot{
		Generation: runtime.Generation{ID: uuid.NewString(), Ordinal: 1, ActivatedAt: time.Now()},
		Providers:  map[string]runtime.Provider{a.ID: a, b.ID: b},
		Routes: map[string]runtime.Route{routeSlug: {
			ID: routeID, Slug: routeSlug, Operations: []string{"generation"}, OverallTimeout: 5000, MaxAttempts: 3, RoutingID: routeID, RevisionID: uuid.NewString(), Revision: 1, PublishedAt: time.Now(),
			Targets: []runtime.Target{
				{ID: uuid.NewString(), ProviderID: a.ID, ProviderModel: modelA, Priority: 0, Weight: 1, Timeout: 2000, RoutingID: uuid.NewString()},
				{ID: uuid.NewString(), ProviderID: b.ID, ProviderModel: modelB, Priority: 1, Weight: 1, Timeout: 2000, RoutingID: uuid.NewString()},
			},
		}},
	}
	release, err := runtime.NewRelease(uuid.NewString(), 7, snapshot, map[string][]byte{credA: []byte(secretA), credB: []byte(secretB)})
	if err != nil {
		t.Fatal(err)
	}
	keyID := uuid.NewString()
	rt := &fakeRuntime{release: release, revoked: map[string]bool{}, keys: map[string]access.Authority{
		fullKey:  {ID: keyID, Policy: access.KeyPolicy{Scopes: []string{"inference", "models_read"}}},
		readKey:  {ID: uuid.NewString(), Policy: access.KeyPolicy{Scopes: []string{"models_read"}}},
		otherKey: {ID: uuid.NewString(), Policy: access.KeyPolicy{Scopes: []string{"inference", "models_read"}, AllowedRoutes: []string{"another-route"}}},
	}}
	policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	if cfg.MaxInFlight == 0 {
		cfg = Config{MaxInFlight: 8, MaxBodyBytes: 64 * 1024, MaxResponseBytes: 1 << 20, MaxEventBytes: 4096}
	}
	sink := &capture{}
	gw := New(rt, &policy, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	gw.Sink = sink
	mux := http.NewServeMux()
	gw.Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return &harness{t: t, rt: rt, mock: m, sink: sink, gateway: gw, server: server, keyID: keyID, slotA: slotA, credA: credA, upstream: upstream}
}

func (h *harness) do(ctx context.Context, method, path, key string, body []byte, headers map[string]string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, h.server.URL+path, bytes.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func (h *harness) chat(key string, extra map[string]string, fields ...string) (*http.Response, map[string]any) {
	h.t.Helper()
	body := `{"model":"` + routeSlug + `","messages":[{"role":"user","content":"hi"}]` + strings.Join(fields, "") + `}`
	resp := h.do(h.t.Context(), http.MethodPost, "/v1/chat/completions", key, []byte(body), extra)
	defer resp.Body.Close()
	var decoded map[string]any
	data, _ := io.ReadAll(resp.Body)
	if len(data) > 0 {
		if err := json.Unmarshal(data, &decoded); err != nil {
			h.t.Fatalf("invalid JSON response %q: %v", data, err)
		}
	}
	return resp, decoded
}

func errorCode(t *testing.T, body map[string]any) string {
	t.Helper()
	e, _ := body["error"].(map[string]any)
	if e == nil {
		t.Fatalf("no error object in %v", body)
	}
	code, _ := e["code"].(string)
	return code
}

func TestUnaryChatRewritesModelAndEmitsEnvelope(t *testing.T) {
	h := newHarness(t, Config{})
	resp, body := h.chat(fullKey, map[string]string{"X-Request-Id": "req-123"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
	if body["model"] != routeSlug {
		t.Fatalf("model not rewritten: %v", body["model"])
	}
	if resp.Header.Get("X-Request-Id") != "req-123" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("headers %v", resp.Header)
	}
	env := h.sink.last(t)
	if env.Outcome != "success" || env.Status != 200 || env.RequestID != "req-123" || env.ClientIP != "127.0.0.1" || env.KeyID != h.keyID || env.Route != routeSlug || env.ReleaseSequence != 7 || env.Usage == nil || env.Usage.TotalTokens != 5 {
		t.Fatalf("envelope %+v", env)
	}
	if len(env.Attempts) != 1 || env.Attempts[0].Class != classSuccess || env.Attempts[0].SlotID != h.slotA || env.Attempts[0].CredentialID != h.credA || env.Attempts[0].CredentialVersion != 1 {
		t.Fatalf("attempts %+v", env.Attempts)
	}
}

func TestCredentialInjection(t *testing.T) {
	h := newHarness(t, Config{})
	seen := make(chan string, 1)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		completion(modelA, answerText)(w, r)
	})
	if resp, _ := h.chat(fullKey, nil); resp.StatusCode != http.StatusOK {
		t.Fatal(resp.Status)
	}
	if got := <-seen; got != "Bearer "+secretA {
		t.Fatalf("authorization %q", got)
	}
}

func TestStreamingChatFragmented(t *testing.T) {
	h := newHarness(t, Config{})
	data, err := fixtures.Files.ReadFile("streams/openai-chat.sse")
	if err != nil {
		t.Fatal(err)
	}
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		if err := testutil.Stream(w, r, data, 1, 0); err != nil {
			t.Error(err)
		}
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"hi"}],"stream":true}`), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d type %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var text string
	done := false
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			done = true
			continue
		}
		var chunk struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatal(err)
		}
		if chunk.Model != routeSlug {
			t.Fatalf("chunk model %q", chunk.Model)
		}
		for _, c := range chunk.Choices {
			text += c.Delta.Content
		}
	}
	if text != "héllo 🌍" || !done {
		t.Fatalf("text %q done %v", text, done)
	}
	env := h.sink.last(t)
	if env.Outcome != "success" || env.Mode != "streaming" || !env.Committed || env.Usage == nil || env.Usage.TotalTokens != 5 {
		t.Fatalf("envelope %+v", env)
	}
}

func TestPreCommitFailoverWithinBudget(t *testing.T) {
	h := newHarness(t, Config{})
	h.mock.set("a", status(http.StatusServiceUnavailable, `{"error":{"message":"down","type":"server_error"}}`))
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusOK || body["model"] != routeSlug {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
	env := h.sink.last(t)
	if len(env.Attempts) != 2 || env.Attempts[0].Class != classUpstreamServer || env.Attempts[0].Status != 503 || env.Attempts[1].Class != classSuccess || env.Attempts[1].Ordinal != 2 {
		t.Fatalf("attempts %+v", env.Attempts)
	}
	if h.mock.count("a") != 1 || h.mock.count("b") != 1 {
		t.Fatalf("calls a=%d b=%d", h.mock.count("a"), h.mock.count("b"))
	}
}

func TestRateLimitCoolsSlotAndFailsOver(t *testing.T) {
	h := newHarness(t, Config{})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		status(http.StatusTooManyRequests, `{"error":{"message":"slow down","type":"rate_limit_error"}}`)(w, r)
	})
	resp, _ := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.Status)
	}
	env := h.sink.last(t)
	if len(env.Attempts) != 2 || env.Attempts[0].Class != classRateLimit || env.Attempts[1].Class != classSuccess {
		t.Fatalf("attempts %+v", env.Attempts)
	}
	// The slot cooled down for the advertised interval: the next request
	// skips provider a without consuming an attempt.
	if resp, _ := h.chat(fullKey, nil); resp.StatusCode != http.StatusOK {
		t.Fatal(resp.Status)
	}
	if h.mock.count("a") != 1 || len(h.sink.last(t).Attempts) != 1 {
		t.Fatalf("provider a called %d times during cooldown", h.mock.count("a"))
	}
}

func TestRateLimitPropagatesRetryAfterWhenExhausted(t *testing.T) {
	h := newHarness(t, Config{})
	limited := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		status(http.StatusTooManyRequests, `{"error":{"message":"slow down","type":"rate_limit_error"}}`)(w, r)
	}
	h.mock.set("a", limited)
	h.mock.set("b", limited)
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusTooManyRequests || errorCode(t, body) != "upstream_rate_limit" || resp.Header.Get("Retry-After") != "7" {
		t.Fatalf("status %d retry-after %q body %v", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	env := h.sink.last(t)
	if env.Outcome != "failure" || env.Status != 429 || len(env.Attempts) != 2 {
		t.Fatalf("envelope %+v", env)
	}
}

func TestCredentialFailureCoolsVersionAndFailsOver(t *testing.T) {
	h := newHarness(t, Config{})
	h.mock.set("a", status(http.StatusUnauthorized, `{"error":{"message":"bad key","type":"invalid_request_error","code":"invalid_api_key"}}`))
	resp, _ := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.Status)
	}
	env := h.sink.last(t)
	if env.Attempts[0].Class != classCredential || env.Attempts[1].Class != classSuccess {
		t.Fatalf("attempts %+v", env.Attempts)
	}
	if !h.gateway.health.coolingDown(env.Attempts[0].ProviderID, "credential:"+env.Attempts[0].CredentialID) {
		t.Fatal("rejected credential version should be cooling down")
	}
}

func TestUpstreamClientErrorIsTerminal(t *testing.T) {
	h := newHarness(t, Config{})
	h.mock.set("a", status(http.StatusBadRequest, `{"error":{"message":"context too long","type":"invalid_request_error","code":"context_length_exceeded"}}`))
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusBadRequest || errorCode(t, body) != "upstream_rejected" {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
	if h.mock.count("b") != 0 {
		t.Fatal("terminal client error must not fail over")
	}
}

func TestAttemptBudgetAndRoutingHeader(t *testing.T) {
	h := newHarness(t, Config{})
	h.mock.set("a", status(http.StatusBadGateway, `{}`))
	resp, body := h.chat(fullKey, map[string]string{routingHeader: `{"strategy":"weighted","max_attempts":1}`})
	if resp.StatusCode != http.StatusBadGateway || errorCode(t, body) != "upstream_unavailable" || h.mock.count("b") != 0 {
		t.Fatalf("status %d body %v calls b=%d", resp.StatusCode, body, h.mock.count("b"))
	}
	for _, header := range []string{`{"max_attempts":999}`, `{"strategy":"unknown"}`, `{"unknown":1}`, `not json`} {
		resp, body := h.chat(fullKey, map[string]string{routingHeader: header})
		if resp.StatusCode != http.StatusBadRequest || errorCode(t, body) != "invalid_request" {
			t.Fatalf("%s: status %d body %v", header, resp.StatusCode, body)
		}
	}
}

func TestPostCommitFailureCannotRestart(t *testing.T) {
	h := newHarness(t, Config{})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"model-a","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`+"\n\n")
		w.(http.Flusher).Flush()
		// Connection drops mid-stream without a completion marker.
		panic(http.ErrAbortHandler)
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"hi"}],"stream":true}`), nil)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)
	if resp.StatusCode != http.StatusOK || !strings.Contains(text, `"content":"partial"`) || strings.Contains(text, "[DONE]") {
		t.Fatalf("status %d body %q", resp.StatusCode, text)
	}
	if !strings.Contains(text, `"code":"provider_protocol_error"`) {
		t.Fatalf("committed failure must be signalled in-band: %q", text)
	}
	env := h.sink.last(t)
	if env.Outcome != "failure" || !env.Committed || len(env.Attempts) != 1 || env.Attempts[0].Class != classProtocol || !env.Attempts[0].Committed {
		t.Fatalf("envelope %+v", env)
	}
	if h.mock.count("b") != 0 {
		t.Fatal("a committed stream must never restart on another provider")
	}
}

func TestOversizedEventFailsBeforeCommit(t *testing.T) {
	h := newHarness(t, Config{})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"padding\":%q}\n\n", strings.Repeat("x", 5000))
	})
	resp, body := h.chat(fullKey, nil, `,"stream":true`)
	if resp.StatusCode != http.StatusBadGateway || errorCode(t, body) != "provider_protocol_error" {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
}

func TestOversizedUnaryResponseRejected(t *testing.T) {
	h := newHarness(t, Config{MaxInFlight: 8, MaxBodyBytes: 64 * 1024, MaxResponseBytes: 512, MaxEventBytes: 256})
	h.mock.set("a", completion(modelA, strings.Repeat("y", 1000)))
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusBadGateway || errorCode(t, body) != "provider_protocol_error" {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
}

func TestAuthenticationAndAuthorization(t *testing.T) {
	h := newHarness(t, Config{})
	cases := []struct {
		name, key, header, code string
		status                  int
	}{
		{"missing key", "", "", "invalid_api_key", 401},
		{"unknown key", "olp_nope", "", "invalid_api_key", 401},
		{"scope missing", readKey, "", "permission_denied", 403},
		{"route not allowed", otherKey, "", "route_forbidden", 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := h.chat(tc.key, nil)
			if resp.StatusCode != tc.status || errorCode(t, body) != tc.code {
				t.Fatalf("status %d body %v", resp.StatusCode, body)
			}
		})
	}
	resp := h.do(t.Context(), http.MethodGet, "/v1/models", "", nil, map[string]string{"x-litellm-api-key": fullKey})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("retired header authenticated: %d", resp.StatusCode)
	}
	h.rt.mu.Lock()
	h.rt.stale = true
	h.rt.mu.Unlock()
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusServiceUnavailable || errorCode(t, body) != "authority_unavailable" {
		t.Fatalf("stale authority: status %d body %v", resp.StatusCode, body)
	}
}

func TestRequestValidation(t *testing.T) {
	h := newHarness(t, Config{})
	post := func(body []byte, headers map[string]string) (*http.Response, map[string]any) {
		resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, body, headers)
		defer resp.Body.Close()
		var decoded map[string]any
		json.NewDecoder(resp.Body).Decode(&decoded)
		return resp, decoded
	}
	if resp, body := post([]byte(`{"model":"nope","messages":[{"role":"user","content":"x"}]}`), nil); resp.StatusCode != 404 || errorCode(t, body) != "route_not_found" {
		t.Fatalf("unknown model: %d %v", resp.StatusCode, body)
	}
	if resp, body := post([]byte(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"x"}],"max_tokens":1,"max_completion_tokens":2}`), nil); resp.StatusCode != 400 || errorCode(t, body) != "invalid_value" {
		t.Fatalf("conflicting limits: %d %v", resp.StatusCode, body)
	}
	if resp, body := post([]byte(`{"model":`), nil); resp.StatusCode != 400 || errorCode(t, body) != "invalid_json" {
		t.Fatalf("invalid json: %d %v", resp.StatusCode, body)
	}
	if resp, body := post([]byte(`{}`), map[string]string{"Content-Type": "text/plain"}); resp.StatusCode != 415 || errorCode(t, body) != "unsupported_media_type" {
		t.Fatalf("media type: %d %v", resp.StatusCode, body)
	}
	if resp, body := post([]byte(`{}`), map[string]string{"Content-Encoding": "br"}); resp.StatusCode != 415 || errorCode(t, body) != "unsupported_content_encoding" {
		t.Fatalf("encoding: %d %v", resp.StatusCode, body)
	}
	big := []byte(`{"model":"` + routeSlug + `","messages":[{"role":"user","content":"` + strings.Repeat("z", 70*1024) + `"}]}`)
	if resp, body := post(big, nil); resp.StatusCode != 413 || errorCode(t, body) != "request_too_large" {
		t.Fatalf("oversized: %d %v", resp.StatusCode, body)
	}
	var packed bytes.Buffer
	gz := gzip.NewWriter(&packed)
	gz.Write(big)
	gz.Close()
	if resp, body := post(packed.Bytes(), map[string]string{"Content-Encoding": "gzip"}); resp.StatusCode != 413 || errorCode(t, body) != "request_too_large" {
		t.Fatalf("oversized after decompression: %d %v", resp.StatusCode, body)
	}
	packed.Reset()
	gz = gzip.NewWriter(&packed)
	gz.Write([]byte(`{"model":"` + routeSlug + `","messages":[{"role":"user","content":"hi"}]}`))
	gz.Close()
	if resp, body := post(packed.Bytes(), map[string]string{"Content-Encoding": "gzip"}); resp.StatusCode != 200 {
		t.Fatalf("gzip request: %d %v", resp.StatusCode, body)
	}
}

func TestModelsFilteredByKey(t *testing.T) {
	h := newHarness(t, Config{})
	list := func(key, path string) (int, map[string]any) {
		resp := h.do(t.Context(), http.MethodGet, path, key, nil, nil)
		defer resp.Body.Close()
		var body map[string]any
		json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}
	if code, body := list(fullKey, "/v1/models"); code != 200 || len(body["data"].([]any)) != 1 || body["data"].([]any)[0].(map[string]any)["id"] != routeSlug {
		t.Fatalf("%d %v", code, body)
	}
	if code, body := list(otherKey, "/v1/models"); code != 200 || len(body["data"].([]any)) != 0 {
		t.Fatalf("restricted key sees %v", body)
	}
	if code, body := list(fullKey, "/v1/models/"+routeSlug); code != 200 || body["owned_by"] != "openllmproxy" {
		t.Fatalf("%d %v", code, body)
	}
	if code, body := list(otherKey, "/v1/models/"+routeSlug); code != 404 || errorCode(t, body) != "route_not_found" {
		t.Fatalf("%d %v", code, body)
	}
	if code, _ := list(fullKey, "/v1/not-enabled"); code != 404 {
		t.Fatalf("unknown endpoint %d", code)
	}
}

func TestAdmissionLimit(t *testing.T) {
	h := newHarness(t, Config{MaxInFlight: 1, MaxBodyBytes: 64 * 1024, MaxResponseBytes: 1 << 20, MaxEventBytes: 4096})
	hold := make(chan struct{})
	entered := make(chan struct{})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-hold
		completion(modelA, answerText)(w, r)
	})
	first := make(chan int, 1)
	go func() {
		resp, _ := h.chat(fullKey, nil)
		first <- resp.StatusCode
	}()
	<-entered
	resp, body := h.chat(fullKey, nil)
	close(hold)
	if code := <-first; code != http.StatusOK {
		t.Fatalf("admitted request failed with %d", code)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || errorCode(t, body) != "request_admission_overloaded" {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
}

func TestClientCancellationClosesUpstream(t *testing.T) {
	h := newHarness(t, Config{})
	upstreamDone := make(chan struct{})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"model-a","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`+"\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamDone)
	})
	ctx, cancel := context.WithCancel(t.Context())
	resp := h.do(ctx, http.MethodPost, "/v1/chat/completions", fullKey, []byte(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"hi"}],"stream":true}`), nil)
	buf := make([]byte, 16)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatal(err)
	}
	cancel()
	resp.Body.Close()
	select {
	case <-upstreamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request was not cancelled")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.sink.mu.Lock()
		n := len(h.sink.envs)
		h.sink.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected one envelope, have %d", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	env := h.sink.last(t)
	if env.Outcome != "cancelled" || env.Status != 0 || !env.Committed || env.Attempts[0].Class != classCancelled {
		t.Fatalf("envelope %+v", env)
	}
	if h.mock.count("b") != 0 {
		t.Fatal("cancellation must not fail over")
	}
}

func TestCircuitOpensAfterRepeatedFailures(t *testing.T) {
	h := newHarness(t, Config{})
	h.mock.set("a", status(http.StatusInternalServerError, `{}`))
	for i := 0; i < circuitFailures; i++ {
		if resp, _ := h.chat(fullKey, nil); resp.StatusCode != http.StatusOK {
			t.Fatal(resp.Status)
		}
	}
	if h.mock.count("a") != circuitFailures {
		t.Fatalf("provider a called %d times", h.mock.count("a"))
	}
	if resp, _ := h.chat(fullKey, nil); resp.StatusCode != http.StatusOK {
		t.Fatal(resp.Status)
	}
	if h.mock.count("a") != circuitFailures {
		t.Fatal("open circuit still admitted an attempt")
	}
	env := h.sink.last(t)
	if len(env.Attempts) != 1 || env.Attempts[0].UpstreamModel != modelB {
		t.Fatalf("attempts %+v", env.Attempts)
	}
	stats := h.gateway.Health().ProviderHealth(time.Minute)
	if s := stats[env.Attempts[0].ProviderID]; s.Attempts != circuitFailures+1 || s.Successes != circuitFailures+1 {
		t.Fatalf("health for b %+v", s)
	}
}

func TestRevokedCredentialIsNeverSelected(t *testing.T) {
	h := newHarness(t, Config{})
	h.rt.mu.Lock()
	h.rt.revoked[h.credA] = true
	h.rt.mu.Unlock()
	if resp, _ := h.chat(fullKey, nil); resp.StatusCode != http.StatusOK {
		t.Fatal(resp.Status)
	}
	if h.mock.count("a") != 0 || h.sink.last(t).Attempts[0].UpstreamModel != modelB {
		t.Fatal("revoked credential was used")
	}
}

func TestRetryTaxonomyFixture(t *testing.T) {
	type entry struct {
		Name              string `json:"name"`
		Class             string `json:"class"`
		ResponseCommitted bool   `json:"response_committed"`
		AllowsFailover    bool   `json:"allows_failover"`
	}
	for _, e := range testutil.JSON[[]entry](t, fixtures.Files, "routing/retry-taxonomy.json") {
		if got := failoverAllowed(e.Class, e.ResponseCommitted); got != e.AllowsFailover {
			t.Errorf("%s: failover %v, want %v", e.Name, got, e.AllowsFailover)
		}
	}
}

func TestResponsesFamily(t *testing.T) {
	h := newHarness(t, Config{})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed","model":"model-a","output":[{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"output_text","text":"done","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`)
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/responses", fullKey, []byte(`{"model":"`+routeSlug+`","input":"hi"}`), nil)
	defer resp.Body.Close()
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != http.StatusOK || body["model"] != routeSlug {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
	resp = h.do(t.Context(), http.MethodPost, "/v1/responses", fullKey, []byte(`{"model":"`+routeSlug+`","input":"hi","previous_response_id":"resp_0"}`), nil)
	defer resp.Body.Close()
	json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != http.StatusBadRequest || errorCode(t, body) != "unsupported_stateful_reference" {
		t.Fatalf("stateful reference: status %d body %v", resp.StatusCode, body)
	}
}

func TestClientIP(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.1.1.1:5000"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.2.2.2")
	if got := ClientIP(r, trusted); got != "203.0.113.9" {
		t.Fatalf("forwarded client %q", got)
	}
	r.RemoteAddr = "198.51.100.4:5000"
	if got := ClientIP(r, trusted); got != "198.51.100.4" {
		t.Fatalf("untrusted peer must not spoof: %q", got)
	}
}

func TestCORSPreflight(t *testing.T) {
	h := newHarness(t, Config{MaxInFlight: 8, CORSAllowedOrigins: []string{"https://app.example"}})
	// The browser asks permission for the headers generated by the pinned
	// OpenAI SDK, including streaming helpers and retries.
	headers := []string{"authorization", "content-type", "x-stainless-lang", "x-stainless-package-version", "x-stainless-os", "x-stainless-arch", "x-stainless-runtime", "x-stainless-runtime-version", "x-stainless-retry-count", "x-stainless-timeout", "x-stainless-helper-method"}
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		resp := h.do(t.Context(), http.MethodOptions, path, "", nil, map[string]string{"Origin": "https://app.example", "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": strings.Join(headers, ", ")})
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != "https://app.example" {
			t.Fatalf("status %d headers %v", resp.StatusCode, resp.Header)
		}
		allowed := ", " + strings.ToLower(resp.Header.Get("Access-Control-Allow-Headers")) + ", "
		for _, header := range headers {
			if !strings.Contains(allowed, ", "+header+", ") {
				t.Errorf("%s: SDK header %q is not allowed by preflight: %s", path, header, allowed)
			}
		}
	}
}

func TestClientWriteFailureIsCancellation(t *testing.T) {
	st := &attemptState{parent: context.Background()}
	if got := st.classify(fmt.Errorf("%w: broken pipe", errClientWrite), true); got != classCancelled {
		t.Fatalf("client write failure classified as %q, want %q", got, classCancelled)
	}
	if got := st.classify(errors.New("unexpected EOF"), true); got != classProtocol {
		t.Fatalf("committed upstream failure classified as %q, want %q", got, classProtocol)
	}
	if got := st.classify(errors.New("connection refused"), false); got != classConnect {
		t.Fatalf("pre-commit transport failure classified as %q, want %q", got, classConnect)
	}
}
