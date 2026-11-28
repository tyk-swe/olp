package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	limitadmission "github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/limits/scripts"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/platform/staging"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/rates"
	"github.com/tyk-swe/olp/internal/usage/routinginputs"
)

type deadlineFailWriter struct {
	*httptest.ResponseRecorder
	ctx context.Context
}

func (w *deadlineFailWriter) Write([]byte) (int, error) {
	<-w.ctx.Done()
	return 0, io.ErrClosedPipe
}

func TestRetainedResponseStreamClassifiesDeadlineAndProtocolFailures(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		timeout, frame, writeFailure bool
	}{
		{"idle deadline", true, false, false},
		{"truncated stream", false, false, false},
		{"committed deadline", true, true, false},
		{"client write failure at deadline", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, Config{})
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				if tc.frame {
					_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"m\",\"content_index\":0,\"delta\":\"hello\"}\n\n")
				}
				_ = http.NewResponseController(w).Flush()
				if tc.timeout {
					<-r.Context().Done()
				}
			})
			snapshot := h.rt.release.Snapshot().Clone()
			route := snapshot.Routes[routeSlug]
			route.OverallTimeout = 150
			target := route.Targets[0]
			provider := snapshot.Providers[target.ProviderID]
			p := &Pin{Provider: provider, Target: target, Model: modelA, Slot: provider.Slots[0], attempt: routing.Attempt{ProviderID: provider.ID, UpstreamModel: modelA}}
			x := &Call{request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release}, family: wire.Responses, callIdentity: callIdentity{actor: "api_key_resource", keyID: h.keyID}}
			x.Route = &route
			ctx, cancel := h.gateway.StateDeadline(t.Context(), &route)
			defer cancel()
			recorder := httptest.NewRecorder()
			var writer http.ResponseWriter = recorder
			if tc.writeFailure {
				writer = &deadlineFailWriter{ResponseRecorder: recorder, ctx: ctx}
			}
			failure := h.gateway.StreamStoredResponse(ctx, writer, x, &resources.Resource{ID: uuid.NewString(), Kind: resources.KindResponse, UpstreamID: "response-local-fixture", RouteSlug: routeSlug}, p, nil)
			code, status, class := "response_stream_incomplete", http.StatusBadGateway, ClassProtocol
			if tc.timeout {
				code, status, class = "gateway_timeout", http.StatusGatewayTimeout, ClassTimeout
			}
			if tc.frame {
				if failure != nil || recorder.Code != http.StatusOK {
					t.Fatalf("committed response: failure=%v status=%d", failure, recorder.Code)
				}
				outcome := "failure"
				envelopeStatus := http.StatusOK
				if tc.writeFailure {
					code, class, outcome, envelopeStatus = "client_cancelled", ClassCancelled, "cancelled", 0
				}
				h.sink.mu.Lock()
				defer h.sink.mu.Unlock()
				if len(h.sink.envs) != 1 {
					t.Fatalf("terminal envelopes=%d, want1", len(h.sink.envs))
				}
				env := h.sink.envs[0]
				if env.ErrorClass != code || env.Outcome != outcome || env.Status != envelopeStatus || !env.Committed || len(env.Attempts) != 1 || env.Attempts[0].Class != class {
					t.Fatalf("terminal envelope=%+v", env)
				}
				if !tc.writeFailure && !strings.Contains(recorder.Body.String(), "hello") {
					t.Fatal("nonterminal frame was not delivered")
				}
			} else if failure == nil || failure.Code != code || failure.Status != status || x.facts[0].Class != class {
				t.Fatalf("failure=%+v class=%s; want%d %s %s", failure, x.facts[0].Class, status, code, class)
			}

			if tc.timeout && ctx.Err() != context.DeadlineExceeded {
				t.Fatalf("route deadline did not expire: %v", ctx.Err())
			}
		})
	}
}

