//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/grants"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// grantRefresher is a worker replica's grant refresh task over the harness's
// database, with its own plugin host, which runs unconfined plugins where the
// harness's deployment enables them.
func grantRefresher(t *testing.T, h *accessHarness) *grants.Refresher {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pluginRuntime, err := plugins.NewRuntime(t.Context(), plugins.Interpreted, plugins.DefaultLimits, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pluginRuntime.Close(context.Background()) })
	var unconfined *plugins.Unconfined
	if h.UnconfinedDir != "" {
		unconfined = plugins.NewUnconfined(h.UnconfinedDir, plugins.DefaultLimits, log)
	}
	host := plugins.NewHost(pluginRuntime, unconfined, h.Pool)
	t.Cleanup(func() { host.Close(context.Background()) })
	policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	return &grants.Refresher{Pool: h.Pool, Keys: h.Server.Keys, Installation: h.Server.Installation, Plugins: host, Egress: &policy, Log: log}
}

// enrollGrant enrolls a grant for a reference plugin provider at path and
// returns the grant's credential version.
func enrollGrant(t *testing.T, h *accessHarness, owner *browser, path string) string {
	t.Helper()
	enrollment := startGrantEnrollment(t, h, owner, path)
	return continueGrantEnrollment(h, owner, path, enrollment, signIn(t, enrollment).String(), 201)["credential_id"].(string)
}

// servingGrant enrolls a grant for a reference plugin provider, publishes the
// provider behind a strict route and returns the credential version and an
// API key for the route.
func servingGrant(t *testing.T, h *accessHarness, owner *browser, path string) (string, string) {
	t.Helper()
	credentialID := enrollGrant(t, h, owner, path)
	certifyPluginProvider(t, h, owner, path)
	key := publishRoute(t, h, owner, strictDraft(fidelityDraft("reference-account", strings.TrimPrefix(path, "/api/v1/providers/"))), "Account")
	h.refresh()
	return credentialID, key
}

// grantState is a grant's refresh state as stored, with the number of
// refresh tokens held for it.
type grantState struct {
	generation       int64
	refreshToken     *string
	attempt          *string
	expires, refresh *time.Time
	lapsed           *time.Time
	failures         int
	failure          *string
	refreshTokens    int
}

func readGrant(t *testing.T, h *accessHarness, credentialID string) grantState {
	t.Helper()
	var g grantState
	if err := h.Pool.QueryRow(t.Context(), `SELECT generation,refresh_token_id::text,refresh_attempt_id::text,expires_at,refresh_at,lapsed_at,refresh_failures,refresh_failure,
		(SELECT count(*) FROM olp.secrets s WHERE s.id=g.refresh_token_id AND s.purpose=$2) FROM olp.provider_grants g WHERE credential_id=$1`, credentialID, secrets.ProviderGrantRefresh).
		Scan(&g.generation, &g.refreshToken, &g.attempt, &g.expires, &g.refresh, &g.lapsed, &g.failures, &g.failure, &g.refreshTokens); err != nil {
		t.Fatal(err)
	}
	return g
}

// dueNow makes a grant due a refresh, as time passing would.
func dueNow(t *testing.T, h *accessHarness, credentialID string) {
	t.Helper()
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp.provider_grants SET refresh_at=now() WHERE credential_id=$1", credentialID); err != nil {
		t.Fatal(err)
	}
}

func pass(t *testing.T, r *grants.Refresher) bool {
	t.Helper()
	outcome, progress := r.Pass(t.Context())
	if outcome != usage.OutcomeSuccess {
		t.Fatalf("grant refresh pass outcome %v", outcome)
	}
	return progress
}

