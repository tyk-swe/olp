//go:build integration

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
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
	runtime, err := plugins.NewRuntime(t.Context(), plugins.Interpreted, plugins.DefaultLimits, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close(context.Background()) })
	var unconfined *plugins.Unconfined
	if h.UnconfinedDir != "" {
		unconfined = plugins.NewUnconfined(h.UnconfinedDir, plugins.DefaultLimits, log)
	}
	host := plugins.NewHost(runtime, unconfined, h.Pool)
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
	draft := fidelityDraft("reference-account", strings.TrimPrefix(path, "/api/v1/providers/"))
	draft["fidelity"] = map[string]any{"mode": "strict"}
	route := h.want(owner, "POST", "/api/v1/route-drafts", draft, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+route["id"].(string)+"/activate", nil, withMatch(route, idem(uuid.NewString())), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Account", "scopes": []string{"inference"}, "allowed_routes": []string{"reference-account"}}, idem(uuid.NewString()), 201)["secret"].(string)
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

// A worker refreshes a grant ahead of its access token's expiry, and gateways
// serve the new access token after their next poll, without a new release,
// an authority reload or ever reading the refresh token.
func TestWorkersRefreshGrantsAheadOfExpiryAndGatewaysServeTheNewAccessToken(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	authority := testutil.NewOAuthServer(t)
	upstream := newGrantUpstream(t, authority)
	digest := installReferencePlugin(t, h, owner, upstream, "0.1.0", "-X=main.authority="+authority.URL)
	credentialID, key := servingGrant(t, h, owner, grantProvider(t, h, owner, digest, nil))
	enrolled := authority.Issued()

	// An hour's access token refreshes ten minutes before it expires.
	grant := readGrant(t, h, credentialID)
	if grant.generation != 1 || grant.refresh == nil || grant.expires == nil || grant.expires.Sub(*grant.refresh) != 10*time.Minute {
		t.Fatalf("enrolled grant %+v", grant)
	}
	refresher := grantRefresher(t, h)
	if pass(t, refresher) || len(authority.Issued()) != len(enrolled) {
		t.Fatal("a worker refreshed a grant before it was due")
	}

	dueNow(t, h, credentialID)
	var releases, authoritySequence int64
	if err := h.Pool.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM olp.runtime_releases),(SELECT authority_sequence FROM olp.installation)").Scan(&releases, &authoritySequence); err != nil {
		t.Fatal(err)
	}
	if !pass(t, refresher) {
		t.Fatal("the due grant was not refreshed")
	}
	issued := authority.Issued()
	access, refresh := issued[len(issued)-2], issued[len(issued)-1]
	grant = readGrant(t, h, credentialID)
	if len(issued) != len(enrolled)+2 || grant.generation != 2 || grant.failures != 0 || grant.refresh == nil || time.Until(*grant.refresh) < 49*time.Minute {
		t.Fatalf("refreshed grant %+v, issued %d tokens", grant, len(issued))
	}
	stored, err := h.Server.Keys.Read(t.Context(), h.Pool, h.Server.Installation, *grant.refreshToken, secrets.ProviderGrantRefresh)
	if err != nil || string(stored) != refresh {
		t.Fatalf("the rotated refresh token was not kept: %v", err)
	}

	// Gateway code reads only access tokens: with the refresh token made
	// unreadable, a poll serves the new access token.
	if _, err = h.Pool.Exec(t.Context(), "UPDATE olp.secrets SET ciphertext='unreadable' WHERE purpose=$1", secrets.ProviderGrantRefresh); err != nil {
		t.Fatal(err)
	}
	h.refresh()
	served, generation, err := h.Runtime.Secret(t.Context(), h.Runtime.Release(), credentialID)
	if err != nil || generation != 2 || !strings.Contains(string(served), access) || strings.Contains(string(served), refresh) {
		t.Fatalf("the credential source serves %s: %v", served, err)
	}
	before := len(upstream.received())
	if status := chat(t, h, key); status != 200 {
		t.Fatalf("serving the refreshed grant: %d", status)
	}
	if headers := upstream.received()[before:]; len(headers) != 1 || headers[0].Get("Authorization") != "Bearer "+access {
		t.Fatalf("the upstream received %v", headers)
	}
	var releasesAfter, authorityAfter int64
	if err = h.Pool.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM olp.runtime_releases),(SELECT authority_sequence FROM olp.installation)").Scan(&releasesAfter, &authorityAfter); err != nil {
		t.Fatal(err)
	}
	if releasesAfter != releases || authorityAfter != authoritySequence || h.Runtime.Authority().Sequence != authoritySequence {
		t.Fatalf("the refresh published a release (%d, was %d) or advanced authority (%d, was %d)", releasesAfter, releases, authorityAfter, authoritySequence)
	}
}

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

