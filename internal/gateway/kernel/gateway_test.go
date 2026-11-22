package kernel

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
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
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	limitadmission "github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/limits/scripts"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/resources"

	"errors"

	"regexp"

	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/health"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
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

// RegisterGateway is set from assembly_test.go, which may import the assembly while this package may not.
var RegisterGateway func(*Server, *http.ServeMux)

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
	RegisterGateway(gw, mux)
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
		{"missing input", `{"index":0,"embedding":[0.5,0]}`, http.StatusBadGateway},
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

func TestEmbeddingsCountTokenIDInputs(t *testing.T) {
	for _, tc := range []struct {
		name, input, data string
		want              int
	}{
		{"batches", `[[1,2],[3,4]]`, `{"index":0,"embedding":[0.5,0]},{"index":1,"embedding":[0.25,0.5]}`, http.StatusOK},
		{"missing batch", `[[1,2],[3,4]]`, `{"index":0,"embedding":[0.5,0]}`, http.StatusBadGateway},
		{"one tokenized input", `[1024,2048]`, `{"index":0,"embedding":[0.5,0]}`, http.StatusOK},
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
			resp := h.do(t.Context(), http.MethodPost, "/v1/embeddings", fullKey, []byte(`{"model":"team-chat","input":`+tc.input+`,"encoding_format":"float"}`), nil)
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

func TestTransformedMediaPreservesUsage(t *testing.T) {
	h := newHarness(t, Config{MaxMediaBodyBytes: 1 << 20})
	spool, err := staging.NewSpool(t.TempDir(), staging.MinCapacityBytes, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spool.Close() })
	h.gateway.Deps.Media = &MediaDeps{Transport: transport.New(transport.Deps{
		Client: h.gateway.client, Auth: h.gateway.auth, Egress: h.gateway.Egress,
		Spool: spool, MaxResponseBytes: 1 << 20, Log: h.gateway.Log,
	})}
	h.gateway.Deps.Staging = spool
	h.gateway.Deps.Uploads = staging.NewAdmission(spool.CapacityBytes())
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
	RegisterGateway(h.gateway, mux)
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
		{"client cancelled", ClassCancelled, "cancelled", "client_cancelled", 5000},
		{"overall timeout", ClassTimeout, "failure", "gateway_timeout", 300},
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
			if tc.class == ClassCancelled {
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
		h.republish(func(snapshot *routing.Snapshot, _, _ routing.Provider) {
			for id, provider := range snapshot.Providers {
				provider.Kind, provider.VendorID, provider.ProfileID, provider.ProfileRevision = "bedrock", "", "", ""
				provider.CloudRegion, provider.AuthMode = "us-east-1", "static"
				provider.Capabilities = []routing.Capability{{Model: provider.Capabilities[0].Model, Operation: "generation", Surface: "bedrock", Mode: "streaming"}}
				snapshot.Providers[id] = provider
			}
		})
		serveBedrock(t, h)
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
		req := httptest.NewRequest(http.MethodPost, "/bedrock/model/team-chat/converse-stream", strings.NewReader(`{"messages":[{"role":"user","content":[{"text":"hello"}]}],"inferenceConfig":{"maxTokens":16}}`))
		req.SetPathValue("model", routeSlug)
		req.Header.Set("Authorization", "Bearer "+fullKey)
		req.Header.Set("Content-Type", "application/json")
		writer := &bedrockFrameWriter{ResponseRecorder: httptest.NewRecorder()}
		BedrockConverseStream(h.gateway)(writer, req)
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
		if attempt.Class != ClassSuccess || !attempt.UsageComplete || attempt.Usage == nil || attempt.Usage.InputTokens != 2 || attempt.Usage.OutputTokens != 3 {
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
			x := &Call{family: wire.Chat, routeLeg: routeLeg{Route: &route}}
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
			p := &Pin{Provider: provider, Target: target, Model: modelA, Slot: provider.Slots[0], attempt: routing.Attempt{ProviderID: provider.ID, UpstreamModel: modelA}}
			x := &Call{request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release}, family: wire.Responses, actor: "api_key_resource", keyID: h.keyID}
			x.Route = &route
			ctx, cancel := h.gateway.StateDeadline(t.Context(), &route)
			defer cancel()
			recorder := httptest.NewRecorder()
			var writer http.ResponseWriter = recorder
			if tc.writeFailure {
				writer = &deadlineFailWriter{ResponseRecorder: recorder, ctx: ctx}
			}
			failure := h.gateway.StreamStoredResponse(ctx, writer, x, &resources.Resource{ID: uuid.NewString(), Kind: resources.KindResponse, UpstreamID: "response-local-fixture", RouteSlug: routeSlug}, p, nil)
			code, status, class := "response_stream_incomplete", http.StatusBadGateway, ClassProtocol
			if tc.timeout {
				code, status, class = "gateway_timeout", http.StatusGatewayTimeout, ClassTimeout
			}
			if tc.frame {
				if failure != nil || recorder.Code != http.StatusOK {
					t.Fatalf("committed response: failure=%v status=%d", failure, recorder.Code)
				}
				outcome := "failure"
				envelopeStatus := http.StatusOK
				if tc.writeFailure {
					code, class, outcome, envelopeStatus = "client_cancelled", ClassCancelled, "cancelled", 0
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
		{"valid terminal result", `"response"`, ClassSuccess},
		{"invalid terminal result", `7`, ClassProtocol},
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
			p.Provider.VendorID = "openai"
			probed, granted := openTestProbe(t, h, p.Provider.ID)
			if !probed || !granted {
				t.Fatal("could not claim half-open probe")
			}
			p.Hold = &dispatchHold{provider: p.Provider.ID, probed: probed}
			x := &Call{request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release}, family: wire.Responses, actor: "api_key_resource", keyID: h.keyID}
			x.Route = &routing.Route{Slug: routeSlug, Fidelity: routing.RouteFidelity{Mode: routing.FidelityStrict}}
			res := &resources.Resource{ID: resources.LocalID(resources.KindStrictResponse, uuid.New()), Kind: resources.KindStrictResponse, UpstreamID: "native-response", RouteSlug: routeSlug}
			recorder := httptest.NewRecorder()
			failure := h.gateway.StreamStoredResponse(t.Context(), recorder, x, res, p, nil)
			if failure == nil || failure.Code != "response_stream_incomplete" {
				t.Fatalf("unexpected stream result: %+v", failure)
			}
			h.gateway.Refuse(x, recorder, failure)
			h.gateway.SettlePinnedHold(t.Context(), p.Hold)
			env := h.sink.last(t)
			if len(env.Attempts) != 1 || env.Attempts[0].Class != tc.class {
				t.Fatalf("unexpected attempt evidence: %+v", env.Attempts)
			}
			if probed, granted := h.gateway.health.Claim(p.Provider.ID); probed || granted != (tc.class == ClassSuccess) {
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
	h.gateway.Deps.Staging = spool
	h.mock.set("download", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"key":"huge","error":{"message":"`+strings.Repeat("a", managedBatchLineLimit+2)+`"}}`+"\n")
	})
	p := testPinnedTarget(h, 0)
	probed, granted := openTestProbe(t, h, p.Provider.ID)
	if !probed || !granted {
		t.Fatal("could not claim half-open probe")
	}
	p.Hold = &dispatchHold{provider: p.Provider.ID, probed: probed}
	x := &Call{request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release}, family: wire.File, actor: "api_key_resource", keyID: h.keyID}
	res := &resources.Resource{UpstreamID: "files/result", RouteSlug: routeSlug, Metadata: json.RawMessage(`{"role":"error"}`)}
	recorder := httptest.NewRecorder()
	failure := h.gateway.GeminiManagedFileContent(t.Context(), recorder, x, res, p)
	if failure == nil || failure.Code != "fidelity_protocol_violation" {
		t.Fatalf("unexpected download result: %+v", failure)
	}
	h.gateway.Refuse(x, recorder, failure)
	h.gateway.SettlePinnedHold(t.Context(), p.Hold)
	if probed, granted := h.gateway.health.Claim(p.Provider.ID); probed || granted {
		t.Fatalf("oversized native line left circuit available, probe=%v granted=%v", probed, granted)
	}
}

type failedFlushWriter struct{ *httptest.ResponseRecorder }

func (w failedFlushWriter) FlushError() error { return io.ErrClosedPipe }

func TestPinnedAcknowledgementRequiresFlush(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{})
	h.mock.set("a", status(http.StatusNoContent, ""))
	p := testPinnedTarget(h, 0)
	route := routing.Route{Fidelity: routing.RouteFidelity{Mode: routing.FidelityStrict}}
	x := &Call{request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release}, family: wire.GeminiInteractions, actor: "api_key_resource", keyID: h.keyID}
	x.Route = &route
	response, failure := h.gateway.PinnedDo(t.Context(), x, p, http.MethodDelete, p.Provider.Endpoint+"/interactions/native", nil, "")
	if failure != nil {
		t.Fatal(failure.Fault())
	}
	w := failedFlushWriter{httptest.NewRecorder()}
	defer response.Body.Close()
	response.Exchange.Complete(ClassSuccess)
	h.gateway.WriteStateJSONStatus(w, x, nil, http.StatusOK, response.Exchange)
	env := h.sink.last(t)
	if w.Code != http.StatusOK || env.Outcome != "cancelled" || env.ErrorClass != "client_delivery_failed" || len(env.Attempts) != 1 ||
		env.Attempts[0].Class != ClassSuccess || env.Attempts[0].Interaction == nil || env.Attempts[0].Interaction.ClientState != usage.ClientUnobserved {
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
		{"accepted deletion", `{"id":"resource-native","deleted":true}`, ClassSuccess, true, "0.25", store},
		{"malformed provider result", `{"id":"resource-native","deleted":true,"unknown_id":"unexpected"}`, ClassProtocol, false, "0", store},
		{"unrelated native identity", `{"id":"unrelated","deleted":true}`, ClassProtocol, false, "0", store},
		{"conversation item returns parent identity", `{"id":"conversation-native"}`, ClassSuccess, true, "0.25", item},
		{"conversation item returns child identity", `{"id":"resource-native"}`, ClassProtocol, false, "0", item},
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
			probed, granted := openTestProbe(t, h, p.Provider.ID)
			if !granted || !probed {
				t.Fatal("could not claim half-open probe")
			}
			p.Hold = &dispatchHold{provider: p.Provider.ID, probed: probed}
			p.attempt.Price = rates.NewRoutingPrice(rates.RoutingPrice{Price: rates.Price{PerRequest: new("0.25")}})
			x := &Call{request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release}, family: wire.File, actor: "api_key_resource", keyID: h.keyID}
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
			endpoint := strings.TrimRight(p.Provider.Endpoint, "/") + "/resources/resource-native"
			status := h.gateway.RetainedDispatch(w, r, x, p, retainedapi.Endpoint{Family: tc.family, Method: http.MethodDelete, Action: "delete"}, resource, parent, endpoint, nil, "", nil, func(e *fault.Error) {
				h.gateway.Refuse(x, w, e)
			})
			h.gateway.SettlePinnedHold(t.Context(), p.Hold)
			env := h.sink.last(t)
			if status != http.StatusServiceUnavailable || w.Code != status || env.Outcome != "failure" || env.ErrorClass != "provider_state_unavailable" {
				t.Fatalf("local deletion failure changed, status=%d body=%s outcome=%s error=%s", status, w.Body.String(), env.Outcome, env.ErrorClass)
			}
			if len(env.Attempts) != 1 || env.Attempts[0].Class != tc.class || h.gateway.settledCharge(x).String() != tc.charge {
				t.Fatalf("unexpected attempt evidence or charge: %+v, %s", env.Attempts, h.gateway.settledCharge(x).String())
			}
			if probed, granted := h.gateway.health.Claim(p.Provider.ID); probed || granted != tc.available {
				t.Fatalf("after settlement, probe=%v granted=%v", probed, granted)
			}
		})
	}
}

func TestRetainedResultPersistenceFailureKeepsProviderProbeAvailable(t *testing.T) {
	t.Parallel()
	pool, err := pgxpool.New(t.Context(), "postgres://unused@127.0.0.1:1/unused")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	h := newHarness(t, Config{}, Dependencies{Resources: resources.New(pool)})
	h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"store-native","status":"completed"}`)
	})
	p := testPinnedTarget(h, 0)
	probed, granted := openTestProbe(t, h, p.Provider.ID)
	if !granted || !probed {
		t.Fatal("could not claim half-open probe")
	}
	p.Hold = &dispatchHold{provider: p.Provider.ID, probed: probed}
	p.attempt.Price = rates.NewRoutingPrice(rates.RoutingPrice{Price: rates.Price{PerRequest: new("0.25")}})
	x := &Call{
		request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release},
		family:  wire.File, actor: "api_key_resource", keyID: h.keyID,
		retainedAdmission: &RetainedAdmissionPlan{attempt: p.attempt, billing: retainedPriced},
	}
	id := uuid.New()
	resource := &resources.Resource{UUID: id, ID: resources.LocalID("vector_store", id), Kind: "vector_store", UpstreamID: "store-native"}
	e := retainedapi.Endpoint{
		Family: retainedapi.Family{Operation: "vector_store", Kind: resource.Kind, Identity: "id"},
		Method: http.MethodGet, Action: "get",
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/vector_stores/"+resource.ID, nil)
	w := httptest.NewRecorder()
	endpoint := strings.TrimRight(p.Provider.Endpoint, "/") + "/vector_stores/store-native"
	status := h.gateway.RetainedDispatch(w, r, x, p, e, resource, nil, endpoint, nil, "", nil, func(e *fault.Error) {
		h.gateway.Refuse(x, w, e)
	})
	h.gateway.SettlePinnedHold(t.Context(), p.Hold)
	env := h.sink.last(t)
	if status != http.StatusServiceUnavailable || w.Code != status || env.ErrorClass != "provider_state_unavailable" {
		t.Fatalf("local persistence failure returned %d, %s", status, w.Body.String())
	}
	if len(env.Attempts) != 1 || env.Attempts[0].Status != http.StatusOK || !env.Attempts[0].LocalRefusal || !env.Attempts[0].BillingUncertain {
		t.Fatalf("local persistence failure changed acceptance or billing evidence: %+v", env.Attempts)
	}
	if probed, granted := h.gateway.health.Claim(p.Provider.ID); !probed || !granted {
		t.Fatalf("a local persistence failure changed provider health: probe=%v granted=%v", probed, granted)
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
	probed, granted := openTestProbe(t, h, p.Provider.ID)
	if !granted || !probed {
		t.Fatal("could not claim half-open probe")
	}
	p.Hold = &dispatchHold{provider: p.Provider.ID, probed: probed}
	p.attempt.Price = rates.NewRoutingPrice(rates.RoutingPrice{Price: rates.Price{PerRequest: new("0.25")}})
	x := &Call{
		request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release},
		family:  wire.File, actor: "api_key_resource", keyID: h.keyID,
		retainedAdmission: &RetainedAdmissionPlan{billing: retainedPriced},
	}
	id := uuid.New()
	parent := &resources.Resource{UUID: id, ID: resources.LocalID("vector_store", id), Kind: "vector_store", UpstreamID: "store-native", TargetID: p.Target.ID}
	e := retainedapi.Endpoint{
		Family:    retainedapi.Family{Operation: "vector_store", Kind: "vector_store_file", Identity: "id", Collection: "data"},
		Method:    http.MethodGet,
		Action:    "list",
		Ancestors: []string{"vector_store"},
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/vector_stores/"+parent.ID+"/files", nil)
	w := httptest.NewRecorder()
	endpoint := strings.TrimRight(p.Provider.Endpoint, "/") + "/vector_stores/store-native/files"
	status := h.gateway.RetainedDispatch(w, r, x, p, e, parent, parent, endpoint, nil, "", nil, func(e *fault.Error) {
		h.gateway.Refuse(x, w, e)
	})
	h.gateway.SettlePinnedHold(t.Context(), p.Hold)
	env := h.sink.last(t)
	if status != http.StatusServiceUnavailable || w.Code != status || env.ErrorClass != "provider_state_unavailable" {
		t.Fatalf("binding store failure was not reported as unavailable: %d, %s", status, w.Body.String())
	}
	if len(env.Attempts) != 1 || env.Attempts[0].Class != ClassProtocol || env.Attempts[0].UsageObserved || !h.gateway.settledCharge(x).IsZero() {
		t.Fatalf("unexpected attempt evidence or charge: %+v, %s", env.Attempts, h.gateway.settledCharge(x).String())
	}
	if probed, granted := h.gateway.health.Claim(p.Provider.ID); !probed || !granted {
		t.Fatalf("a local binding failure changed provider health: probe=%v granted=%v", probed, granted)
	}
}

func TestCompactionRejectsUnavailableProviderUsage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, usage string
	}{
		{name: "missing"},
		{name: "null", usage: `,"usage":null`},
		{name: "malformed", usage: `,"usage":{"input_tokens":-1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{}, Dependencies{Resources: resources.New(nil)})
			h.reissue(func(provider *routing.Provider) { provider.Kind = "openai" }, nil)
			h.rt.inputs = &routinginputs.Inputs{RefreshedAt: time.Now()}
			authority := h.rt.keys[fullKey]
			authority.Policy.AllowProviderState = true
			h.rt.keys[fullKey] = authority
			h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"object":"response.compaction","id":"cmp-native","output":[{"type":"compaction","encrypted_content":"abc"}]`+tc.usage+`}`)
			})
			response := h.do(t.Context(), http.MethodPost, "/v1/responses/compact", fullKey, []byte(`{"input":"compact this"}`), map[string]string{StateRouteHeader: routeSlug})
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			env := h.sink.last(t)
			if response.StatusCode != http.StatusBadGateway || env.ErrorClass != "provider_protocol_error" || env.Outcome != "failure" {
				t.Fatalf("compaction = %d %s, outcome = %s (%s)", response.StatusCode, body, env.Outcome, env.ErrorClass)
			}
			if len(env.Attempts) != 1 {
				t.Fatalf("attempts = %+v", env.Attempts)
			}
			attempt := env.Attempts[0]
			if attempt.Class != ClassProtocol || attempt.Committed || attempt.UsageObserved || attempt.UsageComplete || !attempt.BillingUncertain {
				t.Fatalf("compaction accounting = %+v", attempt)
			}
		})
	}
}