func TestStrictRetainedResponseSettlesAfterNativeValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, object, class string
	}{
		{"valid terminal result", `"response"`, ClassSuccess},
		{"invalid terminal result", `7`, ClassProtocol},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, err := pgxpool.New(t.Context(), "postgres://unused@127.0.0.1:1/unused")
			if err != nil {
				t.Fatal(err)
			}
			pool.Close()
			h := newHarness(t, Config{}, Dependencies{Resources: resources.New(pool)})
			h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"native-response\",\"object\":%s,\"status\":\"completed\",\"output\":[]}}\n\n", tc.object)
			})
			p := testPinnedTarget(h, 0)
			p.Provider.VendorID = "openai"
			probed, granted := openTestProbe(t, h, p.Provider.ID)
			if !probed || !granted {
				t.Fatal("could not claim half-open probe")
			}
			p.Hold = &dispatchHold{provider: p.Provider.ID, probed: probed}
			x := &Call{request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release}, family: wire.Responses, callIdentity: callIdentity{actor: "api_key_resource", keyID: h.keyID}}
			x.Route = &routing.Route{Slug: routeSlug, Fidelity: routing.RouteFidelity{Mode: routing.FidelityStrict}}
			res := &resources.Resource{ID: resources.LocalID(resources.KindStrictResponse, uuid.New()), Kind: resources.KindStrictResponse, UpstreamID: "native-response", RouteSlug: routeSlug}
			recorder := httptest.NewRecorder()
			failure := h.gateway.StreamStoredResponse(t.Context(), recorder, x, res, p, nil)
			if failure == nil || failure.Code != "response_stream_incomplete" {
				t.Fatalf("unexpected stream result: %+v", failure)
			}
			h.gateway.Refuse(x, recorder, failure)
			h.gateway.SettlePinnedHold(t.Context(), p.Hold)
			env := h.sink.last(t)
			if len(env.Attempts) != 1 || env.Attempts[0].Class != tc.class {
				t.Fatalf("unexpected attempt evidence: %+v", env.Attempts)
			}
			if probed, granted := h.gateway.health.Claim(p.Provider.ID); probed || granted != (tc.class == ClassSuccess) {
				t.Fatalf("after settlement, probe=%v granted=%v", probed, granted)
			}
		})
	}
}

func TestInterruptedPinnedExchangeSettlesProbeByAttribution(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		local bool
	}{
		{"provider protocol failure keeps the circuit open", false},
		{"local refusal releases the probe", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, Config{})
			p := testPinnedTarget(h, 0)
			probed, granted := openTestProbe(t, h, p.Provider.ID)
			if !probed || !granted {
				t.Fatal("could not claim half-open probe")
			}
			p.Hold = &dispatchHold{provider: p.Provider.ID, probed: probed}
			x := &Call{request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release}, family: wire.File, callIdentity: callIdentity{actor: "api_key_resource", keyID: h.keyID}}
			response, failure := h.gateway.PinnedDo(t.Context(), x, p, http.MethodGet, p.Provider.Endpoint+"/files/file-native", nil, "")
			if failure != nil {
				t.Fatalf("pinned request failed: %+v", failure)
			}
			defer func() { _ = response.Body.Close() }()
			response.Exchange.Interrupt(ClassProtocol, tc.local)
			h.gateway.SettlePinnedHold(t.Context(), p.Hold)
			if probed, granted := h.gateway.health.Claim(p.Provider.ID); probed != tc.local || granted != tc.local {
				t.Fatalf("after settlement, probe=%v granted=%v", probed, granted)
			}
		})
	}
}

type failedFlushWriter struct{ *httptest.ResponseRecorder }

func (w failedFlushWriter) FlushError() error { return io.ErrClosedPipe }

func TestPinnedAcknowledgementRequiresFlush(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{})
	h.mock.set("a", status(http.StatusNoContent, ""))
	p := testPinnedTarget(h, 0)
	route := routing.Route{Fidelity: routing.RouteFidelity{Mode: routing.FidelityStrict}}
	x := &Call{request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release}, family: wire.GeminiInteractions, callIdentity: callIdentity{actor: "api_key_resource", keyID: h.keyID}}
	x.Route = &route
	response, failure := h.gateway.PinnedDo(t.Context(), x, p, http.MethodDelete, p.Provider.Endpoint+"/interactions/native", nil, "")
	if failure != nil {
		t.Fatal(failure.Fault())
	}
	w := failedFlushWriter{httptest.NewRecorder()}
	defer response.Body.Close()
	response.Exchange.Complete(ClassSuccess)
	h.gateway.WriteStateJSONStatus(w, x, nil, http.StatusOK, response.Exchange)
	env := h.sink.last(t)
	if w.Code != http.StatusOK || env.Outcome != "cancelled" || env.ErrorClass != "client_delivery_failed" || len(env.Attempts) != 1 ||
		env.Attempts[0].Class != ClassSuccess || env.Attempts[0].Interaction == nil || env.Attempts[0].Interaction.ClientState != usage.ClientUnobserved {
		t.Fatalf("unexpected delete acknowledgement evidence, status=%d envelope=%+v attempts=%+v", w.Code, env, env.Attempts)
	}
}

