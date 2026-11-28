package gateway_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	surfacemedia "github.com/tyk-swe/olp/internal/gateway/surface/media"
	"github.com/tyk-swe/olp/internal/keyauthority"
	limitadmission "github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/limits/scripts"
	"github.com/tyk-swe/olp/internal/media/transport"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/platform/staging"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage/routinginputs"
	"github.com/tyk-swe/olp/tests/harness/money"
)

const (
	surfaceGolden = "testdata/surfaces.golden.json"
	routeSlug     = "team-chat"
	fullKey       = "olp_full_key"
	readKey       = "olp_read_key"
	otherKey      = "olp_other_key"
	secretA       = "secret-a"
	secretB       = "secret-b"
	modelA        = "model-a"
	modelB        = "model-b"
	answerText    = "hello from upstream"
)

var updateSurfaces = flag.Bool("update", false, "rewrite testdata/surfaces.golden.json from the current behavior")

type surfaceRuntime struct {
	mu      sync.Mutex
	release *release.Release
	inputs  *routinginputs.Inputs
	keys    map[string]keyauthority.Authority
	stale   bool
}

func (f *surfaceRuntime) Release() *release.Release {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.release
}

func (f *surfaceRuntime) RoutingInputs() *routinginputs.Inputs {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inputs
}

func (f *surfaceRuntime) Authenticate(secret string) (*keyauthority.Authority, error) {
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

func (f *surfaceRuntime) CheckRouteFidelity(context.Context, routing.Route) error { return nil }

func (f *surfaceRuntime) Eligibility(string) routing.Eligibility {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stale {
		return routing.StaleAuthority
	}
	return routing.Eligible
}

func (f *surfaceRuntime) Secret(_ context.Context, pinned *release.Release, id string) ([]byte, int64, error) {
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

func (f *surfaceRuntime) NetworkSecret(ctx context.Context, rel *release.Release, _, id string) ([]byte, error) {
	secret, _, err := f.Secret(ctx, rel, id)
	return secret, err
}

func (*surfaceRuntime) GrantGeneration(string) int64    { return 0 }
func (*surfaceRuntime) CredentialRefused(string, int64) {}

type surfaceEnvelopes struct {
	mu   sync.Mutex
	envs []events.Envelope
}

func (c *surfaceEnvelopes) Terminal(e events.Envelope) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.envs = append(c.envs, e)
}

type surfaceMock struct {
	mu       sync.Mutex
	handlers map[string]http.HandlerFunc
	calls    map[string]int
}

func (m *surfaceMock) count(provider string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[provider]
}

func (m *surfaceMock) answer(provider string, handler http.HandlerFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers[provider] = handler
}

func streamedCompletion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	chunk := func(rest string) string {
		return `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"` + modelA + `",` + rest + "}\n\n"
	}
	io.WriteString(w, chunk(`"choices":[{"index":0,"delta":{"role":"assistant","content":"`+answerText+`"},"finish_reason":null}]`))
	io.WriteString(w, chunk(`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`))
	io.WriteString(w, chunk(`"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}`))
	io.WriteString(w, "data: [DONE]\n\n")
}

func failedCompletion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	io.WriteString(w, `{"error":{"message":"upstream is down","type":"server_error"}}`)
}