func TestCompactionBindingFailurePreservesProviderUsage(t *testing.T) {
	t.Parallel()
	pool, err := pgxpool.New(t.Context(), "postgres://unused:unused@localhost/unused")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	h := newHarness(t, Config{}, Dependencies{Resources: resources.New(pool)})
	h.reissue(func(provider *routing.Provider) { provider.Kind = "openai" }, nil)
	h.rt.inputs = &routinginputs.Inputs{RefreshedAt: time.Now()}
	authority := h.rt.keys[fullKey]
	authority.Policy.AllowProviderState = true
	h.rt.keys[fullKey] = authority
	h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"object":"response.compaction","id":"cmp-native","output":[{"type":"compaction","id":"item-native","file_id":"file-native"}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`)
	})
	response := h.do(t.Context(), http.MethodPost, "/v1/responses/compact", fullKey, []byte(`{"input":"compact this"}`), map[string]string{StateRouteHeader: routeSlug})
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	env := h.sink.last(t)
	if response.StatusCode != http.StatusServiceUnavailable || env.ErrorClass != "provider_state_unavailable" || env.Outcome != "failure" {
		t.Fatalf("compaction binding failure = %d %s, outcome = %s", response.StatusCode, body, env.Outcome)
	}
	if len(env.Attempts) != 1 {
		t.Fatalf("attempts = %+v", env.Attempts)
	}
	attempt := env.Attempts[0]
	if attempt.Class != ClassProtocol || !attempt.LocalRefusal || !attempt.UsageObserved || !attempt.UsageComplete || attempt.BillingUncertain ||
		attempt.Usage == nil || attempt.Usage.InputTokens != 10 || attempt.Usage.OutputTokens != 5 || attempt.Usage.TotalTokens != 15 {
		t.Fatalf("compaction accounting after binding failure = %+v, usage = %+v", attempt, attempt.Usage)
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
			probed, granted := openTestProbe(t, h, p.Provider.ID)
			if !granted || !probed {
				t.Fatal("could not claim half-open probe")
			}
			p.Hold = &dispatchHold{provider: p.Provider.ID, probed: probed}
			x := &Call{request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release}, family: wire.File, actor: "api_key_resource", keyID: h.keyID}
			id := uuid.New()
			res := &resources.Resource{UUID: id, ID: resources.LocalID(resources.KindFile, id), Kind: resources.KindFile, UpstreamID: "file-native", Metadata: json.RawMessage(tc.storedMetadata)}
			w := httptest.NewRecorder()
			failure := h.gateway.GetPinnedFile(t.Context(), w, x, keyauthority.Authority{ID: h.keyID}, res, p)
			if failure == nil || failure.Status != http.StatusBadGateway || failure.Code != "fidelity_protocol_violation" {
				t.Fatalf("unexpected file refresh failure: %+v", failure)
			}
			h.gateway.Refuse(x, w, failure)
			h.gateway.SettlePinnedHold(t.Context(), p.Hold)
			env := h.sink.last(t)
			if len(env.Attempts) != 1 || env.Attempts[0].Class != ClassProtocol || env.Attempts[0].LocalRefusal != tc.local {
				t.Fatalf("unexpected attempt evidence: %+v", env.Attempts)
			}
			if probed, granted := h.gateway.health.Claim(p.Provider.ID); probed != tc.local || granted != tc.local {
				t.Fatalf("after settlement, probe=%v granted=%v", probed, granted)
			}
		})
	}
}

func testPinnedTarget(h *harness, index int) *Pin {
	snapshot := h.rt.release.Snapshot().Clone()
	target := snapshot.Routes[routeSlug].Targets[index]
	provider := snapshot.Providers[target.ProviderID]
	return &Pin{Provider: provider, Target: target, Model: target.ProviderModel, Slot: provider.Slots[0], attempt: routing.Attempt{ProviderID: provider.ID, UpstreamModel: target.ProviderModel}}
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
		h.gateway.health.Record(provider, events.AttemptFact{Class: ClassProtocol}, false)
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
	case 16:
		if c.used[args[3]] {
			return []any{int64(2), int64(0), "rpm", int64(30000), int64(42), int64(0)}, nil
		}
		c.used[args[3]] = true
		return []any{int64(2), int64(1), "ok", int64(0), int64(42), int64(0)}, nil
	case 11:
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
	for _, p := range []*Pin{sent, unsent} {
		p.Provider.Limits = &routing.Limits{RequestsPerMinute: new(int64(1))}
	}
	probed, granted := openTestProbe(t, h, unsent.Provider.ID)
	if !probed || !granted {
		t.Fatal("could not claim half-open probe")
	}
	h.gateway.health.ReleaseProbe(unsent.Provider.ID)
	reserve := func(p *Pin) gateResult {
		return acquireDispatchHold(t.Context(), h.gateway, &p.Provider, &p.Slot, quota.CombinedDemand(1), time.Minute, "", "", 0)
	}
	first := reserve(sent)
	if first.verdict != gateAdmitted {
		t.Fatalf("first admission: %+v", first)
	}
	sent.Hold = first.hold
	x := &Call{request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release}, family: wire.File, actor: "api_key_resource", keyID: h.keyID}
	response, failure := h.gateway.PinnedDo(t.Context(), x, sent, http.MethodGet, sent.Provider.Endpoint, nil, "")
	if failure != nil {
		t.Fatalf("first exchange: %+v", failure)
	}
	_, err = response.ReadBody(1024)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	response.Exchange.Complete(ClassSuccess)
	h.gateway.SettlePinnedHold(t.Context(), sent.Hold)

	second := reserve(unsent)
	if second.verdict != gateAdmitted {
		t.Fatalf("second admission: %+v", second)
	}
	unsent.Hold = second.hold
	if _, failure := h.gateway.PinnedDo(t.Context(), x, unsent, http.MethodGet, "://invalid", nil, ""); failure == nil {
		t.Fatal("invalid endpoint dispatched")
	}
	h.gateway.SettlePinnedHold(t.Context(), unsent.Hold)
	if after := reserve(unsent); after.verdict != gateAdmitted {
		t.Fatalf("second provider remained blocked by a quota or probe: %+v", after)
	} else {
		h.gateway.ReleaseHold(t.Context(), after.hold)
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

func TestListedResourcePageSkipsStaleRowsAndRefills(t *testing.T) {
	t.Parallel()
	all := []*resources.Resource{
		{ID: "stale-1"}, {ID: "hidden-1"}, {ID: "valid-1"}, {ID: "valid-2"}, {ID: "stale-2"}, {ID: "valid-3"},
	}
	fetchFrom := func(rows []*resources.Resource) func(resources.ListCursor, int) ([]*resources.Resource, *fault.Error) {
		return func(cursor resources.ListCursor, count int) ([]*resources.Resource, *fault.Error) {
			start := 0
			if cursor.ID != "" {
				start = len(rows)
				for i, row := range rows {
					if row.ID == cursor.ID {
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
			return PinUnavailable()
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

	page, continuation, err := listedResourcePage(10, resources.ListCursor{}, fetchFrom(all), authorize)
	if err != nil {
		t.Fatalf("mixed page error: %v", err)
	}
	if continuation.ID != "" || !slices.Equal(ids(page), []string{"valid-1", "valid-2", "valid-3"}) {
		t.Fatalf("mixed page = %v continuation=%q, want only the valid rows and no continuation", ids(page), continuation.ID)
	}

	page, continuation, err = listedResourcePage(2, resources.ListCursor{}, fetchFrom(all), authorize)
	if err != nil {
		t.Fatalf("refilled page error: %v", err)
	}
	if continuation.ID != "valid-2" || !slices.Equal(ids(page), []string{"valid-1", "valid-2"}) {
		t.Fatalf("refilled page = %v continuation=%q, want [valid-1 valid-2] with a continuation", ids(page), continuation.ID)
	}
	page, continuation, err = listedResourcePage(2, continuation, fetchFrom(all), authorize)
	if err != nil {
		t.Fatalf("following page error: %v", err)
	}
	if continuation.ID != "" || !slices.Equal(ids(page), []string{"valid-3"}) {
		t.Fatalf("following page = %v continuation=%q, want [valid-3] and no continuation", ids(page), continuation.ID)
	}

	staleOnly := []*resources.Resource{{ID: "stale-1"}, {ID: "stale-2"}, {ID: "stale-3"}}
	page, continuation, err = listedResourcePage(2, resources.ListCursor{}, fetchFrom(staleOnly), authorize)
	if err != nil {
		t.Fatalf("all-stale page error: %v", err)
	}
	if continuation.ID != "" || len(page) != 0 {
		t.Fatalf("all-stale page = %v continuation=%q, want empty with no continuation", ids(page), continuation.ID)
	}

	broken := []*resources.Resource{{ID: "valid-1"}, {ID: "broken-1"}}
	if _, _, err := listedResourcePage(2, resources.ListCursor{}, fetchFrom(broken), authorize); err == nil || err.Status != http.StatusInternalServerError {
		t.Fatalf("broken row error = %v, want the request to fail with 500", err)
	}
	fetchErr := fault.ServerError(http.StatusInternalServerError, "internal_error", "The resource list could not be read.")
	failingFetch := func(resources.ListCursor, int) ([]*resources.Resource, *fault.Error) { return nil, fetchErr }
	if _, _, err := listedResourcePage(2, resources.ListCursor{}, failingFetch, authorize); err != fetchErr {
		t.Fatalf("fetch error = %v, want %v", err, fetchErr)
	}

	t.Run("bounded scan can continue without exposing denied IDs", func(t *testing.T) {
		denied := make([]*resources.Resource, 500)
		for i := range denied {
			denied[i] = &resources.Resource{ID: fmt.Sprintf("stale-%d", i)}
		}
		fetch := fetchFrom(append(denied, &resources.Resource{ID: "valid-1"}))
		server := Server{Deps: Dependencies{Digester: secrets.NewAuthKey(make([]byte, 32), "test-installation")}}
		page, cursor, err := server.ListedResourcePage("key-a", "pageToken", 2, "", fetch, authorize)
		if err != nil || len(page) != 0 || cursor == "" || strings.Contains(cursor, "stale-") {
			t.Fatalf("first page = %v cursor=%q err=%v, want empty page with an opaque continuation", ids(page), cursor, err)
		}
		if _, _, err := server.ListedResourcePage("key-b", "pageToken", 2, cursor, fetch, authorize); err == nil || err.Status != http.StatusBadRequest {
			t.Fatalf("another key's continuation error = %v, want 400", err)
		}
		page, cursor, err = server.ListedResourcePage("key-a", "pageToken", 2, cursor, fetch, authorize)
		if err != nil || cursor != "" || !slices.Equal(ids(page), []string{"valid-1"}) {
			t.Fatalf("next page = %v cursor=%q err=%v, want visible row and no continuation", ids(page), cursor, err)
		}
	})
}

func TestFileDownloadHeadersForceAttachmentForHTML(t *testing.T) {
	t.Parallel()
	const body = "<html><body>hello</body></html>"
	recorder := httptest.NewRecorder()
	SetFileDownloadHeaders(recorder.Header(), "text/html; charset=utf-8", int64(len(body)))
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

type entryReply struct {
	status int
	retry  string
	code   string
	param  string
}

func enter(t *testing.T, h *harness, headers map[string]string, body string) entryReply {
	t.Helper()
	resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(body), headers)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var decoded struct {
		Error struct {
			Code  string  `json:"code"`
			Param *string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("reply %q is not JSON: %v", raw, err)
	}
	reply := entryReply{status: resp.StatusCode, retry: resp.Header.Get("X-Should-Retry"), code: decoded.Error.Code}
	if decoded.Error.Param != nil {
		reply.param = *decoded.Error.Param
	}
	return reply
}

func failingProvider(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	io.WriteString(w, `{"error":{"message":"boom","type":"server_error"}}`)
}

const entryChat = `{"model":"` + routeSlug + `","messages":[{"role":"user","content":"hi"}]}`

func TestAProviderFailureFallsBackOnATransformedRouteAndIsFinalWithNoRetryHintOnAStrictOne(t *testing.T) {
	for name, tc := range map[string]struct {
		harness *harness
		want    entryReply
		second  int
	}{
		"transformed": {newHarness(t, Config{}), entryReply{status: http.StatusBadGateway, code: "upstream_unavailable"}, 1},
		"strict":      {strictHarness(t, nil), entryReply{status: http.StatusBadGateway, code: "ambiguous_upstream_result", retry: "false"}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			h := tc.harness
			h.mock.set("a", failingProvider)
			h.mock.set("b", failingProvider)
			if got := enter(t, h, nil, entryChat); got != tc.want {
				t.Errorf("reply = %+v, want %+v", got, tc.want)
			}
			if first, second := h.mock.count("a"), h.mock.count("b"); first != 1 || second != tc.second {
				t.Errorf("provider calls = %d and %d, want 1 and %d", first, second, tc.second)
			}
			if env := h.sink.last(t); env.Status != http.StatusBadGateway || len(env.Attempts) != 1+tc.second {
				t.Errorf("recorded status %d with %d attempts, want 502 with %d", env.Status, len(env.Attempts), 1+tc.second)
			}
		})
	}
}

type messagesTransport func(*http.Request) (*http.Response, error)

func (f messagesTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNativeMessagesProviderRequestIDsStayWithTheirAttempts(t *testing.T) {
	const (
		failedID  = "req_Unavailable123"
		successID = "req_Served456"
		gatewayID = "gateway-messages-1"
	)
	official := connectors.DefaultEndpoint(connectors.KindAnthropic, "", "")
	officialURL, err := url.Parse(official)
	if err != nil {
		t.Fatal(err)
	}
	withPort := func(port string) string {
		endpoint := *officialURL
		endpoint.Host = net.JoinHostPort(endpoint.Hostname(), port)
		return endpoint.String()
	}
	for _, tc := range []struct {
		name, endpoint string
		firstHeader    []string
		refuse         bool
		wantFirst      string
		wantSecond     string
	}{
		{name: "official fallback", endpoint: official, firstHeader: []string{failedID}, wantFirst: failedID, wantSecond: successID},
		{name: "explicit HTTPS port", endpoint: withPort("443"), firstHeader: []string{failedID}, wantFirst: failedID, wantSecond: successID},
		{name: "nondefault port", endpoint: withPort("8443"), firstHeader: []string{failedID}},
		{name: "custom host", endpoint: "https://custom.example/v1", firstHeader: []string{failedID}},
		{name: "duplicate header", endpoint: official, firstHeader: []string{failedID, failedID}, wantSecond: successID},
		{name: "combined header", endpoint: official, firstHeader: []string{failedID + ", " + successID}, wantSecond: successID},
		{name: "oversized header", endpoint: official, firstHeader: []string{"req_" + strings.Repeat("A", 125)}, wantSecond: successID},
		{name: "wrong format", endpoint: official, firstHeader: []string{"sk_secret"}, wantSecond: successID},
		{name: "missing header", endpoint: official, wantSecond: successID},
		{name: "local refusal", endpoint: official, refuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := strictRelease(t, newHarness(t, Config{}), func(snapshot *routing.Snapshot) {
				route := snapshot.Routes[routeSlug]
				route.Fidelity.Mode = routing.FidelityTransformed
				snapshot.Routes[routeSlug] = route
				for id, provider := range snapshot.Providers {
					provider.Endpoint, provider.Kind = tc.endpoint, connectors.KindAnthropic
					provider.ProfileID, provider.ProfileRevision = "", ""
					provider.Capabilities = []routing.Capability{{Model: provider.Capabilities[0].Model, Operation: "generation", Surface: "anthropic", Mode: "unary"}}
					snapshot.Providers[id] = provider
				}
			})
			var calls atomic.Int32
			h.gateway.client.Transport = messagesTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != tc.endpoint+"/messages" {
					return nil, fmt.Errorf("unexpected upstream URL %s", r.URL)
				}
				stub := http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {"req_OtherHeader"}}
				if calls.Add(1) == 1 {
					stub["Request-Id"] = tc.firstHeader
					return &http.Response{StatusCode: 503, Header: stub, Request: r, Body: io.NopCloser(strings.NewReader(
						`{"type":"error","error":{"type":"overloaded_error","message":"unavailable"},"request_id":"req_BodyID"}`))}, nil
				}
				stub.Set("Request-Id", successID)
				return &http.Response{StatusCode: 200, Header: stub, Request: r, Body: io.NopCloser(strings.NewReader(
					`{"id":"msg_1","type":"message","role":"assistant","model":"model-b","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}`))}, nil
			})
			headers := map[string]string{"Anthropic-Version": "2023-06-01", "X-Request-Id": gatewayID, "Request-Id": "req_ClientSupplied"}
			if tc.refuse {
				headers["X-OLP-Routing"] = "{"
			}
			resp := h.do(t.Context(), "POST", "/anthropic/v1/messages", fullKey,
				[]byte(`{"model":"`+routeSlug+`","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`), headers)
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			env := h.sink.last(t)
			if resp.Header.Get("X-Request-Id") != gatewayID || resp.Header.Get("Request-Id") != gatewayID || env.RequestID != gatewayID {
				t.Fatalf("gateway request ID changed: headers=%v request=%q", resp.Header, env.RequestID)
			}
			if tc.refuse {
				if resp.StatusCode != 400 || calls.Load() != 0 || len(env.Attempts) != 0 {
					t.Fatalf("local refusal returned %d, sent %d calls and recorded %d attempts", resp.StatusCode, calls.Load(), len(env.Attempts))
				}
				return
			}
			if resp.StatusCode != 200 || calls.Load() != 2 || bytes.Contains(body, []byte(successID)) || bytes.Contains(body, []byte(failedID)) {
				t.Fatalf("fallback returned %d with %d calls: %s", resp.StatusCode, calls.Load(), body)
			}
			if len(env.Attempts) != 2 || env.Attempts[0].Status != 503 || env.Attempts[1].Status != 200 {
				t.Fatalf("unexpected attempts: %+v", env.Attempts)
			}
			event, err := events.UsageEvent(env)
			if err != nil || event == nil {
				t.Fatalf("usage event unavailable: %v", err)
			}
			if event.Attempts[0].ProviderRequestID != tc.wantFirst || event.Attempts[1].ProviderRequestID != tc.wantSecond {
				t.Errorf("provider request IDs = %q and %q, want %q and %q", event.Attempts[0].ProviderRequestID, event.Attempts[1].ProviderRequestID, tc.wantFirst, tc.wantSecond)
			}
		})
	}
}

