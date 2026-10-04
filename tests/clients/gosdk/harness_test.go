//go:build integration

// Package gosdk qualifies the official Go SDKs against the OLP gateway that
// tests/clients/run.sh starts in front of the scripted upstream. The harness
// contract, its environment variables and the recording API are documented in
// tests/clients/README.md.
//
// Every test asserts two things: what the SDK returned, and what the upstream
// recorded. An SDK that bypassed OLP would be refused by the upstream, which
// accepts only its own credential, and a gateway that mangled a request would
// show in the recorded body, path or model.
package gosdk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/openai/openai-go/v3"
	openaioption "github.com/openai/openai-go/v3/option"
	"google.golang.org/genai"
)

// harness is the gateway and upstream a suite runs against.
type harness struct {
	origin, apiKey, restrictedKey string
	openAIBase, anthropicBase     string
	geminiBase, upstream          string
	models, upstreamModels        map[string]string
	defaultReply, toolPrefix      string
}

// connect reads the harness contract and forgets the previous test's
// recording. A missing variable fails the test, as an explicitly selected suite
// must never pass without its harness.
func connect(t *testing.T) harness {
	t.Helper()
	get := func(name string) string {
		value := os.Getenv(name)
		if value == "" {
			t.Fatalf("%s is not set; run the suites through tests/clients/run.sh", name)
		}
		return value
	}
	h := harness{
		origin: get("OLP_CLIENTS_ORIGIN"), apiKey: get("OLP_CLIENTS_API_KEY"), restrictedKey: get("OLP_CLIENTS_RESTRICTED_API_KEY"),
		openAIBase: get("OLP_CLIENTS_OPENAI_BASE_URL"), anthropicBase: get("OLP_CLIENTS_ANTHROPIC_BASE_URL"),
		geminiBase: get("OLP_CLIENTS_GEMINI_BASE_URL"), upstream: get("OLP_CLIENTS_UPSTREAM_URL"),
		defaultReply: get("OLP_CLIENTS_DEFAULT_REPLY"), toolPrefix: get("OLP_CLIENTS_TOOL_RESULTS_PREFIX"),
		models: map[string]string{}, upstreamModels: map[string]string{},
	}
	for _, name := range []string{"OPENAI", "ANTHROPIC", "GEMINI", "OPENAI_STRICT", "ANTHROPIC_STRICT", "GEMINI_STRICT", "GEMINI_EMBED_STRICT"} {
		h.models[strings.ToLower(name)] = get("OLP_CLIENTS_MODEL_" + name)
	}
	for _, name := range []string{"OPENAI", "ANTHROPIC", "GEMINI"} {
		h.upstreamModels[strings.ToLower(name)] = get("OLP_CLIENTS_UPSTREAM_MODEL_" + name)
	}
	h.reset(t)
	return h
}

// afterTools is the final text of a tool loop that returned these results.
func (h harness) afterTools(results ...string) string {
	return h.toolPrefix + strings.Join(results, " | ")
}

// The directives that script the upstream from inside a prompt; the grammar is
// in tests/clientfixture/scripted/script.go.
func tool(name string, args map[string]any) string { return directive("tool", name, args) }

func also(name string, args map[string]any) string { return directive("also", name, args) }

func directive(keyword, name string, args map[string]any) string {
	encoded, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("[[olp:%s %s %s]]", keyword, name, encoded)
}

func think(text string) string {
	encoded, _ := json.Marshal(text)
	return "[[olp:think " + string(encoded) + "]]"
}

func fail(status int) string { return fmt.Sprintf("[[olp:fail %d]]", status) }

// record is one request the upstream received.
type record struct {
	Seq                    int               `json:"seq"`
	Dialect                string            `json:"dialect"`
	Method                 string            `json:"method"`
	Path                   string            `json:"path"`
	Query                  string            `json:"query"`
	Headers                map[string]string `json:"headers"`
	Model                  string            `json:"model"`
	Body                   json.RawMessage   `json:"body"`
	Authorized             bool              `json:"authorized"`
	Status                 int               `json:"status"`
	Script                 string            `json:"script"`
	Stream                 bool              `json:"stream"`
	LeakedClientCredential bool              `json:"leaked_client_credential"`
}

// json decodes the recorded body.
func (r record) json(t *testing.T) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatalf("recorded body of %s: %v: %s", r.Path, err, r.Body)
	}
	return body
}

func (h harness) reset(t *testing.T) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, h.upstream+"/__recorded", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("resetting the recording: status %d", resp.StatusCode)
	}
}

