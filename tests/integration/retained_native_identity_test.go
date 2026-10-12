//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/usage"
)

func retainedNativeState(t *testing.T, h *accessHarness, local string) string {
	t.Helper()
	var snapshot string
	err := h.Pool.QueryRow(t.Context(), `SELECT jsonb_build_object('upstream_id',upstream_id,'state',state,
 'metadata',metadata,'expires_at',expires_at,'updated_at',updated_at)::text
	 FROM olp.provider_resources WHERE id=$1`, local[strings.LastIndexByte(local, '_')+1:]).Scan(&snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func ambiguousNativeIDs(body, id string) map[string]string {
	field := `"id":"` + id + `"`
	return map[string]string{
		"duplicate":          strings.Replace(body, field, `"id":"another-native-resource",`+field, 1),
		"escaped-duplicate":  strings.Replace(body, field, `"id":"another-native-resource","\u0069d":"`+id+`"`, 1),
		"unpaired-surrogate": strings.Replace(body, field, `"id":"native-\ud800"`, 1),
		"invalid-utf8":       strings.Replace(body, field, "\"id\":\"native-\xff\"", 1),
	}
}

func assertNoNativeMapping(t *testing.T, h *accessHarness, slug, kind string) {
	t.Helper()
	var count int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.provider_resources WHERE route_slug=$1 AND kind=$2`, slug, kind).Scan(&count); err != nil || count != 0 {
		t.Fatalf("malformed native identity created authority: kind=%s count=%d %v", kind, count, err)
	}
}

func assertRetainedPendingUsage(t *testing.T, h *accessHarness, local, original string, input, output, cached int64) {
	t.Helper()
	var raw []byte
	if err := h.Pool.QueryRow(t.Context(), `SELECT metadata->'pending_usage' FROM olp.provider_resources WHERE id=$1`, local[strings.LastIndexByte(local, '_')+1:]).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	event, err := usage.Decode(raw)
	if err != nil || event.RequestID != original || len(event.Attempts) != 1 {
		t.Fatalf("pending original observation: %+v %v", event, err)
	}
	u := event.Attempts[0].Usage
	if u == nil || !u.Observed || u.Complete || !u.BillingUncertain || u.InputTokens == nil || *u.InputTokens != input || u.OutputTokens == nil || *u.OutputTokens != output || u.CachedInputTokens == nil || *u.CachedInputTokens != cached {
		t.Fatalf("lost provisional native snapshot: %+v", u)
	}
	var facts int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.attempt_usage_facts WHERE request_id=$1`, original).Scan(&facts); err != nil || facts != 0 {
		t.Fatalf("provisional work settled: facts=%d %v", facts, err)
	}
}

func TestRetainedResponseRejectsUnboundUnaryIdentityBeforeAccounting(t *testing.T) {
	for _, fidelity := range []string{"strict", "transformed"} {
		t.Run(fidelity, func(t *testing.T) {
			fixture := newOpenAIFixture(t, "")
			h := newAccessHarness(t)
			owner, _, slug, _ := provisionOpenAIWith(t, h, fixture.URL,
				[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}, []string{"generation"},
				map[string]any{"fidelity": map[string]any{"mode": fidelity}},
				map[string]any{"profile_id": "azure-legacy-responses", "profile_revision": "1"})
			key := stateKey(t, h, owner, slug, true)
			h.refresh()
			fixture.resps["resp-up-1"]["status"] = "in_progress"
			fixture.resps["resp-up-1"]["usage"] = nil
			status, created, _ := h.gateway(http.MethodPost, "/v1/responses", key,
				map[string]any{"model": slug, "input": "retained identity", "store": true, "background": true})
			if status != http.StatusOK {
				t.Fatalf("create: %d %v", status, created)
			}
			local := created["id"].(string)
			var original string
			if err := h.Pool.QueryRow(t.Context(), `SELECT metadata->'pending_usage'->>'request_id' FROM olp.provider_resources WHERE id=$1`, local[strings.LastIndexByte(local, '_')+1:]).Scan(&original); err != nil || original == "" {
				t.Fatalf("background usage owner: %q %v", original, err)
			}
			fixture.respFetchRaw.Store(responseUsageCheckpoint("in_progress", 10, 4, 2))
			if status, raw, _ := h.gatewayRaw(http.MethodGet, "/v1/responses/"+local, key, nil, nil); status != http.StatusOK {
				t.Fatalf("partial observation: %d %s", status, raw)
			}
			before := retainedNativeState(t, h, local)
			assertRetainedPendingUsage(t, h, local, original, 10, 4, 2)
			sink := &captureSink{}
			h.Gateway.Sink = sink
			terminal := responseUsageCheckpoint("completed", 90, 80, 20)
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				path := "/v1/responses/" + local
				if method == http.MethodPost {
					path += "/cancel"
				}
				for name, body := range map[string]string{
					"wrong":             strings.Replace(terminal, `"id":"resp-up-1"`, `"id":"another-response"`, 1),
					"missing":           strings.Replace(terminal, `"id":"resp-up-1",`, "", 1),
					"null-id":           strings.Replace(terminal, `"id":"resp-up-1"`, `"id":null`, 1),
					"null-object":       `null`,
					"duplicate":         strings.Replace(terminal, `"id":"resp-up-1"`, `"id":"another-response","id":"resp-up-1"`, 1),
					"escaped-duplicate": strings.Replace(terminal, `"id":"resp-up-1"`, `"id":"another-response","\u0069d":"resp-up-1"`, 1),
				} {
					t.Run(method+"-"+name, func(t *testing.T) {
						if method == http.MethodGet {
							fixture.respFetchRaw.Store(body)
						} else {
							fixture.respCancelRaw.Store(body)
						}
						status, raw, _ := h.gatewayRaw(method, path, key, nil, nil)
						if status != http.StatusBadGateway {
							t.Fatalf("unbound native identity admitted: %d %s", status, raw)
						}
						if after := retainedNativeState(t, h, local); after != before {
							t.Fatalf("unbound observation changed retained state:\n before %s\n after %s", before, after)
						}
						assertRetainedPendingUsage(t, h, local, original, 10, 4, 2)
						if event := sink.last(); len(event.Attempts) != 1 || event.Attempts[0].Class != "protocol" || event.Committed {
							t.Fatalf("unbound identity became valid upstream completion: %+v", event)
						}
					})
				}
			}
			if fidelity == "transformed" {
				for _, statusField := range []string{"", `"status":null,`} {
					body := strings.Replace(responseUsageCheckpoint("in_progress", 11, 5, 2), `"status":"in_progress",`, statusField, 1)
					fixture.respFetchRaw.Store(body)
					if status, raw, _ := h.gatewayRaw(http.MethodGet, "/v1/responses/"+local, key, nil, nil); status != http.StatusOK {
						t.Fatalf("provisional native status refused: %d %s", status, raw)
					}
					assertRetainedPendingUsage(t, h, local, original, 11, 5, 2)
				}
			}
			if status, deleted, _ := h.gateway(http.MethodDelete, "/v1/responses/"+local, key, nil); status != http.StatusOK {
				t.Fatalf("cleanup retained response: %d %v", status, deleted)
			}
			if fidelity == "transformed" {
				assertGeminiUsageSnapshot(t, h, original, false, 11, 5, 2)
			} else {
				assertGeminiUsageSnapshot(t, h, original, false, 10, 4, 2)
			}
		})
	}
}

