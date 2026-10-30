//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"
	"github.com/shopspring/decimal"
	"github.com/tyk-swe/olp/internal/management/contract"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/runtime"
)

type codeForwardRuntime struct {
	gateway.Runtime
	pinned    *runtime.Release
	authority access.Authority
}

func (r *codeForwardRuntime) Release() *runtime.Release              { return r.pinned }
func (r *codeForwardRuntime) Eligibility(string) routing.Eligibility { return routing.Eligible }
func (r *codeForwardRuntime) Authenticate(key string) (*access.Authority, error) {
	if key != "olp-code-fixture" {
		return nil, runtime.ErrInvalidKey
	}
	return &r.authority, nil
}

type codeForwardAuthorizer struct{}

func (codeForwardAuthorizer) AuthorizeCode(_ context.Context, _ routing.Configuration, account codemode.Account, _ codemode.Dispatch) (codemode.Authorization, error) {
	return codemode.Authorization{Principal: account.Principal, CredentialID: account.CredentialID, Headers: http.Header{"Authorization": {"Bearer fixture-subscription"}}}, nil
}

func codeForwardServer(t *testing.T, f *codeFixture, upstream string) (*httptest.Server, *limits.Limiter, string) {
	t.Helper()
	lookup := limLookup()
	policy := access.KeyPolicy{Scopes: []string{"inference"}, AllowedRoutes: []string{"coding"}}
	one := int64(1)
	perMinute := int64(100)
	policy.MaxConcurrency = &one
	policy.RequestsPerMinute = &perMinute
	tokens := int64(10000)
	policy.TokensPerMinute = &tokens
	document, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE olp.api_keys SET lookup_id=$2,policy=$3 WHERE id=$1`, f.key, lookup, document)
	configuration := routing.Configuration{Kind: "plugin", AuthMode: "grant", ProfileID: "codex-subscription", Endpoint: upstream}
	// The fixture supplies the publication image; runtime selection uses only it.
	pinned, err := runtime.NewRelease(access.NewID(), 1, &routing.Snapshot{Generation: routing.Generation{ID: access.NewID(), Ordinal: 1, ActivatedAt: time.Now()}, CodeRoutes: map[string]codemode.Route{"coding": f.route}, CodeConnections: map[string]routing.Configuration{f.route.RevisionID + ":" + f.provider: configuration}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.h.Gateway.Runtime = &codeForwardRuntime{Runtime: f.h.Runtime, pinned: pinned, authority: access.Authority{ID: f.key, LookupID: lookup, ProjectID: &f.project, Policy: policy}}
	f.h.Gateway.CodeLedger = f.store
	f.h.Gateway.CodeAuthorizer = codeForwardAuthorizer{}
	_, _, limiter := limSetup(t, "code-forwarding")
	f.h.Gateway.Admission = gateway.NewAdmission(limiter, func() limits.OutagePolicy { return limits.FailClosed }, slog.New(slog.DiscardHandler))
	mux := http.NewServeMux()
	f.h.Gateway.RegisterCode(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, limiter, lookup
}

func codeForwardRequest(t *testing.T, server *httptest.Server, identity, parent string) (int, []byte) {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/responses", strings.NewReader(`{ "model":"native-model", "input":"private prompt", "stream":true }`))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer olp-code-fixture")
	r.Header.Set("Thread-Id", identity)
	if parent != "" {
		r.Header.Set("X-Codex-Parent-Thread-Id", parent)
	}
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, body
}

// codeForwardAwait waits for count finished attempts in state. A dispatched
// attempt is already uncertain while its response is still being finished.
func codeForwardAwait(t *testing.T, f *codeFixture, state string, count int) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var got int
		if err := f.h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.code_attempts WHERE state=$1 AND finished_at IS NOT NULL`, state).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got == count {
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("state %s count=%d want %d", state, got, count)
		}
	}
}

// All rate assertions below measure one fixed Valkey server minute.

// Each request keeps its 16-token reservation: reported usage (15) can
// only raise the count, never refund the local allowance.