// recorded returns the upstream requests matching the filters, once none is
// still in flight, so a stream the SDK has finished reading is complete.
func (h harness) recorded(t *testing.T, filter url.Values) []record {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := http.Get(h.upstream + "/__recorded?" + filter.Encode())
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Requests []record `json:"requests"`
			InFlight int      `json:"in_flight"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if body.InFlight == 0 {
			return body.Requests
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d upstream requests are still in flight", body.InFlight)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// clean asserts that every upstream request carried the upstream credential and
// no caller credential, and answered with the status, and returns the requests.
func (h harness) clean(t *testing.T, requests []record, status int) []record {
	t.Helper()
	for _, r := range requests {
		if !r.Authorized || r.LeakedClientCredential || r.Status != status {
			t.Fatalf("upstream request %d to %s was not clean (want status %d): %+v", r.Seq, r.Path, status, r)
		}
		if r.Headers["x-olp-routing"] != "" {
			t.Fatalf("the routing preference reached the provider: %+v", r.Headers)
		}
	}
	return requests
}

// requests asserts the upstream saw exactly n clean successful requests.
func (h harness) requests(t *testing.T, n int) []record {
	t.Helper()
	requests := h.recorded(t, nil)
	if len(requests) != n {
		t.Fatalf("upstream received %d requests, want %d: %+v", len(requests), n, requests)
	}
	return h.clean(t, requests, http.StatusOK)
}

// onlyRequest asserts the upstream saw exactly one clean successful request.
func (h harness) onlyRequest(t *testing.T) record { return h.requests(t, 1)[0] }

// untouched asserts the gateway answered without reaching the upstream.
func (h harness) untouched(t *testing.T) {
	t.Helper()
	if requests := h.recorded(t, nil); len(requests) != 0 {
		t.Fatalf("the gateway should have refused the request itself; upstream saw %+v", requests)
	}
}

// refused asserts the upstream saw exactly one request, that the gateway
// forwarded it with the upstream credential, and that the upstream refused it
// with this status.
func (h harness) refused(t *testing.T, status int) record {
	t.Helper()
	requests := h.recorded(t, nil)
	if len(requests) != 1 {
		t.Fatalf("upstream received %d requests, want 1: %+v", len(requests), requests)
	}
	return h.clean(t, requests, status)[0]
}

// timeout bounds one SDK call. It does not derive from t.Context, which is
// cancelled before the cleanups run, and a cleanup may call an SDK.
func timeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// get walks a decoded JSON value: string keys into objects, int keys into
// arrays. It returns nil where the path does not exist.
func get(v any, keys ...any) any {
	for _, k := range keys {
		switch key := k.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[key]
		case int:
			s, ok := v.([]any)
			if !ok || key < 0 || key >= len(s) {
				return nil
			}
			v = s[key]
		}
	}
	return v
}

// expect fails the test unless the value at the path equals want. Numbers and
// strings compare after JSON decoding, so want is a string, a float64 or a bool.
func expect(t *testing.T, v any, want any, keys ...any) {
	t.Helper()
	if got := get(v, keys...); got != want {
		t.Fatalf("%v is %#v, want %#v", keys, got, want)
	}
}

// A request to anywhere but the gateway fails the test instead of leaving the
// machine, whichever way the SDK sends it. The proxy settings of run.sh refuse
// everything else as well. The Anthropic and Google SDKs send through the HTTP
// client they are given; the OpenAI SDK sends loopback requests through a
// transport of its own, so it is guarded by middleware.

// gatewayOnly is an HTTP transport that fails a request outside the gateway.
type gatewayOnly struct{ origin string }

func (g gatewayOnly) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := onlyOrigin(g.origin, req); err != nil {
		return nil, err
	}
	return http.DefaultTransport.RoundTrip(req)
}

func onlyOrigin(origin string, req *http.Request) error {
	if got := req.URL.Scheme + "://" + req.URL.Host; got != origin {
		return fmt.Errorf("the SDK attempted a request outside OLP: %s", got)
	}
	return nil
}

func (h harness) httpClient() *http.Client {
	return &http.Client{Transport: gatewayOnly{h.origin}}
}

// openAI returns an OpenAI client. The SDK sends credentials only over HTTPS,
// except to loopback when told, and retries are off so one call is one request.
func (h harness) openAI(opts ...openaioption.RequestOption) *openai.Client {
	base := []openaioption.RequestOption{
		openaioption.WithAPIKey(h.apiKey), openaioption.WithBaseURL(h.openAIBase),
		openaioption.WithMiddleware(func(req *http.Request, next openaioption.MiddlewareNext) (*http.Response, error) {
			if err := onlyOrigin(h.origin, req); err != nil {
				return nil, err
			}
			return next(req)
		}),
		openaioption.WithUnsafeAllowHTTP(), openaioption.WithMaxRetries(0), openaioption.WithHeader("X-OLP-Routing", routing),
	}
	client := openai.NewClient(append(base, opts...)...)
	return &client
}

// anthropic returns an Anthropic client; the SDK appends /v1 to the base URL.
func (h harness) anthropic(opts ...anthropicoption.RequestOption) *anthropic.Client {
	base := []anthropicoption.RequestOption{
		anthropicoption.WithAPIKey(h.apiKey), anthropicoption.WithBaseURL(h.anthropicBase),
		anthropicoption.WithHTTPClient(h.httpClient()), anthropicoption.WithMaxRetries(0), anthropicoption.WithHeader("X-OLP-Routing", routing),
	}
	client := anthropic.NewClient(append(base, opts...)...)
	return &client
}

// genai returns a Google Gen AI client for the Gemini Developer API. The SDK
// does not retry unless asked to, and appends the API version to the base URL.
func (h harness) genai(t *testing.T, apiKey string) *genai.Client {
	t.Helper()
	client, err := genai.NewClient(timeout(t), &genai.ClientConfig{
		APIKey: apiKey, Backend: genai.BackendGeminiAPI, HTTPClient: h.httpClient(),
		HTTPOptions: genai.HTTPOptions{BaseURL: h.geminiBase, APIVersion: "v1beta", Headers: http.Header{"X-OLP-Routing": {routing}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// routing is a request preference every client sends, as the SDK smoke suites
// do. The gateway reads it and must not forward it to the provider, which the
// recording checks on every request.
const routing = `{"strategy":"weighted"}`

// invalidKey is a well-formed key the gateway does not know.
const invalidKey = "olp_not-a-real-key"

// awaitServing returns once probe succeeds. An upstream rate limit parks the
// credential slot of a route for the Retry-After the upstream named, and the
// gateway answers 503 meanwhile, so a test that provoked one waits the slot out
// before the next test, or the next suite, uses the route.
func awaitServing(t *testing.T, probe func() error) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		err := probe()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the route is still unavailable after its Retry-After: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
