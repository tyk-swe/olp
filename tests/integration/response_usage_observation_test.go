//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func responseUsageCheckpoint(status string, input, output, cached int64) string {
	return fmt.Sprintf(`{"id":"resp-up-1","object":"response","status":%q,"model":%q,"output":[],"usage":{"input_tokens":%d,"output_tokens":%d,"total_tokens":%d,"input_tokens_details":{"cached_tokens":%d}}}`,
		status, vendorModel, input, output, input+output, cached)
}

func newResponseObservationFixture(t *testing.T) (*openaiFixture, string) {
	t.Helper()
	fixture := newOpenAIFixture(t, "")
	// Automatic Azure uses deployment paths. Reuse the fixture's atomic
	// lifecycle controls for those paths only after a test installs an override;
	// certification still receives the unchanged native deployment replies.
	handler := fixture.Config.Handler
	prefix := "/openai/deployments/" + vendorModel + "/responses"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		override := r.Method == http.MethodGet && (fixture.respFetchRaw.Load() != nil || fixture.respGetStream.Load() != nil) ||
			r.Method == http.MethodPost && r.URL.Path == prefix && fixture.respPostStream.Load() != nil
		if override && strings.HasPrefix(r.URL.Path, prefix) {
			r = r.Clone(r.Context())
			target := *r.URL
			target.Path, target.RawPath = "/openai/responses"+strings.TrimPrefix(target.Path, prefix), ""
			r.URL = &target
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return fixture, server.URL
}

func TestResponseTerminalUsageSurvivesModelProjectionLimit(t *testing.T) {
	for _, scenario := range []string{"creation", "retrieval"} {
		t.Run(scenario, func(t *testing.T) {
			fixture, endpoint := newResponseObservationFixture(t)
			h := newAccessHarness(t)
			owner, provider, _, _ := provisionOpenAI(t, h, endpoint,
				[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"},
					map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"}}, []string{"generation"})
			alias := "overlay-" + strings.Repeat("x", 48) + uuid.NewString()[:8]
			draft := h.want(owner, http.MethodPost, "/api/v1/route-drafts", map[string]any{
				"slug": alias, "operations": []string{"generation"}, "overall_timeout_ms": 10000, "max_attempts": 1,
				"fidelity": map[string]any{"mode": "transformed"},
				"targets":  []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}},
			}, map[string]string{"Idempotency-Key": uuid.NewString()}, http.StatusCreated)
			h.want(owner, http.MethodPost, "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil,
				withMatch(draft, map[string]string{"Idempotency-Key": uuid.NewString()}), http.StatusOK)
			key := stateKey(t, h, owner, alias, true)
			h.refresh()

			// The public harness configures a 64 KiB event limit. Native bytes fit
			// that wire limit; only the longer published model binding exceeds
			// its JSON/OIF limit. No production limit is changed for this test.
			const maxEvent = 1 << 16
			base := `{"type":"response.completed","sequence_number":1,"response":` + responseUsageCheckpoint("completed", 17, 9, 3) + `,"pad":""}`
			prefix, suffix := "event: response.completed\ndata: ", "\n\n"
			padding := maxEvent - 1 - len(prefix) - len(suffix) - len(base)
			terminal := strings.Replace(base, `"pad":""`, `"pad":"`+strings.Repeat("x", padding)+`"`, 1)
			wire := prefix + terminal + suffix
			if !json.Valid([]byte(terminal)) || len(wire) >= maxEvent || len(terminal)+len(alias)-len(vendorModel) <= maxEvent {
				t.Fatalf("fixture does not isolate model projection limit: wire=%d native=%d binding=%d", len(wire), len(terminal), len(alias)-len(vendorModel))
			}
			partial := responseUsageCheckpoint("in_progress", 10, 4, 2)
			if scenario == "creation" {
				fixture.respPostStream.Store("event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":" + partial + "}\n\n" + wire)
			} else {
				fixture.resps["resp-up-1"]["status"] = "in_progress"
				fixture.resps["resp-up-1"]["usage"] = nil
			}
			requestBody, err := json.Marshal(map[string]any{"model": alias, "input": "projection accounting", "store": true, "background": true, "stream": scenario == "creation"})
			if err != nil {
				t.Fatal(err)
			}
			status, raw, headers := h.gatewayRaw(http.MethodPost, "/v1/responses", key,
				bytes.NewReader(requestBody), map[string]string{"Content-Type": "application/json"})
			if status != http.StatusOK {
				t.Fatalf("background create: %d %s", status, raw)
			}
			original := headers.Get("X-Request-Id")
			var local string
			if scenario == "creation" {
				for _, line := range strings.Split(string(raw), "\n") {
					if !strings.HasPrefix(line, "data: ") {
						continue
					}
					var event struct {
						Response struct {
							ID string `json:"id"`
						} `json:"response"`
					}
					if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil && strings.HasPrefix(event.Response.ID, "response_") {
						local = event.Response.ID
						break
					}
				}
			} else {
				var result struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(raw, &result); err != nil {
					t.Fatal(err)
				}
				local = result.ID
				fixture.respFetchRaw.Store(partial)
				if status, body, _ := h.gatewayRaw(http.MethodGet, "/v1/responses/"+local, key, nil, nil); status != http.StatusOK {
					t.Fatalf("partial observation: %d %s", status, body)
				}
				fixture.respGetStream.Store(wire)
				status, raw, _ = h.gatewayRaw(http.MethodGet, "/v1/responses/"+local+"?stream=true", key, nil, nil)
				if status != http.StatusBadGateway {
					t.Fatalf("undeliverable terminal accepted: %d %s", status, raw)
				}
			}
			if local == "" || original == "" || strings.Contains(string(raw), "event: response.completed\n") || strings.Contains(string(raw), `"type":"response.completed"`) {
				t.Fatalf("terminal projection was delivered or original identity lost: local=%q request=%q body=%s", local, original, raw)
			}
			assertGeminiUsageSnapshot(t, h, original, true, 17, 9, 3)
			var state string
			var pending bool
			if err := h.Pool.QueryRow(t.Context(), `SELECT state,metadata ? 'pending_usage' FROM olp.provider_resources WHERE id=$1`, strings.TrimPrefix(local, "response_")).Scan(&state, &pending); err != nil || state != "completed" || pending {
				t.Fatalf("native terminal did not settle original owner: state=%q pending=%v err=%v", state, pending, err)
			}
			fixture.respGetStream.Store("event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":" + responseUsageCheckpoint("completed", 1, 1, 0) + "}\n\n")
			if status, body, _ := h.gatewayRaw(http.MethodGet, "/v1/responses/"+local+"?stream=true", key, nil, nil); status != http.StatusOK {
				t.Fatalf("terminal replay: %d %s", status, body)
			}
			if status, body, _ := h.gatewayRaw(http.MethodDelete, "/v1/responses/"+local, key, nil, nil); status != http.StatusOK {
				t.Fatalf("cleanup: %d %s", status, body)
			}
			assertGeminiUsageSnapshot(t, h, original, true, 17, 9, 3)
		})
	}
}

func TestResponseNonterminalNullableSnapshotsRemainPending(t *testing.T) {
	testResponseNonterminalSnapshotsRemainPending(t, `null`)
}

func testResponseNonterminalSnapshotsRemainPending(t *testing.T, snapshotStatus string) {
	t.Helper()
	for _, scenario := range []string{"creation", "retrieval"} {
		for _, cleanup := range []string{"delete", "expiry"} {
			t.Run(scenario+"-"+cleanup, func(t *testing.T) {
				fixture, endpoint := newResponseObservationFixture(t)
				h := newAccessHarness(t)
				owner, _, slug, _ := provisionOpenAI(t, h, endpoint,
					[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"},
						map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"}}, []string{"generation"})
				key := stateKey(t, h, owner, slug, true)
				h.refresh()
				// A typed nonterminal event carries a generic Response snapshot.
				// Its optional status remains native wire data, not proof that
				// this event phase ended the original generation's accounting.
				snapshot := strings.Replace(responseUsageCheckpoint("completed", 10, 4, 2), `"status":"completed"`, `"status":`+snapshotStatus, 1)
				wire := "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":" + snapshot + "}\n\n" +
					"event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"sequence_number\":1,\"response\":" + snapshot + "}\n\n"
				if scenario == "creation" {
					fixture.respPostStream.Store(wire)
				} else {
					fixture.resps["resp-up-1"]["status"] = "in_progress"
					fixture.resps["resp-up-1"]["usage"] = nil
				}
				body, err := json.Marshal(map[string]any{"model": slug, "input": "generic response snapshot", "store": true, "background": true, "stream": scenario == "creation"})
				if err != nil {
					t.Fatal(err)
				}
				status, raw, headers := h.gatewayRaw(http.MethodPost, "/v1/responses", key,
					bytes.NewReader(body), map[string]string{"Content-Type": "application/json"})
				if status != http.StatusOK {
					t.Fatalf("background create: %d %s", status, raw)
				}
				original := headers.Get("X-Request-Id")
				var local string
				if scenario == "creation" {
					for _, line := range strings.Split(string(raw), "\n") {
						if !strings.HasPrefix(line, "data: ") {
							continue
						}
						var event struct {
							Response struct {
								ID string `json:"id"`
							} `json:"response"`
						}
						if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil && strings.HasPrefix(event.Response.ID, "response_") {
							local = event.Response.ID
							break
						}
					}
				} else {
					var created struct {
						ID string `json:"id"`
					}
					if err := json.Unmarshal(raw, &created); err != nil {
						t.Fatal(err)
					}
					local = created.ID
					fixture.respGetStream.Store(wire)
					status, raw, _ = h.gatewayRaw(http.MethodGet, "/v1/responses/"+local+"?stream=true", key, nil, nil)
				}
				if status != http.StatusOK || local == "" || original == "" || strings.Count(string(raw), `"status":`+snapshotStatus) != 2 ||
					!strings.Contains(string(raw), "event: response.created\n") || !strings.Contains(string(raw), "event: response.in_progress\n") || strings.Contains(string(raw), "event: response.completed\n") {
					t.Fatalf("valid generic snapshots changed or refused: status=%d local=%q original=%q body=%s", status, local, original, raw)
				}
				var state string
				var pending bool
				if err := h.Pool.QueryRow(t.Context(), `SELECT state,metadata ? 'pending_usage' FROM olp.provider_resources WHERE id=$1`, strings.TrimPrefix(local, "response_")).Scan(&state, &pending); err != nil || state != "in_progress" || !pending {
					t.Fatalf("snapshot status ended typed pending work: state=%q pending=%v err=%v", state, pending, err)
				}
				var count int
				if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.attempt_usage_facts WHERE request_id=$1`, original).Scan(&count); err != nil || count != 0 {
					t.Fatalf("nonterminal snapshot accounted as final: count=%d err=%v", count, err)
				}
				if cleanup == "delete" {
					if status, result, _ := h.gatewayRaw(http.MethodDelete, "/v1/responses/"+local, key, nil, nil); status != http.StatusOK {
						t.Fatalf("cleanup: %d %s", status, result)
					}
				} else {
					if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.provider_resources SET expires_at=now()-interval '1 second' WHERE id=$1`, strings.TrimPrefix(local, "response_")); err != nil {
						t.Fatal(err)
					}
					if _, err := h.Gateway.Resources.Expire(t.Context(), time.Now()); err != nil {
						t.Fatal(err)
					}
				}
				assertGeminiUsageSnapshot(t, h, original, false, 10, 4, 2)
				if _, err := h.Gateway.Resources.Expire(t.Context(), time.Now()); err != nil {
					t.Fatal(err)
				}
				assertGeminiUsageSnapshot(t, h, original, false, 10, 4, 2)
			})
		}
	}
}