func TestCodeForwardingWebSocketUnavailableCloseAppliesAccountCooldown(t *testing.T) {
	for _, origin := range []string{"unavailable", "disconnect", "client"} {
		t.Run(origin, func(t *testing.T) {
			f := newCodeFixture(t)
			var calls atomic.Int64
			started := make(chan struct{})
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				if _, _, err := conn.Read(r.Context()); err != nil {
					t.Error(err)
					return
				}
				if calls.Add(1) != 1 {
					_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_ws","usage":{"total_tokens":3,"input_tokens":1,"output_tokens":2}}}`))
					return
				}
				close(started)
				switch origin {
				case "client":
					_, _, _ = conn.Read(r.Context())
				case "unavailable":
					_ = conn.Close(websocket.StatusTryAgainLater, "controlled unavailable")
				}
			}))
			defer up.Close()
			server, _, _ := codeForwardServer(t, f, up.URL)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			headers := http.Header{"Authorization": {"Bearer olp-code-fixture"}, "Thread-Id": {"socket-root"}}
			conn, _, err := websocket.Dial(ctx, server.URL+"/code/coding/responses", &websocket.DialOptions{HTTPHeader: headers})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"native-model","input":"private"}`)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("no upstream generation")
			}
			if origin == "client" {
				_ = conn.CloseNow()
			} else if _, _, err := conn.Read(ctx); err == nil {
				t.Fatal("upstream close was not relayed")
			}
			codeForwardAwait(t, f, "uncertain", 1)
			var health string
			var until *time.Time
			if err := f.h.Pool.QueryRow(ctx, `SELECT health,unavailable_until FROM olp.code_accounts WHERE id=$1`, f.account).Scan(&health, &until); err != nil {
				t.Fatal(err)
			}
			if origin == "unavailable" {
				if health != "unavailable" || until == nil || time.Until(*until) < 45*time.Second || time.Until(*until) > time.Minute {
					t.Fatalf("one-minute unavailable cooldown missing: health=%s until=%v", health, until)
				}
				var outcome, outcomeOrigin string
				if err := f.h.Pool.QueryRow(ctx, `SELECT outcome,outcome_origin FROM olp.code_attempts`).Scan(&outcome, &outcomeOrigin); err != nil {
					t.Fatal(err)
				}
				if outcome != "transport_error" || outcomeOrigin != "gateway" {
					t.Fatalf("transport outcome changed: %s/%s", outcomeOrigin, outcome)
				}
				retry, response, err := websocket.Dial(ctx, server.URL+"/code/coding/responses", &websocket.DialOptions{HTTPHeader: headers})
				if retry != nil {
					retry.CloseNow()
				}
				if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
					t.Fatalf("reconnect bypassed cooldown: response=%v err=%v", response, err)
				}
				// The rate lease releases just after the attempt records
				// uncertain, so a new conversation can still meet 429 for a beat;
				// poll through it until the account gate answers.
				deadline := time.Now().Add(5 * time.Second)
				status, body := codeForwardRequest(t, server, "new-conversation", "")
				for status == http.StatusTooManyRequests && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
					status, body = codeForwardRequest(t, server, "new-conversation", "")
				}
				if status != http.StatusServiceUnavailable {
					t.Fatalf("new conversation bypassed cooldown: %d %s", status, body)
				}
				if calls.Load() != 1 {
					t.Fatal("cooled-down account was dispatched again")
				}
				return
			}
			if health != "unknown" || until != nil {
				t.Fatalf("%s cooled down the account: health=%s until=%v", origin, health, until)
			}
			if origin == "client" {
				return
			}
			var outcome, outcomeOrigin string
			if err := f.h.Pool.QueryRow(ctx, `SELECT outcome,outcome_origin FROM olp.code_attempts`).Scan(&outcome, &outcomeOrigin); err != nil {
				t.Fatal(err)
			}
			if outcome != "transport_error" || outcomeOrigin != "gateway" {
				t.Fatalf("transport outcome changed: %s/%s", outcomeOrigin, outcome)
			}
			retry, _, err := websocket.Dial(ctx, server.URL+"/code/coding/responses", &websocket.DialOptions{HTTPHeader: headers})
			if err != nil {
				t.Fatalf("transport noise refused reconnect: %v", err)
			}
			defer retry.CloseNow()
			if err := retry.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"native-model","input":"private"}`)); err != nil {
				t.Fatal(err)
			}
			if _, _, err := retry.Read(ctx); err != nil {
				t.Fatalf("recovery generation did not return: %v", err)
			}
			codeForwardAwait(t, f, "settled", 1)
			if calls.Load() != 2 {
				t.Fatalf("recovery did not dispatch: calls=%d", calls.Load())
			}
		})
	}
}