func TestRetainedDeletionSettlesProviderBeforeLocalWrite(t *testing.T) {
	t.Parallel()
	store := retainedapi.Family{Operation: "vector_store", Kind: "vector_store", Identity: "id"}
	item := retainedapi.Family{Operation: "conversation", Kind: "conversation_item", Identity: "id"}
	for _, tc := range []struct {
		name, body, class string
		available         bool
		charge            string
		family            retainedapi.Family
	}{
		{"accepted deletion", `{"id":"resource-native","deleted":true}`, ClassSuccess, true, "0.25", store},
		{"malformed provider result", `{"id":"resource-native","deleted":true,"unknown_id":"unexpected"}`, ClassProtocol, false, "0", store},
		{"unrelated native identity", `{"id":"unrelated","deleted":true}`, ClassProtocol, false, "0", store},
		{"conversation item returns parent identity", `{"id":"conversation-native"}`, ClassSuccess, true, "0.25", item},
		{"conversation item returns child identity", `{"id":"resource-native"}`, ClassProtocol, false, "0", item},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, err := pgxpool.New(t.Context(), "postgres://unused@127.0.0.1:1/unused")
			if err != nil {
				t.Fatal(err)
			}
			pool.Close()
			h := newHarness(t, Config{}, Dependencies{Resources: resources.New(pool)})
			h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			})
			p := testPinnedTarget(h, 0)
			probed, granted := openTestProbe(t, h, p.Provider.ID)
			if !granted || !probed {
				t.Fatal("could not claim half-open probe")
			}
			p.Hold = &dispatchHold{provider: p.Provider.ID, probed: probed}
			p.attempt.Price = rates.NewRoutingPrice(rates.RoutingPrice{Price: rates.Price{PerRequest: new("0.25")}})
			x := &Call{request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release}, family: wire.File, callIdentity: callIdentity{actor: "api_key_resource", keyID: h.keyID}}
			id := uuid.New()
			resource := &resources.Resource{UUID: id, ID: resources.LocalID(tc.family.Kind, id), Kind: tc.family.Kind, UpstreamID: "resource-native"}
			var parent *resources.Resource
			if tc.family.Kind == "conversation_item" {
				id := uuid.New()
				parent = &resources.Resource{UUID: id, ID: resources.LocalID("conversation", id), Kind: "conversation", UpstreamID: "conversation-native"}
				resource.ParentID = &parent.UUID
			}
			r := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/v1/resources/"+resource.ID, nil)
			w := httptest.NewRecorder()
			endpoint := strings.TrimRight(p.Provider.Endpoint, "/") + "/resources/resource-native"
			status := h.gateway.RetainedDispatch(w, r, RetainedDispatchRequest{
				Call: x, Pin: p, Endpoint: retainedapi.Endpoint{Family: tc.family, Method: http.MethodDelete, Action: "delete"}, Resource: resource, Parent: parent, URL: endpoint,
				Fail: func(e *fault.Error) {
					h.gateway.Refuse(x, w, e)
				},
			})
			h.gateway.SettlePinnedHold(t.Context(), p.Hold)
			env := h.sink.last(t)
			if status != http.StatusServiceUnavailable || w.Code != status || env.Outcome != "failure" || env.ErrorClass != "provider_state_unavailable" {
				t.Fatalf("local deletion failure changed, status=%d body=%s outcome=%s error=%s", status, w.Body.String(), env.Outcome, env.ErrorClass)
			}
			if len(env.Attempts) != 1 || env.Attempts[0].Class != tc.class || h.gateway.settledCharge(x).String() != tc.charge {
				t.Fatalf("unexpected attempt evidence or charge: %+v, %s", env.Attempts, h.gateway.settledCharge(x).String())
			}
			if probed, granted := h.gateway.health.Claim(p.Provider.ID); probed || granted != tc.available {
				t.Fatalf("after settlement, probe=%v granted=%v", probed, granted)
			}
		})
	}
}