func TestRequestsTheGatewayCannotUnderstandAreRefusedBeforeAnyProviderAndRecorded(t *testing.T) {
	image := `{"model":"` + routeSlug + `","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAAAAAA"}}]}]}`
	approval := `{"model":"` + routeSlug + `","messages":[{"role":"user","content":"hi"}],"olp_approval":{"approved":true}}`
	for name, tc := range map[string]struct {
		headers map[string]string
		body    string
		want    entryReply
	}{
		"routing preferences that are not an object": {map[string]string{"X-OLP-Routing": "{"}, entryChat,
			entryReply{status: http.StatusBadRequest, code: "invalid_request", param: "X-OLP-Routing"}},
		"a priority above the key's ceiling": {map[string]string{"X-OLP-Routing": `{"priority":"critical"}`}, entryChat,
			entryReply{status: http.StatusBadRequest, code: "priority_increase_forbidden", param: "X-OLP-Routing"}},
		"attribution that is not an object": {map[string]string{"X-OLP-Attribution": "!!!"}, entryChat,
			entryReply{status: http.StatusBadRequest, code: "invalid_attribution"}},
		"inline media over the installation limit": {nil, image,
			entryReply{status: http.StatusBadRequest, code: "unsupported_parameter", param: "content"}},
		"a tool approval on a route that runs no tools": {nil, approval,
			entryReply{status: http.StatusForbidden, code: "tool_approval_invalid"}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, Config{InlineMedia: protocols.InlineMediaLimits{Items: 4, ItemBytes: 2, TotalBytes: 2}})
			if got := enter(t, h, tc.headers, tc.body); got != tc.want {
				t.Errorf("reply = %+v, want %+v", got, tc.want)
			}
			if calls := h.mock.count("a") + h.mock.count("b"); calls != 0 {
				t.Errorf("provider calls = %d, want none", calls)
			}
			if env := h.sink.last(t); env.Status != tc.want.status || len(env.Attempts) != 0 {
				t.Errorf("recorded status %d with %d attempts, want %d with none", env.Status, len(env.Attempts), tc.want.status)
			}
		})
	}
}