func (m *surfaceMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

type unreachableValkey struct{}

func (unreachableValkey) Do(context.Context, ...string) (any, error) {
	return nil, context.DeadlineExceeded
}

func (unreachableValkey) Pipeline(context.Context, ...[]string) ([]any, error) {
	return nil, context.DeadlineExceeded
}

type surfaceEntry struct {
	name, method, path, body string
	headers                  map[string]string
	readsStore               bool
}

type surfaceCondition struct {
	name    string
	key     string
	route   string
	body    string
	only    string
	limited bool
	prepare func(*surfaceHarness)
	entry   bool
	state   bool
}

var statefulRouteOperations = []string{"generation", "batch", "realtime"}

type recordedAttempt struct {
	Mode      string       `json:"mode"`
	Status    int          `json:"status"`
	Class     string       `json:"class"`
	Committed bool         `json:"committed"`
	Usage     any          `json:"usage,omitempty"`
	Price     *money.Price `json:"price,omitempty"`
}

type recordedEnvelope struct {
	Operation  string           `json:"operation"`
	Surface    string           `json:"surface"`
	Mode       string           `json:"mode"`
	Route      string           `json:"route"`
	Outcome    string           `json:"outcome"`
	Status     int              `json:"status"`
	ErrorClass string           `json:"error_class"`
	Attempts   []settledAttempt `json:"attempts,omitempty"`
}

type settledAttempt struct {
	recordedAttempt
	UsageComplete    bool `json:"usage_complete"`
	BillingUncertain bool `json:"billing_uncertain"`
}

type surfaceRecord struct {
	Status      int                `json:"status"`
	ContentType string             `json:"content_type"`
	Headers     map[string]string  `json:"headers,omitempty"`
	Body        any                `json:"body"`
	Upstream    int                `json:"upstream_calls"`
	Envelopes   []recordedEnvelope `json:"envelopes,omitempty"`
	Cost        string             `json:"cost,omitempty"`
	Holds       []money.Hold       `json:"holds,omitempty"`
}

var surfaceRefusalHeaders = []string{"Allow", "Retry-After", "Www-Authenticate", "X-Should-Retry", "Anthropic-Ratelimit-Requests-Reset"}

var volatileSurfaceText = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|req_[A-Za-z0-9]+|"created":[0-9]+`)

func surfaceEntries() []surfaceEntry {
	stateRoute := map[string]string{kernel.StateRouteHeader: "{route}"}
	chat := `{"model":"{route}","messages":[{"role":"user","content":"hi"}]}`
	return []surfaceEntry{
		{name: "chat completions", method: "POST", path: "/v1/chat/completions", body: chat},
		{name: "responses", method: "POST", path: "/v1/responses", body: `{"model":"{route}","input":"hi"}`},
		{name: "embeddings", method: "POST", path: "/v1/embeddings", body: `{"model":"{route}","input":"hi"}`},
		{name: "moderations", method: "POST", path: "/v1/moderations", body: `{"model":"{route}","input":"hi"}`},
		{name: "anthropic messages", method: "POST", path: "/anthropic/v1/messages", body: `{"model":"{route}","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "gemini generate", method: "POST", path: "/gemini/v1beta/models/{route}:generateContent", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
		{name: "gemini interactions", method: "POST", path: "/gemini/v1beta/interactions", body: `{"model":"{route}","input":"hi","store":false}`, headers: stateRoute},
		{name: "gemini stored interactions", method: "POST", path: "/gemini/v1beta/interactions", body: `{"model":"{route}","input":"hi"}`},
		{name: "bedrock converse", method: "POST", path: "/bedrock/model/{route}/converse", body: `{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`},
		{name: "native unary", method: "POST", path: "/native/mistral-ocr/models/{route}", body: `{"document":{"type":"document_url","document_url":"https://example.com/a.pdf"}}`},
		{name: "passthrough", method: "GET", path: "/native/bound-native/routes/{route}/v1/widgets/abc"},
		{name: "image generation", method: "POST", path: "/v1/images/generations", body: `{"model":"{route}","prompt":"a red fox"}`},
		{name: "video list", method: "GET", path: "/v1/videos", headers: stateRoute, readsStore: true},
		{name: "file list", method: "GET", path: "/v1/files", headers: stateRoute, readsStore: true},
		{name: "batch create", method: "POST", path: "/v1/batches", body: `{"input_file_id":"file_1","endpoint":"/v1/chat/completions","completion_window":"24h"}`, headers: stateRoute},
		{name: "response get", method: "GET", path: "/v1/responses/resp_1", headers: stateRoute},
		{name: "response compact", method: "POST", path: "/v1/responses/compact", body: `{"model":"{route}","input":"hi"}`, headers: stateRoute},
		{name: "vector store create", method: "POST", path: "/v1/vector_stores", body: `{"name":"docs"}`, headers: stateRoute},
		{name: "retrieval query", method: "POST", path: "/v1/retrieval/binding-1/query", body: `{"query":"hi"}`, headers: stateRoute},
		{name: "anthropic file list", method: "GET", path: "/anthropic/v1/files", headers: stateRoute, readsStore: true},
		{name: "anthropic file upload", method: "POST", path: "/anthropic/v1/files", body: "--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.txt\"\r\nContent-Type: text/plain\r\n\r\nhi\r\n--b--\r\n", headers: map[string]string{kernel.StateRouteHeader: "{route}", "Content-Type": "multipart/form-data; boundary=b"}},
		{name: "anthropic batch create", method: "POST", path: "/anthropic/v1/messages/batches", body: `{"requests":[{"custom_id":"a","params":{"model":"{route}","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}}]}`, headers: stateRoute},
		{name: "gemini file list", method: "GET", path: "/gemini/v1beta/files", headers: stateRoute, readsStore: true},
		{name: "gemini upload start", method: "POST", path: "/gemini/upload/v1beta/files", body: `{"file":{"display_name":"a"}}`, headers: map[string]string{kernel.StateRouteHeader: "{route}", "X-Goog-Upload-Protocol": "resumable", "X-Goog-Upload-Command": "start", "X-Goog-Upload-Header-Content-Length": "2", "X-Goog-Upload-Header-Content-Type": "text/plain"}},
		{name: "gemini batch list", method: "GET", path: "/gemini/v1beta/batches", headers: stateRoute, readsStore: true},
		{name: "cloud batch create", method: "POST", path: "/native/vertex-batch-prediction/batch-jobs", body: `{"displayName":"a"}`, headers: stateRoute},
		{name: "realtime handshake", method: "GET", path: "/v1/realtime?model={route}"},
		{name: "realtime calls", method: "POST", path: "/v1/realtime/calls?model={route}", body: "v=0\r\n", headers: map[string]string{"Content-Type": "application/sdp"}},
		{name: "gemini live handshake", method: "GET", path: "/ws/" + connectors.GeminiLiveMethod},
		{name: "native duplex handshake", method: "GET", path: "/native/xai-realtime/sessions/{route}"},
		{name: "guardrails apply", method: "POST", path: "/v1/guardrails/apply", body: `{"model":"{route}","input":"hi"}`},
		{name: "delegations", method: "POST", path: "/v1/delegations", body: `{"route":"{route}"}`},
		{name: "traces", method: "POST", path: "/v1/traces", headers: map[string]string{"Content-Type": "application/x-protobuf"}},
		{name: "models", method: "GET", path: "/v1/models"},
		{name: "unknown endpoint", method: "GET", path: "/v1/nothing-here"},
	}
}