func TestRetainedResultPersistenceFailureKeepsProviderProbeAvailable(t *testing.T) {
	t.Parallel()
	pool, err := pgxpool.New(t.Context(), "postgres://unused@127.0.0.1:1/unused")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	h := newHarness(t, Config{}, Dependencies{Resources: resources.New(pool)})
	h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"store-native","status":"completed"}`)
	})
	p := testPinnedTarget(h, 0)
	probed, granted := openTestProbe(t, h, p.Provider.ID)
	if !granted || !probed {
		t.Fatal("could not claim half-open probe")
	}
	p.Hold = &dispatchHold{provider: p.Provider.ID, probed: probed}
	p.attempt.Price = rates.NewRoutingPrice(rates.RoutingPrice{Price: rates.Price{PerRequest: new("0.25")}})
	x := &Call{
		request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release},
		family:  wire.File, callIdentity: callIdentity{actor: "api_key_resource", keyID: h.keyID},
		retainedAdmission: &RetainedAdmissionPlan{attempt: p.attempt, billing: retainedPriced},
	}
	id := uuid.New()
	resource := &resources.Resource{UUID: id, ID: resources.LocalID("vector_store", id), Kind: "vector_store", UpstreamID: "store-native"}
	e := retainedapi.Endpoint{
		Family: retainedapi.Family{Operation: "vector_store", Kind: resource.Kind, Identity: "id"},
		Method: http.MethodGet, Action: "get",
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/vector_stores/"+resource.ID, nil)
	w := httptest.NewRecorder()
	endpoint := strings.TrimRight(p.Provider.Endpoint, "/") + "/vector_stores/store-native"
	status := h.gateway.RetainedDispatch(w, r, RetainedDispatchRequest{
		Call: x, Pin: p, Endpoint: e, Resource: resource, URL: endpoint,
		Fail: func(e *fault.Error) {
			h.gateway.Refuse(x, w, e)
		},
	})
	h.gateway.SettlePinnedHold(t.Context(), p.Hold)
	env := h.sink.last(t)
	if status != http.StatusServiceUnavailable || w.Code != status || env.ErrorClass != "provider_state_unavailable" {
		t.Fatalf("local persistence failure returned %d, %s", status, w.Body.String())
	}
	if len(env.Attempts) != 1 || env.Attempts[0].Status != http.StatusOK || !env.Attempts[0].LocalRefusal || !env.Attempts[0].BillingUncertain {
		t.Fatalf("local persistence failure changed acceptance or billing evidence: %+v", env.Attempts)
	}
	if probed, granted := h.gateway.health.Claim(p.Provider.ID); !probed || !granted {
		t.Fatalf("a local persistence failure changed provider health: probe=%v granted=%v", probed, granted)
	}
}

func TestRetainedListBindingFailureDoesNotSettleProviderSuccess(t *testing.T) {
	t.Parallel()
	pool, err := pgxpool.New(t.Context(), "postgres://unused@127.0.0.1:1/unused")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	h := newHarness(t, Config{}, Dependencies{Resources: resources.New(pool)})
	h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"file-native","vector_store_id":"store-other"}]}`)
	})
	p := testPinnedTarget(h, 0)
	probed, granted := openTestProbe(t, h, p.Provider.ID)
	if !granted || !probed {
		t.Fatal("could not claim half-open probe")
	}
	p.Hold = &dispatchHold{provider: p.Provider.ID, probed: probed}
	p.attempt.Price = rates.NewRoutingPrice(rates.RoutingPrice{Price: rates.Price{PerRequest: new("0.25")}})
	x := &Call{
		request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release},
		family:  wire.File, callIdentity: callIdentity{actor: "api_key_resource", keyID: h.keyID},
		retainedAdmission: &RetainedAdmissionPlan{billing: retainedPriced},
	}
	id := uuid.New()
	parent := &resources.Resource{UUID: id, ID: resources.LocalID("vector_store", id), Kind: "vector_store", UpstreamID: "store-native", TargetID: p.Target.ID}
	e := retainedapi.Endpoint{
		Family:    retainedapi.Family{Operation: "vector_store", Kind: "vector_store_file", Identity: "id", Collection: "data"},
		Method:    http.MethodGet,
		Action:    "list",
		Ancestors: []string{"vector_store"},
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/vector_stores/"+parent.ID+"/files", nil)
	w := httptest.NewRecorder()
	endpoint := strings.TrimRight(p.Provider.Endpoint, "/") + "/vector_stores/store-native/files"
	status := h.gateway.RetainedDispatch(w, r, RetainedDispatchRequest{
		Call: x, Pin: p, Endpoint: e, Resource: parent, Parent: parent, URL: endpoint,
		Fail: func(e *fault.Error) {
			h.gateway.Refuse(x, w, e)
		},
	})
	h.gateway.SettlePinnedHold(t.Context(), p.Hold)
	env := h.sink.last(t)
	if status != http.StatusServiceUnavailable || w.Code != status || env.ErrorClass != "provider_state_unavailable" {
		t.Fatalf("binding store failure was not reported as unavailable: %d, %s", status, w.Body.String())
	}
	if len(env.Attempts) != 1 || env.Attempts[0].Class != ClassProtocol || env.Attempts[0].UsageObserved || !h.gateway.settledCharge(x).IsZero() {
		t.Fatalf("unexpected attempt evidence or charge: %+v, %s", env.Attempts, h.gateway.settledCharge(x).String())
	}
	if probed, granted := h.gateway.health.Claim(p.Provider.ID); !probed || !granted {
		t.Fatalf("a local binding failure changed provider health: probe=%v granted=%v", probed, granted)
	}
}