type lostCaller struct {
	header       http.Header
	write, flush error
}

func (c lostCaller) Header() http.Header            { return c.header }
func (lostCaller) WriteHeader(int)                  {}
func (c lostCaller) Write(b []byte) (int, error)    { return len(b), c.write }
func (lostCaller) SetReadDeadline(time.Time) error  { return nil }
func (lostCaller) SetWriteDeadline(time.Time) error { return nil }
func (c lostCaller) FlushError() error              { return c.flush }

func TestAWholeReplyCountsAsDeliveredOnlyWhenTheCallerReceivesIt(t *testing.T) {
	gone := errors.New("caller is gone")
	lost := func(caller lostCaller) func(*harness) {
		return func(h *harness) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(entryChat))
			req.Header.Set("Authorization", "Bearer "+fullKey)
			req.Header.Set("Content-Type", "application/json")
			h.server.Config.Handler.ServeHTTP(caller, req)
		}
	}
	for name, tc := range map[string]struct {
		send      func(*harness)
		outcome   string
		delivered bool
	}{
		"received": {func(h *harness) {
			resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(entryChat), nil)
			resp.Body.Close()
		}, "success", true},
		"reply not written": {lost(lostCaller{header: http.Header{}, write: gone}), "cancelled", false},
		"reply not flushed": {lost(lostCaller{header: http.Header{}, flush: gone}), "cancelled", false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, Config{})
			tc.send(h)
			env := h.sink.last(t)
			attempt := env.Attempts[len(env.Attempts)-1]
			if env.Outcome != tc.outcome || !env.Committed || !attempt.Committed || (env.FirstByte != nil) != tc.delivered {
				t.Fatalf("envelope outcome %q, committed %t, attempt committed %t, first byte %v, want outcome %q and delivered %t",
					env.Outcome, env.Committed, attempt.Committed, env.FirstByte, tc.outcome, tc.delivered)
			}
		})
	}
}

