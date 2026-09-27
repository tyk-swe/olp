//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/internal/usage"
)

const pluginCredential = "reference-static-secret"

// pluginUpstream is the fictional upstream the reference plugin's profiles
// place requests at: an OpenAI Chat Completions server, with an API per
// workspace too, that takes its key as a token in the Authorization header
// and wants clients to identify themselves. It records the headers and path
// of every request.
type pluginUpstream struct {
	*httptest.Server
	mu          sync.Mutex
	credentials []string
	requests    []http.Header
	paths       []string
}

func newPluginUpstream(t *testing.T, credentials ...string) *pluginUpstream {
	t.Helper()
	u := &pluginUpstream{credentials: credentials}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.requests = append(u.requests, r.Header.Clone())
		u.paths = append(u.paths, r.URL.Path)
		u.mu.Unlock()
		token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Token ")
		if !found || !slices.Contains(u.credentials, token) || r.Header.Get("X-Reference-Client") != "olp" {
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(w, map[string]any{"error": map[string]any{"message": "Unknown token " + r.Header.Get("Authorization"), "type": "invalid_request_error", "code": "invalid_api_key"}})
			return
		}
		if r.Method != http.MethodPost || !pluginUpstreamPath.MatchString(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		usage := map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}
		if !body.Stream {
			writeJSON(w, map[string]any{"id": "chatcmpl-reference", "object": "chat.completion", "created": 1, "model": body.Model,
				"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "Hello from the reference upstream"}, "finish_reason": "stop"}}, "usage": usage})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []map[string]any{
			{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": "Hello"}, "finish_reason": nil}}},
			{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": usage},
		} {
			chunk["id"], chunk["object"], chunk["created"], chunk["model"] = "chatcmpl-reference", "chat.completion.chunk", 1, body.Model
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(u.Close)
	return u
}

var pluginUpstreamPath = regexp.MustCompile(`^/v1(/workspaces/[a-z0-9-]+)?/chat/completions$`)

func (u *pluginUpstream) received() []http.Header {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.requests)
}

// receivedPaths returns the path of every request, in order.
func (u *pluginUpstream) receivedPaths() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.paths)
}

// installReferencePlugin builds the reference plugin against the upstream,
// installs it and approves the origins it declares.
func installReferencePlugin(t *testing.T, h *accessHarness, owner *browser, upstream *pluginUpstream, version string) string {
	t.Helper()
	module := testutil.BuildPlugin(t, "./sdk/plugin/reference", "-X=main.upstream="+upstream.URL+"/v1", "-X=main.version="+version)
	installed := h.want(owner, "POST", "/api/v1/plugins", module, wasm, 201)
	origins := installed["manifest"].(map[string]any)["origins"]
	h.want(owner, "POST", "/api/v1/plugins/"+digestOf(module)+"/approve", map[string]any{"origins": origins}, etagHeader(installed), 200)
	return digestOf(module)
}

// certifyPluginProvider certifies a plugin provider's declared model for
// unary and streaming chat, and activates it.
func certifyPluginProvider(t *testing.T, h *accessHarness, owner *browser, path string) {
	t.Helper()
	detail := h.want(owner, "GET", path, nil, nil, 200)
	if probe := h.want(owner, "POST", path+"/probe", nil, etagHeader(detail), 200); probe["succeeded"] != true {
		t.Fatalf("plugin provider probe: %v", probe)
	}
	models := h.want(owner, "GET", path+"/models", nil, nil, 200)["items"].([]any)
	modelID := models[0].(map[string]any)["id"].(string)
	capabilities := []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"}}
	detail = h.want(owner, "PATCH", path+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": capabilities}, etagHeader(detail), 200)
	if certified := h.want(owner, "POST", path+"/models/"+modelID+"/certify", nil, etagHeader(detail), 200); certified["status"] != "certified" {
		t.Fatalf("plugin provider certification: %v", certified)
	}
	detail = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(detail, idem(uuid.NewString())), 200)
}

// An operator creates a provider from an installed plugin's profile and a
// strict route serves through it: the profile's hosting adaptation places the
// static credential in the headers it declares, over OLP's own transport.
func TestPluginProfileWithAStaticCredentialServesAStrictRoute(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	upstream := newPluginUpstream(t, pluginCredential, "mounted-static-secret")

	kinds := h.want(owner, "GET", "/api/v1/provider-kinds", nil, nil, 200)["items"].([]any)
	plugin := kinds[slices.IndexFunc(kinds, func(k any) bool { return k.(map[string]any)["kind"] == "plugin" })].(map[string]any)
	// No endpoint field: the profile's address is the endpoint. With no upstream
	// discovery, the operator names a model to probe.
	if fields := plugin["fields"].([]any); plugin["default_auth_mode"] != "static_credential" || len(fields) != 1 ||
		fields[0].(map[string]any)["field"] != "model" || fields[0].(map[string]any)["required"] != true {
		t.Fatalf("plugin kind %v", plugin)
	}

	// A plugin awaiting approval offers no profile and can't be pinned.
	module := testutil.BuildPlugin(t, "./sdk/plugin/reference", "-X=main.upstream="+upstream.URL+"/v1")
	digest := digestOf(module)
	installed := h.want(owner, "POST", "/api/v1/plugins", module, wasm, 201)
	create := map[string]any{"name": "Reference", "credential": pluginCredential, "model": vendorModel,
		"configuration": map[string]any{"kind": "plugin", "auth_mode": "static_credential", "profile_id": "reference-chat", "profile_revision": digest}}
	if refusal := h.want(owner, "POST", "/api/v1/providers", create, idem(uuid.NewString()), 422); problemCode(t, refusal) != "plugin_not_approved" || refusal["errors"].(map[string]any)["configuration.profile_revision"] == nil {
		t.Fatalf("pinned an unapproved plugin: %v", refusal)
	}
	for _, profile := range h.want(owner, "GET", "/api/v1/provider-profiles", nil, nil, 200)["items"].([]any) {
		if profile.(map[string]any)["kind"] == "plugin" {
			t.Fatalf("the catalogue offers an unapproved plugin's profile: %v", profile)
		}
	}
	h.want(owner, "POST", "/api/v1/plugins/"+digest+"/approve", map[string]any{"origins": installed["manifest"].(map[string]any)["origins"]}, etagHeader(installed), 200)
	var catalogued map[string]any
	for _, profile := range h.want(owner, "GET", "/api/v1/provider-profiles", nil, nil, 200)["items"].([]any) {
		if profile.(map[string]any)["id"] == "reference-chat" {
			catalogued = profile.(map[string]any)
		}
	}
	if catalogued["id"] != "reference-chat" || catalogued["revision"] != digest || catalogued["dialect"] != "openai-chat" || catalogued["strict"] != true ||
		catalogued["plugin"].(map[string]any)["digest"] != digest || catalogued["plugin"].(map[string]any)["version"] != "0.1.0" {
		t.Fatalf("catalogued %v", catalogued)
	}

	unknown := map[string]any{"name": "Unknown", "credential": pluginCredential,
		"configuration": map[string]any{"kind": "plugin", "auth_mode": "static_credential", "profile_id": "reference-other", "profile_revision": digest}}
	if refusal := h.want(owner, "POST", "/api/v1/providers", unknown, idem(uuid.NewString()), 422); problemCode(t, refusal) != "plugin_profile_unknown" {
		t.Fatalf("pinned an undeclared profile: %v", refusal)
	}
	created := h.want(owner, "POST", "/api/v1/providers", create, idem(uuid.NewString()), 201)
	path := "/api/v1/providers/" + created["id"].(string)
	configuration := h.want(owner, "GET", path, nil, nil, 200)["configuration"].(map[string]any)
	if configuration["endpoint"] != upstream.URL+"/v1" || configuration["options"].(map[string]any)["vendor_id"] != nil {
		t.Fatalf("the provider did not take its profile's address alone: %v", configuration)
	}
	// Plugin providers have no upstream discovery: the declared model is
	// certified instead.
	certifyPluginProvider(t, h, owner, path)

	draft := fidelityDraft("reference-strict", created["id"])
	draft["fidelity"] = map[string]any{"mode": "strict"}
	route := h.want(owner, "POST", "/api/v1/route-drafts", draft, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+route["id"].(string)+"/activate", nil, withMatch(route, idem(uuid.NewString())), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Reference", "scopes": []string{"inference"}, "allowed_routes": []string{"reference-strict"}}, idem(uuid.NewString()), 201)["secret"].(string)
	h.refresh()

	before := len(upstream.received())
	status, reply, _ := h.gateway("POST", "/v1/chat/completions", key, map[string]any{"model": "reference-strict", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if status != 200 || !strings.Contains(fmt.Sprint(reply), "Hello from the reference upstream") {
		t.Fatalf("strict unary through the plugin profile: %d %v", status, reply)
	}
	streamed := map[string]any{"model": "reference-strict", "stream": true, "stream_options": map[string]any{"include_usage": true}, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	raw, _ := json.Marshal(streamed)
	if code, body, _ := h.gatewayRaw("POST", "/v1/chat/completions", key, bytes.NewReader(raw), map[string]string{"Content-Type": "application/json"}); code != 200 || !bytes.Contains(body, []byte("data: [DONE]")) {
		t.Fatalf("strict stream through the plugin profile: %d %s", code, body)
	}
	served := upstream.received()[before:]
	if len(served) != 2 {
		t.Fatalf("the upstream received %d requests", len(served))
	}
	for _, headers := range served {
		if headers.Get("Authorization") != "Token "+pluginCredential || headers.Get("X-Reference-Client") != "olp" {
			t.Fatalf("the upstream received %v", headers)
		}
	}

	// The published snapshot carries the pinned digest and the profile's
	// hosting adaptation, so gateways need nothing else.
	var snapshot []byte
	if err := h.Pool.QueryRow(t.Context(), "SELECT snapshot FROM olp.runtime_releases ORDER BY sequence DESC LIMIT 1").Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	var release struct {
		Providers map[string]struct {
			ProfileRevision string `json:"profile_revision"`
			Plugin          struct {
				Plugin struct {
					Digest string `json:"digest"`
				} `json:"plugin"`
			} `json:"plugin"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(snapshot, &release); err != nil {
		t.Fatal(err)
	}
	if published := release.Providers[created["id"].(string)]; published.ProfileRevision != digest || published.Plugin.Plugin.Digest != digest {
		t.Fatalf("the snapshot pins %+v", published)
	}

	current := h.want(owner, "GET", "/api/v1/plugins/"+digest, nil, nil, 200)
	refusal := h.want(owner, "DELETE", "/api/v1/plugins/"+digest, nil, etagHeader(current), 409)
	if problemCode(t, refusal) != "plugin_pinned" || !strings.Contains(refusal["detail"].(string), `"Reference"`) {
		t.Fatalf("uninstalled a pinned plugin: %v", refusal)
	}

	t.Run("a mounted gateway mounts the static credential", func(t *testing.T) {
		directory := t.TempDir()
		credentialFile := filepath.Join(directory, "credential")
		if err := os.WriteFile(credentialFile, []byte("mounted-static-secret"), 0600); err != nil {
			t.Fatal(err)
		}
		document, _ := json.Marshal(map[string]any{"providers": []any{map[string]any{"provider_id": created["id"], "credential_file": credentialFile, "configuration": configuration}}})
		mountedFile := filepath.Join(directory, "connectors.json")
		if err := os.WriteFile(mountedFile, document, 0600); err != nil {
			t.Fatal(err)
		}
		policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
		mounted, err := providers.LoadMounted(mountedFile, &policy)
		if err != nil {
			t.Fatal(err)
		}
		installation, err := database.Installation(t.Context(), h.Pool)
		if err != nil {
			t.Fatal(err)
		}
		auth, err := secrets.DecodeKey(h.AuthHex)
		if err != nil {
			t.Fatal(err)
		}
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		manager := runtime.NewManager(h.Pool, installation, secrets.NewAuthKey(auth, installation), nil, log)
		manager.Mounted = mounted
		if err = manager.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		serving := gateway.New(manager, &policy, gateway.Config{MaxInFlight: 4, MaxBodyBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxEventBytes: 65536}, log)
		mux := http.NewServeMux()
		serving.Register(mux)
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		request, _ := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"reference-strict","messages":[{"role":"user","content":"hi"}]}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+key)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		content, _ := io.ReadAll(response.Body)
		last := upstream.received()[len(upstream.received())-1]
		if response.StatusCode != 200 || last.Get("Authorization") != "Token mounted-static-secret" {
			t.Fatalf("mounted dispatch: %d %s with %v", response.StatusCode, content, last)
		}
	})
}

// Moving a provider to another installed build of its plugin is an ordinary
// new revision, which the revision diff shows; every revision keeps pinning
// its digest.
func TestSwitchingPluginDigestsIsANewProviderRevision(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	upstream := newPluginUpstream(t, pluginCredential)
	first := installReferencePlugin(t, h, owner, upstream, "0.1.0")
	second := installReferencePlugin(t, h, owner, upstream, "0.2.0")
	create := map[string]any{"name": "Reference", "credential": pluginCredential, "model": vendorModel,
		"configuration": map[string]any{"kind": "plugin", "auth_mode": "static_credential", "profile_id": "reference-chat", "profile_revision": first}}
	created := h.want(owner, "POST", "/api/v1/providers", create, idem(uuid.NewString()), 201)
	path := "/api/v1/providers/" + created["id"].(string)
	certifyPluginProvider(t, h, owner, path)

	detail := h.want(owner, "GET", path, nil, nil, 200)
	moved := detail["configuration"].(map[string]any)
	moved["profile_revision"] = second
	detail = h.want(owner, "PATCH", path, map[string]any{"name": "Reference", "configuration": moved}, etagHeader(detail), 200)
	if detail["configuration"].(map[string]any)["profile_revision"] != second {
		t.Fatalf("the draft pins %v", detail["configuration"])
	}
	certifyPluginProvider(t, h, owner, path)
	diff := h.want(owner, "GET", path+"/revisions/diff?from=1&to=2", nil, nil, 200)
	if diff["plugin_changed"] != true || diff["profile_changed"] != true || diff["endpoint_changed"] != false || diff["connector_changed"] != false {
		t.Fatalf("revision diff %v", diff)
	}
	if same := h.want(owner, "GET", path+"/revisions/diff?from=2&to=2", nil, nil, 200); same["plugin_changed"] != false {
		t.Fatalf("a revision differs from itself: %v", same)
	}
	// The superseded revision still pins the first build.
	current := h.want(owner, "GET", "/api/v1/plugins/"+first, nil, nil, 200)
	if refusal := h.want(owner, "DELETE", "/api/v1/plugins/"+first, nil, etagHeader(current), 409); problemCode(t, refusal) != "plugin_pinned" {
		t.Fatalf("uninstalled a plugin a published revision pins: %v", refusal)
	}
}

// No list price applies to a plugin provider: its attempts are unpriced until
// an operator sets a price scoped to that provider, which then applies.
func TestPluginProviderAttemptsAreUnpricedUntilTheOperatorPricesThem(t *testing.T) {
	t.Parallel()
	fixture := acctSeed(t, acctPool(t))
	provider, _ := acctProvider(t, fixture, "plugin",
		`{"kind":"plugin","auth_mode":"static_credential","endpoint":"https://127.0.0.1:9/v1","profile_id":"reference-chat","profile_revision":"`+strings.Repeat("ab", 32)+`","options":{}}`)
	observed := time.Now().UTC().Add(-time.Minute)
	acctPricing(t, fixture, 1, observed.Add(-2*time.Hour),
		acctPrice{Kind: "openai", VendorID: acctPtr("openai"), Model: vendorModel, Operation: "generation", Input: acctPtr("1"), Output: acctPtr("1")},
		acctPrice{Kind: "openai", Model: vendorModel, Operation: "generation", Input: acctPtr("1"), Output: acctPtr("1")})
	attempt := func() acctFact {
		event := acctEvent(t, fixture, acctEventOptions{ObservedAt: observed, Attempts: []usage.Attempt{
			acctAttempt(t, provider, 1, vendorModel, 200, acctObserved(1000, 1000, nil, nil)),
		}})
		acctPersist(t, fixture, event)
		return acctLoadFact(t, fixture, event.RequestID, 1)
	}
	if fact := attempt(); fact.Cost != nil || !fact.Unpriced {
		t.Fatalf("a list price applied to a plugin provider: %+v", fact)
	}
	if _, err := fixture.Pool.Exec(t.Context(), `INSERT INTO olp.prices (pricing_revision_id, provider_kind, model, operation, input_per_million, currency)
		SELECT id, 'plugin', $1, 'generation', 1, 'USD' FROM olp.pricing_revisions WHERE revision=1`, vendorModel); err == nil {
		t.Fatal("stored a plugin price for every plugin provider")
	}
	acctPricing(t, fixture, 2, observed.Add(-time.Hour),
		acctPrice{Kind: "plugin", ProviderID: acctPtr(provider), Model: vendorModel, Operation: "generation", Input: acctPtr("2"), Output: acctPtr("4")})
	fact := attempt()
	acctSameMoney(t, fixture, fact.Cost, "0.006")
	if fact.Unpriced {
		t.Fatalf("the provider-scoped price did not apply: %+v", fact)
	}
}