// A lost response can hide a successful token rotation. Even another worker
// must not spend the old refresh token again when the attempt becomes due.
func TestGrantRefreshDoesNotReuseATokenAfterLosingItsResponse(t *testing.T) {
	for _, failure := range []string{"connection closed", "truncated body", "follow-up refused"} {
		t.Run(failure, func(t *testing.T) { testLostRefreshResponse(t, failure) })
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

// A refresh keeps the grant facts that place the provider's requests: the
// refreshed access token serves at the base URL the grant's enrollment named,
// although the authority's refresh doesn't name it again.
func TestARefreshedGrantServesAtItsBaseURL(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	authority := testutil.NewOAuthServer(t)
	upstream := newGrantUpstream(t, authority)
	digest := installReferencePlugin(t, h, owner, upstream, "0.1.0", "-X=main.authority="+authority.URL)
	authority.SignInAs(testutil.OAuthIdentity{Subject: "operator@reference.example", Account: "acct-eu", APIBase: upstream.URL + "/regions/eu/v1"})
	credentialID, key := servingGrant(t, h, owner, grantProvider(t, h, owner, digest, nil))

	dueNow(t, h, credentialID)
	if !pass(t, grantRefresher(t, h)) {
		t.Fatal("the due grant was not refreshed")
	}
	if grant := readGrant(t, h, credentialID); grant.generation != 2 || grant.failures != 0 {
		t.Fatalf("refreshed grant %+v", grant)
	}
	issued := authority.Issued()
	access := issued[len(issued)-2]
	h.refresh()
	before := len(upstream.receivedPaths())
	if status := chat(t, h, key); status != 200 {
		t.Fatalf("serving the refreshed grant: %d", status)
	}
	headers, paths := upstream.received()[before:], upstream.receivedPaths()[before:]
	if len(paths) != 1 || paths[0] != "/regions/eu/v1/chat/completions" || headers[0].Get("Authorization") != "Bearer "+access {
		t.Fatalf("the upstream received %v with %v", paths, headers)
	}
}

// Workers racing to refresh the same grants spend each rotating refresh
// token once: the authority, which refuses a spent refresh token, never sees
// one again.
func TestRacingWorkersSpendEachRefreshTokenOnce(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	authority := testutil.NewOAuthServer(t)
	digest := installReferencePlugin(t, h, owner, newGrantUpstream(t, authority), "0.1.0", "-X=main.authority="+authority.URL)
	second := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Second reference account", "model": vendorModel, "configuration": map[string]any{
		"kind": "plugin", "auth_mode": "grant", "profile_id": "reference-grant-chat", "profile_revision": digest,
	}}, idem(uuid.NewString()), 201)
	credentials := []string{enrollGrant(t, h, owner, grantProvider(t, h, owner, digest, nil)), enrollGrant(t, h, owner, "/api/v1/providers/"+second["id"].(string))}
	workers := []*grants.Refresher{grantRefresher(t, h), grantRefresher(t, h), grantRefresher(t, h)}
	const rounds = 4
	for range rounds {
		for _, credentialID := range credentials {
			dueNow(t, h, credentialID)
		}
		var wg sync.WaitGroup
		for _, worker := range workers {
			wg.Go(func() {
				if outcome, _ := worker.Pass(t.Context()); outcome != usage.OutcomeSuccess {
					t.Errorf("grant refresh pass outcome %v", outcome)
				}
			})
		}
		wg.Wait()
	}
	if reused := authority.Reused(); len(reused) != 0 {
		t.Fatalf("workers spent %d refresh tokens again", len(reused))
	}
	for _, credentialID := range credentials {
		if grant := readGrant(t, h, credentialID); grant.generation != 1+rounds || grant.failures != 0 {
			t.Fatalf("after %d rounds the grant is %+v", rounds, grant)
		}
	}
	if issued := len(authority.Issued()); issued != 2*len(credentials)*(1+rounds) {
		t.Fatalf("the authority issued %d tokens", issued)
	}
}