func TestCodePaidVendorPricingSettlesDistinctChargeAndEnforcesSpend(t *testing.T) {
	var dispatched atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dispatched.Add(1)
		if r.Header.Get("Authorization") != "Bearer fixture-paid-secret" {
			t.Error("paid request did not use its encrypted provider credential")
		}
		_, _ = io.WriteString(w, `{"id":"paid-priced","object":"response","model":"native-model","status":"completed","output":[],"usage":{"input_tokens":6,"output_tokens":1,"total_tokens":7,"input_tokens_details":{"cached_tokens":4}}}`)
	}))
	t.Cleanup(upstream.Close)
	f, provider, _ := newPaidCodeFixture(t, upstream.URL)
	lookup := limLookup()
	f.exec(t, `UPDATE olp.api_keys SET lookup_id=$2 WHERE id=$1`, f.key, lookup)
	f.h.want(f.owner, "POST", "/api/v1/pricing/revisions", map[string]any{"effective_at": time.Now().UTC().Format(time.RFC3339Nano), "prices": []any{map[string]any{"provider_kind": provider.Kind, "vendor_id": provider.VendorID, "model": "native-model", "operation": "generation", "currency": "USD", "input_per_million": "2", "cached_input_per_million": "1", "output_per_million": "3"}}}, idem("paid-vendor-price"), 201)
	card := f.h.want(f.owner, "POST", "/api/v1/rate-cards", map[string]any{"owner": map[string]any{"kind": "installation"}}, idem("paid-card"), 201)
	f.h.want(f.owner, "POST", "/api/v1/rate-cards/"+card["id"].(string)+"/revisions", map[string]any{"markup": "2"}, idem("paid-card-revision"), 201)
	eventually(t, "paid vendor price", 15*time.Second, func() bool {
		f.h.refresh()
		price := f.h.Runtime.RoutingInputs().Price(provider.Kind, provider.ID, provider.VendorID, "native-model", "generation", time.Now())
		return price != nil
	})
	policy := access.KeyPolicy{Scopes: []string{"inference"}, AllowedRoutes: []string{"coding"}}
	f.h.Gateway.Runtime = &codeForwardRuntime{Runtime: f.h.Runtime, pinned: f.h.Runtime.Release(), authority: access.Authority{ID: f.key, LookupID: lookup, ProjectID: &f.project, Policy: policy}}
	f.h.Gateway.CodeLedger = f.store
	_, _, limiter := limSetup(t, "paid-vendor-cost")
	f.h.Gateway.Admission = gateway.NewAdmission(limiter, nil, slog.New(slog.DiscardHandler))
	f.h.Gateway.Admission.PendingCostAccountingReady = (&limits.DebitStore{Pool: f.h.Pool, Limiter: limiter}).CheckOwnerAccounting
	await := endUserAccounting(t, f.h, limiter)
	mux := http.NewServeMux()
	f.h.Gateway.RegisterCode(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	call := func() (int, []byte) {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/responses", strings.NewReader(`{"model":"native-model","input":"hello","max_output_tokens":1}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer olp-code-fixture")
		request.Header.Set("Session_id", "paid-cost")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, body
	}
	if status, body := call(); status != 200 {
		t.Fatalf("paid request refused %d %s", status, body)
	}
	await()
	var cost, charge *string
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT estimated_cost::text,charge::text FROM olp.attempt_usage_facts WHERE provider_id=$1`, provider.ID).Scan(&cost, &charge); err != nil {
		t.Fatal(err)
	}
	if cost == nil || charge == nil {
		t.Fatalf("vendor-scoped paid usage was unpriced %v %v", cost, charge)
	}
	measuredCost, costErr := decimal.NewFromString(*cost)
	measuredCharge, chargeErr := decimal.NewFromString(*charge)
	expected := decimal.NewFromInt((6-4)*2 + 4*1 + 1*3).Div(decimal.NewFromInt(1_000_000))
	if costErr != nil || chargeErr != nil || !measuredCost.Equal(expected) || !measuredCharge.Equal(expected.Mul(decimal.NewFromInt(2))) {
		t.Fatalf("paid cache cost/charge diverged from admission prices %s %s", *cost, *charge)
	}
	exhausted := access.KeyPolicy{Scopes: policy.Scopes, AllowedRoutes: policy.AllowedRoutes, CostBasis: "charge", DailyCostLimit: charge}
	encoded, err := json.Marshal(exhausted)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE olp.api_keys SET policy=$2 WHERE id=$1`, f.key, encoded)
	if status, body := call(); status != 429 || !strings.Contains(string(body), "code_budget_exhausted") {
		t.Fatalf("settled charge did not enforce the next admission %d %s", status, body)
	}
	if dispatched.Load() != 1 {
		t.Fatal("exhausted paid spend dispatched another upstream request")
	}
}

