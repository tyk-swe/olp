//go:build integration

package integration_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/limits"
)

// rhLimiter is the Valkey limiter a harness's gateway admits through, with the
// namespace its windows live in. The harness has none of its own: without one a
// key's limits are not counted, and a response has no allowance to state, which
// would make a check that it states none say nothing.
type rhLimiter struct {
	client    *coordination.Client
	namespace string
}

// rhLimit puts a limiter in front of the harness's gateway.
func rhLimit(t *testing.T, h *accessHarness, label string) rhLimiter {
	t.Helper()
	c := limClient(t)
	namespace := limNamespace(t, c, label)
	h.Gateway.Admission = gateway.NewAdmission(limLimiter(t, c, namespace), func() limits.OutagePolicy { return limits.FailClosed }, slog.New(slog.DiscardHandler))
	return rhLimiter{c, namespace}
}

// counted is the requests the limiter has counted in a key's window.
func (l rhLimiter) counted(t *testing.T, secret string) int64 {
	t.Helper()
	rateKey, _ := limRateKeys(l.namespace, strings.SplitN(secret, "_", 3)[1])
	return limField(t, limHash(t, l.client, rateKey), "rpm")
}

// rhEnvelope waits for the terminal envelope of the request that has just been
// answered, which the gateway emits as it finishes the response.
func rhEnvelope(t *testing.T, sink *captureSink, before int) gateway.Envelope {
	t.Helper()
	glEventually(t, "the request's terminal envelope", func() bool { return sink.count() > before })
	return sink.last()
}

// rhWantServedBy holds the metadata headers of a response to the envelope the
// gateway recorded for it: the attempts it made, the route revision and the
// vendor of the attempt that served it. A request that made no attempt has no
// metadata. None of these fixtures has a price list, so none states a cost.
func rhWantServedBy(t *testing.T, name string, header http.Header, env gateway.Envelope) {
	t.Helper()
	if len(env.Attempts) == 0 {
		if got := rhMetadata(header); len(got) != 0 {
			t.Fatalf("%s: a request that made no attempt carried %v", name, got)
		}
		return
	}
	if env.RouteRevisionID == "" {
		t.Fatalf("%s: the envelope names no route revision: %+v", name, env)
	}
	last := env.Attempts[len(env.Attempts)-1]
	want := map[string]string{"X-Olp-Attempts": strconv.Itoa(len(env.Attempts)), "X-Olp-Route-Revision": env.RouteRevisionID}
	if last.VendorID != "" {
		want["X-Olp-Provider"] = last.VendorID
	}
	got := rhMetadata(header)
	if len(got) != len(want) {
		t.Fatalf("%s: metadata = %v, want %v", name, got, want)
	}
	for header, value := range want {
		if got[header] != value {
			t.Fatalf("%s: %s = %q, want %q", name, header, got[header], value)
		}
	}
}