func surfaceConditions() []surfaceCondition {
	return []surfaceCondition{
		{name: "as published", key: fullKey, route: routeSlug},
		{name: "no credential", route: routeSlug, entry: true},
		{name: "unknown key", key: "olp_unknown_key", route: routeSlug, entry: true},
		{name: "scope denied", key: readKey, route: routeSlug, entry: true},
		{name: "route denied", key: otherKey, route: routeSlug},
		{name: "unknown route", key: fullKey, route: "missing-route"},
		{name: "stale authority", key: fullKey, route: routeSlug, entry: true, prepare: func(h *surfaceHarness) { h.rt.stale = true }},
		{name: "malformed body", key: fullKey, route: routeSlug, body: "{"},
		{name: "limits unavailable", key: fullKey, route: routeSlug, limited: true},
		{name: "provider state denied", key: fullKey, route: routeSlug, state: true, prepare: (*surfaceHarness).denyProviderState},
		{name: "route header missing", key: fullKey, state: true},
		{name: "stateful operations allowed", key: fullKey, route: routeSlug, state: true, prepare: (*surfaceHarness).allowStatefulOperations},
		{name: "limits unavailable after pin", key: fullKey, route: routeSlug, limited: true, state: true, prepare: (*surfaceHarness).pinNativeBatchProviders},
		{name: "strict route", key: fullKey, route: routeSlug, state: true, prepare: (*surfaceHarness).enableStrictStateful},
		{name: "streamed", key: fullKey, route: routeSlug, only: "chat completions", body: `{"model":"` + routeSlug + `","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`, prepare: func(h *surfaceHarness) { h.mock.answer("a", streamedCompletion) }},
		{name: "first provider fails", key: fullKey, route: routeSlug, only: "chat completions", prepare: func(h *surfaceHarness) { h.mock.answer("a", failedCompletion) }},
		{name: "every provider fails", key: fullKey, route: routeSlug, only: "chat completions", prepare: func(h *surfaceHarness) {
			h.mock.answer("a", failedCompletion)
			h.mock.answer("b", failedCompletion)
		}},
	}
}