// When the upstream refuses a grant's access token, the gateway asks for an
// early refresh; once a worker refreshed the grant and the gateway polled, the
// credential slot serves again.
func TestAnUpstreamCredentialFailureRefreshesTheGrantEarly(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	authority := testutil.NewOAuthServer(t)
	upstream := newGrantUpstream(t, authority)
	digest := installReferencePlugin(t, h, owner, upstream, "0.1.0", "-X=main.authority="+authority.URL)
	credentialID, key := servingGrant(t, h, owner, grantProvider(t, h, owner, digest, nil))
	if status := chat(t, h, key); status != 200 {
		t.Fatalf("serving the grant: %d", status)
	}

	authority.RevokeAccessTokens()
	if status := chat(t, h, key); status == 200 {
		t.Fatal("the upstream served a revoked access token")
	}
	deadline := time.Now().Add(10 * time.Second)
	for grant := readGrant(t, h, credentialID); grant.refresh.After(time.Now()); grant = readGrant(t, h, credentialID) {
		if time.Now().After(deadline) {
			t.Fatalf("no early refresh was requested: %+v", grant)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if status := chat(t, h, key); status == 200 {
		t.Fatal("the refused credential version served before its grant was refreshed")
	}
	if !pass(t, grantRefresher(t, h)) {
		t.Fatal("the requested refresh did not run")
	}
	// The poll serves the refreshed token, and ends the cooldown apart from
	// the poll.
	h.refresh()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		status := chat(t, h, key)
		if status == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the refresh, the slot answered %d", status)
		}
	}
}

func TestAnAmbiguousStreamCredentialFailureRefreshesAGrantWithoutExpiry(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	authority := testutil.NewOAuthServer(t)
	authority.IssueFor(0)
	upstream := newGrantUpstream(t, authority)
	healthy := upstream.Server.Config.Handler
	upstream.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := authority.Authorized(r); !ok {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"error\":{\"message\":\"expired token\",\"type\":\"authentication_error\",\"code\":\"invalid_api_key\"}}\n\n")
			return
		}
		healthy.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Server.Close)
	digest := installReferencePlugin(t, h, owner, upstream, "0.1.0", "-X=main.authority="+authority.URL)
	credentialID, key := servingGrant(t, h, owner, grantProvider(t, h, owner, digest, nil))
	if grant := readGrant(t, h, credentialID); grant.expires != nil || grant.refresh != nil {
		t.Fatalf("the grant already has an expiry or scheduled refresh: %+v", grant)
	}
	stream := func() (int, []byte) {
		status, body, _ := h.gatewayRaw("POST", "/v1/chat/completions", key,
			strings.NewReader(`{"model":"reference-account","messages":[{"role":"user","content":"hi"}],"stream":true}`),
			map[string]string{"Content-Type": "application/json"})
		return status, body
	}
	authority.RevokeAccessTokens()
	if status, body := stream(); status != http.StatusBadGateway || !strings.Contains(string(body), "ambiguous_upstream_result") {
		t.Fatalf("the strict stream did not retain its ambiguous outcome: %d %s", status, body)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		grant := readGrant(t, h, credentialID)
		if grant.refresh != nil && !grant.refresh.After(time.Now()) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stream's credential refusal requested no early refresh")
		}
	}
	if !pass(t, grantRefresher(t, h)) || readGrant(t, h, credentialID).generation != 2 {
		t.Fatal("the refused grant was not refreshed")
	}
	h.refresh()
	if status, body := stream(); status != http.StatusOK || !strings.Contains(string(body), "data: [DONE]") {
		t.Fatalf("the refreshed grant did not serve the stream: %d %s", status, body)
	}
}

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

