package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/media/transport"
	"github.com/tyk-swe/olp/internal/staging"
	"github.com/tyk-swe/olp/internal/usage/routinginputs"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/admission"
	batchupstream "github.com/tyk-swe/olp/internal/batches/upstream"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	limitadmission "github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/limits/scripts"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/resources"

	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/health"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/rates"
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
	release *release.Release
	inputs  *routinginputs.Inputs
	keys    map[string]keyauthority.Authority
	stale   bool
	revoked map[string]bool
	// lapsed holds the credential versions whose grants lapsed.
	lapsed map[string]bool
	// refused records the credential versions the upstream refused.
	refused            []string
	refusedGenerations []int64
	onSecret           func(id string)
}

func (f *fakeRuntime) Release() *release.Release { f.mu.Lock(); defer f.mu.Unlock(); return f.release }

func (f *fakeRuntime) RoutingInputs() *routinginputs.Inputs {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inputs
}

func (f *fakeRuntime) Authenticate(secret string) (*keyauthority.Authority, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stale {
		return nil, release.ErrStaleAuthority
	}
	a, ok := f.keys[secret]
	if !ok {
		return nil, release.ErrInvalidKey
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
func (f *fakeRuntime) Secret(_ context.Context, pinned *release.Release, id string) ([]byte, int64, error) {
	f.mu.Lock()
	onSecret := f.onSecret
	f.mu.Unlock()
	if onSecret != nil {
		onSecret(id)
	}
	if f.Eligibility(id) != routing.Eligible {
		return nil, 0, release.ErrCredentialUnavailable
	}
	if pinned == nil {
		pinned = f.Release()
	}
	if secret, ok := pinned.Credential(id); ok {
		return secret, 0, nil
	}
	return nil, 0, release.ErrCredentialUnavailable
}

func (f *fakeRuntime) NetworkSecret(ctx context.Context, release *release.Release, _, id string) ([]byte, error) {
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

type envelopes struct {
	mu   sync.Mutex
	envs []events.Envelope
}

func (c *envelopes) Terminal(e events.Envelope) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.envs = append(c.envs, e)
}

func (c *envelopes) last(t *testing.T) events.Envelope {
	t.Helper()
	envs := c.await(t, 1)
	return envs[len(envs)-1]
}

func (c *envelopes) await(t *testing.T, n int) []events.Envelope {
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
	sink     *envelopes
	gateway  *Server
	server   *httptest.Server
	keyID    string
	slotA    string
	credA    string
	upstream *httptest.Server
}

func TestBrowserDelegationPreflightAllowsConsent(t *testing.T) {
	const origin = "https://client.example"
	h := newHarness(t, Config{CORSAllowedOrigins: []string{origin}})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodOptions, h.server.URL+"/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "x-olp-delegation-consent")
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	allowed := strings.Split(resp.Header.Get("Access-Control-Allow-Headers"), ",")
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != origin ||
		!slices.ContainsFunc(allowed, func(value string) bool {
			return strings.EqualFold(strings.TrimSpace(value), "X-OLP-Delegation-Consent")
		}) {
		t.Fatalf("delegated request cannot pass preflight: %d, %v", resp.StatusCode, resp.Header)
	}
}

func newHarness(t *testing.T, cfg Config, extra ...Dependencies) *harness {
	return newHarnessWrapped(t, cfg, func(h http.Handler) http.Handler { return h }, extra...)
}

func newHarnessWrapped(t *testing.T, cfg Config, wrap func(http.Handler) http.Handler, extra ...Dependencies) *harness {
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
	release, err := release.NewRelease(uuid.NewString(), 7, snapshot, map[string][]byte{credA: []byte(secretA), credB: []byte(secretB)})
	if err != nil {
		t.Fatal(err)
	}
	keyID := uuid.NewString()
	rt := &fakeRuntime{release: release, revoked: map[string]bool{}, keys: map[string]keyauthority.Authority{
		fullKey:  {ID: keyID, Policy: keyauthority.KeyPolicy{Scopes: []string{"inference", "models_read"}}},
		readKey:  {ID: uuid.NewString(), Policy: keyauthority.KeyPolicy{Scopes: []string{"models_read"}}},
		otherKey: {ID: uuid.NewString(), Policy: keyauthority.KeyPolicy{Scopes: []string{"inference", "models_read"}, AllowedRoutes: []string{"another-route"}}},
	}}
	policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	if cfg.MaxInFlight == 0 {
		cfg.MaxInFlight = 8
	}
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = 64 * 1024
	}
	if cfg.MaxResponseBytes == 0 {
		cfg.MaxResponseBytes = 1 << 20
	}
	if cfg.MaxEventBytes == 0 {
		cfg.MaxEventBytes = 4096
	}
	sink := &envelopes{}
	dependencies := Dependencies{}
	if len(extra) > 0 {
		dependencies = extra[0]
	}
	dependencies.Runtime, dependencies.Egress, dependencies.Log, dependencies.Sink = rt, &policy, slog.New(slog.NewTextHandler(io.Discard, nil)), sink
	dependencies.Getenv = func(string) string { return "" }
	gw := New(dependencies, cfg)
	mux := http.NewServeMux()
	gw.Register(mux)
	server := httptest.NewServer(wrap(mux))
	t.Cleanup(server.Close)
	return &harness{t: t, rt: rt, mock: m, sink: sink, gateway: gw, server: server, keyID: keyID, slotA: slotA, credA: credA, upstream: upstream}
}

// admissionAuthority builds a key with the limits a case needs.
func admissionAuthority(policy keyauthority.KeyPolicy) keyauthority.Authority {
	return keyauthority.Authority{ID: uuid.Must(uuid.NewV7()).String(), LookupID: "lookupid0123", Policy: policy}
}

// reissue publishes a release in which the harness's first provider is
// edited, with credential as its secret unless that is nil.
func (h *harness) reissue(edit func(*routing.Provider), credential []byte) {
	h.t.Helper()
	snapshot := h.rt.release.Snapshot().Clone()
	secrets := map[string][]byte{}
	for id, provider := range snapshot.Providers {
		for _, slot := range provider.Slots {
			secrets[*slot.CredentialID], _ = h.rt.release.Credential(*slot.CredentialID)
		}
		if provider.Slots[0].ID == h.slotA {
			edit(&provider)
			snapshot.Providers[id] = provider
		}
	}
	if credential != nil {
		secrets[h.credA] = credential
	}
	release, err := release.NewRelease(uuid.NewString(), 7, snapshot, secrets)
	if err != nil {
		h.t.Fatal(err)
	}
	h.rt.release = release
}

func strictHarness(t *testing.T, configure func(*routing.Snapshot)) *harness {
	t.Helper()
	return strictHarnessCfg(t, Config{}, configure)
}

func strictHarnessCfg(t *testing.T, cfg Config, configure func(*routing.Snapshot)) *harness {
	t.Helper()
	return strictRelease(t, newHarness(t, cfg), configure)
}

func strictRelease(t *testing.T, h *harness, configure func(*routing.Snapshot)) *harness {
	t.Helper()
	current := h.rt.release
	snapshot := current.Snapshot().Clone()
	credentials := map[string][]byte{}
	for id, provider := range snapshot.Providers {
		provider.ProfileID, provider.ProfileRevision = "compatible-chat", connectors.ProfileRevision
		snapshot.Providers[id] = provider
		for _, slot := range provider.Slots {
			credentials[*slot.CredentialID], _ = current.Credential(*slot.CredentialID)
		}
	}
	route := snapshot.Routes[routeSlug]
	route.Fidelity = routing.RouteFidelity{Mode: routing.FidelityStrict}
	snapshot.Routes[routeSlug] = route
	if configure != nil {
		configure(snapshot)
	}
	var err error
	h.rt.release, err = release.NewRelease(current.ID, current.Sequence, snapshot, credentials)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func strictEmbeddings(snapshot *routing.Snapshot) {
	for id, provider := range snapshot.Providers {
		provider.Capabilities = []routing.Capability{{Model: provider.Capabilities[0].Model, Operation: "embeddings", Surface: "openai", Mode: "unary"}}
		snapshot.Providers[id] = provider
	}
	route := snapshot.Routes[routeSlug]
	route.Operations = []string{"embeddings"}
	snapshot.Routes[routeSlug] = route
}

// republish installs a release whose snapshot edit has changed. The harness
// route serves provider a first and b second; edit may reshape it and add
// routes of its own.
func (h *harness) republish(edit func(s *routing.Snapshot, a, b routing.Provider)) {
	h.t.Helper()
	old := h.rt.Release()
	snapshot := &routing.Snapshot{
		Generation: routing.Generation{ID: uuid.NewString(), Ordinal: old.Snapshot().Generation().Ordinal + 1, ActivatedAt: old.Snapshot().Generation().ActivatedAt},
		Providers:  maps.Collect(old.Snapshot().Providers()),
		Routes:     maps.Collect(old.Snapshot().Routes()),
	}
	var a, b routing.Provider
	credentials := map[string][]byte{}
	for _, provider := range snapshot.Providers {
		switch provider.Name {
		case "a":
			a = provider
		case "b":
			b = provider
		}
		if secret, ok := old.Credential(*provider.ActiveCredential); ok {
			credentials[*provider.ActiveCredential] = secret
		}
	}
	edit(snapshot, a, b)
	release, err := release.NewRelease(uuid.NewString(), old.Sequence+1, snapshot, credentials)
	if err != nil {
		h.t.Fatal(err)
	}
	h.rt.mu.Lock()
	h.rt.release = release
	h.rt.mu.Unlock()
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

func TestTransformedRerankPreservesDocumentIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result string
		status int
	}{
		{"wrong string document", `{"index":0,"relevance_score":0.5,"document":"second"}`, http.StatusBadGateway},
		{"wrong object document", `{"index":0,"relevance_score":0.5,"document":{"text":"second"}}`, http.StatusBadGateway},
		{"null score", `{"index":0,"relevance_score":null,"document":"first"}`, http.StatusBadGateway},
		{"zero score", `{"index":0,"relevance_score":0,"document":"first"}`, http.StatusOK},
		{"matching object", `{"index":0,"relevance_score":0.5,"document":{"text":"first"}}`, http.StatusOK},
		{"omitted document", `{"index":0,"relevance_score":0.5}`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{})
			h.republish(func(snapshot *routing.Snapshot, _, _ routing.Provider) {
				for id, provider := range snapshot.Providers {
					provider.VendorID = "cohere"
					provider.Capabilities = []routing.Capability{{Model: provider.Capabilities[0].Model, Operation: "rerank", Surface: "openai", Mode: "unary"}}
					snapshot.Providers[id] = provider
				}
				route := snapshot.Routes[routeSlug]
				route.Operations, route.MaxAttempts = []string{"rerank"}, 1
				snapshot.Routes[routeSlug] = route
			})
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"results":[`+tc.result+`]}`)
			})
			resp := h.do(t.Context(), http.MethodPost, "/v1/rerank", fullKey, []byte(`{"model":"team-chat","query":"q","documents":["first","second"],"top_n":1,"return_documents":true}`), nil)
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.status {
				t.Fatalf("status %d, want %d, body %s", resp.StatusCode, tc.status, body)
			}
			if tc.status == http.StatusBadGateway && !strings.Contains(string(body), `"code":"provider_protocol_error"`) {
				t.Fatalf("unexpected rejection: %s", body)
			}
			if tc.status == http.StatusOK && !strings.Contains(string(body), `"document":"first"`) {
				t.Fatalf("document identity lost: %s", body)
			}
		})
	}
}

func TestTransformedEmbeddingsRejectIncompleteVectors(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		want           int
	}{
		{"complete", `{"embeddings":[{"values":[0.5,0.25]},{"values":[0.25,0.5]}]}`, http.StatusOK},
		{"missing input", `{"embeddings":[{"values":[0.5,0.25]}]}`, http.StatusBadGateway},
		{"extra input", `{"embeddings":[{"values":[0.5,0.25]},{"values":[0.25,0.5]},{"values":[1,1]}]}`, http.StatusBadGateway},
		{"null coordinate", `{"embeddings":[{"values":[0.5,null]},{"values":[0.25,0.5]}]}`, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{})
			h.republish(func(snapshot *routing.Snapshot, a, b routing.Provider) {
				strictEmbeddings(snapshot)
				for id, provider := range snapshot.Providers {
					provider.Kind = "gemini"
					snapshot.Providers[id] = provider
				}
				route := snapshot.Routes[routeSlug]
				route.MaxAttempts = 1
				snapshot.Routes[routeSlug] = route
			})
			h.mock.set("a", status(http.StatusOK, tc.response))
			resp := h.do(t.Context(), http.MethodPost, "/v1/embeddings", fullKey, []byte(`{"model":"team-chat","input":["a","b"],"encoding_format":"float"}`), nil)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.want {
				t.Fatalf("status %d, want %d, body %s", resp.StatusCode, tc.want, body)
			}
		})
	}
}

func TestEmbeddingsRejectInvalidResultIndices(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		want       int
	}{
		{"complete", `{"index":0,"embedding":[0.5,0]},{"index":1,"embedding":[0.25,0.5]}`, http.StatusOK},
		{"reordered", `{"index":1,"embedding":[0.25,0.5]},{"index":0,"embedding":[0.5,0]}`, http.StatusOK},
		{"out of range", `{"index":0,"embedding":[0.5,0]},{"index":2,"embedding":[0.25,0.5]}`, http.StatusBadGateway},
		{"shifted", `{"index":1,"embedding":[0.5,0]},{"index":2,"embedding":[0.25,0.5]}`, http.StatusBadGateway},
		{"duplicate", `{"index":0,"embedding":[0.5,0]},{"index":0,"embedding":[0.25,0.5]}`, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{})
			h.republish(func(snapshot *routing.Snapshot, a, b routing.Provider) {
				strictEmbeddings(snapshot)
				route := snapshot.Routes[routeSlug]
				route.MaxAttempts = 1
				snapshot.Routes[routeSlug] = route
			})
			h.mock.set("a", status(http.StatusOK, `{"data":[`+tc.data+`]}`))
			resp := h.do(t.Context(), http.MethodPost, "/v1/embeddings", fullKey, []byte(`{"model":"team-chat","input":["a","b"],"encoding_format":"float"}`), nil)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.want {
				t.Fatalf("status %d, want %d, body %s", resp.StatusCode, tc.want, body)
			}
			if tc.want == http.StatusBadGateway && !strings.Contains(string(body), `"code":"provider_protocol_error"`) {
				t.Fatalf("unexpected rejection %s", body)
			}
			if tc.want == http.StatusOK {
				var result struct {
					Data []struct {
						Index     int       `json:"index"`
						Embedding []float64 `json:"embedding"`
					} `json:"data"`
				}
				if err := json.Unmarshal(body, &result); err != nil || len(result.Data) != 2 {
					t.Fatalf("invalid result %s", body)
				}
				for _, item := range result.Data {
					want := [][]float64{{0.5, 0}, {0.25, 0.5}}[item.Index]
					if !slices.Equal(item.Embedding, want) {
						t.Fatalf("vector identity changed %s", body)
					}
				}
			}
		})
	}
}

func TestTransformedMediaPreservesUsage(t *testing.T) {
	h := newHarness(t, Config{MaxMediaBodyBytes: 1 << 20})
	spool, err := staging.NewSpool(t.TempDir(), staging.MinCapacityBytes, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spool.Close() })
	h.gateway.deps.Media = &MediaDeps{Transport: transport.New(transport.Deps{
		Client: h.gateway.client, Auth: h.gateway.auth, Egress: h.gateway.egress,
		Spool: spool, MaxResponseBytes: 1 << 20, Log: h.gateway.log,
	})}
	h.gateway.deps.Staging = spool
	h.gateway.deps.Uploads = staging.NewAdmission(spool.CapacityBytes())
	h.republish(func(snapshot *routing.Snapshot, a, b routing.Provider) {
		for _, provider := range []routing.Provider{a, b} {
			provider.Capabilities = []routing.Capability{
				{Model: provider.Capabilities[0].Model, Operation: string(media.OpImageGeneration), Surface: "openai", Mode: "unary"},
				{Model: provider.Capabilities[0].Model, Operation: string(media.OpTranscription), Surface: "openai", Mode: "unary"},
			}
			snapshot.Providers[provider.ID] = provider
		}
		route := snapshot.Routes[routeSlug]
		route.Operations = []string{string(media.OpImageGeneration), string(media.OpTranscription)}
		snapshot.Routes[routeSlug] = route
	})
	mux := http.NewServeMux()
	h.gateway.Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	for _, tc := range []struct {
		name, path, response string
	}{
		{"image breakdown", "/v1/images/generations", `{"created":1,"data":[{"url":"https://example.invalid/generated.png"}],"usage":{"input_tokens":7,"output_tokens":11,"total_tokens":18,"input_tokens_details":{"text_tokens":3,"image_tokens":4},"output_tokens_details":{"image_tokens":11},"extension":{"count":2}}}`},
		{"transcription tokens", "/v1/audio/transcriptions", `{"text":"fixture transcript","usage":{"type":"tokens","input_tokens":7,"output_tokens":11,"total_tokens":18,"input_token_details":{"text_tokens":3,"audio_tokens":4},"extension":{"count":2}}}`},
		{"transcription duration", "/v1/audio/transcriptions", `{"text":"fixture transcript","usage":{"type":"duration","seconds":3.5}}`},
		{"transcription without usage", "/v1/audio/transcriptions", `{"text":"fixture transcript"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.mock.set("a", status(http.StatusOK, tc.response))
			var body bytes.Buffer
			contentType := "application/json"
			if tc.path == "/v1/audio/transcriptions" {
				form := multipart.NewWriter(&body)
				if err := form.WriteField("model", routeSlug); err != nil {
					t.Fatal(err)
				}
				file, err := form.CreateFormFile("file", "fixture.wav")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.Write(media.SilentWAV(10)); err != nil {
					t.Fatal(err)
				}
				if err := form.Close(); err != nil {
					t.Fatal(err)
				}
				contentType = form.FormDataContentType()
			} else {
				fmt.Fprintf(&body, `{"model":%q,"prompt":"fixture image"}`, routeSlug)
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+tc.path, &body)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+fullKey)
			req.Header.Set("Content-Type", contentType)
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			gotBody, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("media response status=%d body=%s error=%v", resp.StatusCode, gotBody, err)
			}
			var got, upstream map[string]any
			if err := json.Unmarshal(gotBody, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.response), &upstream); err != nil {
				t.Fatal(err)
			}
			actualUsage, actualPresent := got["usage"]
			expectedUsage, expectedPresent := upstream["usage"]
			if actualPresent != expectedPresent || !reflect.DeepEqual(actualUsage, expectedUsage) {
				t.Fatalf("usage changed from %v to %v, client response %s", expectedUsage, actualUsage, gotBody)
			}
		})
	}
}

