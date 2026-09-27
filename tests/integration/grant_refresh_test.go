//go:build integration

package integration_test

import (
	"context"
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

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/grants"
	"github.com/tyk-swe/olp/internal/plugins"
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
	expires, refresh *time.Time
	failures         int
	failure          *string
	refreshTokens    int
}

func readGrant(t *testing.T, h *accessHarness, credentialID string) grantState {
	t.Helper()
	var g grantState
	if err := h.Pool.QueryRow(t.Context(), `SELECT generation,refresh_token_id::text,expires_at,refresh_at,refresh_failures,refresh_failure,
		(SELECT count(*) FROM olp.secrets s WHERE s.id=g.refresh_token_id AND s.purpose=$2) FROM olp.provider_grants g WHERE credential_id=$1`, credentialID, grants.RefreshPurpose).
		Scan(&g.generation, &g.refreshToken, &g.expires, &g.refresh, &g.failures, &g.failure, &g.refreshTokens); err != nil {
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
	stored, err := h.Server.Keys.Read(t.Context(), h.Pool, h.Server.Installation, *grant.refreshToken, grants.RefreshPurpose)
	if err != nil || string(stored) != refresh {
		t.Fatalf("the rotated refresh token was not kept: %v", err)
	}

	// Gateway code reads only access tokens: with the refresh token made
	// unreadable, a poll serves the new access token.
	if _, err = h.Pool.Exec(t.Context(), "UPDATE olp.secrets SET ciphertext='unreadable' WHERE purpose=$1", grants.RefreshPurpose); err != nil {
		t.Fatal(err)
	}
	h.refresh()
	served, err := h.Runtime.Secret(t.Context(), h.Runtime.Release(), credentialID)
	if err != nil || !strings.Contains(string(served), access) || strings.Contains(string(served), refresh) {
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
	h.refresh()
	if status := chat(t, h, key); status != 200 {
		t.Fatalf("after the refresh, the slot answered %d", status)
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
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	authority.Close()
	dueNow(t, h, credentialID)
	if !pass(t, refresher) {
		t.Fatal("the failed refresh was not recorded")
	}
	grant := readGrant(t, h, credentialID)
	if grant.generation != 2 || grant.failures != 1 || grant.failure == nil || !strings.Contains(*grant.failure, abi.CodeHTTPFailed) ||
		grant.refreshTokens != 1 || grant.refresh == nil || time.Until(*grant.refresh) < 20*time.Second {
		t.Fatalf("after a transient failure the grant is %+v", grant)
	}
	if pass(t, refresher) {
		t.Fatal("a worker retried the refresh before its backoff ended")
	}
}

// A refresh the upstream refuses for good ends the grant's refresh: its
// refresh token is discarded, why is recorded, and neither workers nor a
// gateway's credential failure refresh it again. The grant is left for lapse.
func TestPermanentRefreshFailureEndsTheGrantsRefresh(t *testing.T) {
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
	if grant.generation != 1 || grant.refreshToken != nil || grant.refresh != nil || grant.failures != 1 || grant.failure == nil || !strings.Contains(*grant.failure, "invalid_grant") {
		t.Fatalf("after a permanent failure the grant is %+v", grant)
	}
	var tokens int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.secrets WHERE purpose=$1", grants.RefreshPurpose).Scan(&tokens); err != nil || tokens != 0 {
		t.Fatalf("%d refresh tokens survived: %v", tokens, err)
	}
	if err := grants.RequestRefresh(t.Context(), h.Pool, credentialID, 1); err != nil {
		t.Fatal(err)
	}
	if grant = readGrant(t, h, credentialID); grant.refresh != nil || pass(t, refresher) {
		t.Fatalf("a grant that can't be refreshed was scheduled again: %+v", grant)
	}
}
