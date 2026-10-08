//go:build integration

package integration_test

import (
	"bytes"
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

	"github.com/coder/websocket"

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
func (r *codeForwardRuntime) Eligibility(string) runtime.Eligibility { return runtime.Eligible }
func (r *codeForwardRuntime) Authenticate(key string) (access.Authority, error) {
	if key != "olp-code-fixture" {
		return access.Authority{}, runtime.ErrInvalidKey
	}
	return r.authority, nil
}

type codeForwardAuthorizer struct{}

func (codeForwardAuthorizer) AuthorizeCode(_ context.Context, _ runtime.Configuration, account codemode.Account, _ codemode.Dispatch) (codemode.Authorization, error) {
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
	configuration := runtime.Configuration{Kind: "plugin", AuthMode: "grant", ProfileID: "codex-subscription", Endpoint: upstream}
	// The fixture supplies the publication image; runtime selection uses only it.
	pinned, err := runtime.NewRelease(access.NewID(), 1, &runtime.Snapshot{Generation: runtime.Generation{ID: access.NewID(), Ordinal: 1, ActivatedAt: time.Now()}, CodeRoutes: map[string]codemode.Route{"coding": f.route}, CodeConnections: map[string]runtime.Configuration{f.route.RevisionID + ":" + f.provider: configuration}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.h.Gateway.Runtime = &codeForwardRuntime{Runtime: f.h.Runtime, pinned: pinned, authority: access.Authority{ID: f.key, LookupID: lookup, ProjectID: &f.project, Policy: policy}}
	f.h.Gateway.CodeLedger = f.store
	f.h.Gateway.CodeAuthorizer = codeForwardAuthorizer{}
	c := limClient(t)
	limiter := limLimiter(t, c, limNamespace(t, c, "code-forwarding"))
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

func codeForwardAwait(t *testing.T, f *codeFixture, state string, count int) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var got int
		// Dispatch itself marks an attempt uncertain. Wait for terminal
		// accounting as well before inspecting settlement and cleanup.
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

func TestCodeForwardingHTTPHealthRequiresSuccessfulGeneration(t *testing.T) {
	for _, test := range []struct {
		name, wire, health string
	}{
		{"failed", "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n", "unknown"},
		{"interrupted", "data: {\"type\":\"response.created\"}\n\n", "unknown"},
		{"completed without usage", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n", "healthy"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCodeFixture(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, test.wire)
			}))
			defer upstream.Close()
			server, _, _ := codeForwardServer(t, f, upstream.URL)
			status, body := codeForwardRequest(t, server, "health-root", "")
			if status != http.StatusOK || string(body) != test.wire {
				t.Fatalf("response changed: %d %q", status, body)
			}
			codeForwardAwait(t, f, "uncertain", 1)
			var health string
			if err := f.h.Pool.QueryRow(t.Context(), `SELECT health FROM olp.code_accounts WHERE id=$1`, f.account).Scan(&health); err != nil || health != test.health {
				t.Fatalf("health=%s want %s: %v", health, test.health, err)
			}
			var reported *int64
			var finished *time.Time
			if err := f.h.Pool.QueryRow(t.Context(), `SELECT reported_tokens,finished_at FROM olp.code_attempts`).Scan(&reported, &finished); err != nil || reported != nil || finished == nil {
				t.Fatalf("uncertain usage or cleanup changed: reported=%v finished=%v: %v", reported, finished, err)
			}
		})
	}
}

func TestCodeForwardingDurableTreeUsageRateAndRefusals(t *testing.T) {
	f := newCodeFixture(t)
	var calls atomic.Int64
	wire := []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"total_tokens\":15}}}\n\n")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, []byte(`{ "model":"native-model", "input":"private prompt", "stream":true }`)) || r.Header.Get("Authorization") != "Bearer fixture-subscription" {
			t.Error("native request/auth changed")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(wire)
	}))
	defer up.Close()
	server, limiter, lookup := codeForwardServer(t, f, up.URL)
	for _, identity := range []struct{ conversation, parent string }{{"root", ""}, {"child", "root"}, {"grandchild", "child"}, {"root", ""}} {
		status, body := codeForwardRequest(t, server, identity.conversation, identity.parent)
		if status != 200 || !bytes.Equal(body, wire) {
			t.Fatalf("forward failed: %d %s", status, body)
		}
		codeForwardAwait(t, f, "settled", int(calls.Load()))
	}
	var accounts, roots, bindings int
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT count(DISTINCT account_id),count(DISTINCT root_id),count(*) FROM olp.code_bindings`).Scan(&accounts, &roots, &bindings); err != nil {
		t.Fatal(err)
	}
	if accounts != 1 || roots != 1 || bindings != 3 {
		t.Fatalf("tree rebound: accounts=%d roots=%d bindings=%d", accounts, roots, bindings)
	}
	var reported int64
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT sum(reported_tokens) FROM olp.code_attempts`).Scan(&reported); err != nil || reported != 60 {
		t.Fatalf("durable usage=%d err=%v", reported, err)
	}
	usage, err := limiter.ProviderUsage(t.Context(), lookup)
	if err != nil || usage.ConcurrentRequests != 0 || usage.TokensThisMinute != 60 {
		t.Fatalf("ordinary rate not reconciled: %+v %v", usage, err)
	}
	status, _ := codeForwardRequest(t, server, "orphan", "unknown")
	if status != 409 {
		t.Fatalf("orphan admitted: %d", status)
	}
	f.exec(t, `UPDATE olp.code_accounts SET health='quota_limited' WHERE id=$1`, f.account)
	status, _ = codeForwardRequest(t, server, "root", "")
	if status < 400 || calls.Load() != 4 {
		t.Fatal("unavailable pin dispatched or switched")
	}
	f.exec(t, `UPDATE olp.code_accounts SET health='healthy' WHERE id=$1`, f.account)
	f.exec(t, `UPDATE olp.code_bindings SET retired_at=now() WHERE conversation='root'`)
	status, _ = codeForwardRequest(t, server, "child", "root")
	if status != 410 || calls.Load() != 4 {
		t.Fatal("retired tree dispatched")
	}
	usage, err = limiter.ProviderUsage(t.Context(), lookup)
	if err != nil || usage.ConcurrentRequests != 0 || usage.RequestsThisMinute != 4 {
		t.Fatalf("refused requests leaked ordinary leases: %+v %v", usage, err)
	}
}