type unreachableValkey struct{}

func (unreachableValkey) Do(context.Context, ...string) (any, error) {
	return nil, context.DeadlineExceeded
}

func (unreachableValkey) Pipeline(context.Context, ...[]string) ([]any, error) {
	return nil, context.DeadlineExceeded
}

func TestResearchStreamFollowsOutagePolicy(t *testing.T) {
	runner, err := scripts.NewRunner(unreachableValkey{}, "test")
	if err != nil {
		t.Fatal(err)
	}
	limiter := limitadmission.NewDurable(runner, nil)
	slots := int64(1)
	authority := keyauthority.Authority{ID: uuid.NewString(), Policy: keyauthority.KeyPolicy{MaxConcurrency: &slots}}
	for _, test := range []struct {
		name       string
		policy     limitadmission.OutagePolicy
		provider   *routing.Limits
		wantRefuse bool
	}{
		{"fail open admits key concurrency", limitadmission.FailOpen, nil, false},
		{"fail closed refuses key concurrency", limitadmission.FailClosed, nil, true},
		{"fail open still refuses provider concurrency", limitadmission.FailOpen, &routing.Limits{MaxConcurrency: &slots}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			admission := quota.NewAdmission(limiter, fleet.New(runner), func() limitadmission.OutagePolicy { return test.policy }, slog.New(slog.DiscardHandler))
			lease, failure := admission.ReserveResearchStream(t.Context(), authority, routeSlug, "service", "provider", test.provider, time.Minute)
			if test.wantRefuse {
				if failure == nil || failure.Code != "distributed_limits_unavailable" {
					t.Fatalf("outage admitted the stream: lease=%v failure=%v", lease, failure)
				}
				return
			}
			if failure != nil {
				t.Fatalf("fail-open installation refused the stream: %v", failure)
			}
			if admission.FailOpenTotal() != 1 {
				t.Fatalf("fail-open total = %d, want 1", admission.FailOpenTotal())
			}
		})
	}
}