type codePaidFallbackForwardFixture struct {
	*codeFixture
	server             *httptest.Server
	write              contract.CodeRouteWrite
	paid, subscription *atomic.Bool
}

func newCodePaidFallbackForwardFixture(t *testing.T) codePaidFallbackForwardFixture {
	t.Helper()
	f := newCodeFixtureWithProfile(t, "codex-subscription")
	paid, subscription := new(atomic.Bool), new(atomic.Bool)
	paidPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paid.Store(true)
		if r.Header.Get("Authorization") != "Bearer fixture-paid-secret" {
			t.Error("fallback credential missing")
		}
		var request struct {
			Model string `json:"model"`
			Input string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != "alternate-model" || request.Input != "complete portable input" {
			t.Errorf("fallback selected model or complete input lost %+v %v", request, err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"paid-fallback","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`)
	}))
	subscriptionPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { subscription.Store(true); w.WriteHeader(500) }))
	t.Cleanup(paidPeer.Close)
	t.Cleanup(subscriptionPeer.Close)
	provider, slot := newCodePaidProvider(t, f.h, f.owner, f.project, paidPeer.URL, "alternate-model")
	f.exec(t, `UPDATE olp.providers SET configuration=jsonb_set(configuration::jsonb,'{endpoint}',to_jsonb($2::text))::json WHERE id=$1`, f.provider, subscriptionPeer.URL)
	f.exec(t, `UPDATE olp.code_accounts SET models=$2 WHERE id=$1`, f.account, []byte(`["native-model","alternate-model"]`))
	targets := map[string]contract.CodePaidTarget{"alternate-model": {ProviderId: uuid.MustParse(provider), SlotId: uuid.MustParse(slot.ID)}}
	pools := map[string][]string{"native-model": {"native-model", "alternate-model"}}
	write := contract.CodeRouteWrite{ProjectId: uuid.MustParse(f.project), PoolId: uuid.MustParse(f.pool), Slug: f.route.Slug, Models: []string{"native-model", "alternate-model"}, Enabled: true, ModelRewriting: new(true), ModelPools: &pools, PaidParticipation: new(true), InitialRecoveryAttempts: new(1), PaidFallbackTargets: &targets}
	saved := f.h.want(f.owner, "PUT", "/api/v1/code/routes/"+f.route.ID, write, etagHeader(f.routeRecord), 200)
	published := f.h.want(f.owner, "POST", "/api/v1/code/routes/"+f.route.ID+"/publish", nil, withMatch(saved, idem("fallback-forward")), 200)
	f.route = codePublicDecode[codemode.Route](t, published)
	f.routeRecord = published
	price := contract.PricingRevisionRequest{EffectiveAt: time.Now().UTC(), Prices: []contract.PriceRequest{{ProviderKind: "openai", Model: "alternate-model", Operation: "generation", Currency: "USD", InputPerMillion: nullable.NewNullableWithValue("1"), OutputPerMillion: nullable.NewNullableWithValue("2")}}}
	f.h.want(f.owner, "POST", "/api/v1/pricing/revisions", price, idem("fallback-forward-price"), 201)
	eventually(t, "fallback price and frozen route", 15*time.Second, func() bool {
		f.h.refresh()
		return f.h.Runtime.RoutingInputs().Price("openai", provider, "openai", "alternate-model", "generation", time.Now()) != nil && f.h.Runtime.Release().Snapshot.CodeRoutes["coding"].RevisionID == f.route.RevisionID
	})
	f.exec(t, `UPDATE olp.code_accounts SET models=$2 WHERE id=$1`, f.account, []byte(`["alternate-model"]`))
	policy := access.KeyPolicy{Scopes: []string{"inference"}, AllowedRoutes: []string{"coding"}}
	f.h.Gateway.Runtime = &codeForwardRuntime{Runtime: f.h.Runtime, pinned: f.h.Runtime.Release(), authority: access.Authority{ID: f.key, LookupID: f.key, ProjectID: &f.project, Policy: policy}}
	f.h.Gateway.CodeLedger = f.store
	f.h.Gateway.CodeAuthorizer = codeForwardAuthorizer{}
	_, _, limiter := limSetup(t, "fallback-forward")
	f.h.Gateway.Admission = gateway.NewAdmission(limiter, nil, slog.New(slog.DiscardHandler))
	mux := http.NewServeMux()
	f.h.Gateway.RegisterCode(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return codePaidFallbackForwardFixture{codeFixture: f, server: server, write: write, paid: paid, subscription: subscription}
}