func TestCodeForwardingUnknownBoundRefusesOnlyConfiguredBudget(t *testing.T) {
	f := newCodeFixture(t)
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`)
	}))
	defer up.Close()
	server, _, _ := codeForwardServer(t, f, up.URL)
	f.exec(t, `INSERT INTO olp.code_token_budgets(id,project_id,daily_tokens,enabled,etag,created_by) VALUES($1,$2,100,true,$3,$4)`, access.NewID(), f.project, access.NewID(), f.user)
	status, body := codeForwardRequest(t, server, "budgeted", "")
	if status != 422 || !bytes.Contains(body, []byte("code_token_bound_unavailable")) || calls.Load() != 0 {
		t.Fatalf("unproven bound admitted: %d %s", status, body)
	}
	f.exec(t, `UPDATE olp.code_token_budgets SET enabled=false`)
	status, body = codeForwardRequest(t, server, "unbudgeted", "")
	if status != 200 || calls.Load() != 1 {
		t.Fatalf("nonbudgeted request refused: %d %s", status, body)
	}
	codeForwardAwait(t, f, "settled", 1)
}

func TestCodeForwardingWebSocketRechecksLivePool(t *testing.T) {
	f := newCodeFixture(t)
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		for {
			_, _, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			calls.Add(1)
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_ws","usage":{"total_tokens":3,"input_tokens":1,"output_tokens":2}}}`)); err != nil {
				return
			}
		}
	}))
	defer up.Close()
	server, limiter, lookup := codeForwardServer(t, f, up.URL)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, server.URL+"/code/coding/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer olp-code-fixture"}, "Thread-Id": {"socket-root"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	frame := []byte(`{"type":"response.create","model":"native-model","input":"private"}`)
	if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}
	codeForwardAwait(t, f, "settled", 1)
	path := "/api/v1/code/routes/" + f.route.ID
	input := map[string]any{"project_id": f.project, "slug": f.route.Slug, "pool_id": strings.ToUpper(f.pool), "models": f.route.Models, "enabled": true}
	draft := f.h.want(f.owner, "PUT", path, input, map[string]string{"If-Match": `"` + f.route.ETag + `"`}, 200)
	f.h.want(f.owner, "POST", path+"/publish", nil, withMatch(draft, idem("uppercase-pool-publication")), 200)
	if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatal(err)
	}
	if _, response, err := conn.Read(ctx); err != nil || !bytes.Contains(response, []byte(`"type":"response.completed"`)) {
		t.Fatalf("pool UUID spelling interrupted the pinned socket: %s %v", response, err)
	}
	codeForwardAwait(t, f, "settled", 2)
	f.exec(t, `DELETE FROM olp.code_pool_keys WHERE api_key_id=$1`, f.key)
	if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatal(err)
	}
	_, _, err = conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation || calls.Load() != 2 {
		t.Fatalf("long-lived socket bypassed live authority: %v", err)
	}
	usage, err := limiter.ProviderUsage(t.Context(), lookup)
	if err != nil || usage.ConcurrentRequests != 0 || usage.RequestsThisMinute != 2 {
		t.Fatalf("refused socket generation leaked lease: %+v %v", usage, err)
	}
}

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

func TestCodeForwardingLostCompletionIsDurablyUnknown(t *testing.T) {
	f := newCodeFixture(t)
	var calls atomic.Int64
	wire := []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"private response\"}\n\n")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "999")
		_, _ = w.Write(wire)
	}))
	defer up.Close()
	server, limiter, lookup := codeForwardServer(t, f, up.URL)
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/responses", strings.NewReader(`{"model":"native-model","input":"private prompt"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer olp-code-fixture")
	r.Header.Set("Thread-Id", "lost")
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(response.Body)
	if response.StatusCode != 200 || !bytes.Equal(body, wire) || readErr != io.ErrUnexpectedEOF || calls.Load() != 1 {
		t.Fatalf("loss replayed or synthesized bytes: status=%d err=%v body=%s", response.StatusCode, readErr, body)
	}
	codeForwardAwait(t, f, "uncertain", 1)
	var reported *int64
	var metadata string
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT reported_tokens,row_to_json(a)::text FROM olp.code_attempts a`).Scan(&reported, &metadata); err != nil {
		t.Fatal(err)
	}
	if reported != nil || strings.Contains(metadata, "private") || strings.Contains(metadata, "Bearer") {
		t.Fatal("uncertainty fabricated usage or stored private payload")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		usage, err := limiter.ProviderUsage(t.Context(), lookup)
		if err != nil {
			t.Fatal(err)
		}
		if usage.ConcurrentRequests == 0 {
			if usage.TokensThisMinute <= 0 || usage.RequestsThisMinute != 1 {
				t.Fatalf("lost generation refunded dispatched rate: %+v", usage)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lost generation leaked concurrency")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