func TestBedrockStreamClassifiesRequestEndings(t *testing.T) {
	for _, tc := range []struct {
		name, class, outcome, errorClass string
		overallTimeout                   int64
	}{
		{"client cancelled", classCancelled, "cancelled", "client_cancelled", 5000},
		{"overall timeout", classTimeout, "failure", "gateway_timeout", 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{})
			h.republish(func(snapshot *routing.Snapshot, _, _ routing.Provider) {
				for id, provider := range snapshot.Providers {
					provider.Kind, provider.ProfileID, provider.ProfileRevision, provider.VendorID = "bedrock", "bedrock-converse", connectors.ProfileRevision, ""
					provider.CloudRegion, provider.AuthMode = "us-east-1", "static"
					provider.Capabilities = []routing.Capability{{Model: provider.Capabilities[0].Model, Operation: "generation", Surface: "bedrock", Mode: "streaming"}}
					snapshot.Providers[id] = provider
				}
				route := snapshot.Routes[routeSlug]
				route.OverallTimeout = tc.overallTimeout
				snapshot.Routes[routeSlug] = route
			})
			serveBedrock(t, h)
			started := bedrockEvent(t, "messageStart", map[string]any{"role": "assistant"})
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
				if err := eventstream.NewEncoder().Encode(w, *started); err != nil {
					t.Error(err)
					return
				}
				_ = http.NewResponseController(w).Flush()
				<-r.Context().Done()
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			resp := h.do(ctx, http.MethodPost, "/bedrock/model/team-chat/converse-stream", fullKey, []byte(`{"messages":[{"role":"user","content":[{"text":"hello"}]}],"inferenceConfig":{"maxTokens":16}}`), nil)
			defer resp.Body.Close()
			if _, err := eventstream.NewDecoder().Decode(resp.Body, nil); err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("stream did not start: status=%d err=%v", resp.StatusCode, err)
			}
			if tc.class == classCancelled {
				cancel()
			} else {
				_, _ = io.ReadAll(resp.Body)
			}
			env := h.sink.last(t)
			if env.Outcome != tc.outcome || env.ErrorClass != tc.errorClass || len(env.Attempts) != 1 || env.Attempts[0].Class != tc.class {
				t.Fatalf("outcome=%q error=%q attempts=%+v", env.Outcome, env.ErrorClass, env.Attempts)
			}
		})
	}
	t.Run("terminal frame could not be delivered", func(t *testing.T) {
		h := newHarness(t, Config{})
		var source bytes.Buffer
		for _, frame := range []*eventstream.Message{
			bedrockMetadata(`{"usage":{"inputTokens":2,"outputTokens":3,"totalTokens":5}}`),
			bedrockEvent(t, "messageStop", map[string]string{"stopReason": "end_turn"}),
		} {
			if err := eventstream.NewEncoder().Encode(&source, *frame); err != nil {
				t.Fatal(err)
			}
		}
		h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			_, _ = w.Write(source.Bytes())
		})
		p := testPinnedTarget(h, 0)
		x := &call{request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release}, family: wire.Bedrock, keyID: h.keyID}
		resp, failure := h.gateway.pinnedDo(t.Context(), x, p, http.MethodPost, p.provider.Endpoint, nil, "")
		if failure != nil {
			t.Fatalf("provider stream: %+v", failure)
		}
		defer resp.Body.Close()
		writer := &bedrockFrameWriter{ResponseRecorder: httptest.NewRecorder()}
		h.gateway.relayBedrockStream(t.Context(), writer, x, p, resp)
		h.sink.mu.Lock()
		defer h.sink.mu.Unlock()
		if len(h.sink.envs) != 1 {
			t.Fatalf("terminal envelopes=%d, want 1", len(h.sink.envs))
		}
		env := h.sink.envs[0]
		if env.Outcome != "cancelled" || env.ErrorClass != "client_delivery_failed" || !env.Committed || len(env.Attempts) != 1 {
			t.Fatalf("terminal envelope=%+v", env)
		}
		attempt := env.Attempts[0]
		if attempt.Class != classSuccess || !attempt.UsageComplete || attempt.Usage == nil || attempt.Usage.InputTokens != 2 || attempt.Usage.OutputTokens != 3 {
			t.Fatalf("validated provider result was lost: %+v", attempt)
		}
	})
}

type bedrockFrameWriter struct {
	*httptest.ResponseRecorder
}

func (w *bedrockFrameWriter) Write(body []byte) (int, error) {
	if bytes.Contains(body, []byte("messageStop")) {
		return 0, io.ErrClosedPipe
	}
	return w.ResponseRecorder.Write(body)
}