type surfaceHarness struct {
	t        *testing.T
	rt       *surfaceRuntime
	mock     *surfaceMock
	sink     *surfaceEnvelopes
	gateway  *kernel.Server
	deps     kernel.Dependencies
	server   *httptest.Server
	upstream *httptest.Server
	returned chan struct{}
	ledger   *money.Ledger
}

func (h *surfaceHarness) republish(edit func(s *routing.Snapshot, a, b routing.Provider)) {
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
	rel, err := release.NewRelease(uuid.NewString(), old.Sequence+1, snapshot, credentials)
	if err != nil {
		h.t.Fatal(err)
	}
	h.rt.mu.Lock()
	h.rt.release = rel
	h.rt.mu.Unlock()
}

func (h *surfaceHarness) allowStatefulOperations() {
	h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
		route := s.Routes[routeSlug]
		route.Operations = statefulRouteOperations
		s.Routes[routeSlug] = route
	})
}

func (h *surfaceHarness) pinNativeBatchProviders() {
	h.republish(func(s *routing.Snapshot, a, b routing.Provider) {
		route := s.Routes[routeSlug]
		route.Operations = statefulRouteOperations
		s.Routes[routeSlug] = route
		a.Kind, b.Kind = "anthropic", "gemini"
		a.Capabilities = append(a.Capabilities, routing.Capability{Model: modelA, Operation: "batch", Surface: "anthropic", Mode: "unary"})
		b.Capabilities = append(b.Capabilities, routing.Capability{Model: modelB, Operation: "batch", Surface: "gemini", Mode: "unary"})
		s.Providers[a.ID], s.Providers[b.ID] = a, b
	})
}

func (h *surfaceHarness) enableStrictStateful() {
	current := h.rt.Release()
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
	route.Operations = []string{"generation", "batch"}
	snapshot.Routes[routeSlug] = route
	rel, err := release.NewRelease(current.ID, current.Sequence, snapshot, credentials)
	if err != nil {
		h.t.Fatal(err)
	}
	h.rt.mu.Lock()
	h.rt.release = rel
	h.rt.mu.Unlock()
}

func (h *surfaceHarness) denyProviderState() {
	authority := h.rt.keys[fullKey]
	authority.Policy.AllowProviderState = false
	h.rt.keys[fullKey] = authority
}