func TestRetainedFileAndBatchRejectUnboundUnaryIdentityBeforeMutation(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	_, _, slug, key := provisionOpenAI(t, h, fixture.URL,
		[]any{map[string]any{"operation": "batch", "surface": "openai", "mode": "unary"}}, []string{"batch"})
	status, file := h.uploadTestFile(slug, key, "retained-file-identity\n")
	if status != http.StatusOK {
		t.Fatalf("upload: %d %v", status, file)
	}
	localFile := file["id"].(string)
	status, batch, _ := h.gateway(http.MethodPost, "/v1/batches", key,
		map[string]any{"input_file_id": localFile, "endpoint": "/v1/chat/completions", "completion_window": "24h"})
	if status != http.StatusOK {
		t.Fatalf("batch create: %d %v", status, batch)
	}
	for _, resource := range []struct{ kind, collection, upstream, local string }{
		{"file", "files", "file-up-1", localFile}, {"batch", "batches", "batch-up-1", batch["id"].(string)},
	} {
		before := retainedNativeState(t, h, resource.local)
		sink := &captureSink{}
		h.Gateway.Sink = sink
		valid := `{"id":"` + resource.upstream + `","object":"` + resource.kind + `","status":"completed","bytes":900,"expires_at":2000000000}`
		for name, body := range map[string]string{
			"wrong":             strings.Replace(valid, resource.upstream, "another-native-resource", 1),
			"missing":           strings.Replace(valid, `"id":"`+resource.upstream+`",`, "", 1),
			"null-id":           strings.Replace(valid, `"id":"`+resource.upstream+`"`, `"id":null`, 1),
			"null-object":       `null`,
			"duplicate":         strings.Replace(valid, `"id":"`+resource.upstream+`"`, `"id":"another-native-resource","id":"`+resource.upstream+`"`, 1),
			"escaped-duplicate": strings.Replace(valid, `"id":"`+resource.upstream+`"`, `"id":"another-native-resource","\u0069d":"`+resource.upstream+`"`, 1),
		} {
			t.Run(resource.kind+"-"+name, func(t *testing.T) {
				if resource.kind == "file" {
					fixture.fileFetchRaw.Store(body)
				} else {
					fixture.batchFetchRaw.Store(body)
				}
				status, raw, _ := h.gatewayRaw(http.MethodGet, "/v1/"+resource.collection+"/"+resource.local, key, nil, nil)
				if status != http.StatusBadGateway {
					t.Fatalf("unbound native identity admitted: %d %s", status, raw)
				}
				if after := retainedNativeState(t, h, resource.local); after != before {
					t.Fatalf("unbound observation changed retained state:\n before %s\n after %s", before, after)
				}
				if event := sink.last(); len(event.Attempts) != 1 || event.Attempts[0].Class != "protocol" || event.Committed {
					t.Fatalf("unbound identity became valid upstream completion: %+v", event)
				}
			})
		}
	}
}