// A grant's refresh reaches its authority over the provider's network path,
// through the provider's proxy. A refresh that fails there is retried with
// backoff, keeping the grant's refresh token.
func TestGrantRefreshTakesTheProviderNetworkPathAndRetriesWithBackoff(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	authority := testutil.NewOAuthServer(t)
	var tunnels atomic.Int64
	var unavailable atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unavailable.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.Method != http.MethodConnect || r.Host != strings.TrimPrefix(authority.URL, "http://") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		tunnels.Add(1)
		target, err := net.Dial("tcp", r.Host)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer target.Close()
		client, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		buffered.Flush()
		go io.Copy(target, buffered)
		io.Copy(client, target)
	}))
	t.Cleanup(proxy.Close)
	digest := installReferencePlugin(t, h, owner, newGrantUpstream(t, authority), "0.1.0", "-X=main.authority="+authority.URL)
	credentialID := enrollGrant(t, h, owner, grantProvider(t, h, owner, digest, map[string]any{"network": map[string]any{"proxy_url": proxy.URL}}))
	refresher := grantRefresher(t, h)

	dueNow(t, h, credentialID)
	enrolled := tunnels.Load()
	if !pass(t, refresher) || tunnels.Load() == enrolled || readGrant(t, h, credentialID).generation != 2 {
		t.Fatalf("the refresh took %d tunnels through the provider's proxy", tunnels.Load()-enrolled)
	}

	unavailable.Store(true)
	dueNow(t, h, credentialID)
	if !pass(t, refresher) {
		t.Fatal("the failed refresh was not recorded")
	}
	grant := readGrant(t, h, credentialID)
	if grant.generation != 2 || grant.failures != 1 || grant.failure == nil || !strings.Contains(*grant.failure, abi.CodeHTTPFailed) ||
		grant.refreshTokens != 1 || grant.refresh == nil || time.Until(*grant.refresh) < 20*time.Second || grant.lapsed != nil || grant.attempt != nil {
		t.Fatalf("after a transient failure the grant is %+v", grant)
	}
	if pass(t, refresher) {
		t.Fatal("a worker retried the refresh before its backoff ended")
	}
	unavailable.Store(false)
	dueNow(t, h, credentialID)
	if !pass(t, refresher) || readGrant(t, h, credentialID).generation != 3 {
		t.Fatal("the unsent refresh did not recover when the proxy became available")
	}
}

// A refresh the upstream refuses for good lapses the grant: its refresh token
// is discarded, why is recorded, and neither workers nor a gateway's
// credential failure refresh it again.
func TestPermanentRefreshFailureLapsesTheGrant(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	authority := testutil.NewOAuthServer(t)
	digest := installReferencePlugin(t, h, owner, newGrantUpstream(t, authority), "0.1.0", "-X=main.authority="+authority.URL)
	credentialID := enrollGrant(t, h, owner, grantProvider(t, h, owner, digest, nil))
	refresher := grantRefresher(t, h)

	authority.RevokeRefreshTokens()
	dueNow(t, h, credentialID)
	if !pass(t, refresher) {
		t.Fatal("the failed refresh was not recorded")
	}
	grant := readGrant(t, h, credentialID)
	if grant.generation != 1 || grant.refreshToken != nil || grant.refresh != nil || grant.lapsed == nil || grant.failures != 1 || grant.failure == nil || !strings.Contains(*grant.failure, "invalid_grant") {
		t.Fatalf("after a permanent failure the grant is %+v", grant)
	}
	var tokens int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.secrets WHERE purpose=$1", secrets.ProviderGrantRefresh).Scan(&tokens); err != nil || tokens != 0 {
		t.Fatalf("%d refresh tokens survived: %v", tokens, err)
	}
	if err := runtime.RequestRefresh(t.Context(), h.Pool, credentialID, 1); err != nil {
		t.Fatal(err)
	}
	if grant = readGrant(t, h, credentialID); grant.refresh != nil || pass(t, refresher) {
		t.Fatalf("a grant that can't be refreshed was scheduled again: %+v", grant)
	}
}