func newSurfaceHarness(t *testing.T, limited bool) *surfaceHarness {
	t.Helper()
	completion := func(model string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`, model, answerText)
		}
	}
	m := &surfaceMock{
		handlers: map[string]http.HandlerFunc{"a": completion(modelA), "b": completion(modelB)},
		calls:    map[string]int{},
	}
	upstream := httptest.NewServer(m)
	t.Cleanup(upstream.Close)
	credA, credB := uuid.NewString(), uuid.NewString()
	version := 1
	provider := func(name, secretID, model string) routing.Provider {
		return routing.Provider{
			ID: uuid.NewString(), Name: name, Kind: "openai_compatible", Enabled: true, ActiveCredential: &secretID, RevisionID: uuid.NewString(),
			Capabilities: []routing.Capability{
				{Model: model, Operation: "generation", Surface: "openai", Mode: "unary"},
				{Model: model, Operation: "generation", Surface: "openai", Mode: "streaming"},
			},
			Endpoint: upstream.URL + "/" + name + "/v1", AuthMode: "api_key",
			Slots: []routing.Slot{{ID: uuid.NewString(), Name: "default", Enabled: true, Weight: 1, CredentialID: &secretID, CredentialVersion: &version}},
		}
	}
	a, b := provider("a", credA, modelA), provider("b", credB, modelB)
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
	rel, err := release.NewRelease(uuid.NewString(), 7, snapshot, map[string][]byte{credA: []byte(secretA), credB: []byte(secretB)})
	if err != nil {
		t.Fatal(err)
	}
	rt := &surfaceRuntime{release: rel, keys: map[string]keyauthority.Authority{
		fullKey:  {ID: uuid.NewString(), Policy: keyauthority.KeyPolicy{Scopes: []string{"inference", "models_read"}, AllowProviderState: true}},
		readKey:  {ID: uuid.NewString(), Policy: keyauthority.KeyPolicy{Scopes: []string{"models_read"}, AllowProviderState: true}},
		otherKey: {ID: uuid.NewString(), Policy: keyauthority.KeyPolicy{Scopes: []string{"inference", "models_read"}, AllowedRoutes: []string{"another-route"}, AllowProviderState: true}},
	}}
	if limited {
		requests := int64(60)
		for secret, authority := range rt.keys {
			authority.LookupID = "lookup" + secret
			authority.Policy.RequestsPerMinute = &requests
			rt.keys[secret] = authority
		}
	}
	keys, err := secrets.ParseRingWithUnwrapper(context.Background(), fmt.Appendf(nil, `{"active_version":1,"keys":[{"version":1,"key":%q}]}`, strings.Repeat("01", 32)), nil)
	if err != nil {
		t.Fatal(err)
	}
	spool, err := staging.NewSpool(t.TempDir(), staging.MinCapacityBytes, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spool.Close() })
	policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	sink := &surfaceEnvelopes{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dependencies := kernel.Dependencies{
		Runtime: rt, Egress: &policy, Log: log, Sink: sink, Getenv: func(string) string { return "" },
		Resources: resources.NewEncrypted(nil, "surface-golden", keys),
		Staging:   spool, Uploads: staging.NewAdmission(spool.CapacityBytes()),
	}
	mediaDeps := surfacemedia.Deps{Transport: transport.New(transport.Deps{
		Client: policy.Client(kernel.UpstreamHeaderTimeout), Auth: connectors.NewAuth(&policy, func(string) string { return "" }),
		Egress: &policy, Spool: spool, MaxResponseBytes: 1 << 20, Log: log,
	})}
	admission, ledger := money.Scripted(t)
	dependencies.Admission = admission
	if limited {
		runner, err := scripts.NewRunner(unreachableValkey{}, "test")
		if err != nil {
			t.Fatal(err)
		}
		dependencies.Admission = quota.NewAdmission(limitadmission.NewDurable(runner, nil), fleet.New(runner), func() limitadmission.OutagePolicy { return limitadmission.FailClosed }, slog.New(slog.DiscardHandler))
	}
	gw := kernel.New(dependencies, kernel.Config{MaxInFlight: 8, MaxBodyBytes: 64 * 1024, MaxMediaBodyBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxEventBytes: 4096})
	mux := http.NewServeMux()
	gateway.Register(gw, mux, dependencies, gateway.WithMedia(mediaDeps))
	returned := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { returned <- struct{}{} }()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return &surfaceHarness{t: t, rt: rt, mock: m, sink: sink, gateway: gw, deps: dependencies, server: server, upstream: upstream, returned: returned, ledger: ledger}
}

func (h *surfaceHarness) price(budgeted bool) {
	h.t.Helper()
	h.republish(func(snapshot *routing.Snapshot, _, _ routing.Provider) {
		if budgeted {
			h.rt.keys[fullKey] = h.ledger.Budget(h.rt.keys[fullKey])
			h.ledger.Cap(snapshot, routeSlug)
		}
		inputs := money.Inputs(h.t, snapshot, money.Operations...)
		h.rt.mu.Lock()
		h.rt.inputs = inputs
		h.rt.mu.Unlock()
	})
}

func (h *surfaceHarness) do(ctx context.Context, method, path, key string, body []byte, headers map[string]string) *http.Response {
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

func (h *surfaceHarness) record(t *testing.T, response *http.Response) surfaceRecord {
	t.Helper()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	<-h.returned
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	record := surfaceRecord{Status: response.StatusCode, ContentType: mediaType, Upstream: h.mock.count("a") + h.mock.count("b")}
	record.Body = h.stable(raw)
	if mediaType == "application/x-protobuf" {
		sum := sha256.Sum256(raw)
		record.Body = hex.EncodeToString(sum[:8]) + "/" + strconv.Itoa(len(raw))
	}
	headers := map[string]string{}
	for _, name := range surfaceRefusalHeaders {
		if value := response.Header.Get(name); value != "" {
			headers[name] = value
		}
	}
	if len(headers) > 0 {
		record.Headers = headers
	}
	record.Envelopes = h.envelopes()
	record.Cost, record.Holds = response.Header.Get(money.CostHeader), h.ledger.Drain()
	return record
}

func (h *surfaceHarness) stable(raw []byte) any {
	scrubbed := volatileSurfaceText.ReplaceAllFunc(raw, func(match []byte) []byte {
		if bytes.HasPrefix(match, []byte(`"created"`)) {
			return []byte(`"created":0`)
		}
		return []byte("{id}")
	})
	var v any
	if json.Unmarshal(scrubbed, &v) == nil {
		if out, err := json.Marshal(v); err == nil {
			return json.RawMessage(strings.ReplaceAll(string(out), h.upstream.URL, "{upstream}"))
		}
	}
	return strings.ReplaceAll(string(scrubbed), h.upstream.URL, "{upstream}")
}

func (h *surfaceHarness) envelopes() []recordedEnvelope {
	h.sink.mu.Lock()
	defer h.sink.mu.Unlock()
	recorded := make([]recordedEnvelope, 0, len(h.sink.envs))
	for _, envelope := range h.sink.envs {
		attempts := make([]settledAttempt, 0, len(envelope.Attempts))
		for _, attempt := range envelope.Attempts {
			settled := settledAttempt{
				recordedAttempt: recordedAttempt{Mode: attempt.Mode, Status: attempt.Status, Class: attempt.Class, Committed: attempt.Committed, Price: money.AttemptPrice(envelope.Operation, &attempt)},
				UsageComplete:   attempt.UsageComplete, BillingUncertain: attempt.BillingUncertain,
			}
			if attempt.Usage != nil {
				settled.Usage = attempt.Usage
			}
			attempts = append(attempts, settled)
		}
		recorded = append(recorded, recordedEnvelope{
			Operation: envelope.Operation, Surface: envelope.Surface, Mode: envelope.Mode, Route: envelope.Route,
			Outcome: envelope.Outcome, Status: envelope.Status, ErrorClass: envelope.ErrorClass, Attempts: attempts,
		})
	}
	return recorded
}

func runSurfaceCase(t *testing.T, entry surfaceEntry, condition surfaceCondition) surfaceRecord {
	t.Helper()
	h := newSurfaceHarness(t, condition.limited)
	if condition.prepare != nil {
		condition.prepare(h)
	}
	h.price(!condition.limited)
	headers := map[string]string{}
	for name, value := range entry.headers {
		headers[name] = strings.ReplaceAll(value, "{route}", condition.route)
	}
	body := strings.ReplaceAll(entry.body, "{route}", condition.route)
	if condition.body != "" {
		body = condition.body
	}
	var payload []byte
	if entry.method != http.MethodGet {
		payload = []byte(body)
	}
	response := h.do(t.Context(), entry.method, strings.ReplaceAll(entry.path, "{route}", condition.route), condition.key, payload, headers)
	defer func() { _ = response.Body.Close() }()
	return h.record(t, response)
}

func surfaceCaseApplies(entry surfaceEntry, condition surfaceCondition) bool {
	switch {
	case condition.only != "" && entry.name != condition.only:
		return false
	case condition.body != "" && entry.body == "":
		return false
	case condition.state && entry.headers[kernel.StateRouteHeader] == "":
		return false
	case entry.readsStore && !condition.entry:
		return false
	}
	return true
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

func TestSurfaceRefusalsGolden(t *testing.T) {
	type goldenCase struct {
		name string
		run  func(*testing.T) any
	}
	cases := []goldenCase{}
	for _, entry := range surfaceEntries() {
		for _, condition := range surfaceConditions() {
			if surfaceCaseApplies(entry, condition) {
				cases = append(cases, goldenCase{entry.name + ", " + condition.name, func(t *testing.T) any { return runSurfaceCase(t, entry, condition) }})
			}
		}
	}
	want := map[string]json.RawMessage{}
	if !*updateSurfaces {
		raw, err := os.ReadFile(surfaceGolden)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &want); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	got := map[string]json.RawMessage{}
	t.Run("cases", func(t *testing.T) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				encoded, err := json.Marshal(c.run(t))
				if err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				got[c.name] = encoded
				mu.Unlock()
				if !*updateSurfaces && !jsonEqual(want[c.name], encoded) {
					t.Errorf("behavior changed\nwant %s\ngot  %s", want[c.name], encoded)
				}
			})
		}
	})
	if !*updateSurfaces {
		if len(want) != len(got) {
			t.Errorf("golden holds %d cases and the suite ran %d", len(want), len(got))
		}
		return
	}
	lines := make([]string, 0, len(got))
	for _, name := range slices.Sorted(maps.Keys(got)) {
		lines = append(lines, strconv.Quote(name)+": "+string(got[name]))
	}
	if err := os.WriteFile(surfaceGolden, []byte("{\n"+strings.Join(lines, ",\n")+"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