func TestCompactionRejectsUnavailableProviderUsage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, usage string
	}{
		{name: "missing"},
		{name: "null", usage: `,"usage":null`},
		{name: "malformed", usage: `,"usage":{"input_tokens":-1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{}, Dependencies{Resources: resources.New(nil)})
			h.reissue(func(provider *routing.Provider) { provider.Kind = "openai" }, nil)
			h.rt.inputs = &routinginputs.Inputs{RefreshedAt: time.Now()}
			authority := h.rt.keys[fullKey]
			authority.Policy.AllowProviderState = true
			h.rt.keys[fullKey] = authority
			h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"object":"response.compaction","id":"cmp-native","output":[{"type":"compaction","encrypted_content":"abc"}]`+tc.usage+`}`)
			})
			response := h.do(t.Context(), http.MethodPost, "/v1/responses/compact", fullKey, []byte(`{"input":"compact this"}`), map[string]string{StateRouteHeader: routeSlug})
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			env := h.sink.last(t)
			if response.StatusCode != http.StatusBadGateway || env.ErrorClass != "provider_protocol_error" || env.Outcome != "failure" {
				t.Fatalf("compaction = %d %s, outcome = %s (%s)", response.StatusCode, body, env.Outcome, env.ErrorClass)
			}
			if len(env.Attempts) != 1 {
				t.Fatalf("attempts = %+v", env.Attempts)
			}
			attempt := env.Attempts[0]
			if attempt.Class != ClassProtocol || attempt.Committed || attempt.UsageObserved || attempt.UsageComplete || !attempt.BillingUncertain {
				t.Fatalf("compaction accounting = %+v", attempt)
			}
		})
	}
}