// Revoking a credential version ends its grant: the refresh token is deleted
// at once, and no worker refreshes the grant again.
func TestRevokingACredentialVersionEndsItsGrant(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	authority := testutil.NewOAuthServer(t)
	digest := installReferencePlugin(t, h, owner, newGrantUpstream(t, authority), "0.1.0", "-X=main.authority="+authority.URL)
	path := grantProvider(t, h, owner, digest, nil)
	credentialID := enrollGrant(t, h, owner, path)

	detail := h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/credentials/"+credentialID+"/revoke", nil, withMatch(detail, idem(uuid.NewString())), 200)
	if grant := readGrant(t, h, credentialID); grant.refreshToken != nil || grant.refresh != nil || grant.lapsed != nil {
		t.Fatalf("the revoked version's grant is %+v", grant)
	}
	var tokens int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.secrets WHERE purpose=$1", secrets.ProviderGrantRefresh).Scan(&tokens); err != nil || tokens != 0 {
		t.Fatalf("%d refresh tokens survived the revocation: %v", tokens, err)
	}
	issued := len(authority.Issued())
	dueNow(t, h, credentialID)
	if pass(t, grantRefresher(t, h)) || len(authority.Issued()) != issued {
		t.Fatal("a worker refreshed the grant of a revoked version")
	}
}

// A refresh the upstream answered is kept although the pass that ran it is
// interrupted while recording it: cancelled, as a worker's shutdown does, and
// its database connection lost. Otherwise the upstream's rotated refresh token
// would be lost while the spent one stays stored, and the next refresh would
// lapse the grant.
func TestARefreshIsKeptWhenItsPassIsInterrupted(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	authority := testutil.NewOAuthServer(t)
	digest := installReferencePlugin(t, h, owner, newGrantUpstream(t, authority), "0.1.0", "-X=main.authority="+authority.URL)
	credentialID := enrollGrant(t, h, owner, grantProvider(t, h, owner, digest, nil))
	refresher := grantRefresher(t, h)

	// Writing a secret waits for the installation row, which the test holds
	// until the refresh reached the upstream and is recording what it got.
	held, err := h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback(context.Background())
	if _, err = held.Exec(t.Context(), "SELECT FROM olp.installation WHERE singleton FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	dueNow(t, h, credentialID)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	passed := make(chan struct{})
	go func() {
		defer close(passed)
		refresher.Pass(ctx)
	}()
	var recording int
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(20 * time.Millisecond) {
		err = h.Pool.QueryRow(t.Context(), "SELECT pid FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE '%active_key_version%'").Scan(&recording)
		if err == nil {
			break
		}
		if !errors.Is(err, pgx.ErrNoRows) || time.Now().After(deadline) {
			t.Fatalf("the refresh never recorded what it got: %v", err)
		}
	}
	cancel()
	if _, err = h.Pool.Exec(t.Context(), "SELECT pg_terminate_backend($1)", recording); err != nil {
		t.Fatal(err)
	}
	if err = held.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-passed

	issued := authority.Issued()
	grant := readGrant(t, h, credentialID)
	if grant.generation != 2 || grant.refreshToken == nil {
		t.Fatalf("the interrupted refresh left the grant %+v", grant)
	}
	if stored, err := h.Server.Keys.Read(t.Context(), h.Pool, h.Server.Installation, *grant.refreshToken, secrets.ProviderGrantRefresh); err != nil || string(stored) != issued[len(issued)-1] {
		t.Fatalf("the rotated refresh token was not kept: %v", err)
	}
	dueNow(t, h, credentialID)
	if !pass(t, refresher) || readGrant(t, h, credentialID).generation != 3 || len(authority.Reused()) != 0 {
		t.Fatalf("the next refresh spent a spent refresh token: %v", authority.Reused())
	}
}
