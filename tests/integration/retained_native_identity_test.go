//go:build integration

package integration_test

import (
	"net/http"
	"strings"
	"testing"

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

// Native response bytes fit the actual harness cap; local ID/model bindings
// alone expand the immutable document past that cap.
