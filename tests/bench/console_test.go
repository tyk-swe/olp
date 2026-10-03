//go:build bench

package bench_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// console is a browser-less management client: it signs in as the first owner
// and provisions an installation through the same API the console uses, so a
// scenario's gateway serves a configuration that was built the way an
// operator builds one.
//
// The session cookies are Secure and __Host- prefixed, which a cookie jar
// refuses to send over the plain HTTP the benchmark listens on, so they are
// attached by hand, as the integration harness does.
type console struct {
	t       testing.TB
	base    string
	origin  string
	client  *http.Client
	cookies map[string]*http.Cookie
	csrf    string
}

const (
	benchPassword = "a long benchmark password"
	// benchOrigin is the browser origin the process is configured with. The
	// management API refuses a mutation from any other.
	benchOrigin = "https://olp.bench.test"
)

func newConsole(t testing.TB, base string) *console {
	return &console{t: t, base: base, origin: benchOrigin, cookies: map[string]*http.Cookie{},
		client: &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// do performs one request and returns the status, the body and the headers.
func (c *console) do(method, path string, body any, headers map[string]string) (int, []byte, http.Header) {
	c.t.Helper()
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			c.t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(payload))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Origin", c.origin)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Chrome bench-provisioning")
	if c.csrf != "" {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	for _, cookie := range c.cookies {
		req.AddCookie(cookie)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	for _, cookie := range resp.Cookies() {
		if cookie.MaxAge < 0 {
			delete(c.cookies, cookie.Name)
		} else {
			c.cookies[cookie.Name] = cookie
		}
	}
	if token := resp.Header.Get("X-CSRF-Token"); token != "" {
		c.csrf = token
	}
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if json.Unmarshal(raw, &session) == nil && session.CSRF != "" {
		c.csrf = session.CSRF
	}
	return resp.StatusCode, raw, resp.Header
}

// want performs a request that must return status, and decodes its object.
func (c *console) want(method, path string, body any, headers map[string]string, status int) map[string]any {
	c.t.Helper()
	got, raw, _ := c.do(method, path, body, headers)
	if got != status {
		c.t.Fatalf("%s %s: status %d, want %d: %s", method, strings.Split(path, "?")[0], got, status, raw)
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			c.t.Fatalf("%s %s: the body is not a JSON object: %v: %s", method, path, err, raw)
		}
	}
	return out
}

// mutate is want for a request that changes state: it carries a fresh
// idempotency key and, when current is given, the entity tag of the record
// being changed.
func (c *console) mutate(method, path string, body any, current map[string]any, status int) map[string]any {
	c.t.Helper()
	headers := map[string]string{"Idempotency-Key": uuid.NewString()}
	if current != nil {
		headers["If-Match"] = `"` + c.text(current, "etag") + `"`
	}
	return c.want(method, path, body, headers, status)
}

func (c *console) get(path string) map[string]any {
	c.t.Helper()
	return c.want(http.MethodGet, path, nil, nil, http.StatusOK)
}

// text reads a string field, failing the test when the API's shape is not what
// the harness was written against.
func (c *console) text(record map[string]any, field string) string {
	c.t.Helper()
	s, ok := record[field].(string)
	if !ok || s == "" {
		c.t.Fatalf("the response has no %q string: %v", field, record)
	}
	return s
}

// signIn creates the installation's owner with the bootstrap token.
func (c *console) signIn(bootstrapToken string) {
	c.t.Helper()
	c.want(http.MethodPost, "/api/v1/setup", map[string]any{
		"email": "bench@example.com", "display_name": "Benchmark", "password": benchPassword, "installation_name": "Benchmark",
	}, map[string]string{"X-OLP-Setup-Token": bootstrapToken}, http.StatusCreated)
}

// networkTuning is a provider's connection pool, which an operator sizes for
// the traffic the provider carries. A provider that sets any network option, as
// every benchmark provider does, opens at most 64 connections to its host unless
// told otherwise, keeps 16 of them idle, and accepts at most 4,096 of either (a
// provider that sets none shares the gateway's transport, which caps none): a
// gateway serving a thousand streams at once needs the limits raised, and more
// than 4,096 needs more than one provider.
type networkTuning struct {
	MaxIdleConns        int `json:"max_idle_conns"`
	MaxIdleConnsPerHost int `json:"max_idle_conns_per_host"`
	MaxConnsPerHost     int `json:"max_conns_per_host"`
}

// providerRecord is an activated provider and its one model.
type providerRecord struct {
	ID, Model string
}

// providerKind is the kind of every provider the benchmark creates: an OpenAI
// provider whose endpoint is the mock, which stands in for OpenAI. Only the
// OpenAI kind can be declared for the Anthropic and Gemini surfaces, which
// S5's translation needs; an OpenAI-compatible provider serves the OpenAI
// surface alone.
const providerKind = "openai"

// addProvider creates a provider in front of endpoint, certifies its model
// against the upstream and activates it. A model starts with the capabilities
// of the OpenAI surface; surfaces names every surface a client will call it
// through, which a model serving another dialect's clients must declare and
// have certified, since the gateway proves a translation against the upstream
// before it routes through one.
func (c *console) addProvider(name, endpoint, model string, tuning networkTuning, surfaces []string) providerRecord {
	c.t.Helper()
	configuration := map[string]any{"kind": providerKind, "auth_mode": "api_key", "endpoint": endpoint,
		"options": map[string]any{"network": tuning}}
	created := c.mutate(http.MethodPost, "/api/v1/providers", map[string]any{
		"name": name, "model": model, "credential": "bench-upstream-secret", "configuration": configuration,
	}, nil, http.StatusCreated)
	id := c.text(created, "id")
	path := "/api/v1/providers/" + id
	if probe := c.mutate(http.MethodPost, path+"/probe", nil, created, http.StatusOK); probe["succeeded"] != true {
		c.t.Fatalf("the upstream failed its probe: %v", probe)
	}
	var modelID string
	for _, item := range c.get(path + "/models")["items"].([]any) {
		if m := item.(map[string]any); m["upstream_model"] == model {
			modelID = c.text(m, "id")
		}
	}
	if modelID == "" {
		c.t.Fatalf("provider %s has no model %q after creation", name, model)
	}
	if len(surfaces) > 0 {
		var capabilities []any
		for _, surface := range surfaces {
			for _, mode := range []string{"unary", "streaming"} {
				capabilities = append(capabilities, map[string]any{"operation": "generation", "surface": surface, "mode": mode})
			}
		}
		c.mutate(http.MethodPatch, path+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": capabilities}, c.get(path), http.StatusOK)
	}
	if certified := c.mutate(http.MethodPost, path+"/models/"+modelID+"/certify", nil, c.get(path), http.StatusOK); certified["status"] != "certified" {
		c.t.Fatalf("certification of %s failed: %v", model, certified)
	}
	c.mutate(http.MethodPost, path+"/activate", nil, c.get(path), http.StatusOK)
	return providerRecord{ID: id, Model: model}
}

// routeTarget is one target of a route; targets are tried by priority.
type routeTarget struct {
	Provider providerRecord
	Priority int
	Timeout  time.Duration
}

// addRoute publishes a route that may translate between dialects.
func (c *console) addRoute(slug string, targets []routeTarget, maxAttempts int, overall time.Duration) {
	c.t.Helper()
	var requested []any
	for _, target := range targets {
		requested = append(requested, map[string]any{"provider_id": target.Provider.ID, "provider_model": target.Provider.Model,
			"priority": target.Priority, "weight": 1, "timeout_ms": target.Timeout.Milliseconds()})
	}
	draft := c.mutate(http.MethodPost, "/api/v1/route-drafts", map[string]any{
		"slug": slug, "overall_timeout_ms": overall.Milliseconds(), "max_attempts": maxAttempts,
		"fidelity": map[string]any{"mode": "transformed"}, "targets": requested,
	}, nil, http.StatusCreated)
	path := "/api/v1/route-drafts/" + c.text(draft, "id")
	validated := c.mutate(http.MethodPost, path+"/validate", nil, draft, http.StatusOK)
	if validated["state"] != "validated" {
		c.t.Fatalf("route %s did not validate: %v", slug, validated)
	}
	c.mutate(http.MethodPost, path+"/activate", nil, validated, http.StatusOK)
}

// addPrice prices a provider's model, in dollars per million tokens.
func (c *console) addPrice(provider providerRecord, input, output string) {
	c.t.Helper()
	price := map[string]any{"provider_kind": providerKind, "provider_id": provider.ID, "model": provider.Model,
		"operation": "generation", "currency": "USD", "input_per_million": input, "output_per_million": output}
	c.mutate(http.MethodPost, "/api/v1/pricing/revisions", map[string]any{
		"effective_at": time.Now().UTC().Format(time.RFC3339Nano), "prices": []any{price},
	}, nil, http.StatusCreated)
}

// keyRecord is a minted API key. The secret is shown once, at creation.
type keyRecord struct {
	ID, Secret string
}

// addKey mints an inference key restricted to one route. A budget is a daily
// and monthly cost limit in dollars, left unset for a key without one.
func (c *console) addKey(name, route string, budget string) keyRecord {
	c.t.Helper()
	body := map[string]any{"name": name, "scopes": []string{"inference"}, "allowed_routes": []string{route}}
	if budget != "" {
		body["daily_cost_limit"], body["monthly_cost_limit"] = budget, budget
	}
	created := c.mutate(http.MethodPost, "/api/v1/api-keys", body, nil, http.StatusCreated)
	return keyRecord{ID: c.text(created, "id"), Secret: c.text(created, "secret")}
}
