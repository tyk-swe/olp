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
	"github.com/shopspring/decimal"

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
	f.h.want(f.owner, "POST", "/api/v1/pricing/revisions", map[string]any{"effective_at": time.Now().UTC().Format(time.RFC3339Nano), "prices": []any{map[string]any{"provider_kind": provider.Kind, "vendor_id": provider.VendorID, "model": "native-model", "operation": "generation", "currency": "USD", "input_per_million": "2", "cached_input_per_million": "1", "output_per_million": "3"}}}, idem("paid-vendor-price"), 201)
	card := f.h.want(f.owner, "POST", "/api/v1/rate-cards", map[string]any{"owner": map[string]any{"kind": "installation"}}, idem("paid-card"), 201)
	f.h.want(f.owner, "POST", "/api/v1/rate-cards/"+card["id"].(string)+"/revisions", map[string]any{"markup": "2"}, idem("paid-card-revision"), 201)
	eventually(t, "paid vendor price", 15*time.Second, func() bool {
		f.h.refresh()
		price := f.h.Runtime.RoutingInputs().Price(provider.Kind, provider.ID, provider.VendorID, "native-model", "generation", time.Now())
		return price != nil
	})
	policy := access.KeyPolicy{Scopes: []string{"inference"}, AllowedRoutes: []string{"coding"}}
	f.h.Gateway.Runtime = &codeForwardRuntime{Runtime: f.h.Runtime, pinned: f.h.Runtime.Release(), authority: access.Authority{ID: f.key, LookupID: f.key, ProjectID: &f.project, Policy: policy}}
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