func (f codePaidFallbackForwardFixture) request(t *testing.T, conversation string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), "POST", f.server.URL+"/code/coding/responses", strings.NewReader(`{"model":"native-model","input":"complete portable input","max_output_tokens":1}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer olp-code-fixture")
	request.Header.Set("Thread-Id", conversation)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, body
}

func TestCodePaidFallbackChecksEverySubscriptionModelBeforeSpending(t *testing.T) {
	f := newCodePaidFallbackForwardFixture(t)
	if status, body := f.request(t, "eligible-alternate"); status != 409 || !strings.Contains(string(body), "code_paid_fallback_not_needed") {
		t.Fatalf("eligible alternate subscription bypassed %d %s", status, body)
	}
	if f.paid.Load() || f.subscription.Load() {
		t.Fatal("admission refusal dispatched upstream")
	}
	f.exec(t, `UPDATE olp.code_accounts SET health='unavailable' WHERE id=$1`, f.account)
	if status, body := f.request(t, "exhausted-alternate"); status != 200 || !strings.Contains(string(body), "paid-fallback") {
		t.Fatalf("exhausted authorized fallback refused %d %s", status, body)
	}
	if !f.paid.Load() || f.subscription.Load() {
		t.Fatal("exhausted work dispatched to the wrong destination")
	}
}

func TestCodeAliasRepointPreservesPaidAndSubscriptionContinuation(t *testing.T) {
	for _, scenario := range []struct {
		name, pinned, repointed, response string
		paid                              bool
	}{{"paid", "alternate-model", "native-model", "paid-fallback", true}, {"subscription", "native-model", "alternate-model", "subscription-alias-owner", false}} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newCodePaidFallbackForwardFixture(t)
			var subscriptionCalls atomic.Int64
			subscription := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				subscriptionCalls.Add(1)
				var body struct {
					Model string `json:"model"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "native-model" || r.Header.Get("Authorization") != "Bearer fixture-subscription" {
					t.Errorf("subscription continuation changed model or owner %+v %v", body, err)
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"subscription-alias-owner","model":"native-model","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`)
			}))
			t.Cleanup(subscription.Close)
			f.exec(t, `UPDATE olp.providers SET configuration=jsonb_set(configuration::jsonb,'{endpoint}',to_jsonb($2::text))::json WHERE id=$1`, f.provider, subscription.URL)
			f.exec(t, `UPDATE olp.code_accounts SET models=$2 WHERE id=$1`, f.account, []byte(`["native-model"]`))
			publish := func(target string, current codemode.Route) codemode.Route {
				t.Helper()
				path := "/api/v1/code/routes/" + current.ID
				aliases := map[string]string{"assistant": target}
				write := contract.CodeRouteWrite{ProjectId: uuid.MustParse(f.project), PoolId: uuid.MustParse(f.pool), Slug: current.Slug, Models: current.Models, Enabled: true, ModelRewriting: new(true), Aliases: &aliases, PaidParticipation: new(true), PaidTargets: f.write.PaidFallbackTargets}
				saved := f.h.want(f.owner, "PUT", path, write, map[string]string{"If-Match": `"` + current.ETag + `"`}, 200)
				published := f.h.want(f.owner, "POST", path+"/publish", nil, withMatch(saved, idem("alias-"+target)), 200)
				route := codePublicDecode[codemode.Route](t, published)
				eventually(t, "alias publication", 15*time.Second, func() bool {
					f.h.refresh()
					return f.h.Runtime.Release().Snapshot.CodeRoutes[route.Slug].RevisionID == route.RevisionID
				})
				policy := access.KeyPolicy{Scopes: []string{"inference"}, AllowedRoutes: []string{route.Slug}}
				f.h.Gateway.Runtime = &codeForwardRuntime{Runtime: f.h.Runtime, pinned: f.h.Runtime.Release(), authority: access.Authority{ID: f.key, LookupID: f.key, ProjectID: &f.project, Policy: policy}}
				return route
			}
			call := func(conversation, previous string) {
				t.Helper()
				payload, err := json.Marshal(struct {
					Model    string `json:"model"`
					Input    string `json:"input"`
					Max      int    `json:"max_output_tokens"`
					Previous string `json:"previous_response_id,omitempty"`
				}{"assistant", "complete portable input", 1, previous})
				if err != nil {
					t.Fatal(err)
				}
				request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL+"/code/coding/responses", strings.NewReader(string(payload)))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Authorization", "Bearer olp-code-fixture")
				request.Header.Set("Thread-Id", conversation)
				response, err := http.DefaultClient.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				if err != nil || response.StatusCode != 200 || !strings.Contains(string(body), scenario.response) {
					t.Fatalf("published alias lost its original exact owner %d %s %v", response.StatusCode, body, err)
				}
			}
			pinned := publish(scenario.pinned, f.route)
			call("alias-owner-root", "")
			codeForwardAwait(t, f.codeFixture, "settled", 1)
			publish(scenario.repointed, pinned)
			call("alias-owner-root", "")
			codeForwardAwait(t, f.codeFixture, "settled", 2)
			call("alias-owner-child", scenario.response)
			codeForwardAwait(t, f.codeFixture, "settled", 3)
			if scenario.paid && subscriptionCalls.Load() != 0 || !scenario.paid && (subscriptionCalls.Load() != 3 || f.paid.Load()) {
				t.Fatal("alias repoint dispatched through a different ownership class")
			}
		})
	}
}