type attemptSeen struct {
	class     string
	status    int
	committed bool
}

const stalled = "<stalled>"

func providerStream(frames ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range frames {
			if frame == stalled {
				<-r.Context().Done()
				return
			}
			io.WriteString(w, "data: "+frame+"\n\n")
			w.(http.Flusher).Flush()
		}
	}
}

func providerReply(status int, headers map[string]string, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for name, value := range headers {
			w.Header().Set(name, value)
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

var replyErrorCode = regexp.MustCompile(`"error":\{"code":"([a-z_]+)"`)

func TestEachKindOfProviderFailureIsClassifiedAndFallsBackOnlyWhileTheReplyIsUncommitted(t *testing.T) {
	const (
		delta     = `{"choices":[{"index":0,"delta":{"content":"partial"}}]}`
		inBand    = `{"error":{"message":"boom","type":"server_error"}}`
		oversized = 5000

		routeDeadline = 5 * time.Second
	)
	served := attemptSeen{"success", http.StatusOK, true}
	for name, tc := range map[string]struct {
		provider http.HandlerFunc
		streamed bool
		timeout  int64
		status   int
		code     string
		attempts []attemptSeen
	}{
		"rate limited": {
			provider: providerReply(http.StatusTooManyRequests, map[string]string{"Retry-After": "7"}, `{"error":{"message":"slow down","type":"rate_limit_error"}}`),
			status:   http.StatusOK, attempts: []attemptSeen{{"rate_limit", http.StatusTooManyRequests, false}, served},
		},
		"credential refused": {
			provider: providerReply(http.StatusUnauthorized, nil, `{"error":{"message":"bad key","type":"invalid_request_error"}}`),
			status:   http.StatusOK, attempts: []attemptSeen{{"credential", http.StatusUnauthorized, false}, served},
		},
		"silent provider": {
			provider: providerStream(stalled), timeout: 80,
			status: http.StatusOK, attempts: []attemptSeen{{"timeout", 0, false}, served},
		},
		"stream opening with a provider error": {
			provider: providerStream(inBand), streamed: true,
			status: http.StatusOK, attempts: []attemptSeen{{"upstream_server", http.StatusOK, false}, served},
		},
		"unreadable reply": {
			provider: providerReply(http.StatusOK, nil, `not json`),
			status:   http.StatusBadGateway, code: "provider_protocol_error", attempts: []attemptSeen{{"protocol", http.StatusOK, false}},
		},
		"whole reply to a streamed request": {
			provider: providerReply(http.StatusOK, nil, `{}`), streamed: true,
			status: http.StatusBadGateway, code: "provider_protocol_error", attempts: []attemptSeen{{"protocol", http.StatusOK, false}},
		},
		"stream frame over the event limit": {
			provider: providerStream(`{"choices":[{"index":0,"delta":{"content":"` + strings.Repeat("a", oversized) + `"}}]}`), streamed: true,
			status: http.StatusBadGateway, code: "provider_protocol_error", attempts: []attemptSeen{{"protocol", http.StatusOK, false}},
		},
		"stream stalling after its first frame": {
			provider: providerStream(delta, stalled), streamed: true, timeout: 80,
			status: http.StatusOK, code: "gateway_timeout", attempts: []attemptSeen{{"timeout", http.StatusOK, true}},
		},
		"stream ending without a final frame": {
			provider: providerStream(delta), streamed: true,
			status: http.StatusOK, code: "provider_protocol_error", attempts: []attemptSeen{{"protocol", http.StatusOK, true}},
		},
		"provider error after the first frame": {
			provider: providerStream(delta, inBand), streamed: true,
			status: http.StatusOK, code: "provider_protocol_error", attempts: []attemptSeen{{"protocol", http.StatusOK, true}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, Config{})
			if tc.timeout > 0 {
				h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
					route := s.Routes[routeSlug]
					for i := range route.Targets {
						route.Targets[i].Timeout = tc.timeout
					}
					s.Routes[routeSlug] = route
				})
			}
			h.mock.set("a", tc.provider)
			body := entryChat
			if tc.streamed {
				h.mock.set("b", streamedChat(answerText))
				body = strings.TrimSuffix(entryChat, "}") + `,"stream":true}`
			}
			started := time.Now()
			resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(body), nil)
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if elapsed := time.Since(started); tc.timeout > 0 && elapsed > routeDeadline/2 {
				t.Fatalf("a provider silent past its %dms target timeout held the caller for %v", tc.timeout, elapsed)
			}

			code := ""
			if found := replyErrorCode.FindSubmatch(raw); found != nil {
				code = string(found[1])
			}
			if resp.StatusCode != tc.status || code != tc.code {
				t.Fatalf("reply = %d %q, want %d %q: %.300s", resp.StatusCode, code, tc.status, tc.code, raw)
			}
			if strings.Contains(string(raw), strings.Repeat("a", oversized)) {
				t.Fatal("the caller received a frame over the event limit")
			}
			if tc.code == "" && !strings.Contains(string(raw), answerText) {
				t.Fatalf("the caller did not get the second provider's answer: %.300s", raw)
			}
			if last := tc.attempts[len(tc.attempts)-1]; tc.code != "" && strings.Contains(string(raw), "partial") != last.committed {
				t.Fatalf("partial output reached the caller = %t, want %t", !last.committed, last.committed)
			}

			env := h.sink.last(t)
			seen := make([]attemptSeen, len(env.Attempts))
			for i, attempt := range env.Attempts {
				seen[i] = attemptSeen{attempt.Class, attempt.Status, attempt.Committed}
			}
			if len(seen) != len(tc.attempts) {
				t.Fatalf("attempts = %+v, want %+v", seen, tc.attempts)
			}
			for i, want := range tc.attempts {
				if seen[i] != want {
					t.Fatalf("attempt %d = %+v, want %+v", i, seen[i], want)
				}
			}
			first := env.Attempts[0]
			if name == "rate limited" && (first.RetryAfter == nil || *first.RetryAfter != 7*time.Second) {
				t.Fatalf("retry after = %v, want the provider's 7s", first.RetryAfter)
			}
			if name == "silent provider" && first.FirstByte != nil {
				t.Fatal("a provider that never answered has a first byte time")
			}
			if succeeded := env.Outcome == "success"; succeeded != (tc.code == "") {
				t.Fatalf("outcome = %s for reply code %q", env.Outcome, tc.code)
			}
		})
	}
}