func TestModerationRejectsIncompleteDecisions(t *testing.T) {
	const valid = `{"flagged":false,"categories":{"fixture":false},"category_scores":{"fixture":0.01}}`
	const batch = `["first","second"]`
	for _, tc := range []struct {
		name, input, results string
		want                 int
	}{
		{"complete batch", batch, valid + "," + valid, http.StatusOK},
		{"single text", `"first"`, valid, http.StatusOK},
		{"joint multimodal", `[{"type":"text","text":"first"},{"type":"text","text":"second"}]`, valid, http.StatusOK},
		{"missing result", batch, valid, http.StatusBadGateway},
		{"extra result", batch, valid + "," + valid + "," + valid, http.StatusBadGateway},
		{"null flag", batch, `{"flagged":null,"categories":{"fixture":false},"category_scores":{"fixture":0.01}},` + valid, http.StatusBadGateway},
		{"null category", batch, `{"flagged":false,"categories":{"fixture":null},"category_scores":{"fixture":0.01}},` + valid, http.StatusBadGateway},
		{"null score", batch, `{"flagged":false,"categories":{"fixture":false},"category_scores":{"fixture":null}},` + valid, http.StatusBadGateway},
		{"mismatched categories", batch, `{"flagged":false,"categories":{"fixture":false},"category_scores":{"other":0.01}},` + valid, http.StatusBadGateway},
		{"empty categories", batch, `{"flagged":false,"categories":{},"category_scores":{}},` + valid, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{})
			h.republish(func(snapshot *routing.Snapshot, a, b routing.Provider) {
				for id, provider := range snapshot.Providers {
					provider.Kind = "openai"
					provider.Capabilities = []routing.Capability{{Model: provider.Capabilities[0].Model, Operation: "moderation", Surface: "openai", Mode: "unary"}}
					snapshot.Providers[id] = provider
				}
				route := snapshot.Routes[routeSlug]
				route.Operations = []string{"moderation"}
				route.MaxAttempts = 1
				snapshot.Routes[routeSlug] = route
			})
			h.mock.set("a", status(http.StatusOK, `{"id":"modr-fixture","model":"model-a","results":[`+tc.results+`]}`))
			resp := h.do(t.Context(), http.MethodPost, "/v1/moderations", fullKey, []byte(`{"model":"team-chat","input":`+tc.input+`}`), nil)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.want {
				t.Fatalf("status %d, want %d, body %s", resp.StatusCode, tc.want, body)
			}
			if tc.want == http.StatusBadGateway && !strings.Contains(string(body), `"code":"provider_protocol_error"`) {
				t.Fatalf("unexpected rejection %s", body)
			}
			if tc.want == http.StatusOK {
				var result struct {
					Results json.RawMessage `json:"results"`
					Model   string          `json:"model"`
				}
				if json.Unmarshal(body, &result) != nil || string(result.Results) != "["+tc.results+"]" || result.Model != routeSlug {
					t.Fatalf("valid decisions changed %s", body)
				}
			}
		})
	}
}

func TestBraveSearchRejectsNonObjectResponses(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		want           int
	}{
		{"null", `null`, http.StatusBadGateway},
		{"array", `[]`, http.StatusBadGateway},
		{"string", `"not a result"`, http.StatusBadGateway},
		{"boolean", `false`, http.StatusBadGateway},
		{"number", `1`, http.StatusBadGateway},
		{"omitted web", `{"type":"search","query":{"original":"test"}}`, http.StatusOK},
		{"empty results", `{"type":"search","web":{"results":[]}}`, http.StatusOK},
		{"null web", `{"type":"search","web":null}`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{})
			h.republish(func(snapshot *routing.Snapshot, _, _ routing.Provider) {
				for id, provider := range snapshot.Providers {
					provider.VendorID = "brave"
					provider.ProfileID, provider.ProfileRevision = "brave-search", connectors.ProfileRevision
					provider.Capabilities = []routing.Capability{{Model: provider.Capabilities[0].Model, Operation: "search", Surface: "openai", Mode: "unary"}}
					snapshot.Providers[id] = provider
				}
				route := snapshot.Routes[routeSlug]
				route.Operations, route.MaxAttempts = []string{"search"}, 1
				snapshot.Routes[routeSlug] = route
			})
			h.mock.set("a", status(http.StatusOK, tc.response))
			resp := h.do(t.Context(), http.MethodPost, "/v1/search", fullKey, []byte(`{"query":"test","max_results":1}`), map[string]string{"X-OLP-Route": routeSlug})
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("provider=%s status=%d body=%s", tc.response, resp.StatusCode, body)
			if resp.StatusCode != tc.want {
				t.Fatalf("status %d, want %d", resp.StatusCode, tc.want)
			}
			if tc.want == http.StatusBadGateway && !strings.Contains(string(body), `"code":"fidelity_protocol_violation"`) {
				t.Fatalf("unexpected rejection: %s", body)
			}
		})
	}
}