func TestCompactionBindingFailurePreservesProviderUsage(t *testing.T) {
	t.Parallel()
	pool, err := pgxpool.New(t.Context(), "postgres://unused:unused@localhost/unused")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	h := newHarness(t, Config{}, Dependencies{Resources: resources.New(pool)})
	h.reissue(func(provider *routing.Provider) { provider.Kind = "openai" }, nil)
	h.rt.inputs = &routinginputs.Inputs{RefreshedAt: time.Now()}
	authority := h.rt.keys[fullKey]
	authority.Policy.AllowProviderState = true
	h.rt.keys[fullKey] = authority
	h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"object":"response.compaction","id":"cmp-native","output":[{"type":"compaction","id":"item-native","file_id":"file-native"}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`)
	})
	response := h.do(t.Context(), http.MethodPost, "/v1/responses/compact", fullKey, []byte(`{"input":"compact this"}`), map[string]string{StateRouteHeader: routeSlug})
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	env := h.sink.last(t)
	if response.StatusCode != http.StatusServiceUnavailable || env.ErrorClass != "provider_state_unavailable" || env.Outcome != "failure" {
		t.Fatalf("compaction binding failure = %d %s, outcome = %s", response.StatusCode, body, env.Outcome)
	}
	if len(env.Attempts) != 1 {
		t.Fatalf("attempts = %+v", env.Attempts)
	}
	attempt := env.Attempts[0]
	if attempt.Class != ClassProtocol || !attempt.LocalRefusal || !attempt.UsageObserved || !attempt.UsageComplete || attempt.BillingUncertain ||
		attempt.Usage == nil || attempt.Usage.InputTokens != 10 || attempt.Usage.OutputTokens != 5 || attempt.Usage.TotalTokens != 15 {
		t.Fatalf("compaction accounting after binding failure = %+v, usage = %+v", attempt, attempt.Usage)
	}
}

type pinnedLimitCounter struct {
	unreachableValkey
	used map[string]bool
}

func (c pinnedLimitCounter) Do(_ context.Context, args ...string) (any, error) {
	switch len(args) {
	case 16:
		if c.used[args[3]] {
			return []any{int64(2), int64(0), "rpm", int64(30000), int64(42), int64(0)}, nil
		}
		c.used[args[3]] = true
		return []any{int64(2), int64(1), "ok", int64(0), int64(42), int64(0)}, nil
	case 11:
		delete(c.used, args[3])
		return int64(1), nil
	default:
		return nil, fmt.Errorf("unexpected rate command with %d arguments", len(args))
	}
}

func TestPinnedUnsentReservationRefundsAfterAnotherExchange(t *testing.T) {
	t.Parallel()
	runner, err := scripts.NewRunner(pinnedLimitCounter{used: make(map[string]bool)}, "test")
	if err != nil {
		t.Fatal(err)
	}
	admission := quota.NewAdmission(limitadmission.NewDurable(runner, nil), nil, nil, slog.New(slog.DiscardHandler))
	h := newHarness(t, Config{}, Dependencies{Admission: admission})
	h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	sent, unsent := testPinnedTarget(h, 0), testPinnedTarget(h, 1)
	for _, p := range []*Pin{sent, unsent} {
		p.Provider.Limits = &routing.Limits{RequestsPerMinute: new(int64(1))}
	}
	probed, granted := openTestProbe(t, h, unsent.Provider.ID)
	if !probed || !granted {
		t.Fatal("could not claim half-open probe")
	}
	h.gateway.health.ReleaseProbe(unsent.Provider.ID)
	reserve := func(p *Pin) gateResult {
		return acquireDispatchHold(t.Context(), h.gateway, &p.Provider, &p.Slot, quota.CombinedDemand(1), time.Minute, "", "", 0)
	}
	first := reserve(sent)
	if first.verdict != gateAdmitted {
		t.Fatalf("first admission: %+v", first)
	}
	sent.Hold = first.hold
	x := &Call{request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release}, family: wire.File, callIdentity: callIdentity{actor: "api_key_resource", keyID: h.keyID}}
	response, failure := h.gateway.PinnedDo(t.Context(), x, sent, http.MethodGet, sent.Provider.Endpoint, nil, "")
	if failure != nil {
		t.Fatalf("first exchange: %+v", failure)
	}
	_, err = response.ReadBody(1024)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	response.Exchange.Complete(ClassSuccess)
	h.gateway.SettlePinnedHold(t.Context(), sent.Hold)

	second := reserve(unsent)
	if second.verdict != gateAdmitted {
		t.Fatalf("second admission: %+v", second)
	}
	unsent.Hold = second.hold
	if _, failure := h.gateway.PinnedDo(t.Context(), x, unsent, http.MethodGet, "://invalid", nil, ""); failure == nil {
		t.Fatal("invalid endpoint dispatched")
	}
	h.gateway.SettlePinnedHold(t.Context(), unsent.Hold)
	if after := reserve(unsent); after.verdict != gateAdmitted {
		t.Fatalf("second provider remained blocked by a quota or probe: %+v", after)
	} else {
		h.gateway.ReleaseHold(t.Context(), after.hold)
	}
	if after := reserve(sent); after.verdict != gateRejected {
		t.Fatalf("first provider's dispatched request did not consume its allowance: %+v", after)
	}
}

func TestResourceListsRejectInvalidCursor(t *testing.T) {
	t.Parallel()
	pool, err := pgxpool.New(t.Context(), "postgres://unused@127.0.0.1:1/unused")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	spool, err := staging.NewSpool(t.TempDir(), staging.MinCapacityBytes, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spool.Close() })
	h := newHarness(t, Config{}, Dependencies{Resources: resources.New(pool), Staging: spool})
	for _, list := range []struct{ kind, other, path string }{
		{"batch", "file", "/v1/batches"},
		{"file", "batch", "/v1/files"},
	} {
		for _, tc := range []struct {
			name, cursor string
			status       int
			code, param  string
		}{
			{"missing separator", "not-an-id", 400, "invalid_value", "after"},
			{"malformed UUID", list.kind + "_not-a-uuid", 400, "invalid_value", "after"},
			{"wrong kind", resources.LocalID(list.other, uuid.New()), 400, "invalid_value", "after"},
			{"database failure", resources.LocalID(list.kind, uuid.New()), 500, "internal_error", ""},
		} {
			t.Run(list.kind+"/"+tc.name, func(t *testing.T) {
				response := h.do(t.Context(), http.MethodGet, list.path+"?after="+tc.cursor, fullKey, nil, nil)
				defer response.Body.Close()
				var body struct {
					Error struct {
						Code  string `json:"code"`
						Param string `json:"param"`
					} `json:"error"`
				}
				if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != tc.status || body.Error.Code != tc.code || body.Error.Param != tc.param {
					t.Errorf("cursor response = %d %#v, want %d %s at %s", response.StatusCode, body, tc.status, tc.code, tc.param)
				}
			})
		}
		t.Run(list.kind+"/trailing limit characters", func(t *testing.T) {
			response := h.do(t.Context(), http.MethodGet, list.path+"?limit=10abc", fullKey, nil, nil)
			defer response.Body.Close()
			var body struct {
				Error struct {
					Code  string `json:"code"`
					Param string `json:"param"`
				} `json:"error"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != 400 || body.Error.Code != "invalid_value" || body.Error.Param != "limit" {
				t.Errorf("limit response = %d %#v, want 400 invalid_value at limit", response.StatusCode, body)
			}
		})
	}
}

