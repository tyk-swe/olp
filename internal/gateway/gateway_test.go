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
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codesessions"
	"github.com/tyk-swe/olp/internal/staging"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/google/uuid"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codeclient"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	limitadmission "github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/limits/scripts"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/release"

	"github.com/tyk-swe/olp/internal/routing"
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
	keys    map[string]keyauthority.Authority
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
	envs []Envelope
}

func (c *envelopes) Terminal(e Envelope) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.envs = append(c.envs, e)
}

func (c *envelopes) last(t *testing.T) Envelope {
	t.Helper()
	envs := c.await(t, 1)
	return envs[len(envs)-1]
}

func (c *envelopes) await(t *testing.T, n int) []Envelope {
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

func TestHalfOpenProbeIsReleasedOnlyByItsOwnCompletion(t *testing.T) {
	const provider = "provider"
	for _, test := range []struct {
		name       string
		probeClass string
		wantOpen   bool
	}{
		{"success closes the circuit", classSuccess, false},
		{"failure reopens the circuit", "upstream_server", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			clock := time.Date(2026, time.March, 10, 12, 0, 0, 0, time.UTC)
			health := newHealthTracker(func() time.Time { return clock })
			for range circuitFailures {
				health.record(provider, AttemptFact{Class: "upstream_server"}, false)
			}
			if !health.open(provider) {
				t.Fatal("repeated failures did not open the circuit")
			}
			clock = clock.Add(circuitOpenFor)
			if probed, granted := health.claim(provider); !probed || !granted {
				t.Fatalf("recovering circuit refused its probe: probed=%v granted=%v", probed, granted)
			}
			for _, unrelated := range []string{"rate_limit", "upstream_server"} {
				health.record(provider, AttemptFact{Class: unrelated}, false)
				if _, granted := health.claim(provider); granted {
					t.Fatalf("in-flight %s completion admitted a second probe", unrelated)
				}
			}
			health.record(provider, AttemptFact{Class: test.probeClass}, true)
			if open := health.open(provider); open != test.wantOpen {
				t.Fatalf("open after probe = %v, want %v", open, test.wantOpen)
			}
		})
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
func (l *chargeLedger) Admit(context.Context, codesessions.Admission) (codesessions.Permit, error) {
	return codesessions.Permit{}, nil
}
func (l *chargeLedger) BindConnection(context.Context, codemode.Route, string, codemode.Identity, string, []string) (codesessions.Permit, error) {
	return codesessions.Permit{}, nil
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
			attempt := &codeAttempt{server: &Server{deps: Dependencies{CodeLedger: ledger}}, adapter: tc.adapter, permit: codesessions.Permit{Attempt: codemode.Attempt{ID: "attempt-1"}}}
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
	attempt := &codeAttempt{server: &Server{deps: Dependencies{CodeLedger: ledger}}, adapter: codemode.AdapterOpenCodeGo, permit: codesessions.Permit{Attempt: codemode.Attempt{ID: "attempt-1"}}}
	attempt.observe(codemode.Observation{Terminal: true, AllowanceCharge: json.RawMessage(`{"billing_source":"go","allowance":0.001}`)})
	terminal := json.RawMessage(`{"billing_source":"go","allowance":0.0021}`)
	attempt.observe(codemode.Observation{Terminal: true, AllowanceCharge: terminal})
	if string(ledger.charge) != string(terminal) {
		t.Fatalf("charge = %s, want the last terminal charge %s", ledger.charge, terminal)
	}
}

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
	spool, err := staging.NewSpool(t.TempDir(), staging.MinCapacityBytes, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spool.Close() })
	h.gateway.deps.Media = &MediaDeps{
		Jobs: &media.Service{Transport: &media.Transport{
			Client: h.gateway.client, Auth: h.gateway.auth, Egress: h.gateway.egress,
			Spool: spool, MaxResponseBytes: 1 << 20,
		}},
	}
	h.gateway.deps.Staging = spool
	h.gateway.deps.Uploads = staging.NewAdmission(spool.CapacityBytes())
	h.republish(func(snapshot *routing.Snapshot, a, b routing.Provider) {
		for _, provider := range []routing.Provider{a, b} {
			provider.Capabilities = []routing.Capability{
				{Model: provider.Capabilities[0].Model, Operation: media.OpImageGeneration, Surface: "openai", Mode: "unary"},
				{Model: provider.Capabilities[0].Model, Operation: media.OpTranscription, Surface: "openai", Mode: "unary"},
			}
			snapshot.Providers[provider.ID] = provider
		}
		route := snapshot.Routes[routeSlug]
		route.Operations = []string{media.OpImageGeneration, media.OpTranscription}
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

func TestResearchStreamFollowsOutagePolicy(t *testing.T) {
	runner, err := scripts.NewRunner(unreachableValkey{}, "test")
	if err != nil {
		t.Fatal(err)
	}
	limiter := limitadmission.New(runner)
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
			admission := NewAdmission(limiter, fleet.New(runner), func() limitadmission.OutagePolicy { return test.policy }, slog.New(slog.DiscardHandler))
			lease, failure := admission.reserveResearchStream(t.Context(), authority, routeSlug, "service", "provider", test.provider)
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
