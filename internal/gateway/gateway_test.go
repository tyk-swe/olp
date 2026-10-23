package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
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
	inputs  *usage.RoutingInputs
	keys    map[string]access.Authority
	stale   bool
	revoked map[string]bool
	// lapsed holds the credential versions whose grants lapsed.
	lapsed map[string]bool
	// refused records the credential versions the upstream refused.
	refused            []string
	refusedGenerations []int64
}

func (f *fakeRuntime) Release() *runtime.Release { f.mu.Lock(); defer f.mu.Unlock(); return f.release }

func (f *fakeRuntime) RoutingInputs() *usage.RoutingInputs {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inputs
}

func (f *fakeRuntime) Authenticate(secret string) (*access.Authority, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stale {
		return nil, runtime.ErrStaleAuthority
	}
	a, ok := f.keys[secret]
	if !ok {
		return nil, runtime.ErrInvalidKey
	}
	return &a, nil
}

func (f *fakeRuntime) Eligibility(id string) routing.Eligibility {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.stale:
		return routing.StaleAuthority
	case f.revoked[id]:
		return routing.Revoked
	case f.lapsed[id]:
		return routing.Lapsed
	}
	return routing.Eligible
}

// Secret serves eligible credentials the pinned release installed. The fake's
// secret authority, for callers that pin no release, is its current release.
func (f *fakeRuntime) Secret(_ context.Context, release *runtime.Release, id string) ([]byte, int64, error) {
	if f.Eligibility(id) != routing.Eligible {
		return nil, 0, runtime.ErrCredentialUnavailable
	}
	if release == nil {
		release = f.Release()
	}
	if secret, ok := release.Credential(id); ok {
		return secret, 0, nil
	}
	return nil, 0, runtime.ErrCredentialUnavailable
}

func (f *fakeRuntime) NetworkSecret(ctx context.Context, release *runtime.Release, _, id string) ([]byte, error) {
	secret, _, err := f.Secret(ctx, release, id)
	return secret, err
}

func (f *fakeRuntime) GrantGeneration(string) int64 { return 0 }

func (f *fakeRuntime) CredentialRefused(id string, generation int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refused = append(f.refused, id)
	f.refusedGenerations = append(f.refusedGenerations, generation)
}

type capture struct {
	mu   sync.Mutex
	envs []Envelope
}

func (c *capture) Terminal(e Envelope) { c.mu.Lock(); defer c.mu.Unlock(); c.envs = append(c.envs, e) }

func (c *capture) last(t *testing.T) Envelope {
	t.Helper()
	envs := c.await(t, 1)
	return envs[len(envs)-1]
}

// nth returns the nth terminal event. A test that sends several requests
// names the one it checks, since the newest event recorded can still be an
// earlier request's.
func (c *capture) nth(t *testing.T, n int) Envelope {
	t.Helper()
	return c.await(t, n)[n-1]
}

func (c *capture) await(t *testing.T, n int) []Envelope {
	t.Helper()
	// Reading a known-length HTTP response can finish before the serving
	// goroutine records its terminal event. Await that event explicitly.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		if len(c.envs) >= n {
			envs := slices.Clone(c.envs)
			c.mu.Unlock()
			return envs
		}
		c.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("envelope %d not emitted within one second", n)
	return nil
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
	return newHarnessWrapped(t, cfg, func(h http.Handler) http.Handler { return h })
}

func newHarnessWrapped(t *testing.T, cfg Config, wrap func(http.Handler) http.Handler) *harness {
	t.Helper()
	m := &mock{handlers: map[string]http.HandlerFunc{}, calls: map[string]int{}}
	m.set("a", completion(modelA, answerText))
	m.set("b", completion(modelB, answerText))
	upstream := httptest.NewServer(m)
	t.Cleanup(upstream.Close)
	credA, credB := uuid.NewString(), uuid.NewString()
	version := 1
	slotA := uuid.NewString()
	provider := func(name, secretID, model, slot string) routing.Provider {
		id := uuid.NewString()
		return routing.Provider{
			ID: id, Name: name, Kind: "openai_compatible", Enabled: true, ActiveCredential: &secretID, RevisionID: uuid.NewString(),
			Capabilities: []routing.Capability{{Model: model, Operation: "generation", Surface: "openai", Mode: "unary"}, {Model: model, Operation: "generation", Surface: "openai", Mode: "streaming"}},
			Endpoint:     upstream.URL + "/" + name + "/v1", AuthMode: "api_key",
			Slots: []routing.Slot{{ID: slot, Name: "default", Enabled: true, Weight: 1, CredentialID: &secretID, CredentialVersion: &version}},
		}
	}
	a := provider("a", credA, modelA, slotA)
	b := provider("b", credB, modelB, uuid.NewString())
	routeID := uuid.NewString()
	snapshot := &routing.Snapshot{
		Generation: routing.Generation{ID: uuid.NewString(), Ordinal: 1, ActivatedAt: time.Now()},
		Providers:  map[string]routing.Provider{a.ID: a, b.ID: b},
		Routes: map[string]routing.Route{routeSlug: {
			ID: routeID, Slug: routeSlug, Operations: []string{"generation"}, OverallTimeout: 5000, MaxAttempts: 3, RoutingID: routeID, RevisionID: uuid.NewString(), Revision: 1, PublishedAt: time.Now(),
			Fidelity: routing.RouteFidelity{Mode: routing.FidelityTransformed},
			Targets: []routing.Target{
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
	server := httptest.NewServer(wrap(mux))
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
	// Callers supply fixture overrides, not duplicate JSON members. Explicit
	// malformed/ambiguous-wire tests use do with their original bytes.
	var merged map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &merged); err != nil {
		h.t.Fatal(err)
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		h.t.Fatal(err)
	}
	resp := h.do(h.t.Context(), http.MethodPost, "/v1/chat/completions", key, encoded, extra)
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

// The slot cooled down for the advertised interval: the next request
// skips provider a without consuming an attempt.

// Connection drops mid-stream without a completion marker.

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
	if code, body := list(fullKey, "/anthropic/v1/models"); code != 200 {
		t.Fatalf("%d %v", code, body)
	} else {
		item := body["data"].([]any)[0].(map[string]any)
		if item["id"] != routeSlug || item["capabilities"] == nil {
			t.Fatalf("anthropic model %v", item)
		}
	}
	if code, body := list(otherKey, "/anthropic/v1/models"); code != 200 || len(body["data"].([]any)) != 0 {
		t.Fatalf("restricted key sees %v", body)
	}
	if code, body := list(fullKey, "/gemini/v1/models"); code != 200 {
		t.Fatalf("%d %v", code, body)
	} else {
		item := body["models"].([]any)[0].(map[string]any)
		if item["name"] != "models/"+routeSlug || item["capabilities"] == nil {
			t.Fatalf("gemini model %v", item)
		}
	}
	if code, body := list(otherKey, "/gemini/v1/models"); code != 200 || len(body["models"].([]any)) != 0 {
		t.Fatalf("restricted key sees %v", body)
	}
}

// The browser asks permission for the headers generated by the pinned
// OpenAI SDK, including streaming helpers and retries.

// This fixture has no asynchronous publication or external database.
func (*fakeRuntime) CheckRouteFidelity(context.Context, routing.Route) error { return nil }