func TestStrictUnaryResponseUsageSurvivesLocalProjectionLimit(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	owner, provider, _, _ := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}, []string{"generation"}, nil,
		map[string]any{"profile_id": "azure-legacy-responses", "profile_revision": "1"})
	alias := "unary-" + strings.Repeat("x", 50) + uuid.NewString()[:8]
	draft := h.want(owner, http.MethodPost, "/api/v1/route-drafts", map[string]any{
		"slug": alias, "operations": []string{"generation"}, "overall_timeout_ms": 10000, "max_attempts": 1,
		"fidelity": map[string]any{"mode": "strict"},
		"targets":  []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}},
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, http.StatusCreated)
	h.want(owner, http.MethodPost, "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil,
		withMatch(draft, map[string]string{"Idempotency-Key": uuid.NewString()}), http.StatusOK)
	key := stateKey(t, h, owner, alias, true)
	h.refresh()
	fixture.resps["resp-up-1"]["status"] = "in_progress"
	fixture.resps["resp-up-1"]["usage"] = nil
	status, created, _ := h.gateway(http.MethodPost, "/v1/responses", key,
		map[string]any{"model": alias, "input": "projection accounting", "store": true, "background": true})
	if status != http.StatusOK {
		t.Fatalf("background create: %d %v", status, created)
	}
	local := created["id"].(string)
	var original string
	if err := h.Pool.QueryRow(t.Context(), `SELECT metadata->'pending_usage'->>'request_id' FROM olp.provider_resources WHERE id=$1`, local[strings.LastIndexByte(local, '_')+1:]).Scan(&original); err != nil || original == "" {
		t.Fatalf("background usage owner: %q %v", original, err)
	}
	fixture.respFetchRaw.Store(responseUsageCheckpoint("in_progress", 10, 4, 2))
	if status, raw, _ := h.gatewayRaw(http.MethodGet, "/v1/responses/"+local, key, nil, nil); status != http.StatusOK {
		t.Fatalf("partial observation: %d %s", status, raw)
	}
	// Native response bytes fit the actual harness cap; local ID/model bindings
	// alone expand the immutable document past that cap.
	const maxResponse = 1 << 20
	base := strings.TrimSuffix(responseUsageCheckpoint("completed", 17, 9, 3), "}") + `,"pad":""}`
	terminal := strings.Replace(base, `"pad":""`, `"pad":"`+strings.Repeat("x", maxResponse-1-len(base))+`"`, 1)
	if !json.Valid([]byte(terminal)) || len(terminal) >= maxResponse || len(terminal)+len(alias)-len(vendorModel) <= maxResponse {
		t.Fatal("fixture does not isolate local projection cap")
	}
	fixture.respFetchRaw.Store(terminal)
	sink := &captureSink{}
	h.Gateway.Sink = sink
	status, raw, _ := h.gatewayRaw(http.MethodGet, "/v1/responses/"+local, key, nil, nil)
	if status != http.StatusBadGateway || !bytes.Contains(raw, []byte(`"code":"fidelity_protocol_violation"`)) {
		t.Fatalf("oversized local projection accepted: %d %s", status, raw)
	}
	assertGeminiUsageSnapshot(t, h, original, true, 17, 9, 3)
	var state string
	var pending bool
	if err := h.Pool.QueryRow(t.Context(), `SELECT state,metadata ? 'pending_usage' FROM olp.provider_resources WHERE id=$1`, local[strings.LastIndexByte(local, '_')+1:]).Scan(&state, &pending); err != nil || state != "completed" || pending {
		t.Fatalf("native terminal did not settle original owner: state=%q pending=%v err=%v", state, pending, err)
	}
	if event := sink.last(); len(event.Attempts) != 1 || event.Attempts[0].Class != "success" || event.Committed {
		t.Fatalf("projection refusal changed valid upstream completion: %+v", event)
	}
}