func TestStrictPreparationKeepsTargetsAndSourcesSeparate(t *testing.T) {
	for _, native := range []bool{false, true} {
		name := "generation"
		configure := (func(*routing.Snapshot))(nil)
		if native {
			name, configure = "native operation", strictEmbeddings
		}
		t.Run(name, func(t *testing.T) {
			h := strictHarness(t, configure)
			route := h.rt.release.Snapshot().Route(routeSlug)
			target := route.Targets[0]
			provider := h.rt.release.Snapshot().Provider(target.ProviderID)
			x := &call{family: wire.Chat, routeLeg: routeLeg{route: &route}}
			x.request.release = h.rt.release
			x.authority = h.rt.keys[fullKey]
			for _, text := range []string{"first", "second"} {
				body := []byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%q}]}`, routeSlug, text))
				if native {
					body = []byte(fmt.Sprintf(`{"model":%q,"input":%q}`, routeSlug, text))
					if x.unary == nil {
						selected, err := h.gateway.selectUnary(x, wire.Embeddings, "", body, "", "")
						if err != nil || !selected {
							t.Fatalf("select operation = %t, error = %v", selected, err)
						}
					} else {
						source, err := operationplan.Parse("openai-embeddings", body, 64<<10)
						if err != nil {
							t.Fatal(err)
						}
						x.unary.source = source
					}
				} else {
					parsed, err := protocols.Parse(wire.Chat, body, routeSlug)
					if err != nil {
						t.Fatal(err)
					}
					x.generation.parsed = parsed
				}
				if _, err := x.prepareCandidate(&provider, target.ID, target.ProviderModel); err != nil {
					t.Fatal(err)
				}
				if _, err := x.prepareCandidate(&provider, "missing-target", target.ProviderModel); err == nil {
					t.Fatal("a target without a compiled contract reused another target's preparation")
				}
				var preparedBody []byte
				if native {
					plan, err := x.unaryPlan(&provider, target.ID, target.ProviderModel)
					if err != nil {
						t.Fatal(err)
					}
					preparedBody = plan.Body()
				} else {
					prepared, err := x.generationRequest(&provider, target.ID, target.ProviderModel)
					if err != nil {
						t.Fatal(err)
					}
					preparedBody = prepared.document.Bytes()
				}
				if !bytes.Contains(preparedBody, []byte(text)) {
					t.Fatalf("prepared request does not contain current input %q: %s", text, preparedBody)
				}
			}
		})
	}
}

func TestRealtimeCallAdmissionHonorsRequestedPriority(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, defaultPriority, header, expected string }{
		{"caller critical", "normal", `{"priority":"critical"}`, "critical"},
		{"key critical", "critical", "", "critical"},
		{"normal", "normal", "", "normal"},
	} {
		t.Run(test.name, func(t *testing.T) {
			pool := admission.NewPool(1).Queue(2, 2*time.Second)
			management := admission.NewPool(1)
			outer := &admission.Public{Inference: pool, Management: management, InferenceEnabled: true, Reject: func(w http.ResponseWriter, _ *http.Request, _ string) { w.WriteHeader(503) }}
			h := newHarnessWrapped(t, Config{}, outer.Wrap)
			critical := "critical"
			authority := h.rt.keys[fullKey]
			authority.Policy.MaxPriority = &critical
			authority.Policy.Priority = &test.defaultPriority
			h.rt.keys[fullKey] = authority
			registry := prometheus.NewRegistry()
			registry.MustRegister(admission.Metrics(pool, management))
			depth := func() map[string]float64 {
				families, e := registry.Gather()
				if e != nil {
					t.Fatal(e)
				}
				out := map[string]float64{}
				for _, f := range families {
					if f.GetName() != "olp_admission_queue_depth" {
						continue
					}
					for _, m := range f.Metric {
						for _, label := range m.Label {
							if label.GetName() == "class" {
								out[label.GetValue()] = m.GetGauge().GetValue()
							}
						}
					}
				}
				return out
			}
			holder := pool.Enter()
			defer holder.Release()
			req, e := http.NewRequest("POST", h.server.URL+"/v1/realtime/calls?model="+routeSlug, strings.NewReader("v=0\r\n"))
			if e != nil {
				t.Fatal(e)
			}
			req.Header.Set("Authorization", "Bearer "+fullKey)
			req.Header.Set("Content-Type", "application/sdp")
			if test.header != "" {
				req.Header.Set("X-OLP-Routing", test.header)
			}
			_, resolved, controlErr := routingControls(req, authority.Policy)
			if controlErr != nil || resolved != test.expected {
				t.Fatalf("fixture priority=%s err=%v", resolved, controlErr)
			}
			done := make(chan error, 1)
			go func() {
				r, e := h.server.Client().Do(req)
				if e == nil {
					io.Copy(io.Discard, r.Body)
					r.Body.Close()
				}
				done <- e
			}()
			var observed map[string]float64
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				observed = depth()
				if observed["normal"]+observed["critical"] > 0 {
					break
				}
				time.Sleep(time.Millisecond)
			}
			holder.Release()
			if e := <-done; e != nil {
				t.Fatal(e)
			}
			t.Logf("priority=%s queued depths=%v", test.expected, observed)
			if pool.Admitted() != 0 {
				t.Fatalf("admission permit leaked: %d", pool.Admitted())
			}
			h.mock.mu.Lock()
			calls := len(h.mock.calls)
			h.mock.mu.Unlock()
			if calls != 0 {
				t.Fatal("unexpected provider dispatch")
			}
			if observed[test.expected] != 1 {
				t.Fatalf("request entered wrong admission class: %v", observed)
			}
		})
	}
}

type deadlineFailWriter struct {
	*httptest.ResponseRecorder
	ctx context.Context
}

func (w *deadlineFailWriter) Write([]byte) (int, error) {
	<-w.ctx.Done()
	return 0, io.ErrClosedPipe
}

func TestRetainedResponseStreamClassifiesDeadlineAndProtocolFailures(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		timeout, frame, writeFailure bool
	}{
		{"idle deadline", true, false, false},
		{"truncated stream", false, false, false},
		{"committed deadline", true, true, false},
		{"client write failure at deadline", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{})
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				if tc.frame {
					_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"m\",\"content_index\":0,\"delta\":\"hello\"}\n\n")
				}
				_ = http.NewResponseController(w).Flush()
				if tc.timeout {
					<-r.Context().Done()
				}
			})
			snapshot := h.rt.release.Snapshot().Clone()
			route := snapshot.Routes[routeSlug]
			route.OverallTimeout = 150
			target := route.Targets[0]
			provider := snapshot.Providers[target.ProviderID]
			p := &pin{provider: provider, target: target, model: modelA, slot: provider.Slots[0], attempt: routing.Attempt{ProviderID: provider.ID, UpstreamModel: modelA}}
			x := &call{request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release}, family: wire.Responses, actor: "api_key_resource", keyID: h.keyID}
			x.route = &route
			ctx, cancel := h.gateway.stateDeadline(t.Context(), &route)
			defer cancel()
			recorder := httptest.NewRecorder()
			var writer http.ResponseWriter = recorder
			if tc.writeFailure {
				writer = &deadlineFailWriter{ResponseRecorder: recorder, ctx: ctx}
			}
			failure := h.gateway.streamStoredResponse(ctx, writer, x, &resources.Resource{ID: uuid.NewString(), Kind: resources.KindResponse, UpstreamID: "response-local-fixture", RouteSlug: routeSlug}, p, nil)
			code, status, class := "response_stream_incomplete", http.StatusBadGateway, classProtocol
			if tc.timeout {
				code, status, class = "gateway_timeout", http.StatusGatewayTimeout, classTimeout
			}
			if tc.frame {
				if failure != nil || recorder.Code != http.StatusOK {
					t.Fatalf("committed response: failure=%v status=%d", failure, recorder.Code)
				}
				outcome := "failure"
				envelopeStatus := http.StatusOK
				if tc.writeFailure {
					code, class, outcome, envelopeStatus = "client_cancelled", classCancelled, "cancelled", 0
				}
				h.sink.mu.Lock()
				defer h.sink.mu.Unlock()
				if len(h.sink.envs) != 1 {
					t.Fatalf("terminal envelopes=%d, want1", len(h.sink.envs))
				}
				env := h.sink.envs[0]
				if env.ErrorClass != code || env.Outcome != outcome || env.Status != envelopeStatus || !env.Committed || len(env.Attempts) != 1 || env.Attempts[0].Class != class {
					t.Fatalf("terminal envelope=%+v", env)
				}
				if !tc.writeFailure && !strings.Contains(recorder.Body.String(), "hello") {
					t.Fatal("nonterminal frame was not delivered")
				}
			} else if failure == nil || failure.Code != code || failure.Status != status || x.facts[0].Class != class {
				t.Fatalf("failure=%+v class=%s; want%d %s %s", failure, x.facts[0].Class, status, code, class)
			}

			if tc.timeout && ctx.Err() != context.DeadlineExceeded {
				t.Fatalf("route deadline did not expire: %v", ctx.Err())
			}
		})
	}
}

func TestStrictRetainedResponseSettlesAfterNativeValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, object, class string
	}{
		{"valid terminal result", `"response"`, classSuccess},
		{"invalid terminal result", `7`, classProtocol},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, err := pgxpool.New(t.Context(), "postgres://unused@127.0.0.1:1/unused")
			if err != nil {
				t.Fatal(err)
			}
			pool.Close()
			h := newHarness(t, Config{}, Dependencies{Resources: resources.New(pool)})
			h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"native-response\",\"object\":%s,\"status\":\"completed\",\"output\":[]}}\n\n", tc.object)
			})
			p := testPinnedTarget(h, 0)
			p.provider.VendorID = "openai"
			probed, granted := openTestProbe(t, h, p.provider.ID)
			if !probed || !granted {
				t.Fatal("could not claim half-open probe")
			}
			p.hold = &dispatchHold{provider: p.provider.ID, probed: probed}
			x := &call{request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release}, family: wire.Responses, actor: "api_key_resource", keyID: h.keyID}
			x.route = &routing.Route{Slug: routeSlug, Fidelity: routing.RouteFidelity{Mode: routing.FidelityStrict}}
			res := &resources.Resource{ID: resources.LocalID(resources.KindStrictResponse, uuid.New()), Kind: resources.KindStrictResponse, UpstreamID: "native-response", RouteSlug: routeSlug}
			recorder := httptest.NewRecorder()
			failure := h.gateway.streamStoredResponse(t.Context(), recorder, x, res, p, nil)
			if failure == nil || failure.Code != "response_stream_incomplete" {
				t.Fatalf("unexpected stream result: %+v", failure)
			}
			h.gateway.refuse(x, recorder, failure)
			h.gateway.settlePinnedHold(t.Context(), p.hold)
			env := h.sink.last(t)
			if len(env.Attempts) != 1 || env.Attempts[0].Class != tc.class {
				t.Fatalf("unexpected attempt evidence: %+v", env.Attempts)
			}
			if probed, granted := h.gateway.health.Claim(p.provider.ID); probed || granted != (tc.class == classSuccess) {
				t.Fatalf("after settlement, probe=%v granted=%v", probed, granted)
			}
		})
	}
}

func TestGeminiBatchSettlementValidatesResponseIdentities(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, nestedName, rootInput string
		valid                       bool
	}{
		{"matching references", "batches/native", "files/native-input", true},
		{"mismatched nested name", "batches/unrelated", "files/native-input", false},
		{"conflicting input alias", "batches/native", "files/unrelated", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{})
			h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"name":"batches/native","metadata":{"name":%q,"state":"BATCH_STATE_RUNNING","inputConfig":{"fileName":"files/native-input"}},"input_config":{"file_name":%q}}`, tc.nestedName, tc.rootInput)
			})
			p := testPinnedTarget(h, 0)
			probed, granted := openTestProbe(t, h, p.provider.ID)
			if !probed || !granted {
				t.Fatal("could not claim half-open probe")
			}
			p.hold = &dispatchHold{provider: p.provider.ID, probed: probed}
			x := &call{request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release}, family: wire.GeminiBatch, actor: "api_key_resource", keyID: h.keyID}
			response, failure := h.gateway.pinnedDo(t.Context(), x, p, http.MethodGet, p.provider.Endpoint+"/batches/native", nil, "")
			if failure != nil {
				t.Fatal(failure.toError())
			}
			defer response.Body.Close()
			body, err := response.readBody(h.gateway.cfg.MaxResponseBytes)
			if err != nil {
				t.Fatal(err)
			}
			batch, err := batchupstream.DecodeGeminiBatch(body)
			if err != nil {
				t.Fatalf("could not decode native fixture: %v", err)
			}
			object, _, err := settleGeminiBatchResult(response.exchange, body, batch, p.model, geminiInputNames{Local: "files/local-input", Upstream: "files/native-input"})
			if (err == nil) != tc.valid {
				t.Fatalf("unexpected batch validation: %v", err)
			}
			if tc.valid {
				projected, err := object.project("local", routeSlug, nil)
				if err != nil || bytes.Contains(projected, []byte("native-input")) || bytes.Count(projected, []byte("files/local-input")) != 2 {
					t.Fatalf("projected references: %s, %v", projected, err)
				}
			}
			h.gateway.settlePinnedHold(t.Context(), p.hold)
			if probed, granted := h.gateway.health.Claim(p.provider.ID); probed || granted != tc.valid {
				t.Fatalf("after settlement, probe=%v granted=%v", probed, granted)
			}
		})
	}
}