// TestRetainedResourceResponsesCarryMetadata proves the calls that answer from a
// provider's retained resource, which commit their responses on paths of their own,
// state how they were served to a key that opts in, and say nothing to one that
// does not.
func TestRetainedResourceResponsesCarryMetadata(t *testing.T) {
	fixture := newOpenAIFixture(t, "file-content-marker\n")
	h := newAccessHarness(t)
	sink := &captureSink{}
	h.Gateway.Sink = sink
	owner, _, slug, _ := provisionOpenAI(t, h, fixture.URL,
		[]any{map[string]any{"operation": "batch", "surface": "openai", "mode": "unary"}},
		[]string{"batch"})
	created := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{
		"name": "metadata key", "scopes": []string{"inference"}, "allowed_routes": []string{slug},
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	keyPath, secret := "/api/v1/api-keys/"+created["id"].(string), created["secret"].(string)
	h.refresh()

	var buf bytes.Buffer
	form := multipart.NewWriter(&buf)
	_ = form.WriteField("purpose", "batch")
	part, _ := form.CreateFormFile("file", "input.jsonl")
	_, _ = part.Write([]byte("upload-content\n"))
	_ = form.Close()
	status, raw, header := h.gatewayRaw("POST", "/v1/files", secret, &buf, map[string]string{"Content-Type": form.FormDataContentType(), "X-OLP-Route": slug})
	if status != http.StatusOK {
		t.Fatalf("upload: %d %s", status, raw)
	}
	fileID := strings.Split(strings.SplitN(string(raw), `"id":"`, 2)[1], `"`)[0]
	calls := map[string]string{
		"get":     "/v1/files/" + fileID,
		"content": "/v1/files/" + fileID + "/content",
		// A list is answered from the gateway's own records: no attempt, no metadata.
		"list": "/v1/files",
	}

	// Without the opt-in a response says nothing of how it was served.
	if got := rhMetadata(header); len(got) != 0 {
		t.Fatalf("upload: a key that did not opt in saw %v", got)
	}
	for name, path := range calls {
		if status, body, header := h.gatewayRaw("GET", path, secret, nil, nil); status != http.StatusOK || len(rhMetadata(header)) != 0 {
			t.Fatalf("%s: %d %s %v", name, status, body, header)
		}
	}

	// The opt-in is the key's policy, and takes effect on the next refresh.
	detail := h.want(owner, "GET", keyPath, nil, nil, 200)
	h.want(owner, "PATCH", keyPath, map[string]any{"response_metadata": true}, etagHeader(detail), 200)
	h.refresh()
	for name, path := range calls {
		before := sink.count()
		status, body, header := h.gatewayRaw("GET", path, secret, nil, nil)
		if status != http.StatusOK {
			t.Fatalf("%s: %d %s", name, status, body)
		}
		env := rhEnvelope(t, sink, before)
		rhWantServedBy(t, name, header, env)
		if name != "list" && len(env.Attempts) != 1 {
			t.Fatalf("%s made %d attempts, want the one pinned attempt", name, len(env.Attempts))
		}
	}
}

// TestBedrockResponsesCarryMetadata does the same for Bedrock ingress, whose
// transformed routes are served by their own handler and whose strict ones by the
// canonical one, unary and streaming alike. The key is limited, which Bedrock's
// SDKs have no header to learn.
func TestBedrockResponsesCarryMetadata(t *testing.T) {
	for _, strict := range []bool{false, true} {
		t.Run("strict="+strconv.FormatBool(strict), func(t *testing.T) {
			fixture := newBedrockFixture(t)
			h := newAccessHarness(t)
			sink := &captureSink{}
			h.Gateway.Sink = sink
			limiter := rhLimit(t, h, "bedrock-metadata")
			owner, _, slug, plain := provisionBedrockContract(t, h, fixture.URL, "anthropic.claude-3-haiku-20240307-v1:0",
				[]any{
					map[string]any{"operation": "generation", "surface": "bedrock", "mode": "unary"},
					map[string]any{"operation": "generation", "surface": "bedrock", "mode": "streaming"},
				}, []string{"generation"}, strict)
			key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{
				"name": "metadata key", "scopes": []string{"inference"}, "allowed_routes": []string{slug}, "response_metadata": true,
				"requests_per_minute": 50, "tokens_per_minute": 1_000_000,
			}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
			opted := key["secret"].(string)
			h.refresh()
			// The requests are to be counted in one minute's window.
			limSettleInMinute(t, limiter.client, 15*time.Second)

			converse := func(secret, action string) (int, []byte, http.Header) {
				return h.gatewayRaw("POST", "/bedrock/model/"+slug+"/"+action, secret,
					strings.NewReader(`{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`),
					map[string]string{"Content-Type": "application/json"})
			}
			for n, action := range []string{"converse", "converse-stream"} {
				status, body, header := converse(plain, action)
				if status != http.StatusOK {
					t.Fatalf("%s: %d %s", action, status, body)
				}
				if got := rhMetadata(header); len(got) != 0 {
					t.Fatalf("%s: a key that did not opt in saw %v", action, got)
				}
				before := sink.count()
				status, body, header = converse(opted, action)
				if status != http.StatusOK {
					t.Fatalf("%s: %d %s", action, status, body)
				}
				rhWantServedBy(t, action, header, rhEnvelope(t, sink, before))
				if header.Get("X-Olp-Attempts") != "1" || header.Get("X-Olp-Provider") == "" {
					t.Fatalf("%s: headers %v", action, header)
				}
				// The limiter counted the request, so the key has an allowance that
				// a mis-resolved surface would state.
				if counted := limiter.counted(t, opted); counted != int64(n+1) {
					t.Fatalf("%s: the limiter counted %d requests of the limited key, want %d", action, counted, n+1)
				}
				if got := rhRateHeaderCount(header); got != 0 {
					t.Fatalf("%s: Bedrock clients read no rate-limit headers, and got %v", action, header)
				}
			}
		})
	}
}

// TestStrictNativeUnaryResponsesCarryMetadata does the same for a strict route's
// unary operations, which the gateway serves from compiled contracts and writes on
// a path of their own.
func TestStrictNativeUnaryResponsesCarryMetadata(t *testing.T) {
	h := newAccessHarness(t)
	sink := &captureSink{}
	h.Gateway.Sink = sink
	owner := h.owner()
	f := newOperationFixture(t, "/v1/embeddings", `{"object":"list","data":[{"index":0,"embedding":[0.25,-0.5]}],"model":"fixture-model","usage":{"total_tokens":3}}`)
	slug, plain := publishOperation(t, h, owner, f, "voyage-embeddings", "embeddings", "openai", nil)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{
		"name": "metadata key", "scopes": []string{"inference"}, "allowed_routes": []string{slug}, "response_metadata": true,
	}, idem(uuid.NewString()), 201)
	opted := key["secret"].(string)
	h.refresh()

	embed := func(secret string) (int, []byte, http.Header) {
		return h.gatewayRaw("POST", "/v1/embeddings", secret,
			strings.NewReader(fmt.Sprintf(`{"model":%q,"input":"native text","dimensions":2,"encoding_format":"float"}`, slug)),
			map[string]string{"Content-Type": "application/json"})
	}
	status, body, header := embed(plain)
	if status != http.StatusOK {
		t.Fatalf("embedding: %d %s", status, body)
	}
	if got := rhMetadata(header); len(got) != 0 {
		t.Fatalf("a key that did not opt in saw %v", got)
	}
	before := sink.count()
	status, body, header = embed(opted)
	if status != http.StatusOK {
		t.Fatalf("embedding: %d %s", status, body)
	}
	env := rhEnvelope(t, sink, before)
	rhWantServedBy(t, "strict embedding", header, env)
	if len(env.Attempts) != 1 || header.Get("X-Olp-Provider") == "" {
		t.Fatalf("headers %v for %+v", header, env.Attempts)
	}
}