func TestListedResourcePageSkipsStaleRowsAndRefills(t *testing.T) {
	t.Parallel()
	all := []*resources.Resource{
		{ID: "stale-1"}, {ID: "hidden-1"}, {ID: "valid-1"}, {ID: "valid-2"}, {ID: "stale-2"}, {ID: "valid-3"},
	}
	fetchFrom := func(rows []*resources.Resource) func(resources.ListCursor, int) ([]*resources.Resource, *fault.Error) {
		return func(cursor resources.ListCursor, count int) ([]*resources.Resource, *fault.Error) {
			start := 0
			if cursor.ID != "" {
				start = len(rows)
				for i, row := range rows {
					if row.ID == cursor.ID {
						start = i + 1
						break
					}
				}
			}
			if start >= len(rows) {
				return nil, nil
			}
			return rows[start:min(start+count, len(rows))], nil
		}
	}
	authorize := func(row *resources.Resource) *fault.Error {
		switch {
		case strings.HasPrefix(row.ID, "stale"):
			return PinUnavailable()
		case strings.HasPrefix(row.ID, "hidden"):
			return fault.NotFoundError("not_found", "No resource with this identifier exists for this key.")
		case row.ID == "broken-1":
			return fault.ServerError(http.StatusInternalServerError, "internal_error", "The stored target could not be rebuilt.")
		}
		return nil
	}
	ids := func(page []*resources.Resource) []string {
		got := make([]string, len(page))
		for i, row := range page {
			got[i] = row.ID
		}
		return got
	}

	page, continuation, err := listedResourcePage(10, resources.ListCursor{}, fetchFrom(all), authorize)
	if err != nil {
		t.Fatalf("mixed page error: %v", err)
	}
	if continuation.ID != "" || !slices.Equal(ids(page), []string{"valid-1", "valid-2", "valid-3"}) {
		t.Fatalf("mixed page = %v continuation=%q, want only the valid rows and no continuation", ids(page), continuation.ID)
	}

	page, continuation, err = listedResourcePage(2, resources.ListCursor{}, fetchFrom(all), authorize)
	if err != nil {
		t.Fatalf("refilled page error: %v", err)
	}
	if continuation.ID != "valid-2" || !slices.Equal(ids(page), []string{"valid-1", "valid-2"}) {
		t.Fatalf("refilled page = %v continuation=%q, want [valid-1 valid-2] with a continuation", ids(page), continuation.ID)
	}
	page, continuation, err = listedResourcePage(2, continuation, fetchFrom(all), authorize)
	if err != nil {
		t.Fatalf("following page error: %v", err)
	}
	if continuation.ID != "" || !slices.Equal(ids(page), []string{"valid-3"}) {
		t.Fatalf("following page = %v continuation=%q, want [valid-3] and no continuation", ids(page), continuation.ID)
	}

	staleOnly := []*resources.Resource{{ID: "stale-1"}, {ID: "stale-2"}, {ID: "stale-3"}}
	page, continuation, err = listedResourcePage(2, resources.ListCursor{}, fetchFrom(staleOnly), authorize)
	if err != nil {
		t.Fatalf("all-stale page error: %v", err)
	}
	if continuation.ID != "" || len(page) != 0 {
		t.Fatalf("all-stale page = %v continuation=%q, want empty with no continuation", ids(page), continuation.ID)
	}

	broken := []*resources.Resource{{ID: "valid-1"}, {ID: "broken-1"}}
	if _, _, err := listedResourcePage(2, resources.ListCursor{}, fetchFrom(broken), authorize); err == nil || err.Status != http.StatusInternalServerError {
		t.Fatalf("broken row error = %v, want the request to fail with 500", err)
	}
	fetchErr := fault.ServerError(http.StatusInternalServerError, "internal_error", "The resource list could not be read.")
	failingFetch := func(resources.ListCursor, int) ([]*resources.Resource, *fault.Error) { return nil, fetchErr }
	if _, _, err := listedResourcePage(2, resources.ListCursor{}, failingFetch, authorize); err != fetchErr {
		t.Fatalf("fetch error = %v, want %v", err, fetchErr)
	}

	t.Run("bounded scan can continue without exposing denied IDs", func(t *testing.T) {
		denied := make([]*resources.Resource, 500)
		for i := range denied {
			denied[i] = &resources.Resource{ID: fmt.Sprintf("stale-%d", i)}
		}
		fetch := fetchFrom(append(denied, &resources.Resource{ID: "valid-1"}))
		server := Server{Deps: Dependencies{Digester: secrets.NewAuthKey(make([]byte, 32), "test-installation")}}
		page, cursor, err := server.ListedResourcePage("key-a", "pageToken", 2, "", fetch, authorize)
		if err != nil || len(page) != 0 || cursor == "" || strings.Contains(cursor, "stale-") {
			t.Fatalf("first page = %v cursor=%q err=%v, want empty page with an opaque continuation", ids(page), cursor, err)
		}
		if _, _, err := server.ListedResourcePage("key-b", "pageToken", 2, cursor, fetch, authorize); err == nil || err.Status != http.StatusBadRequest {
			t.Fatalf("another key's continuation error = %v, want 400", err)
		}
		page, cursor, err = server.ListedResourcePage("key-a", "pageToken", 2, cursor, fetch, authorize)
		if err != nil || cursor != "" || !slices.Equal(ids(page), []string{"valid-1"}) {
			t.Fatalf("next page = %v cursor=%q err=%v, want visible row and no continuation", ids(page), cursor, err)
		}
	})
}

func TestFileDownloadHeadersForceAttachmentForHTML(t *testing.T) {
	t.Parallel()
	const body = "<html><body>hello</body></html>"
	recorder := httptest.NewRecorder()
	SetFileDownloadHeaders(recorder.Header(), "text/html; charset=utf-8", int64(len(body)))
	if _, err := recorder.WriteString(body); err != nil {
		t.Fatal(err)
	}
	response := recorder.Result()
	defer response.Body.Close()
	if got := response.Header.Get("Content-Disposition"); got != "attachment" {
		t.Errorf("Content-Disposition = %q, want attachment", got)
	}
	if got := response.Header.Get("Content-Security-Policy"); !strings.Contains(got, "sandbox") {
		t.Errorf("Content-Security-Policy = %q, want sandbox policy", got)
	}
	if got := response.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := response.Header.Get("Content-Length"); got != fmt.Sprint(len(body)) {
		t.Errorf("Content-Length = %q", got)
	}
}