func TestGeminiManagedFileRejectsOversizedProviderLine(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{MaxResponseBytes: 4 << 20})
	spool, err := staging.NewSpool(t.TempDir(), staging.MinCapacityBytes, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spool.Close() })
	h.gateway.deps.Staging = spool
	h.mock.set("download", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"key":"huge","error":{"message":"`+strings.Repeat("a", managedBatchLineLimit+2)+`"}}`+"\n")
	})
	p := testPinnedTarget(h, 0)
	probed, granted := openTestProbe(t, h, p.provider.ID)
	if !probed || !granted {
		t.Fatal("could not claim half-open probe")
	}
	p.hold = &dispatchHold{provider: p.provider.ID, probed: probed}
	x := &call{request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release}, family: wire.File, actor: "api_key_resource", keyID: h.keyID}
	res := &resources.Resource{UpstreamID: "files/result", RouteSlug: routeSlug, Metadata: json.RawMessage(`{"role":"error"}`)}
	recorder := httptest.NewRecorder()
	failure := h.gateway.geminiManagedFileContent(t.Context(), recorder, x, res, p)
	if failure == nil || failure.Code != "fidelity_protocol_violation" {
		t.Fatalf("unexpected download result: %+v", failure)
	}
	h.gateway.refuse(x, recorder, failure)
	h.gateway.settlePinnedHold(t.Context(), p.hold)
	if probed, granted := h.gateway.health.Claim(p.provider.ID); probed || granted {
		t.Fatalf("oversized native line left circuit available, probe=%v granted=%v", probed, granted)
	}
}

type failedFlushWriter struct{ *httptest.ResponseRecorder }

func (w failedFlushWriter) FlushError() error { return io.ErrClosedPipe }

func TestGeminiInteractionDeleteAcknowledgementRequiresFlush(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{})
	h.mock.set("a", status(http.StatusNoContent, ""))
	p := testPinnedTarget(h, 0)
	route := routing.Route{Fidelity: routing.RouteFidelity{Mode: routing.FidelityStrict}}
	x := &call{request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release}, family: wire.GeminiInteractions, actor: "api_key_resource", keyID: h.keyID}
	x.route = &route
	response, failure := h.gateway.pinnedDo(t.Context(), x, p, http.MethodDelete, p.provider.Endpoint+"/interactions/native", nil, "")
	if failure != nil {
		t.Fatal(failure.toError())
	}
	w := failedFlushWriter{httptest.NewRecorder()}
	if failure := h.gateway.finishGeminiInteractionDeletion(t.Context(), w, x, response, func(context.Context) error { return nil }); failure != nil {
		t.Fatal(failure)
	}
	env := h.sink.last(t)
	if w.Code != http.StatusOK || env.Outcome != "cancelled" || env.ErrorClass != "client_delivery_failed" || len(env.Attempts) != 1 ||
		env.Attempts[0].Class != classSuccess || env.Attempts[0].Interaction == nil || env.Attempts[0].Interaction.ClientState != usage.ClientUnobserved {
		t.Fatalf("unexpected delete acknowledgement evidence, status=%d envelope=%+v attempts=%+v", w.Code, env, env.Attempts)
	}
}

func TestRetainedDeletionSettlesProviderBeforeLocalWrite(t *testing.T) {
	t.Parallel()
	store := retainedapi.Family{Operation: "vector_store", Kind: "vector_store", Identity: "id"}
	item := retainedapi.Family{Operation: "conversation", Kind: "conversation_item", Identity: "id"}
	for _, tc := range []struct {
		name, body, class string
		available         bool
		charge            string
		family            retainedapi.Family
	}{
		{"accepted deletion", `{"id":"resource-native","deleted":true}`, classSuccess, true, "0.25", store},
		{"malformed provider result", `{"id":"resource-native","deleted":true,"unknown_id":"unexpected"}`, classProtocol, false, "0", store},
		{"unrelated native identity", `{"id":"unrelated","deleted":true}`, classProtocol, false, "0", store},
		{"conversation item returns parent identity", `{"id":"conversation-native"}`, classSuccess, true, "0.25", item},
		{"conversation item returns child identity", `{"id":"resource-native"}`, classProtocol, false, "0", item},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, err := pgxpool.New(t.Context(), "postgres://unused@127.0.0.1:1/unused")
			if err != nil {
				t.Fatal(err)
			}
			pool.Close()
			h := newHarness(t, Config{}, Dependencies{Resources: resources.New(pool)})
			h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			})
			p := testPinnedTarget(h, 0)
			probed, granted := openTestProbe(t, h, p.provider.ID)
			if !granted || !probed {
				t.Fatal("could not claim half-open probe")
			}
			p.hold = &dispatchHold{provider: p.provider.ID, probed: probed}
			p.attempt.Price = rates.NewRoutingPrice(rates.RoutingPrice{Price: rates.Price{PerRequest: new("0.25")}})
			x := &call{request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release}, family: wire.File, actor: "api_key_resource", keyID: h.keyID}
			id := uuid.New()
			resource := &resources.Resource{UUID: id, ID: resources.LocalID(tc.family.Kind, id), Kind: tc.family.Kind, UpstreamID: "resource-native"}
			var parent *resources.Resource
			if tc.family.Kind == "conversation_item" {
				id := uuid.New()
				parent = &resources.Resource{UUID: id, ID: resources.LocalID("conversation", id), Kind: "conversation", UpstreamID: "conversation-native"}
				resource.ParentID = &parent.UUID
			}
			r := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/v1/resources/"+resource.ID, nil)
			w := httptest.NewRecorder()
			endpoint := strings.TrimRight(p.provider.Endpoint, "/") + "/resources/resource-native"
			status := h.gateway.retainedDispatch(w, r, x, p, retainedapi.Endpoint{Family: tc.family, Method: http.MethodDelete, Action: "delete"}, resource, parent, endpoint, nil, "", nil, func(e *fault.Error) {
				h.gateway.refuse(x, w, e)
			})
			h.gateway.settlePinnedHold(t.Context(), p.hold)
			env := h.sink.last(t)
			if status != http.StatusServiceUnavailable || w.Code != status || env.Outcome != "failure" || env.ErrorClass != "provider_state_unavailable" {
				t.Fatalf("local deletion failure changed, status=%d body=%s outcome=%s error=%s", status, w.Body.String(), env.Outcome, env.ErrorClass)
			}
			if len(env.Attempts) != 1 || env.Attempts[0].Class != tc.class || h.gateway.settledCharge(x).String() != tc.charge {
				t.Fatalf("unexpected attempt evidence or charge: %+v, %s", env.Attempts, h.gateway.settledCharge(x).String())
			}
			if probed, granted := h.gateway.health.Claim(p.provider.ID); probed || granted != tc.available {
				t.Fatalf("after settlement, probe=%v granted=%v", probed, granted)
			}
		})
	}
}

func TestRetainedListBindingFailureDoesNotSettleProviderSuccess(t *testing.T) {
	t.Parallel()
	pool, err := pgxpool.New(t.Context(), "postgres://unused@127.0.0.1:1/unused")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	h := newHarness(t, Config{}, Dependencies{Resources: resources.New(pool)})
	h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"file-native","vector_store_id":"store-other"}]}`)
	})
	p := testPinnedTarget(h, 0)
	probed, granted := openTestProbe(t, h, p.provider.ID)
	if !granted || !probed {
		t.Fatal("could not claim half-open probe")
	}
	p.hold = &dispatchHold{provider: p.provider.ID, probed: probed}
	p.attempt.Price = rates.NewRoutingPrice(rates.RoutingPrice{Price: rates.Price{PerRequest: new("0.25")}})
	x := &call{
		request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release},
		family:  wire.File, actor: "api_key_resource", keyID: h.keyID,
		retainedAdmission: &retainedAdmissionPlan{billing: retainedPriced},
	}
	id := uuid.New()
	parent := &resources.Resource{UUID: id, ID: resources.LocalID("vector_store", id), Kind: "vector_store", UpstreamID: "store-native", TargetID: p.target.ID}
	e := retainedapi.Endpoint{
		Family:    retainedapi.Family{Operation: "vector_store", Kind: "vector_store_file", Identity: "id", Collection: "data"},
		Method:    http.MethodGet,
		Action:    "list",
		Ancestors: []string{"vector_store"},
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/vector_stores/"+parent.ID+"/files", nil)
	w := httptest.NewRecorder()
	endpoint := strings.TrimRight(p.provider.Endpoint, "/") + "/vector_stores/store-native/files"
	status := h.gateway.retainedDispatch(w, r, x, p, e, parent, parent, endpoint, nil, "", nil, func(e *fault.Error) {
		h.gateway.refuse(x, w, e)
	})
	h.gateway.settlePinnedHold(t.Context(), p.hold)
	env := h.sink.last(t)
	if status != http.StatusServiceUnavailable || w.Code != status || env.ErrorClass != "provider_state_unavailable" {
		t.Fatalf("binding store failure was not reported as unavailable: %d, %s", status, w.Body.String())
	}
	if len(env.Attempts) != 1 || env.Attempts[0].Class != classProtocol || env.Attempts[0].UsageObserved || !h.gateway.settledCharge(x).IsZero() {
		t.Fatalf("unexpected attempt evidence or charge: %+v, %s", env.Attempts, h.gateway.settledCharge(x).String())
	}
	if probed, granted := h.gateway.health.Claim(p.provider.ID); !probed || !granted {
		t.Fatalf("a local binding failure changed provider health: probe=%v granted=%v", probed, granted)
	}
}