type providerCalls struct{ a, b atomic.Int64 }

func TestOversizedProjectedFirstFrameDoesNotCommitStream(t *testing.T) {
	t.Parallel()
	const frame = `{"model":"x","choices":[{"index":0,"delta":{"content":"first frame"}}]}`
	h := newHarness(t, Config{MaxEventBytes: int64(len("data: "+frame+"\n\n") + 1)})
	h.mock.set("a", providerStream(frame))
	body := strings.TrimSuffix(entryChat, "}") + `,"stream":true}`
	resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(body), nil)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusBadGateway || strings.Contains(string(raw), "first frame") {
		t.Fatalf("opening frame rejection = %d %s, read error = %v", resp.StatusCode, raw, err)
	}
	env := h.sink.last(t)
	if env.Committed || len(env.Attempts) != 1 || env.Attempts[0].Committed {
		t.Fatalf("rejected frame recorded a committed stream: request=%t attempts=%+v", env.Committed, env.Attempts)
	}
}

func refusing(calls *atomic.Int64, refusals int64, status int, retryAfter string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) > refusals {
			completion(modelA, answerText)(w, r)
			return
		}
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"message":"refused"}}`)
	}
}

func unreachableLimits(t *testing.T) Dependencies {
	t.Helper()
	runner, err := scripts.NewRunner(unreachableValkey{}, "test")
	if err != nil {
		t.Fatal(err)
	}
	closed := func() limitadmission.OutagePolicy { return limitadmission.FailClosed }
	return Dependencies{Admission: quota.NewAdmission(limitadmission.NewDurable(runner, nil), fleet.New(runner), closed, slog.New(slog.DiscardHandler))}
}

func TestARouteRetriesAProviderOnlyUnderItsRetryRuleThenMovesOnAndSaysWhyNoProviderServed(t *testing.T) {
	const always = 1 << 20
	brief := routing.Retry{classRateLimit: {MaxRetries: 2, BaseBackoffMS: 1, MaxBackoffMS: 1}}
	patient := routing.Retry{classRateLimit: {MaxRetries: 2, BaseBackoffMS: 1, MaxBackoffMS: 1, RespectRetryAfter: true}}
	serverRetry := routing.Retry{ClassUpstreamServer: {MaxRetries: 1, BaseBackoffMS: 1, MaxBackoffMS: 1}}
	serverRetryWithHint := routing.Retry{ClassUpstreamServer: {MaxRetries: 1, BaseBackoffMS: 1, MaxBackoffMS: 1, RespectRetryAfter: true}}
	limited := &routing.Limits{RequestsPerMinute: new(int64(5))}
	for name, tc := range map[string]struct {
		retry      routing.Retry
		refusals   [2]int64
		refusal    int
		retryAfter string
		limits     [2]*routing.Limits
		deadline   int64
		status     int
		code       string
		calls      [2]int64
		retries    []int
		budget     int
	}{
		"a rate limited provider is retried until it answers": {
			retry: brief, refusals: [2]int64{2, 0}, refusal: http.StatusTooManyRequests,
			status: http.StatusOK, calls: [2]int64{3, 0}, retries: []int{0, 1, 2},
		},
		"retries run out and the next provider serves": {
			retry: brief, refusals: [2]int64{always, 0}, refusal: http.StatusTooManyRequests,
			status: http.StatusOK, calls: [2]int64{3, 1}, retries: []int{0, 1, 2, 0}, budget: 4,
		},
		"a failure without a retry rule moves on at once": {
			retry: brief, refusals: [2]int64{always, 0}, refusal: http.StatusInternalServerError,
			status: http.StatusOK, calls: [2]int64{1, 1}, retries: []int{0, 0},
		},
		"a wait the provider asks for past the deadline is not taken": {
			retry: patient, refusals: [2]int64{always, 0}, refusal: http.StatusTooManyRequests, retryAfter: "60", deadline: 400,
			status: http.StatusOK, calls: [2]int64{1, 1}, retries: []int{0, 0},
		},
		"an unavailable provider asking for a wait past the deadline is skipped": {
			retry: serverRetryWithHint, refusals: [2]int64{1, 0}, refusal: http.StatusServiceUnavailable, retryAfter: "60", deadline: 400,
			status: http.StatusOK, calls: [2]int64{1, 1}, retries: []int{0, 0},
		},
		"an unavailable provider is retried without opting in to its wait": {
			retry: serverRetry, refusals: [2]int64{1, 0}, refusal: http.StatusServiceUnavailable, retryAfter: "60", deadline: 400,
			status: http.StatusOK, calls: [2]int64{2, 0}, retries: []int{0, 1},
		},
		"another server error does not set the retry wait": {
			retry: serverRetryWithHint, refusals: [2]int64{1, 0}, refusal: http.StatusInternalServerError, retryAfter: "60", deadline: 400,
			status: http.StatusOK, calls: [2]int64{2, 0}, retries: []int{0, 1},
		},
		"every provider failing reports the last failure": {
			refusals: [2]int64{always, always}, refusal: http.StatusInternalServerError,
			status: http.StatusBadGateway, code: "upstream_unavailable", calls: [2]int64{1, 1}, retries: []int{0, 0},
		},
		"a provider whose limit cannot be read is passed over": {
			limits: [2]*routing.Limits{limited, nil},
			status: http.StatusOK, calls: [2]int64{0, 1}, retries: []int{0},
		},
		"no provider with a readable limit reports the limits": {
			limits: [2]*routing.Limits{limited, limited},
			status: http.StatusServiceUnavailable, code: "distributed_limits_unavailable",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var deps []Dependencies
			if tc.limits != [2]*routing.Limits{} {
				deps = append(deps, unreachableLimits(t))
			}
			h := newHarness(t, Config{}, deps...)
			budget := 0
			h.republish(func(s *routing.Snapshot, a, b routing.Provider) {
				route := s.Routes[routeSlug]
				route.Retry = tc.retry
				route.MaxAttempts = cmp.Or(tc.budget, route.MaxAttempts)
				if tc.deadline > 0 {
					route.OverallTimeout = tc.deadline
				}
				budget = route.MaxAttempts
				s.Routes[routeSlug] = route
				for i, provider := range []routing.Provider{a, b} {
					provider.Limits = tc.limits[i]
					s.Providers[provider.ID] = provider
				}
			})
			var calls providerCalls
			h.mock.set("a", refusing(&calls.a, tc.refusals[0], tc.refusal, tc.retryAfter))
			h.mock.set("b", refusing(&calls.b, tc.refusals[1], tc.refusal, tc.retryAfter))

			reply := enter(t, h, nil, entryChat)

			if reply.status != tc.status || reply.code != tc.code {
				t.Fatalf("reply = %d %q, want %d %q", reply.status, reply.code, tc.status, tc.code)
			}
			if got := [2]int64{calls.a.Load(), calls.b.Load()}; got != tc.calls {
				t.Fatalf("provider calls = %v, want %v", got, tc.calls)
			}
			attempts := h.sink.last(t).Attempts
			retries := make([]int, len(attempts))
			for i, fact := range attempts {
				retries[i] = fact.Retry
			}
			if !slices.Equal(retries, tc.retries) {
				t.Fatalf("recorded retries = %v, want %v", retries, tc.retries)
			}
			if len(attempts) > budget {
				t.Fatalf("%d attempts ran on a route allowing %d", len(attempts), budget)
			}
		})
	}
}

func TestARetryStopsAtTheRoutesAttemptBudgetAndReportsTheProvidersRefusal(t *testing.T) {
	h := newHarness(t, Config{})
	budget := 0
	h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
		route := s.Routes[routeSlug]
		route.Retry = routing.Retry{classRateLimit: {MaxRetries: 10, BaseBackoffMS: 1, MaxBackoffMS: 1}}
		budget = route.MaxAttempts
		s.Routes[routeSlug] = route
	})
	var calls providerCalls
	h.mock.set("a", refusing(&calls.a, 1<<20, http.StatusTooManyRequests, ""))
	h.mock.set("b", refusing(&calls.b, 0, 0, ""))

	reply := enter(t, h, nil, entryChat)

	if reply.status != http.StatusTooManyRequests || reply.code != "upstream_rate_limit" {
		t.Fatalf("reply = %d %q, want the provider's rate limit", reply.status, reply.code)
	}
	if calls.a.Load() != int64(budget) || calls.b.Load() != 0 {
		t.Fatalf("provider calls = %d and %d on a route allowing %d attempts", calls.a.Load(), calls.b.Load(), budget)
	}
}

func TestACallerLeavingDuringARetryWaitEndsTheRequestAsCancelledWithoutAnotherAttempt(t *testing.T) {
	h := newHarness(t, Config{})
	h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
		route := s.Routes[routeSlug]
		route.Retry = routing.Retry{classRateLimit: {MaxRetries: 2, BaseBackoffMS: 1, MaxBackoffMS: 1, RespectRetryAfter: true}}
		s.Routes[routeSlug] = route
	})
	var calls providerCalls
	h.mock.set("a", refusing(&calls.a, 1<<20, http.StatusTooManyRequests, "2"))
	h.mock.set("b", refusing(&calls.b, 0, 0, ""))
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.server.URL+"/v1/chat/completions", strings.NewReader(entryChat))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+fullKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("the caller got status %d while the route was waiting to retry", resp.StatusCode)
	}

	env := h.sink.last(t)
	if env.Outcome != "cancelled" || len(env.Attempts) != 1 || calls.a.Load() != 1 || calls.b.Load() != 0 {
		t.Fatalf("outcome = %q with %d attempts, provider calls %d and %d; want one attempt and a cancelled request", env.Outcome, len(env.Attempts), calls.a.Load(), calls.b.Load())
	}
}

type unaryAttemptSeen struct {
	class     string
	status    int
	uncertain bool
}

func TestAWholeOperationFallsBackOnlyWhenTheProviderRefusedItAndIsNeverRepeatedOnceItMayHaveRun(t *testing.T) {
	const routeDeadline = 5 * time.Second
	const found = `{"type":"search","web":{"results":[{"title":"Found","url":"https://example.com/found","description":"result"}]}}`
	served := unaryAttemptSeen{"success", http.StatusOK, false}
	for name, tc := range map[string]struct {
		provider   http.HandlerFunc
		timeout    int64
		status     int
		code       string
		final      bool
		retryAfter time.Duration
		attempts   []unaryAttemptSeen
	}{
		"rate limited": {
			provider: providerReply(http.StatusTooManyRequests, map[string]string{"Retry-After": "7"}, `{"error":{"message":"slow down"}}`),
			status:   http.StatusOK, retryAfter: 7 * time.Second, attempts: []unaryAttemptSeen{{"rate_limit", http.StatusTooManyRequests, false}, served},
		},
		"credential refused": {
			provider: providerReply(http.StatusUnauthorized, nil, `{"error":{"message":"bad key"}}`),
			status:   http.StatusOK, attempts: []unaryAttemptSeen{{"credential", http.StatusUnauthorized, false}, served},
		},
		"request the provider rejects": {
			provider: providerReply(http.StatusBadRequest, nil, `{"error":{"message":"bad","type":"invalid_request_error"}}`),
			status:   http.StatusBadRequest, code: "upstream_rejected", attempts: []unaryAttemptSeen{{"upstream_client", http.StatusBadRequest, false}},
		},
		"provider error": {
			provider: providerReply(http.StatusInternalServerError, nil, `{"error":{"message":"boom"}}`),
			status:   http.StatusBadGateway, code: "ambiguous_upstream_result", final: true, attempts: []unaryAttemptSeen{{"ambiguous", http.StatusInternalServerError, true}},
		},
		"silent provider": {
			provider: providerStream(stalled), timeout: 80,
			status: http.StatusBadGateway, code: "ambiguous_upstream_result", final: true, attempts: []unaryAttemptSeen{{"ambiguous", 0, true}},
		},
		"unreadable reply": {
			provider: providerReply(http.StatusOK, nil, `not json`),
			status:   http.StatusBadGateway, code: "fidelity_protocol_violation", final: true, attempts: []unaryAttemptSeen{{"protocol", http.StatusOK, true}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, Config{})
			h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
				for id, provider := range s.Providers {
					provider.VendorID = "brave"
					provider.ProfileID, provider.ProfileRevision = "brave-search", connectors.ProfileRevision
					provider.Capabilities = []routing.Capability{{Model: provider.Capabilities[0].Model, Operation: "search", Surface: "openai", Mode: "unary"}}
					s.Providers[id] = provider
				}
				route := s.Routes[routeSlug]
				route.Operations = []string{"search"}
				for i := range route.Targets {
					route.Targets[i].Timeout = cmp.Or(tc.timeout, route.Targets[i].Timeout)
				}
				s.Routes[routeSlug] = route
			})
			var second atomic.Int64
			h.mock.set("a", tc.provider)
			h.mock.set("b", func(w http.ResponseWriter, r *http.Request) {
				second.Add(1)
				providerReply(http.StatusOK, nil, found)(w, r)
			})
			started := time.Now()
			resp := h.do(t.Context(), http.MethodPost, "/v1/search", fullKey, []byte(`{"query":"test","max_results":1}`), map[string]string{"X-OLP-Route": routeSlug})
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if elapsed := time.Since(started); tc.timeout > 0 && elapsed > routeDeadline/2 {
				t.Fatalf("a provider silent past its %dms target timeout held the caller for %v", tc.timeout, elapsed)
			}

			code := ""
			if found := replyErrorCode.FindSubmatch(raw); found != nil {
				code = string(found[1])
			}
			if resp.StatusCode != tc.status || code != tc.code {
				t.Fatalf("reply = %d %q, want %d %q: %.300s", resp.StatusCode, code, tc.status, tc.code, raw)
			}
			if tc.code == "" && !strings.Contains(string(raw), "https://example.com/found") {
				t.Fatalf("the caller did not get the result: %.300s", raw)
			}
			if final := resp.Header.Get("X-Should-Retry") == "false"; final != tc.final {
				t.Fatalf("the reply told the caller not to retry = %t, want %t", final, tc.final)
			}
			if asked := second.Load() == 1; asked != (len(tc.attempts) == 2) {
				t.Fatalf("the second provider was asked %d times after %s", second.Load(), name)
			}

			env := h.sink.last(t)
			seen := make([]unaryAttemptSeen, len(env.Attempts))
			for i, attempt := range env.Attempts {
				seen[i] = unaryAttemptSeen{attempt.Class, attempt.Status, attempt.BillingUncertain}
			}
			if !slices.Equal(seen, tc.attempts) {
				t.Fatalf("attempts = %+v, want %+v", seen, tc.attempts)
			}
			if got := cmp.Or(env.Attempts[0].RetryAfter, new(time.Duration)); *got != tc.retryAfter {
				t.Fatalf("the first attempt recorded a provider wait of %v, want %v", *got, tc.retryAfter)
			}
		})
	}
}

func TestStagedBodyFailureBlamesTheProviderOnlyWhenItsBodyBreaks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		body   io.Reader
		closed bool
		local  bool
	}{
		{"provider body breaks", iotest.ErrReader(errors.New("connection reset")), false, false},
		{"spool is unavailable", strings.NewReader("content"), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{})
			spool, err := staging.NewSpool(t.TempDir(), staging.MinCapacityBytes, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = spool.Close() })
			if tc.closed {
				if err := spool.Close(); err != nil {
					t.Fatal(err)
				}
			}
			x := &Call{request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release}, family: wire.File, actor: "api_key_resource", keyID: h.keyID}
			response := &PinnedResponse{
				Response: &http.Response{Body: io.NopCloser(tc.body)},
				Exchange: h.gateway.beginPinned(t.Context(), x, testPinnedTarget(h, 0), http.MethodGet),
			}
			if _, err := response.StageBody(t.Context(), spool, "file-content", "application/octet-stream", 64); err == nil {
				t.Fatal("staging succeeded")
			}
			if fact := x.facts[0]; fact.Class != ClassProtocol || fact.LocalRefusal != tc.local {
				t.Fatalf("got class %q local %v, want local %v", fact.Class, fact.LocalRefusal, tc.local)
			}
		})
	}
}