func TestRetainedResponseDeleteAcceptsNullSuccessfulBody(t *testing.T) {
	for _, fidelity := range []string{"strict", "transformed"} {
		t.Run(fidelity, func(t *testing.T) {
			fixture := newOpenAIFixture(t, "")
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/responses/") {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte("null"))
					return
				}
				fixture.Config.Handler.ServeHTTP(w, r)
			}))
			t.Cleanup(upstream.Close)
			h := newAccessHarness(t)
			owner, _, slug, _ := provisionOpenAIWith(t, h, upstream.URL,
				[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}, []string{"generation"},
				map[string]any{"fidelity": map[string]any{"mode": fidelity}},
				map[string]any{"profile_id": "azure-legacy-responses", "profile_revision": "1"})
			key := stateKey(t, h, owner, slug, true)
			h.refresh()
			status, created, _ := h.gateway(http.MethodPost, "/v1/responses", key,
				map[string]any{"model": slug, "input": "delete retained result", "store": true})
			if status != http.StatusOK {
				t.Fatalf("create: %d %v", status, created)
			}
			local := created["id"].(string)
			sink := &captureSink{}
			h.Gateway.Sink = sink
			status, deleted, _ := h.gateway(http.MethodDelete, "/v1/responses/"+local, key, nil)
			if status != http.StatusOK || deleted["id"] != local || deleted["deleted"] != true {
				t.Fatalf("successful deletion lost its status proof: %d %v", status, deleted)
			}
			var state string
			if err := h.Pool.QueryRow(t.Context(), `SELECT state FROM olp.provider_resources WHERE id=$1`, local[strings.LastIndexByte(local, '_')+1:]).Scan(&state); err != nil || state != "deleted" {
				t.Fatalf("deletion did not tombstone retained resource: %q %v", state, err)
			}
			if event := sink.last(); len(event.Attempts) != 1 || event.Attempts[0].Class != "success" || !event.Committed {
				t.Fatalf("successful DELETE became an invalid native exchange: %+v", event)
			}
		})
	}
}

func TestFileAndBatchCreateRejectAmbiguousNativeIdentity(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	_, _, slug, key := provisionOpenAI(t, h, fixture.URL,
		[]any{map[string]any{"operation": "batch", "surface": "openai", "mode": "unary"}}, []string{"batch"})
	sink := &captureSink{}
	h.Gateway.Sink = sink
	fileResult := `{"id":"file-up-1","object":"file","status":"processed","bytes":8,"purpose":"batch","filename":"fixture.jsonl"}`
	for name, body := range ambiguousNativeIDs(fileResult, "file-up-1") {
		t.Run("file-"+name, func(t *testing.T) {
			fixture.fileCreateRaw.Store(body)
			status, result := h.uploadTestFile(slug, key, "native-identity\n")
			if status != http.StatusBadGateway {
				t.Fatalf("ambiguous native file create admitted: %d %v", status, result)
			}
			assertNoNativeMapping(t, h, slug, "file")
			if event := sink.last(); len(event.Attempts) != 1 || event.Attempts[0].Class != "protocol" || event.Attempts[0].Status != http.StatusOK || event.Committed {
				t.Fatalf("malformed accepted create became native completion: %+v", event)
			}
		})
	}
	fixture.fileCreateRaw.Store(fileResult)
	status, file := h.uploadTestFile(slug, key, "native-identity\n")
	if status != http.StatusOK {
		t.Fatalf("valid native file create: %d %v", status, file)
	}
	batchResult := `{"id":"batch-up-1","object":"batch","status":"validating","input_file_id":"file-up-1"}`
	for name, body := range ambiguousNativeIDs(batchResult, "batch-up-1") {
		t.Run("batch-"+name, func(t *testing.T) {
			fixture.batchCreateRaw.Store(body)
			status, result, _ := h.gateway(http.MethodPost, "/v1/batches", key,
				map[string]any{"input_file_id": file["id"], "endpoint": "/v1/chat/completions", "completion_window": "24h"})
			if status != http.StatusBadGateway {
				t.Fatalf("ambiguous native batch create admitted: %d %v", status, result)
			}
			assertNoNativeMapping(t, h, slug, "batch")
			if event := sink.last(); len(event.Attempts) != 1 || event.Attempts[0].Class != "protocol" || event.Attempts[0].Status != http.StatusOK || event.Committed {
				t.Fatalf("malformed accepted create became native completion: %+v", event)
			}
		})
	}
}