func TestCodePaidFallbackCurrentSubstitutionPolicyOverridesCachedRelease(t *testing.T) {
	for _, policy := range []string{"pool removed", "rewriting disabled"} {
		t.Run(policy, func(t *testing.T) {
			f := newCodePaidFallbackForwardFixture(t)
			f.exec(t, `UPDATE olp.code_accounts SET health='unavailable',models=$2 WHERE id=$1`, f.account, []byte(`["native-model","alternate-model"]`))
			write := f.write
			write.ModelPools = nil
			if policy == "rewriting disabled" {
				write.ModelRewriting = new(false)
			}
			saved := f.h.want(f.owner, "PUT", "/api/v1/code/routes/"+f.route.ID, write, etagHeader(f.routeRecord), 200)
			f.h.want(f.owner, "POST", "/api/v1/code/routes/"+f.route.ID+"/publish", nil, withMatch(saved, idem("revoke-fallback-substitution")), 200)
			if status, body := f.request(t, "revoked-substitution"); status != 409 || !strings.Contains(string(body), "code_recovery_policy_changed") {
				t.Fatalf("cached release bypassed current substitution policy %d %s", status, body)
			}
			if f.paid.Load() || f.subscription.Load() {
				t.Fatal("revoked substitution dispatched upstream")
			}
		})
	}
}