func chat(t *testing.T, h *accessHarness, key string) int {
	t.Helper()
	status, _, _ := h.gateway("POST", "/v1/chat/completions", key, map[string]any{"model": "reference-account", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	return status
}

// An hour's access token refreshes ten minutes before it expires.

// Gateway code reads only access tokens: with the refresh token made
// unreadable, a poll serves the new access token.

// Cancel exactly after the database commits the refresh claim and before the
// plugin can dispatch HTTP, without relying on worker scheduling or sleeps.
type cancelClaimedRefresh struct {
	cancel context.CancelFunc
}

type refreshClaimContextKey struct{}

func (tracer cancelClaimedRefresh) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "UPDATE olp.provider_grants SET refresh_attempt_id=") {
		return context.WithValue(ctx, refreshClaimContextKey{}, true)
	}
	return ctx
}

func (tracer cancelClaimedRefresh) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(refreshClaimContextKey{}) == true && data.Err == nil && data.CommandTag.RowsAffected() == 1 {
		tracer.cancel()
	}
}

// Another worker can reuse the token once backoff ends, including after
// the abandoned-attempt recovery deadline would have elapsed.

// Losing the advisory lock's session while the upstream rotates a token
// must not let another worker spend it again or discard the rotated tokens.
func TestGrantRefreshSurvivesLossOfItsLockSession(t *testing.T) {
	for _, outcome := range []string{"completed", "abandoned", "revoked", "uninstalled"} {
		t.Run(outcome, func(t *testing.T) { testGrantRefreshLockLoss(t, outcome) })
	}
}

func testGrantRefreshLockLoss(t *testing.T, outcome string) {
	t.Helper()
	h := newAccessHarness(t)
	owner := h.owner()
	authority := testutil.NewOAuthServer(t)
	refreshing, release := make(chan struct{}), make(chan struct{})
	resume := sync.OnceFunc(func() { close(release) })
	var refreshes atomic.Int64
	delayed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Form.Get("grant_type") == "refresh_token" && refreshes.Add(1) == 1 {
			response := httptest.NewRecorder()
			authority.Config.Handler.ServeHTTP(response, r)
			close(refreshing)
			<-release
			for key, values := range response.Header() {
				w.Header()[key] = values
			}
			w.WriteHeader(response.Code)
			w.Write(response.Body.Bytes())
			return
		}
		authority.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(delayed.Close)
	defer resume()
	upstream := newGrantUpstream(t, authority)
	digest := installReferencePlugin(t, h, owner, upstream, "0.1.0", "-X=main.authority="+delayed.URL)
	var upgrade string
	if outcome == "uninstalled" {
		upgrade = installReferencePlugin(t, h, owner, upstream, "0.2.0", "-X=main.authority="+delayed.URL)
	}
	path := grantProvider(t, h, owner, digest, nil)
	credentialID := enrollGrant(t, h, owner, path)
	first, second := grantRefresher(t, h), grantRefresher(t, h)
	dueNow(t, h, credentialID)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		first.Pass(t.Context())
	}()
	select {
	case <-refreshing:
	case <-time.After(30 * time.Second):
		t.Fatal("the first refresh did not reach the upstream")
	}
	var terminated bool
	if err := h.Pool.QueryRow(t.Context(), `SELECT pg_terminate_backend(pid, 5000) FROM pg_locks
		WHERE locktype='advisory' AND granted AND database=(SELECT oid FROM pg_database WHERE datname=current_database())`).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("terminating the refresh lock session: %v (terminated %v)", err, terminated)
	}
	inFlight := readGrant(t, h, credentialID)
	if inFlight.attempt == nil || inFlight.refresh == nil || !inFlight.refresh.After(time.Now()) {
		t.Fatalf("the in-flight refresh has no durable attempt and deadline: %+v", inFlight)
	}
	if err := runtime.RequestRefresh(t.Context(), h.Pool, credentialID, 1); err != nil {
		t.Fatal(err)
	}
	if after := readGrant(t, h, credentialID); after.refresh == nil || !after.refresh.Equal(*inFlight.refresh) {
		t.Fatal("a gateway's early refresh request changed the in-flight attempt's deadline")
	}
	pass(t, second)
	if refreshes.Load() != 1 {
		t.Errorf("workers dispatched %d refreshes with the same rotating token", refreshes.Load())
	}
	switch outcome {
	case "abandoned":
		// Time passing beyond both the refresh and storage limits recovers
		// a lost attempt by lapsing it, without spending the token again.
		dueNow(t, h, credentialID)
		if !pass(t, second) {
			t.Fatal("the abandoned refresh was not recovered")
		}
	case "revoked":
		detail := h.want(owner, "GET", path, nil, nil, 200)
		h.want(owner, "POST", path+"/credentials/"+credentialID+"/revoke", nil, withMatch(detail, idem(uuid.NewString())), 200)
	case "uninstalled":
		moveToBuild(t, h, owner, path, upgrade)
		plugin := h.want(owner, "GET", "/api/v1/plugins/"+digest, nil, nil, 200)
		h.want(owner, "DELETE", "/api/v1/plugins/"+digest, nil, etagHeader(plugin), 204)
	}
	resume()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("the first worker did not record the rotated tokens")
	}
	grant := readGrant(t, h, credentialID)
	if grant.attempt != nil || refreshes.Load() != 1 || len(authority.Reused()) != 0 {
		t.Fatal("the attempt was not cleared or its rotating token was reused")
	}
	if outcome != "completed" {
		if grant.generation != 1 || grant.refreshToken != nil || grant.refresh != nil {
			t.Fatalf("a late refresh revived an ended grant: %+v", grant)
		}
		if outcome == "abandoned" && (grant.lapsed == nil || grant.failure == nil || !strings.Contains(*grant.failure, "outcome was lost")) {
			t.Fatalf("an abandoned attempt did not lapse the grant: %+v", grant)
		}
		if pass(t, second) {
			t.Fatal("an ended grant was refreshed again")
		}
		return
	}
	if grant.generation != 2 || grant.lapsed != nil || grant.refreshTokens != 1 {
		t.Fatalf("after losing the lock session the grant is %+v", grant)
	}
	dueNow(t, h, credentialID)
	if !pass(t, second) || readGrant(t, h, credentialID).generation != 3 {
		t.Fatal("the rotated token could not refresh the grant again")
	}
}