func TestResponseCreateRejectsAmbiguousNativeIdentityWithoutAuthority(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	var rawCreate atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/responses") {
			if raw := rawCreate.Load(); raw != nil {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(raw.(string)))
				return
			}
		}
		fixture.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	h := newAccessHarness(t)
	owner, _, slug, _ := provisionOpenAIWith(t, h, upstream.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}, []string{"generation"}, nil,
		map[string]any{"profile_id": "azure-legacy-responses", "profile_revision": "1"})
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	sink := &captureSink{}
	h.Gateway.Sink = sink
	for name, body := range ambiguousNativeIDs(responseUsageCheckpoint("completed", 17, 9, 3), "resp-up-1") {
		t.Run(name, func(t *testing.T) {
			rawCreate.Store(body)
			status, result, _ := h.gateway(http.MethodPost, "/v1/responses", key,
				map[string]any{"model": slug, "input": "ambiguous native identity", "store": true})
			if status != http.StatusBadGateway {
				t.Fatalf("ambiguous native response create admitted: %d %v", status, result)
			}
			assertNoNativeMapping(t, h, slug, "response")
			event := sink.last()
			if len(event.Attempts) != 1 || event.Attempts[0].Class != "protocol" || event.Attempts[0].Status != http.StatusOK || event.Committed {
				t.Fatalf("malformed accepted create became native completion: %+v", event)
			}
			if attempt := event.Attempts[0]; attempt.Usage != nil || attempt.UsageObserved || attempt.UsageComplete || !attempt.BillingUncertain {
				t.Fatalf("accepted malformed generation lost unknown consumption: %+v", attempt)
			}
		})
	}
}