// TestGeminiInteractionResponsesCarryMetadata does the same for Gemini
// Interactions: a create, a read, a resumed stream and a delete, each committed by
// a path of its own, and a delete the provider reports already done. Gemini's SDKs
// read no rate-limit headers, so none are sent, though the key is limited.
func TestGeminiInteractionResponsesCarryMetadata(t *testing.T) {
	h := newAccessHarness(t)
	sink := &captureSink{}
	h.Gateway.Sink = sink
	limiter := rhLimit(t, h, "gemini-interactions-metadata")
	provider := newGeminiLifecycleProvider(t)
	owner := h.owner()
	slug, plain, _ := provisionGeminiLifecycle(t, h, owner, "gemini-interactions", provider)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{
		"name": "metadata key", "scopes": []string{"inference"}, "allowed_routes": []string{slug},
		"allow_provider_state": true, "response_metadata": true,
		"requests_per_minute": 50, "tokens_per_minute": 1_000_000,
	}, idem(uuid.NewString()), 201)
	opted := key["secret"].(string)
	h.refresh()
	// The requests are to be counted in one minute's window.
	limSettleInMinute(t, limiter.client, 15*time.Second)
	const path = "/gemini/v1beta/interactions"
	create := []byte(fmt.Sprintf(`{"model":%q,"input":"hello","store":true}`, slug))

	response, raw := geminiPublic(t, h, http.MethodPost, path, plain, create)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("create: %d %s", response.StatusCode, raw)
	}
	if got := rhMetadata(response.Header); len(got) != 0 {
		t.Fatalf("a key that did not opt in saw %v", got)
	}

	// call makes one request as the opted-in key and holds the response to the
	// attempts the gateway recorded for it.
	counted := int64(0)
	call := func(name, method, path string, body []byte) string {
		t.Helper()
		before := sink.count()
		response, raw := geminiPublic(t, h, method, path, opted, body)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", name, response.StatusCode, raw)
		}
		env := rhEnvelope(t, sink, before)
		rhWantServedBy(t, name, response.Header, env)
		if len(env.Attempts) != 1 {
			t.Fatalf("%s made %d attempts, want the one pinned attempt", name, len(env.Attempts))
		}
		// The limiter counted the request, so the key has an allowance that a
		// mis-resolved surface would state.
		counted++
		if got := limiter.counted(t, opted); got != counted {
			t.Fatalf("%s: the limiter counted %d requests of the limited key, want %d", name, got, counted)
		}
		if got := rhRateHeaderCount(response.Header); got != 0 {
			t.Fatalf("%s: Gemini clients read no rate-limit headers, and got %v", name, response.Header)
		}
		return string(raw)
	}
	interaction := func() string {
		raw := call("create", http.MethodPost, path, create)
		return strings.Split(strings.SplitN(raw, `"id":"`, 2)[1], `"`)[0]
	}

	// The calls on one Interaction run in order: a delete tombstones it, so every
	// call that reads it comes first.
	id := interaction()
	call("read", http.MethodGet, path+"/"+id, nil)
	call("stream", http.MethodGet, path+"/"+id+"?stream=true&last_event_id=cursor-start", nil)
	call("delete", http.MethodDelete, path+"/"+id, nil)

	// A delete whose provider no longer has the Interaction, because an earlier
	// delete whose response was lost already removed it, still succeeds, and still
	// says it asked the provider.
	id = interaction()
	provider.deleteMissing.Store(true)
	call("delete of a deleted Interaction", http.MethodDelete, path+"/"+id, nil)
}

