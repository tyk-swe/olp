//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/limits"
)

// Each fixture owns its database and Valkey namespace. Native overrides begin
// after certification, so refusal scenarios do not weaken capability proof.
type responseRefusalFixture struct {
	h                 *accessHarness
	native            *openaiFixture
	provider          string
	slug              string
	key               string
	lookup            string
	sink              *captureSink
	valkey            *coordination.Client
	namespace         string
	posts             atomic.Int64
	nativeIdempotency atomic.Value
	reply             atomic.Value
	unary             atomic.Bool
	rejectFirst       atomic.Bool
	stateless         bool
	expectedAttempts  int
}

func newResponseRefusalFixture(t *testing.T, fidelity string) *responseRefusalFixture {
	t.Helper()
	f := &responseRefusalFixture{h: newAccessHarness(t), sink: &captureSink{}}
	native := newOpenAIFixture(t, "")
	f.native = native
	secondBody := strings.Replace(responseUsageCheckpoint("completed", 17, 9, 3), `"id":"resp-up-1"`, `"id":"another-native-response"`, 1)
	secondWire := responseRefusalWire(t, "created", 10, 4, 2, false) +
		strings.Replace(responseRefusalWire(t, "completed", 17, 9, 3, false), `"sequence_number":0`, `"sequence_number":1`, 1)
	secondWire = strings.ReplaceAll(secondWire, `"id":"resp-up-1"`, `"id":"another-native-response"`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reply := f.reply.Load(); reply != nil && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/responses") {
			f.nativeIdempotency.Store(r.Header.Get("Idempotency-Key"))
			ordinal := f.posts.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			if ordinal == 1 && f.rejectFirst.Load() {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"explicit native admission refusal"}}`)
				return
			}
			raw := reply.(string)
			if f.unary.Load() {
				w.Header().Set("Content-Type", "application/json")
				if ordinal > 1 {
					raw = secondBody
				}
			} else {
				w.Header().Set("Content-Type", "text/event-stream")
				if ordinal > 1 {
					raw = secondWire
				}
			}
			_, _ = io.WriteString(w, raw)
			return
		}
		native.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	owner, provider, fallback, _ := provisionOpenAIWith(t, f.h, upstream.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"},
			map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"}}, []string{"generation"},
		map[string]any{"fidelity": map[string]any{"mode": fidelity}},
		map[string]any{"profile_id": "azure-legacy-responses", "profile_revision": "1"})
	f.provider = provider["id"].(string)
	f.slug = "refusal-" + strings.Repeat("r", 48) + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	draft := f.h.want(owner, http.MethodPost, "/api/v1/route-drafts", map[string]any{
		"slug": f.slug, "operations": []string{"generation"}, "overall_timeout_ms": 10000, "max_attempts": 2,
		"fidelity":  map[string]any{"mode": fidelity},
		"fallbacks": []any{map[string]any{"route": fallback, "on": []string{"exhausted"}}},
		"retry":     map[string]any{"connect": map[string]any{"max_retries": 1, "base_backoff_ms": 1, "max_backoff_ms": 1}, "upstream_server": map[string]any{"max_retries": 1, "base_backoff_ms": 1, "max_backoff_ms": 1}, "rate_limit": map[string]any{"max_retries": 1, "base_backoff_ms": 1, "max_backoff_ms": 1}},
		"targets":   []any{map[string]any{"provider_id": f.provider, "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}},
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, http.StatusCreated)
	f.h.want(owner, http.MethodPost, "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil,
		withMatch(draft, map[string]string{"Idempotency-Key": uuid.NewString()}), http.StatusOK)
	created := f.h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{
		"name": "retained refusal", "scopes": []string{"inference"}, "allowed_routes": []string{f.slug, fallback},
		"allow_provider_state": true, "tokens_per_minute": 100000, "requests_per_minute": 100,
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, http.StatusCreated)
	f.key, f.lookup = created["secret"].(string), created["lookup_id"].(string)
	f.valkey = limClient(t)
	f.namespace = limNamespace(t, f.valkey, "response-local-refusal")
	f.h.Gateway.Admission = gateway.NewAdmission(limLimiter(t, f.valkey, f.namespace),
		func() limits.OutagePolicy { return limits.FailClosed }, slog.New(slog.DiscardHandler))
	f.h.Gateway.Sink = f.sink
	f.h.refresh()
	return f
}

func responseRefusalEvent(phase string, input, output, cached int64) string {
	status := "completed"
	if phase == "created" {
		status = "in_progress"
	} else if phase == "failed" {
		status = "failed"
	}
	body := responseUsageCheckpoint(status, input, output, cached)
	if phase == "failed" {
		body = strings.TrimSuffix(body, "}") + `,"error":{"type":"server_error","code":"server_error","message":"native generation failed"}}`
	}
	return `{"type":"response.` + phase + `","sequence_number":0,"response":` + body + `}`
}

func responseRefusalWire(t *testing.T, phase string, input, output, cached int64, pad bool) string {
	t.Helper()
	base := responseRefusalEvent(phase, input, output, cached)
	prefix, suffix := "event: response."+phase+"\ndata: ", "\n\n"
	if pad {
		const maximum = 1 << 16
		padding := maximum - 1 - len(prefix) - len(suffix) - len(base) - len(`,"padding":""`)
		base = base[:len(base)-1] + `,"padding":"` + strings.Repeat("x", padding) + `"}`
		if len(prefix+base+suffix) != maximum-1 || len(base)+64-len(vendorModel) <= maximum {
			t.Fatal("fixture must fit native bounds and exceed them only after local model binding")
		}
	}
	return prefix + base + suffix
}

func (f *responseRefusalFixture) create(t *testing.T, background bool) (int, []byte, http.Header, gateway.Envelope) {
	t.Helper()
	// All rate assertions belong to this actual server minute, not a client
	// approximation near its boundary. Admission deliberately exceeds usage.
	minute := limSettleInMinute(t, f.valkey, 10*time.Second) / 60000
	body := strings.NewReader(fmt.Sprintf(`{"model":%q,"input":%q,"max_output_tokens":256,"store":%t,"background":%t,"stream":%t}`,
		f.slug, strings.Repeat("admission exceeds actual usage ", 16), !f.stateless, background, !f.unary.Load()))
	status, raw, header := f.h.gatewayRaw(http.MethodPost, "/v1/responses", f.key, body, map[string]string{"Content-Type": "application/json"})
	if after := limServerTimeMS(t, f.valkey) / 60000; after != minute {
		t.Fatalf("scenario crossed Valkey minute boundary: %d -> %d", minute, after)
	}
	if f.sink.count() != 1 {
		t.Fatalf("one generation produced %d terminal observations", f.sink.count())
	}
	event := f.sink.last()
	wantAttempts := f.expectedAttempts
	if wantAttempts == 0 {
		wantAttempts = 1
	}
	if f.posts.Load() != int64(wantAttempts) || len(event.Attempts) != wantAttempts {
		t.Fatalf("accepted native work repeated: posts=%d attempts=%+v", f.posts.Load(), event.Attempts)
	}
	return status, raw, header, event
}

func (f *responseRefusalFixture) assertUsage(t *testing.T, event gateway.Envelope, complete bool, input, output, cached int64) {
	t.Helper()
	attempt := event.Attempts[0]
	if attempt.Usage == nil || attempt.Usage.InputTokens != input || attempt.Usage.OutputTokens != output ||
		attempt.Usage.CachedInputTokens == nil || *attempt.Usage.CachedInputTokens != cached ||
		!attempt.UsageObserved || attempt.UsageComplete != complete || attempt.BillingUncertain == complete {
		t.Fatalf("local refusal changed native consumption: %+v", attempt)
	}
	estimate := attempt.EstimatedInputTokens + 256
	if attempt.EstimateProvenance == "" || estimate <= input+output {
		t.Fatalf("fixture did not reserve above observed consumption: estimate=%d fact=%+v", estimate, attempt)
	}
	// TPM is the local admission allowance. Provider evidence may increase it,
	// while authoritative low or zero consumption does not refund allowance.
	actual := max(input+output, estimate*2) // the request admits both configured attempts
	rate, _ := limRateKeys(f.namespace, f.lookup)
	limWantCounters(t, f.valkey, rate, 1, actual)
	stats := f.h.Gateway.Health().ProviderHealth(time.Hour)[f.provider]
	wantSuccess := int64(0)
	if complete {
		wantSuccess = 1
	}
	if stats.Attempts != 1 || stats.Successes != wantSuccess || stats.ServerErrors != 0 || stats.TransportErrors != 0 || stats.RateLimits != 0 {
		t.Fatalf("local refusal distorted provider health: %+v", stats)
	}
}

func (f *responseRefusalFixture) retainedOwner(t *testing.T) (string, string, bool) {
	t.Helper()
	var count int
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.provider_resources WHERE route_slug=$1`, f.slug).Scan(&count); err != nil || count != 1 {
		t.Fatalf("accepted native work has competing retained owners: count=%d %v", count, err)
	}
	var local, original string
	var pending bool
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT kind||'_'||replace(id::text,'-',''),COALESCE(metadata->'pending_usage'->>'request_id',''),metadata ? 'pending_usage'
 FROM olp.provider_resources WHERE route_slug=$1`, f.slug).Scan(&local, &original, &pending); err != nil {
		t.Fatal(err)
	}
	return local, original, pending
}

func assertResponseOriginalMetadata(t *testing.T, h *accessHarness, event gateway.Envelope, status int) {
	t.Helper()
	var storedStatus int
	var committed bool
	last := event.Attempts[len(event.Attempts)-1]
	err := h.Pool.QueryRow(t.Context(), `SELECT r.status_code,a.committed
 FROM olp.requests r JOIN olp.attempts a ON a.request_id=r.id AND a.request_started_at=r.started_at
 WHERE r.id=$1 AND a.ordinal=$2`, event.AccountingID, last.Ordinal).Scan(&storedStatus, &committed)
	if err != nil || storedStatus != status || committed != last.Committed {
		t.Fatalf("financial owner replaced original HTTP delivery metadata: stored_status=%d stored_commit=%t actual_status=%d actual_commit=%t err=%v", storedStatus, committed, status, last.Committed, err)
	}
}

func TestRetainedFailedTerminalLocalRefusalDoesNotRepeatWork(t *testing.T) {
	for _, fidelity := range []string{"strict", "transformed"} {
		t.Run(fidelity, func(t *testing.T) {
			f := newResponseRefusalFixture(t, fidelity)
			f.reply.Store(responseRefusalWire(t, "failed", 17, 9, 3, true))
			status, raw, header, event := f.create(t, true)
			if status != http.StatusBadGateway || header.Get("X-Should-Retry") != "false" || event.Committed || bytes.Contains(raw, []byte(`"type":"response.failed"`)) {
				t.Fatalf("failed retained result repeated or delivered: status=%d header=%v event=%+v body=%s", status, header, event, raw)
			}
			fact := event.Attempts[0]
			if fact.Class != "upstream_server" || fact.Usage == nil || !fact.UsageComplete || fact.BillingUncertain || fact.Usage.InputTokens != 17 || fact.Usage.OutputTokens != 9 || !fact.GenerationUsageDeferred {
				t.Fatalf("native failure lost known consumption or ownership: %+v", fact)
			}
			_, _, pending := f.retainedOwner(t)
			if pending {
				t.Fatal("failed terminal observation did not settle its sole original owner")
			}
			assertGeminiUsageSnapshot(t, f.h, event.AccountingID, true, 17, 9, 3)
			assertResponseOriginalMetadata(t, f.h, event, status)
			stats := f.h.Gateway.Health().ProviderHealth(time.Hour)[f.provider]
			if stats.Attempts != 1 || stats.ServerErrors != 1 || stats.TransportErrors != 0 || stats.Successes != 0 {
				t.Fatalf("genuine native failure was treated as a local health refusal: %+v", stats)
			}
		})
	}
}

func TestKnownFailedStatelessResponseMayRetry(t *testing.T) {
	for _, fidelity := range []string{"strict", "transformed"} {
		t.Run(fidelity, func(t *testing.T) {
			f := newResponseRefusalFixture(t, fidelity)
			f.stateless, f.expectedAttempts = true, 2
			f.reply.Store(responseRefusalWire(t, "failed", 17, 9, 3, true))
			status, raw, _, event := f.create(t, false)
			if status != http.StatusOK || !bytes.Contains(raw, []byte(`"type":"response.completed"`)) || !event.Committed {
				t.Fatalf("known failed stateless attempt was not retried: status=%d event=%+v body=%s", status, event, raw)
			}
			if event.Attempts[0].Class != "upstream_server" || event.Attempts[1].Class != "success" {
				t.Fatalf("separate native attempt outcomes changed: %+v", event.Attempts)
			}
			for _, fact := range event.Attempts {
				if fact.GenerationUsageDeferred || !fact.UsageComplete || fact.BillingUncertain || fact.Usage == nil || fact.Usage.InputTokens != 17 || fact.Usage.OutputTokens != 9 {
					t.Fatalf("independent native consumption was lost: %+v", fact)
				}
			}
			kind := "response"
			if fidelity == "strict" {
				kind = "strict_response"
			}
			assertNoNativeMapping(t, f.h, f.slug, kind)
			stats := f.h.Gateway.Health().ProviderHealth(time.Hour)[f.provider]
			if stats.Attempts != 2 || stats.ServerErrors != 1 || stats.TransportErrors != 0 || stats.Successes != 1 {
				t.Fatalf("real native failure or successful retry distorted health: %+v", stats)
			}
		})
	}
}

func TestRejectedRetainedResponseCreateMayRetry(t *testing.T) {
	for _, fidelity := range []string{"strict", "transformed"} {
		t.Run(fidelity, func(t *testing.T) {
			f := newResponseRefusalFixture(t, fidelity)
			f.expectedAttempts = 2
			f.rejectFirst.Store(true)
			f.reply.Store(responseRefusalWire(t, "created", 10, 4, 2, false) + strings.Replace(responseRefusalWire(t, "completed", 17, 9, 3, false), `"sequence_number":0`, `"sequence_number":1`, 1))
			status, raw, _, event := f.create(t, true)
			if status != http.StatusOK || !event.Committed || !bytes.Contains(raw, []byte(`"type":"response.completed"`)) {
				t.Fatalf("explicit rejection incorrectly suppressed retry: status=%d event=%+v body=%s", status, event, raw)
			}
			first, last := event.Attempts[0], event.Attempts[1]
			if first.GenerationUsageDeferred || first.BillingUncertain || first.Usage != nil || first.Status != http.StatusTooManyRequests || !last.GenerationUsageDeferred || !last.UsageComplete || last.BillingUncertain {
				t.Fatalf("rejected work acquired consumption/ownership or terminal retry lost proof: %+v", event.Attempts)
			}
			_, _, pending := f.retainedOwner(t)
			if pending {
				t.Fatal("successful terminal retry left an original money owner pending")
			}
			assertResponseOriginalMetadata(t, f.h, event, status)
		})
	}
}

func TestResponseLocalProjectionRefusalKeepsOneAcceptedGeneration(t *testing.T) {
	for _, fidelity := range []string{"strict", "transformed"} {
		for _, observation := range []struct {
			name, phase           string
			input, output, cached int64
			complete              bool
		}{
			{"partial", "created", 10, 4, 2, false},
			{"terminal", "completed", 17, 9, 3, true},
			{"terminal-zero", "completed", 0, 0, 0, true},
		} {
			t.Run(fidelity+"/"+observation.name, func(t *testing.T) {
				f := newResponseRefusalFixture(t, fidelity)
				f.reply.Store(responseRefusalWire(t, observation.phase, observation.input, observation.output, observation.cached, true))
				status, raw, header, event := f.create(t, true)
				if status != http.StatusBadGateway || header.Get("X-Should-Retry") != "false" || event.Committed || bytes.Contains(raw, []byte(`"type":"response.`+observation.phase+`"`)) {
					t.Fatalf("pre-delivery local refusal allowed retry or delivery: status=%d headers=%v event=%+v body=%s", status, header, event, raw)
				}
				if observation.complete {
					assertResponseOriginalMetadata(t, f.h, event, status)
				}
				f.assertUsage(t, event, observation.complete, observation.input, observation.output, observation.cached)
				local, original, pending := f.retainedOwner(t)
				if observation.complete {
					if pending {
						t.Fatal("validated terminal observation retained a pending accounting owner")
					}
					assertGeminiUsageSnapshot(t, f.h, event.AccountingID, true, observation.input, observation.output, observation.cached)
				} else {
					if !pending || original != event.AccountingID {
						t.Fatalf("provisional work lost original accounting owner: pending=%t original=%s event=%s", pending, original, event.AccountingID)
					}
					assertRetainedPendingUsage(t, f.h, local, original, observation.input, observation.output, observation.cached)
				}
			})
		}
	}
}

func TestForegroundRetainedResponseLocalRefusalPreservesPartialConsumption(t *testing.T) {
	for _, fidelity := range []string{"strict", "transformed"} {
		t.Run(fidelity, func(t *testing.T) {
			f := newResponseRefusalFixture(t, fidelity)
			f.reply.Store(responseRefusalWire(t, "created", 10, 4, 2, true))
			status, raw, header, event := f.create(t, false)
			if status != http.StatusBadGateway || header.Get("X-Should-Retry") != "false" || event.Committed || bytes.Contains(raw, []byte(`"type":"response.created"`)) {
				t.Fatalf("foreground local refusal repeated or delivered native work: status=%d headers=%v event=%+v body=%s", status, header, event, raw)
			}
			f.assertUsage(t, event, false, 10, 4, 2)
			_, original, pending := f.retainedOwner(t)
			if pending || original != "" || event.Attempts[0].GenerationUsageDeferred {
				t.Fatalf("foreground consumption acquired a deferred money owner: pending=%t original=%s event=%+v", pending, original, event)
			}
		})
	}
}

func TestRetainedResponseValidatedCompletionPreservesNativeConsumption(t *testing.T) {
	for _, fidelity := range []string{"strict", "transformed"} {
		for _, mode := range []string{"unary", "stream"} {
			for _, input := range []int64{17, 0} {
				t.Run(fmt.Sprintf("%s/%s/input-%d", fidelity, mode, input), func(t *testing.T) {
					f := newResponseRefusalFixture(t, fidelity)
					output, cached := int64(9), int64(3)
					if input == 0 {
						output, cached = 0, 0
					}
					if mode == "unary" {
						f.unary.Store(true)
						f.reply.Store(responseUsageCheckpoint("completed", input, output, cached))
					} else {
						f.reply.Store(responseRefusalWire(t, "completed", input, output, cached, false))
					}
					status, raw, _, event := f.create(t, true)
					if status != http.StatusOK || !bytes.Contains(raw, []byte(`"status":"completed"`)) || !event.Committed {
						t.Fatalf("valid native terminal control refused: status=%d event=%+v body=%s", status, event, raw)
					}
					f.assertUsage(t, event, true, input, output, cached)
					_, _, pending := f.retainedOwner(t)
					if pending {
						t.Fatal("valid terminal control retained pending owner")
					}
					assertGeminiUsageSnapshot(t, f.h, event.AccountingID, true, input, output, cached)
				})
			}
		}
	}
}

func responseRefusalTrigger(t *testing.T, h *accessHarness, table, predicate string) func() {
	t.Helper()
	// table and predicate are test-owned literals, scoped to this installation.
	statement := `CREATE FUNCTION olp.reject_response_local_test() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF ` + predicate + ` THEN RAISE EXCEPTION 'injected response local refusal'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER reject_response_local_test BEFORE INSERT ON olp.` + table + `
 FOR EACH ROW EXECUTE FUNCTION olp.reject_response_local_test()`
	if _, err := h.Pool.Exec(t.Context(), statement); err != nil {
		t.Fatal(err)
	}
	removed := false
	remove := func() {
		t.Helper()
		if removed {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := h.Pool.Exec(ctx, `DROP TRIGGER reject_response_local_test ON olp.`+table+`; DROP FUNCTION olp.reject_response_local_test()`); err != nil {
			t.Errorf("remove response refusal injection: %v", err)
			return
		}
		removed = true
	}
	t.Cleanup(remove)
	return remove
}

func TestResponseLocalPersistenceRefusalCannotRepeatAcceptedCreate(t *testing.T) {
	for _, fidelity := range []string{"strict", "transformed"} {
		for _, failure := range []string{"attach", "settle"} {
			t.Run(fidelity+"/"+failure, func(t *testing.T) {
				f := newResponseRefusalFixture(t, fidelity)
				f.reply.Store(responseRefusalWire(t, "completed", 17, 9, 3, false))
				table, predicate := "provider_resources", `NEW.kind IN ('response','strict_response')`
				if failure == "settle" {
					table, predicate = "request_metadata_event_receipts", "true"
				}
				remove := responseRefusalTrigger(t, f.h, table, predicate)
				status, raw, header, event := f.create(t, true)
				if status != http.StatusBadGateway || header.Get("X-Should-Retry") != "false" || event.Committed || bytes.Contains(raw, []byte(`"type":"response.completed"`)) {
					t.Fatalf("local persistence refusal repeated or delivered work: status=%d headers=%v event=%+v body=%s", status, header, event, raw)
				}
				f.assertUsage(t, event, true, 17, 9, 3)
				if failure == "attach" {
					assertNoNativeMapping(t, f.h, f.slug, "response")
					assertNoNativeMapping(t, f.h, f.slug, "strict_response")
					if event.Attempts[0].GenerationUsageDeferred {
						t.Fatal("failed attachment claimed a nonexistent accounting owner")
					}
				} else {
					local, original, pending := f.retainedOwner(t)
					if !pending || original != event.AccountingID || !event.Attempts[0].GenerationUsageDeferred {
						t.Fatalf("failed settlement lost original owner: pending=%t original=%s event=%+v", pending, original, event)
					}
					remove()
					f.native.respFetchRaw.Store(responseUsageCheckpoint("completed", 17, 9, 3))
					status, body, _ := f.h.gatewayRaw(http.MethodGet, "/v1/responses/"+local, f.key, nil, nil)
					if status != http.StatusOK {
						t.Fatalf("settlement recovery retrieval: %d %s", status, body)
					}
					assertGeminiUsageSnapshot(t, f.h, original, true, 17, 9, 3)
					if f.posts.Load() != 1 {
						t.Fatal("settlement recovery repeated the original native create")
					}
				}
				remove()
			})
		}
	}
}

func TestResponsesCallerIdempotencyHasNoQualifiedNativeReplay(t *testing.T) {
	for _, fidelity := range []string{"strict", "transformed"} {
		t.Run(fidelity, func(t *testing.T) {
			f := newResponseRefusalFixture(t, fidelity)
			f.unary.Store(true)
			f.reply.Store(responseUsageCheckpoint("completed", 17, 9, 3))
			status, body, _ := f.h.gatewayRaw(http.MethodPost, "/v1/responses", f.key,
				strings.NewReader(`{"model":"`+f.slug+`","input":"hello","store":false}`),
				map[string]string{"Content-Type": "application/json", "Idempotency-Key": "caller-replay-boundary"})
			if fidelity == "strict" {
				if status != http.StatusBadRequest || !bytes.Contains(body, []byte(`"code":"target_capability"`)) ||
					!bytes.Contains(body, []byte(`"param":"/headers"`)) || f.posts.Load() != 0 {
					t.Fatalf("unqualified strict idempotency reached native work: %d %s posts=%d", status, body, f.posts.Load())
				}
				return
			}
			if status != http.StatusOK || f.posts.Load() != 1 || f.nativeIdempotency.Load() != "" {
				t.Fatalf("transformed caller idempotency acquired native replay semantics: %d %s posts=%d header=%v", status, body, f.posts.Load(), f.nativeIdempotency.Load())
			}
		})
	}
}