func TestTransformedResponseStreamRejectsNativeIdentityDrift(t *testing.T) {
	fixture, endpoint := newResponseObservationFixture(t)
	h := newAccessHarness(t)
	sink := &captureSink{}
	h.Gateway.Sink = sink
	owner, _, slug, _ := provisionOpenAI(t, h, endpoint,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"}}, []string{"generation"})
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	partial := responseUsageCheckpoint("in_progress", 10, 4, 2)
	terminal := strings.Replace(responseUsageCheckpoint("completed", 90, 80, 20), `"id":"resp-up-1"`, `"id":"another-response"`, 1)
	fixture.respPostStream.Store("event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":" + partial + "}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":" + terminal + "}\n\n")
	status, raw, _ := h.gatewayRaw(http.MethodPost, "/v1/responses", key,
		strings.NewReader(`{"model":"`+slug+`","input":"response identity drift","store":true,"background":true,"stream":true}`),
		map[string]string{"Content-Type": "application/json"})
	if status != http.StatusOK || !bytes.Contains(raw, []byte(`"type":"response.created"`)) || bytes.Contains(raw, []byte(`"type":"response.completed"`)) || !bytes.Contains(raw, []byte(`"code":"provider_protocol_error"`)) {
		t.Fatalf("native ID drift admitted a terminal result: %d %s", status, raw)
	}
	fact := sink.last()
	if len(fact.Attempts) != 1 {
		t.Fatalf("original generation attempt missing: %+v", fact)
	}
	attempt := fact.Attempts[0]
	if attempt.Usage == nil || !attempt.UsageObserved || attempt.UsageComplete || !attempt.BillingUncertain ||
		attempt.Usage.InputTokens != 10 || attempt.Usage.OutputTokens != 4 || attempt.Usage.CachedInputTokens == nil || *attempt.Usage.CachedInputTokens != 2 {
		t.Fatalf("rejected native response replaced original attempt usage: usage=%+v attempt=%+v", attempt.Usage, attempt)
	}
	var local, original string
	if err := h.Pool.QueryRow(t.Context(), `SELECT id::text,metadata->'pending_usage'->>'request_id'
 FROM olp.provider_resources WHERE route_slug=$1 AND kind='response' AND upstream_id='resp-up-1'`, slug).Scan(&local, &original); err != nil {
		t.Fatal(err)
	}
	local = "response_" + strings.ReplaceAll(local, "-", "")
	assertRetainedPendingUsage(t, h, local, original, 10, 4, 2)
	var count int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.provider_resources WHERE route_slug=$1 AND kind='response'`, slug).Scan(&count); err != nil || count != 1 {
		t.Fatalf("one accepted invocation acquired multiple resource owners: count=%d %v", count, err)
	}
	if status, deleted, _ := h.gateway(http.MethodDelete, "/v1/responses/"+local, key, nil); status != http.StatusOK {
		t.Fatalf("cleanup original accepted work: %d %v", status, deleted)
	}
	assertGeminiUsageSnapshot(t, h, original, false, 10, 4, 2)
}

func TestTransformedUnaryResponseTerminalRequiresOutputArray(t *testing.T) {
	fixture, endpoint := newResponseObservationFixture(t)
	h := newAccessHarness(t)
	owner, _, slug, _ := provisionOpenAI(t, h, endpoint,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}, []string{"generation"})
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	fixture.resps["resp-up-1"]["status"] = "in_progress"
	fixture.resps["resp-up-1"]["usage"] = nil
	status, created, _ := h.gateway(http.MethodPost, "/v1/responses", key,
		map[string]any{"model": slug, "input": "terminal response grammar", "store": true, "background": true})
	if status != http.StatusOK {
		t.Fatalf("pending create: %d %v", status, created)
	}
	local := created["id"].(string)
	var original string
	if err := h.Pool.QueryRow(t.Context(), `SELECT metadata->'pending_usage'->>'request_id' FROM olp.provider_resources WHERE id=$1`, local[strings.LastIndexByte(local, '_')+1:]).Scan(&original); err != nil {
		t.Fatal(err)
	}
	fixture.respFetchRaw.Store(responseUsageCheckpoint("in_progress", 10, 4, 2))
	if status, raw, _ := h.gatewayRaw(http.MethodGet, "/v1/responses/"+local, key, nil, nil); status != http.StatusOK {
		t.Fatalf("provisional observation: %d %s", status, raw)
	}
	provisional := strings.Replace(responseUsageCheckpoint("in_progress", 10, 4, 2), `"output":[],`, "", 1)
	for name, body := range map[string]string{
		"missing-status": strings.Replace(provisional, `"status":"in_progress",`, "", 1),
		"null-status":    strings.Replace(provisional, `"status":"in_progress"`, `"status":null`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			fixture.respFetchRaw.Store(body)
			if status, raw, _ := h.gatewayRaw(http.MethodGet, "/v1/responses/"+local, key, nil, nil); status != http.StatusOK {
				t.Fatalf("unknown status lost its provisional semantics: %d %s", status, raw)
			}
			assertRetainedPendingUsage(t, h, local, original, 10, 4, 2)
		})
	}
	before := retainedNativeState(t, h, local)
	terminal := responseUsageCheckpoint("completed", 90, 80, 20)
	for name, body := range map[string]string{
		"missing": strings.Replace(terminal, `"output":[],`, "", 1),
		"null":    strings.Replace(terminal, `"output":[]`, `"output":null`, 1),
		"string":  strings.Replace(terminal, `"output":[]`, `"output":"invalid"`, 1),
		"object":  strings.Replace(terminal, `"output":[]`, `"output":{}`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			fixture.respFetchRaw.Store(body)
			if status, raw, _ := h.gatewayRaw(http.MethodGet, "/v1/responses/"+local, key, nil, nil); status != http.StatusBadGateway {
				t.Fatalf("malformed terminal became completion: %d %s", status, raw)
			}
			if after := retainedNativeState(t, h, local); after != before {
				t.Fatalf("malformed terminal changed retained owner:\nbefore %s\nafter %s", before, after)
			}
			assertRetainedPendingUsage(t, h, local, original, 10, 4, 2)
		})
	}
	fixture.respFetchRaw.Store(responseUsageCheckpoint("completed", 17, 9, 3))
	if status, raw, _ := h.gatewayRaw(http.MethodGet, "/v1/responses/"+local, key, nil, nil); status != http.StatusOK {
		t.Fatalf("valid terminal refused: %d %s", status, raw)
	}
	assertGeminiUsageSnapshot(t, h, original, true, 17, 9, 3)
}
