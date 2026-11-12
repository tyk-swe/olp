package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	limitadmission "github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/limits/scripts"
	"github.com/tyk-swe/olp/internal/media/transport"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/staging"
)

const surfaceGolden = "testdata/surfaces.golden.json"

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
	limited bool
	prepare func(*surfaceHarness)
	entry   bool
	state   bool
}

var statefulRouteOperations = []string{"generation", "batch", "realtime"}

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

func (h *surfaceHarness) denyProviderState() {
	authority := h.rt.keys[fullKey]
	authority.Policy.AllowProviderState = false
	h.rt.keys[fullKey] = authority
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
}

var surfaceRefusalHeaders = []string{"Allow", "Retry-After", "Www-Authenticate", "X-Should-Retry", "Anthropic-Ratelimit-Requests-Reset"}

var volatileSurfaceText = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|req_[A-Za-z0-9]+|"created":[0-9]+`)

func surfaceEntries() []surfaceEntry {
	stateRoute := map[string]string{stateRouteHeader: "{route}"}
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
		{name: "anthropic file upload", method: "POST", path: "/anthropic/v1/files", body: "--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.txt\"\r\nContent-Type: text/plain\r\n\r\nhi\r\n--b--\r\n", headers: map[string]string{stateRouteHeader: "{route}", "Content-Type": "multipart/form-data; boundary=b"}},
		{name: "anthropic batch create", method: "POST", path: "/anthropic/v1/messages/batches", body: `{"requests":[{"custom_id":"a","params":{"model":"{route}","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}}]}`, headers: stateRoute},
		{name: "gemini file list", method: "GET", path: "/gemini/v1beta/files", headers: stateRoute, readsStore: true},
		{name: "gemini upload start", method: "POST", path: "/gemini/upload/v1beta/files", body: `{"file":{"display_name":"a"}}`, headers: map[string]string{stateRouteHeader: "{route}", "X-Goog-Upload-Protocol": "resumable", "X-Goog-Upload-Command": "start", "X-Goog-Upload-Header-Content-Length": "2", "X-Goog-Upload-Header-Content-Type": "text/plain"}},
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
		{name: "strict route", key: fullKey, route: routeSlug, state: true, prepare: func(h *surfaceHarness) {
			strictRelease(h.t, h.harness, func(s *routing.Snapshot) {
				route := s.Routes[routeSlug]
				route.Operations = []string{"generation", "batch"}
				s.Routes[routeSlug] = route
			})
		}},
	}
}

type surfaceHarness struct {
	*harness
	returned chan struct{}
}

func newSurfaceHarness(t *testing.T, limited bool) *surfaceHarness {
	t.Helper()
	keys, err := secrets.ParseRing(fmt.Appendf(nil, `{"active_version":1,"keys":[{"version":1,"key":%q}]}`, strings.Repeat("01", 32)))
	if err != nil {
		t.Fatal(err)
	}
	dependencies := Dependencies{Resources: resources.NewEncrypted(nil, "surface-golden", keys)}
	if limited {
		runner, err := scripts.NewRunner(unreachableValkey{}, "test")
		if err != nil {
			t.Fatal(err)
		}
		dependencies.Admission = quota.NewAdmission(limitadmission.New(runner), fleet.New(runner), func() limitadmission.OutagePolicy { return limitadmission.FailClosed }, slog.New(slog.DiscardHandler))
	}
	h := newHarness(t, Config{MaxMediaBodyBytes: 1 << 20}, dependencies)
	spool, err := staging.NewSpool(t.TempDir(), staging.MinCapacityBytes, nil)
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
	requests := int64(60)
	for secret, authority := range h.rt.keys {
		authority.Policy.AllowProviderState = true
		if limited {
			authority.LookupID = "lookup" + secret
			authority.Policy.RequestsPerMinute = &requests
		}
		h.rt.keys[secret] = authority
	}
	mux := http.NewServeMux()
	h.gateway.Register(mux)
	returned := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { returned <- struct{}{} }()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	h.server = server
	return &surfaceHarness{harness: h, returned: returned}
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
		record.Body = digest(raw)
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
	return record
}

func (h *surfaceHarness) stable(raw []byte) any {
	scrubbed := volatileSurfaceText.ReplaceAllFunc(raw, func(match []byte) []byte {
		if bytes.HasPrefix(match, []byte(`"created"`)) {
			return []byte(`"created":0`)
		}
		return []byte("{id}")
	})
	if canonical, ok := canonicalJSON(scrubbed, h.upstream.URL); ok {
		return canonical
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
				recordedAttempt: recordedAttempt{Mode: attempt.Mode, Status: attempt.Status, Class: attempt.Class, Committed: attempt.Committed},
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
	return h.record(t, h.do(t.Context(), entry.method, strings.ReplaceAll(entry.path, "{route}", condition.route), condition.key, payload, headers))
}

func surfaceCaseApplies(entry surfaceEntry, condition surfaceCondition) bool {
	switch {
	case condition.body != "" && entry.body == "":
		return false
	case condition.state && entry.headers[stateRouteHeader] == "":
		return false
	case entry.readsStore && !condition.entry:
		return false
	}
	return true
}

type goldenCase struct {
	name string
	run  func(*testing.T) any
}

func checkGolden(t *testing.T, path string, cases []goldenCase) {
	t.Helper()
	want := map[string]json.RawMessage{}
	if !*update {
		raw, err := os.ReadFile(path)
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
				if !*update && !jsonEqual(want[c.name], encoded) {
					t.Errorf("behavior changed\nwant %s\ngot  %s", want[c.name], encoded)
				}
			})
		}
	})
	if !*update {
		if len(want) != len(got) {
			t.Errorf("golden holds %d cases and the suite ran %d", len(want), len(got))
		}
		return
	}
	lines := make([]string, 0, len(got))
	for _, name := range slices.Sorted(maps.Keys(got)) {
		lines = append(lines, strconv.Quote(name)+": "+string(got[name]))
	}
	if err := os.WriteFile(path, []byte("{\n"+strings.Join(lines, ",\n")+"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSurfaceRefusalsGolden(t *testing.T) {
	cases := []goldenCase{}
	for _, entry := range surfaceEntries() {
		for _, condition := range surfaceConditions() {
			if surfaceCaseApplies(entry, condition) {
				cases = append(cases, goldenCase{entry.name + ", " + condition.name, func(t *testing.T) any { return runSurfaceCase(t, entry, condition) }})
			}
		}
	}
	checkGolden(t, surfaceGolden, cases)
}
