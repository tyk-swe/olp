package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
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
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codeclient"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/resources"
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
	release *release.Release
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

func (f *fakeRuntime) Release() *release.Release { f.mu.Lock(); defer f.mu.Unlock(); return f.release }

func (f *fakeRuntime) RoutingInputs() *usage.RoutingInputs {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inputs
}

func (f *fakeRuntime) Authenticate(secret string) (*access.Authority, error) {
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
	release, err := release.NewRelease(uuid.NewString(), 7, snapshot, map[string][]byte{credA: []byte(secretA), credB: []byte(secretB)})
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
	sink := &capture{}
	gw := New(rt, &policy, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	gw.Sink = sink
	mux := http.NewServeMux()
	gw.Register(mux)
	server := httptest.NewServer(wrap(mux))
	t.Cleanup(server.Close)
	return &harness{t: t, rt: rt, mock: m, sink: sink, gateway: gw, server: server, keyID: keyID, slotA: slotA, credA: credA, upstream: upstream}
}

// admissionAuthority builds a key with the limits a case needs.
func admissionAuthority(policy access.KeyPolicy) access.Authority {
	return access.Authority{ID: uuid.Must(uuid.NewV7()).String(), LookupID: "lookupid0123", Policy: policy}
}

// reissue publishes a release in which the harness's first provider is
// edited, with credential as its secret unless that is nil.
func (h *harness) reissue(edit func(*routing.Provider), credential []byte) {
	h.t.Helper()
	secrets := map[string][]byte{}
	for id, provider := range h.rt.release.Snapshot.Providers {
		for _, slot := range provider.Slots {
			secrets[*slot.CredentialID], _ = h.rt.release.Credential(*slot.CredentialID)
		}
		if provider.Slots[0].ID == h.slotA {
			edit(&provider)
			h.rt.release.Snapshot.Providers[id] = provider
		}
	}
	if credential != nil {
		secrets[h.credA] = credential
	}
	release, err := release.NewRelease(uuid.NewString(), 7, h.rt.release.Snapshot, secrets)
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
	snapshot := current.Snapshot
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
		Generation: routing.Generation{ID: uuid.NewString(), Ordinal: old.Snapshot.Generation.Ordinal + 1, ActivatedAt: old.Snapshot.Generation.ActivatedAt},
		Providers:  maps.Clone(old.Snapshot.Providers),
		Routes:     maps.Clone(old.Snapshot.Routes),
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

type chargeLedger struct {
	charge  json.RawMessage
	attempt string
}

func (l *chargeLedger) Models(context.Context, codemode.Route, string) (codeclient.Offers, error) {
	return nil, nil
}
func (l *chargeLedger) Admit(context.Context, resources.CodeAdmission) (resources.CodePermit, error) {
	return resources.CodePermit{}, nil
}
func (l *chargeLedger) BindConnection(context.Context, codemode.Route, string, codemode.Identity, string, []string) (resources.CodePermit, error) {
	return resources.CodePermit{}, nil
}
func (l *chargeLedger) MarkDispatched(context.Context, string) error { return nil }
func (l *chargeLedger) ConfirmNonexecution(context.Context, string, int) error {
	return nil
}
func (l *chargeLedger) ObserveCooldown(context.Context, string, time.Time) error { return nil }
func (l *chargeLedger) ObserveCooldownClass(context.Context, string, string, codeadapter.Classification, time.Time) error {
	return nil
}
func (l *chargeLedger) Settle(context.Context, string, codemode.Usage) error { return nil }
func (l *chargeLedger) Abort(context.Context, string) error                  { return nil }
func (l *chargeLedger) ObserveHealth(context.Context, string, codemode.HealthObservation) error {
	return nil
}
func (l *chargeLedger) ObserveAllowance(context.Context, string, codemode.Allowance) error {
	return nil
}
func (l *chargeLedger) ObserveAllowanceCharge(_ context.Context, attempt string, charge json.RawMessage) error {
	l.attempt, l.charge = attempt, charge
	return nil
}
func (l *chargeLedger) ObserveReference(context.Context, string, string) error { return nil }
func (l *chargeLedger) ObserveOutcome(context.Context, string, codemode.Outcome) error {
	return nil
}
func (l *chargeLedger) RecordRefusal(context.Context, codemode.Route, string, string, string) error {
	return nil
}

func TestCodeAttemptStoresAllowanceChargeOnlyForOpenCodeGo(t *testing.T) {
	reported := json.RawMessage(`{"billing_source":"go","allowance":0.0021}`)
	for name, tc := range map[string]struct {
		adapter codemode.Adapter
		paid    bool
		want    bool
	}{
		"opencode go attempt":     {adapter: codemode.AdapterOpenCodeGo, want: true},
		"other adapter attempt":   {adapter: codemode.AdapterZAICoding},
		"paid opencode-shaped":    {adapter: codemode.AdapterOpenCodeGo, paid: true},
		"unknown adapter attempt": {},
	} {
		t.Run(name, func(t *testing.T) {
			ledger := &chargeLedger{}
			attempt := &codeAttempt{server: &Server{CodeLedger: ledger}, adapter: tc.adapter, permit: resources.CodePermit{Attempt: codemode.Attempt{ID: "attempt-1"}}}
			if tc.paid {
				attempt.paid = &paidCodeAttempt{}
			}
			attempt.observe(codemode.Observation{Terminal: true, AllowanceCharge: reported})
			if tc.want && (ledger.attempt != "attempt-1" || string(ledger.charge) != string(reported)) {
				t.Fatalf("ledger = %s %s, want the reported charge on attempt-1", ledger.attempt, ledger.charge)
			}
			if !tc.want && ledger.charge != nil {
				t.Fatalf("charge stored = %s, want none", ledger.charge)
			}
		})
	}
}

func TestCodeAttemptLastTerminalChargeWins(t *testing.T) {
	ledger := &chargeLedger{}
	attempt := &codeAttempt{server: &Server{CodeLedger: ledger}, adapter: codemode.AdapterOpenCodeGo, permit: resources.CodePermit{Attempt: codemode.Attempt{ID: "attempt-1"}}}
	attempt.observe(codemode.Observation{Terminal: true, AllowanceCharge: json.RawMessage(`{"billing_source":"go","allowance":0.001}`)})
	terminal := json.RawMessage(`{"billing_source":"go","allowance":0.0021}`)
	attempt.observe(codemode.Observation{Terminal: true, AllowanceCharge: terminal})
	if string(ledger.charge) != string(terminal) {
		t.Fatalf("charge = %s, want the last terminal charge %s", ledger.charge, terminal)
	}
}