func testLostRefreshResponse(t *testing.T, failure string) {
	t.Helper()
	h := newAccessHarness(t)
	owner := h.owner()
	authority := testutil.NewOAuthServer(t)
	var refreshes atomic.Int64
	lost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Form.Get("grant_type") == "refresh_token" {
			refreshes.Add(1)
			response := httptest.NewRecorder()
			authority.Config.Handler.ServeHTTP(response, r)
			switch failure {
			case "follow-up refused":
				w.Write(response.Body.Bytes())
				return
			case "truncated body":
				w.Header().Set("Content-Length", fmt.Sprint(response.Body.Len()+1))
				w.Write(response.Body.Bytes())
				return
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		if failure == "follow-up refused" && refreshes.Load() > 0 && r.URL.Path == "/userinfo" {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":"temporarily_unavailable"}`)
			return
		}
		authority.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(lost.Close)
	digest := installReferencePlugin(t, h, owner, newGrantUpstream(t, authority), "0.1.0", "-X=main.authority="+lost.URL)
	credentialID := enrollGrant(t, h, owner, grantProvider(t, h, owner, digest, nil))
	enrolled := len(authority.Issued())
	dueNow(t, h, credentialID)
	if !pass(t, grantRefresher(t, h)) || refreshes.Load() != 1 || len(authority.Issued()) != enrolled+2 {
		t.Fatal("the upstream did not rotate the token before its response was lost")
	}
	grant := readGrant(t, h, credentialID)
	if grant.generation != 1 || grant.attempt == nil || grant.refresh == nil || time.Until(*grant.refresh) < 90*time.Second {
		t.Errorf("the ambiguous refresh lost its fence: %+v", grant)
	}
	if err := runtime.RequestRefresh(t.Context(), h.Pool, credentialID, 1); err != nil {
		t.Fatal(err)
	}
	if pass(t, grantRefresher(t, h)) || refreshes.Load() != 1 {
		t.Fatal("an early refresh request retried the ambiguous attempt")
	}
	dueNow(t, h, credentialID)
	pass(t, grantRefresher(t, h))
	if refreshes.Load() != 1 {
		t.Fatalf("the spent refresh token was sent upstream %d times", refreshes.Load())
	}
	if grant := readGrant(t, h, credentialID); grant.lapsed == nil || grant.refreshToken != nil || grant.failure == nil || !strings.Contains(*grant.failure, "outcome was lost") {
		t.Fatalf("the lost refresh outcome did not lapse the grant: %+v", grant)
	}
}

// The poll serves the refreshed token, and ends the cooldown apart from
// the poll.

// The old attempt's refusal arrives after the gateway installs a replacement
// token. It must neither refresh that healthy token nor cool its credential.
func TestLateCredentialRefusalDoesNotPenalizeARefreshedGrant(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	authority := testutil.NewOAuthServer(t)
	upstream := newGrantUpstream(t, authority)
	arrived, resume := make(chan struct{}), make(chan struct{})
	var block atomic.Bool
	var release sync.Once
	unblock := func() { release.Do(func() { close(resume) }) }
	defer unblock()
	delayed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if block.Swap(false) {
			close(arrived)
			select {
			case <-resume:
			case <-r.Context().Done():
				return
			}
		}
		upstream.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(delayed.Close)
	digest := installReferencePlugin(t, h, owner, &pluginUpstream{Server: delayed}, "0.1.0", "-X=main.authority="+authority.URL)
	credentialID, key := servingGrant(t, h, owner, grantProvider(t, h, owner, digest, nil))
	refresher := grantRefresher(t, h)
	// Compile and instantiate before the request's deadline starts, including
	// under the race detector. Reading the stored manifest does not load code;
	// call the plugin so only the refresh itself holds the attempt open.
	var manifest abi.Manifest
	if err := refresher.Plugins.Call(t.Context(), digest, plugins.Call{Method: abi.MethodManifest}, &manifest); err != nil {
		t.Fatal(err)
	}
	refreshed := make(chan struct{})
	h.Runtime.GrantRefreshed = func(providerID, credentialID string) {
		h.Gateway.GrantRefreshed(providerID, credentialID)
		close(refreshed)
	}
	block.Store(true)
	type refusal struct {
		status int
		code   string
	}
	status := make(chan refusal, 1)
	go func() {
		code, body, _ := h.gateway("POST", "/v1/chat/completions", key, map[string]any{"model": "reference-account", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
		failure, _ := body["error"].(map[string]any)
		name, _ := failure["code"].(string)
		status <- refusal{code, name}
	}()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the old attempt never reached the upstream")
	}
	authority.RevokeAccessTokens()
	dueNow(t, h, credentialID)
	if !pass(t, refresher) {
		t.Fatal("the grant was not refreshed")
	}
	h.refresh()
	select {
	case <-refreshed:
	case <-time.After(5 * time.Second):
		t.Fatal("the gateway did not install the replacement")
	}
	scheduled := readGrant(t, h, credentialID)
	unblock()
	select {
	case failure := <-status:
		if failure.status != http.StatusBadGateway || failure.code != "upstream_authentication_failed" {
			t.Fatalf("the old attempt did not report the upstream credential refusal: %+v", failure)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the old attempt never finished")
	}
	if code := chat(t, h, key); code != http.StatusOK {
		t.Errorf("the late refusal cooled the replacement: status %d", code)
	}
	if current := readGrant(t, h, credentialID); current.generation != 2 || current.refresh == nil || !current.refresh.Equal(*scheduled.refresh) {
		t.Errorf("the late refusal rescheduled the replacement: before %v, after %v", scheduled.refresh, current.refresh)
	}
}

// Writing a secret waits for the installation row, which the test holds
// until the refresh reached the upstream and is recording what it got.