func TestPlaygroundStructuredOutputPreservesLargeInteger(t *testing.T) {
	t.Parallel()
	const exact = `{"count":9007199254740993}`
	playground := Playground{Gateway: &Server{now: time.Now}}
	x := &call{request: request{startedAt: time.Now()}, routeLeg: routeLeg{route: &routing.Route{Slug: routeSlug}}}
	in := playgroundRequest{ResponseFormat: &playgroundFormat{Type: "json_object"}}
	for _, test := range []struct{ text, structured string }{
		{exact, exact},
		{`{"count":0,"count":9007199254740993}`, exact},
		{exact + ` {"extra":true}`, ""},
	} {
		out := &outcome{completion: &wire.Completion{OutputText: test.text}}
		body, err := json.Marshal(playground.response(x, out, in))
		if err != nil {
			t.Fatal(err)
		}
		var response struct {
			Text       string          `json:"output_text"`
			Structured json.RawMessage `json:"structured_output"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			t.Fatal(err)
		}
		if response.Text != test.text || string(response.Structured) != test.structured {
			t.Fatalf("unexpected Playground result for %q: %s", test.text, body)
		}
	}
}

func TestManagedFileRefreshAttributesMetadataFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, storedMetadata, providerBody string
		local                              bool
	}{
		{"provider byte count", `{"adapter":"mistral"}`, `{"id":"file-native","bytes":-1}`, false},
		{"stored byte count", `{"adapter":"mistral","provider_file":{"bytes":-1}}`, `{"id":"file-native","bytes":10}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{})
			h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.providerBody)
			})
			p := testPinnedTarget(h, 0)
			probed, granted := openTestProbe(t, h, p.provider.ID)
			if !granted || !probed {
				t.Fatal("could not claim half-open probe")
			}
			p.hold = &dispatchHold{provider: p.provider.ID, probed: probed}
			x := &call{request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release}, family: wire.File, actor: "api_key_resource", keyID: h.keyID}
			id := uuid.New()
			res := &resources.Resource{UUID: id, ID: resources.LocalID(resources.KindFile, id), Kind: resources.KindFile, UpstreamID: "file-native", Metadata: json.RawMessage(tc.storedMetadata)}
			w := httptest.NewRecorder()
			failure := h.gateway.getPinnedFile(t.Context(), w, x, keyauthority.Authority{ID: h.keyID}, res, p)
			if failure == nil || failure.Status != http.StatusBadGateway || failure.Code != "fidelity_protocol_violation" {
				t.Fatalf("unexpected file refresh failure: %+v", failure)
			}
			h.gateway.refuse(x, w, failure)
			h.gateway.settlePinnedHold(t.Context(), p.hold)
			env := h.sink.last(t)
			if len(env.Attempts) != 1 || env.Attempts[0].Class != classProtocol || env.Attempts[0].LocalRefusal != tc.local {
				t.Fatalf("unexpected attempt evidence: %+v", env.Attempts)
			}
			if probed, granted := h.gateway.health.Claim(p.provider.ID); probed != tc.local || granted != tc.local {
				t.Fatalf("after settlement, probe=%v granted=%v", probed, granted)
			}
		})
	}
}

func testPinnedTarget(h *harness, index int) *pin {
	snapshot := h.rt.release.Snapshot().Clone()
	target := snapshot.Routes[routeSlug].Targets[index]
	provider := snapshot.Providers[target.ProviderID]
	return &pin{provider: provider, target: target, model: target.ProviderModel, slot: provider.Slots[0], attempt: routing.Attempt{ProviderID: provider.ID, UpstreamModel: target.ProviderModel}}
}

func openTestProbe(t *testing.T, h *harness, provider string) (bool, bool) {
	t.Helper()
	now := time.Now()
	h.gateway.health = health.NewTracker(func() time.Time { return now })
	for range 30 {
		_, ready := h.gateway.health.Claim(provider)
		if !ready {
			now = now.Add(health.CircuitOpenFor + time.Millisecond)
			return h.gateway.health.Claim(provider)
		}
		h.gateway.health.Record(provider, events.AttemptFact{Class: classProtocol}, false)
	}
	t.Fatal("circuit stayed closed after consecutive provider failures")
	return false, false
}

type pinnedLimitCounter struct {
	unreachableValkey
	used map[string]bool
}

func (c pinnedLimitCounter) Do(_ context.Context, args ...string) (any, error) {
	switch len(args) {
	case 12:
		if c.used[args[3]] {
			return []any{int64(2), int64(0), "rpm", int64(30000), int64(42), int64(0)}, nil
		}
		c.used[args[3]] = true
		return []any{int64(2), int64(1), "ok", int64(0), int64(42), int64(0)}, nil
	case 9:
		delete(c.used, args[3])
		return int64(1), nil
	default:
		return nil, fmt.Errorf("unexpected rate command with %d arguments", len(args))
	}
}

func TestPinnedUnsentReservationRefundsAfterAnotherExchange(t *testing.T) {
	t.Parallel()
	runner, err := scripts.NewRunner(pinnedLimitCounter{used: make(map[string]bool)}, "test")
	if err != nil {
		t.Fatal(err)
	}
	admission := quota.NewAdmission(limitadmission.NewDurable(runner, nil), nil, nil, slog.New(slog.DiscardHandler))
	h := newHarness(t, Config{}, Dependencies{Admission: admission})
	h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	sent, unsent := testPinnedTarget(h, 0), testPinnedTarget(h, 1)
	for _, p := range []*pin{sent, unsent} {
		p.provider.Limits = &routing.Limits{RequestsPerMinute: new(int64(1))}
	}
	probed, granted := openTestProbe(t, h, unsent.provider.ID)
	if !probed || !granted {
		t.Fatal("could not claim half-open probe")
	}
	h.gateway.health.ReleaseProbe(unsent.provider.ID)
	reserve := func(p *pin) gateResult {
		return acquireDispatchHold(t.Context(), h.gateway, &p.provider, &p.slot, 1, time.Minute, "")
	}
	first := reserve(sent)
	if first.verdict != gateAdmitted {
		t.Fatalf("first admission: %+v", first)
	}
	sent.hold = first.hold
	x := &call{request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release}, family: wire.File, actor: "api_key_resource", keyID: h.keyID}
	response, failure := h.gateway.pinnedDo(t.Context(), x, sent, http.MethodGet, sent.provider.Endpoint, nil, "")
	if failure != nil {
		t.Fatalf("first exchange: %+v", failure)
	}
	_, err = response.readBody(1024)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	response.exchange.complete(classSuccess)
	h.gateway.settlePinnedHold(t.Context(), sent.hold)

	second := reserve(unsent)
	if second.verdict != gateAdmitted {
		t.Fatalf("second admission: %+v", second)
	}
	unsent.hold = second.hold
	if _, failure := h.gateway.pinnedDo(t.Context(), x, unsent, http.MethodGet, "://invalid", nil, ""); failure == nil {
		t.Fatal("invalid endpoint dispatched")
	}
	h.gateway.settlePinnedHold(t.Context(), unsent.hold)
	if after := reserve(unsent); after.verdict != gateAdmitted {
		t.Fatalf("second provider remained blocked by a quota or probe: %+v", after)
	} else {
		h.gateway.releaseHold(t.Context(), after.hold)
	}
	if after := reserve(sent); after.verdict != gateRejected {
		t.Fatalf("first provider's dispatched request did not consume its allowance: %+v", after)
	}
}