// TestStoredResponseStreamCarriesMetadataAndTheAllowance does the same for a stored
// response read as a stream, which commits its response on a path of its own: the
// first frame carries the rate-limit headers of the key's window and the metadata
// of the one pinned attempt, and no cost, which a stream cannot know.
func TestStoredResponseStreamCarriesMetadataAndTheAllowance(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	sink := &captureSink{}
	h.Gateway.Sink = sink
	limiter := rhLimit(t, h, "stored-response-stream")
	owner, _, slug, _ := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"}},
		[]string{"generation"}, nil, map[string]any{"profile_id": "azure-legacy-responses", "profile_revision": "1"})
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{
		"name": "metadata key", "scopes": []string{"inference"}, "allowed_routes": []string{slug},
		"allow_provider_state": true, "response_metadata": true,
		"requests_per_minute": 20, "tokens_per_minute": 1_000_000,
	}, idem(uuid.NewString()), 201)
	secret := key["secret"].(string)
	h.refresh()
	// Both requests are to be counted in one minute's window.
	limSettleInMinute(t, limiter.client, 15*time.Second)
	status, created, _ := h.gateway("POST", "/v1/responses", secret, map[string]any{"model": slug, "input": "hi", "store": true})
	if status != http.StatusOK {
		t.Fatalf("create response: %d %v", status, created)
	}
	local := created["id"].(string)

	before := sink.count()
	start := limServerTimeMS(t, limiter.client)
	status, raw, header := h.gatewayRaw("GET", "/v1/responses/"+local+"?stream=true", secret, nil, nil)
	end := limServerTimeMS(t, limiter.client)
	if status != http.StatusOK || !bytes.Contains(raw, []byte("response.completed")) {
		t.Fatalf("retained stream: %d %s", status, raw)
	}
	env := rhEnvelope(t, sink, before)
	rhWantServedBy(t, "stored response stream", header, env)
	if len(env.Attempts) != 1 {
		t.Fatalf("the stream made %d attempts, want the one pinned attempt", len(env.Attempts))
	}
	if counted := limiter.counted(t, secret); counted != 2 {
		t.Fatalf("the limiter counted %d requests, want the creation and the stream", counted)
	}
	limitRequests, remainingRequests, resetRequests, limitTokens, _, resetTokens := rhRateHeaders("openai")
	if got := rhInt(t, header, limitRequests); got != 20 {
		t.Fatalf("%s = %d, want 20 (headers %v)", limitRequests, got, header)
	}
	if got := rhInt(t, header, remainingRequests); got != 18 {
		t.Fatalf("%s = %d, want the 18 the window had left after both requests", remainingRequests, got)
	}
	if got := rhInt(t, header, limitTokens); got != 1_000_000 {
		t.Fatalf("%s = %d", limitTokens, got)
	}
	for _, name := range []string{resetRequests, resetTokens} {
		rhWantReset(t, "openai", name, header.Get(name), start, end)
	}
}