func TestResourceListsRejectInvalidCursor(t *testing.T) {
	t.Parallel()
	pool, err := pgxpool.New(t.Context(), "postgres://unused@127.0.0.1:1/unused")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	spool, err := staging.NewSpool(t.TempDir(), staging.MinCapacityBytes, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spool.Close() })
	h := newHarness(t, Config{}, Dependencies{Resources: resources.New(pool), Staging: spool})
	for _, list := range []struct{ kind, other, path string }{
		{"batch", "file", "/v1/batches"},
		{"file", "batch", "/v1/files"},
	} {
		for _, tc := range []struct {
			name, cursor string
			status       int
			code, param  string
		}{
			{"missing separator", "not-an-id", 400, "invalid_value", "after"},
			{"malformed UUID", list.kind + "_not-a-uuid", 400, "invalid_value", "after"},
			{"wrong kind", resources.LocalID(list.other, uuid.New()), 400, "invalid_value", "after"},
			{"database failure", resources.LocalID(list.kind, uuid.New()), 500, "internal_error", ""},
		} {
			t.Run(list.kind+"/"+tc.name, func(t *testing.T) {
				response := h.do(t.Context(), http.MethodGet, list.path+"?after="+tc.cursor, fullKey, nil, nil)
				defer response.Body.Close()
				var body struct {
					Error struct {
						Code  string `json:"code"`
						Param string `json:"param"`
					} `json:"error"`
				}
				if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != tc.status || body.Error.Code != tc.code || body.Error.Param != tc.param {
					t.Errorf("cursor response = %d %#v, want %d %s at %s", response.StatusCode, body, tc.status, tc.code, tc.param)
				}
			})
		}
		t.Run(list.kind+"/trailing limit characters", func(t *testing.T) {
			response := h.do(t.Context(), http.MethodGet, list.path+"?limit=10abc", fullKey, nil, nil)
			defer response.Body.Close()
			var body struct {
				Error struct {
					Code  string `json:"code"`
					Param string `json:"param"`
				} `json:"error"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != 400 || body.Error.Code != "invalid_value" || body.Error.Param != "limit" {
				t.Errorf("limit response = %d %#v, want 400 invalid_value at limit", response.StatusCode, body)
			}
		})
	}
}

func TestRetainedGeminiKeyQueryParamDropped(t *testing.T) {
	t.Parallel()
	endpoint := retainedapi.Endpoint{Family: retainedapi.Family{Operation: "context_cache", Surface: "gemini", Kind: "context_cache"}, Action: "list"}
	server := &Server{}
	query, failure := server.retainedQuery(t.Context(), &call{}, endpoint, nil, url.Values{"key": {"secret"}, "pageSize": {"10"}})
	if failure != nil {
		t.Fatalf("retainedQuery with key: %v", failure)
	}
	if _, forwarded := query["key"]; forwarded {
		t.Fatalf("key forwarded upstream in %v", query)
	}
	if query.Get("pageSize") != "10" {
		t.Fatalf("pageSize = %v, want 10", query)
	}
	openAIEndpoint := retainedapi.Endpoint{Family: retainedapi.Family{Operation: "vector_store", Surface: "openai", Kind: "vector_store"}, Action: "list"}
	if _, failure := server.retainedQuery(t.Context(), &call{}, openAIEndpoint, nil, url.Values{"key": {"secret"}}); failure == nil {
		t.Fatal("key accepted on non-Gemini surface")
	}
	request := httptest.NewRequest(http.MethodGet, "/gemini/v1beta/cachedContents?key=secret&pageSize=0", nil)
	var captured *fault.Error
	status := server.listRetainedGeneric(httptest.NewRecorder(), request, &call{}, keyauthority.Authority{}, endpoint, func(err *fault.Error) { captured = err })
	if status != 400 || captured == nil || !strings.Contains(captured.Message, "Page size") {
		t.Fatalf("list with key and pageSize=0 = %d %+v, want 400 page size error", status, captured)
	}
}

func TestListedResourcePageSkipsStaleRowsAndRefills(t *testing.T) {
	t.Parallel()
	all := []*resources.Resource{
		{ID: "stale-1"}, {ID: "hidden-1"}, {ID: "valid-1"}, {ID: "valid-2"}, {ID: "stale-2"}, {ID: "valid-3"},
	}
	fetchFrom := func(rows []*resources.Resource) func(string, int) ([]*resources.Resource, *fault.Error) {
		return func(cursor string, count int) ([]*resources.Resource, *fault.Error) {
			start := 0
			if cursor != "" {
				start = len(rows)
				for i, row := range rows {
					if row.ID == cursor {
						start = i + 1
						break
					}
				}
			}
			if start >= len(rows) {
				return nil, nil
			}
			return rows[start:min(start+count, len(rows))], nil
		}
	}
	authorize := func(row *resources.Resource) *fault.Error {
		switch {
		case strings.HasPrefix(row.ID, "stale"):
			return pinUnavailable()
		case strings.HasPrefix(row.ID, "hidden"):
			return fault.NotFoundError("not_found", "No resource with this identifier exists for this key.")
		case row.ID == "broken-1":
			return fault.ServerError(http.StatusInternalServerError, "internal_error", "The stored target could not be rebuilt.")
		}
		return nil
	}
	ids := func(page []*resources.Resource) []string {
		got := make([]string, len(page))
		for i, row := range page {
			got[i] = row.ID
		}
		return got
	}

	page, continuation, err := listedResourcePage(10, "", fetchFrom(all), authorize)
	if err != nil {
		t.Fatalf("mixed page error: %v", err)
	}
	if continuation != "" || !slices.Equal(ids(page), []string{"valid-1", "valid-2", "valid-3"}) {
		t.Fatalf("mixed page = %v continuation=%q, want only the valid rows and no continuation", ids(page), continuation)
	}

	page, continuation, err = listedResourcePage(2, "", fetchFrom(all), authorize)
	if err != nil {
		t.Fatalf("refilled page error: %v", err)
	}
	if continuation != "valid-2" || !slices.Equal(ids(page), []string{"valid-1", "valid-2"}) {
		t.Fatalf("refilled page = %v continuation=%q, want [valid-1 valid-2] with a continuation", ids(page), continuation)
	}
	page, continuation, err = listedResourcePage(2, continuation, fetchFrom(all), authorize)
	if err != nil {
		t.Fatalf("following page error: %v", err)
	}
	if continuation != "" || !slices.Equal(ids(page), []string{"valid-3"}) {
		t.Fatalf("following page = %v continuation=%q, want [valid-3] and no continuation", ids(page), continuation)
	}

	staleOnly := []*resources.Resource{{ID: "stale-1"}, {ID: "stale-2"}, {ID: "stale-3"}}
	page, continuation, err = listedResourcePage(2, "", fetchFrom(staleOnly), authorize)
	if err != nil {
		t.Fatalf("all-stale page error: %v", err)
	}
	if continuation != "" || len(page) != 0 {
		t.Fatalf("all-stale page = %v continuation=%q, want empty with no continuation", ids(page), continuation)
	}

	broken := []*resources.Resource{{ID: "valid-1"}, {ID: "broken-1"}}
	if _, _, err := listedResourcePage(2, "", fetchFrom(broken), authorize); err == nil || err.Status != http.StatusInternalServerError {
		t.Fatalf("broken row error = %v, want the request to fail with 500", err)
	}
	fetchErr := fault.ServerError(http.StatusInternalServerError, "internal_error", "The resource list could not be read.")
	failingFetch := func(string, int) ([]*resources.Resource, *fault.Error) { return nil, fetchErr }
	if _, _, err := listedResourcePage(2, "", failingFetch, authorize); err != fetchErr {
		t.Fatalf("fetch error = %v, want %v", err, fetchErr)
	}

	t.Run("bounded scan can continue without exposing denied IDs", func(t *testing.T) {
		denied := make([]*resources.Resource, 500)
		for i := range denied {
			denied[i] = &resources.Resource{ID: fmt.Sprintf("stale-%d", i)}
		}
		fetch := fetchFrom(append(denied, &resources.Resource{ID: "valid-1"}))
		server := Server{deps: Dependencies{Digester: secrets.NewAuthKey(make([]byte, 32), "test-installation")}}
		page, cursor, err := server.listedResourcePage("key-a", "pageToken", 2, "", fetch, authorize)
		if err != nil || len(page) != 0 || cursor == "" || strings.Contains(cursor, "stale-") {
			t.Fatalf("first page = %v cursor=%q err=%v, want empty page with an opaque continuation", ids(page), cursor, err)
		}
		if _, _, err := server.listedResourcePage("key-b", "pageToken", 2, cursor, fetch, authorize); err == nil || err.Status != http.StatusBadRequest {
			t.Fatalf("another key's continuation error = %v, want 400", err)
		}
		page, cursor, err = server.listedResourcePage("key-a", "pageToken", 2, cursor, fetch, authorize)
		if err != nil || cursor != "" || !slices.Equal(ids(page), []string{"valid-1"}) {
			t.Fatalf("next page = %v cursor=%q err=%v, want visible row and no continuation", ids(page), cursor, err)
		}
	})
}

func TestFileDownloadHeadersForceAttachmentForHTML(t *testing.T) {
	t.Parallel()
	const body = "<html><body>hello</body></html>"
	recorder := httptest.NewRecorder()
	setFileDownloadHeaders(recorder.Header(), "text/html; charset=utf-8", int64(len(body)))
	if _, err := recorder.WriteString(body); err != nil {
		t.Fatal(err)
	}
	response := recorder.Result()
	defer response.Body.Close()
	if got := response.Header.Get("Content-Disposition"); got != "attachment" {
		t.Errorf("Content-Disposition = %q, want attachment", got)
	}
	if got := response.Header.Get("Content-Security-Policy"); !strings.Contains(got, "sandbox") {
		t.Errorf("Content-Security-Policy = %q, want sandbox policy", got)
	}
	if got := response.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := response.Header.Get("Content-Length"); got != fmt.Sprint(len(body)) {
		t.Errorf("Content-Length = %q", got)
	}
}
