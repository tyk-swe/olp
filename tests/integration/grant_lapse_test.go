//go:build integration

package integration_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/testutil"
)

// A grant whose refresh token the upstream revoked lapses when a worker next
// refreshes it. Within one authority poll, gateways stop serving its
// credential version: planning skips the version's slot as lapsed without
// spending the attempt budget, and traffic fails over to the route's other
// target. The lapse is audited with the worker as the actor, the console's
// API shows it, and only re-enrolling the grant and activating the provider
// restores service.
func TestALapsedGrantFailsOverUntilItIsReenrolled(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	authority := testutil.NewOAuthServer(t)
	upstream := newGrantUpstream(t, authority)
	digest := installReferencePlugin(t, h, owner, upstream, "0.1.0", "-X=main.authority="+authority.URL)
	operator := testutil.OAuthIdentity{Subject: "operator@reference.example", Account: "acct-reference"}
	lapsing := grantProvider(t, h, owner, digest, nil)
	credentialID := enrollGrant(t, h, owner, lapsing)
	certifyPluginProvider(t, h, owner, lapsing)
	// Another account, through another provider, is the route's fallback.
	authority.SignInAs(testutil.OAuthIdentity{Subject: "colleague@reference.example", Account: "acct-colleague"})
	fallback := "/api/v1/providers/" + h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Fallback account", "model": vendorModel, "configuration": map[string]any{
		"kind": "plugin", "auth_mode": "grant", "profile_id": "reference-grant-chat", "profile_revision": digest,
	}}, idem(uuid.NewString()), 201)["id"].(string)
	enrollGrant(t, h, owner, fallback)
	certifyPluginProvider(t, h, owner, fallback)
	authority.SignInAs(operator)

	lapsingID, fallbackID := strings.TrimPrefix(lapsing, "/api/v1/providers/"), strings.TrimPrefix(fallback, "/api/v1/providers/")
	draft := fidelityDraft("reference-pool", lapsingID)
	draft["fidelity"] = map[string]any{"mode": "strict"}
	draft["targets"] = append(draft["targets"].([]any), map[string]any{"provider_id": fallbackID, "provider_model": vendorModel, "priority": 1, "weight": 1, "timeout_ms": 5000})
	route := h.want(owner, "POST", "/api/v1/route-drafts", draft, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+route["id"].(string)+"/activate", nil, withMatch(route, idem(uuid.NewString())), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Pool", "scopes": []string{"inference"}, "allowed_routes": []string{"reference-pool"}}, idem(uuid.NewString()), 201)["secret"].(string)
	h.refresh()
	// servedBy sends a request through the route, one attempt at most, and
	// returns the account whose access token served it.
	servedBy := func() string {
		t.Helper()
		status, _, _ := h.gateway("POST", "/v1/chat/completions", key, map[string]any{"model": "reference-pool", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
		received := upstream.received()
		identity, ok := authority.Authorized(&http.Request{Header: received[len(received)-1]})
		if status != http.StatusOK || !ok {
			t.Fatalf("the route answered %d", status)
		}
		return identity.Subject
	}
	// simulated returns the reason planning gives for each of the route's
	// providers, or "attempt N".
	simulated := func() map[string]string {
		t.Helper()
		request := map[string]any{"model": "reference-pool", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
		decisions := h.list(owner, "POST", "/api/v1/routing/simulate", map[string]any{"operation": map[string]any{"operation": "generation", "request": request}, "surface": "openai", "mode": "unary", "dialect": "openai-chat", "seed": "lapse"}, nil, 200)
		reasons := map[string]string{}
		for _, raw := range decisions {
			decision := raw.(map[string]any)
			if reason, ok := decision["reason"].(string); ok {
				reasons[decision["provider_id"].(string)] = reason
			} else {
				reasons[decision["provider_id"].(string)] = fmt.Sprintf("attempt %v", decision["attempt"])
			}
		}
		return reasons
	}
	if account := servedBy(); account != operator.Subject {
		t.Fatalf("the route's first target served as %s", account)
	}

	// 1. The upstream revokes the refresh token, and the grant lapses at its
	// next refresh.
	authority.RevokeRefreshTokens()
	dueNow(t, h, credentialID)
	if !pass(t, grantRefresher(t, h)) {
		t.Fatal("the failed refresh was not recorded")
	}
	if grant := readGrant(t, h, credentialID); grant.lapsed == nil || grant.refreshToken != nil || grant.refresh != nil {
		t.Fatalf("after the upstream revoked its refresh token the grant is %+v", grant)
	}
	events := h.want(owner, "GET", "/api/v1/audit?action=provider.grant.lapse", nil, nil, 200)["items"].([]any)
	if len(events) != 1 {
		t.Fatalf("audited lapses %v", events)
	}
	if event := events[0].(map[string]any); event["actor_type"] != "system" || event["actor_user_id"] != nil || event["actor_management_token_id"] != nil ||
		event["user_agent_family"] != "olp-worker" || event["resource_type"] != "provider_credential" || event["resource_id"] != credentialID || event["outcome"] != "success" {
		t.Fatalf("audited lapse %v", event)
	}

	// 2. Within one authority poll, the gateway stops serving the lapsed
	// credential version, although its access token still works upstream:
	// planning skips it as lapsed, the one attempt the route allows goes to
	// the fallback, and traffic fails over.
	h.refresh()
	if eligibility := h.Runtime.Eligibility(credentialID); eligibility != runtime.Lapsed {
		t.Fatalf("the gateway finds the lapsed credential version %q", eligibility)
	}
	if reasons := simulated(); reasons[lapsingID] != "credential_lapsed" || reasons[fallbackID] != "attempt 1" {
		t.Fatalf("planned %v", reasons)
	}
	for range 3 {
		if account := servedBy(); account != "colleague@reference.example" {
			t.Fatalf("after the lapse the route served as %s", account)
		}
	}
	// A lapsed grant is never refreshed again, even when asked.
	h.Runtime.CredentialRefused(credentialID)
	if grant := readGrant(t, h, credentialID); grant.refresh != nil {
		t.Fatalf("a refresh of the lapsed grant was requested: %+v", grant)
	}

	// The console shows the lapse on the credential version and its slot,
	// which can't be validated until its grant is re-enrolled.
	for _, raw := range h.want(owner, "GET", lapsing+"/credentials", nil, nil, 200)["items"].([]any) {
		if version := raw.(map[string]any); version["grant"].(map[string]any)["lapsed_at"] == nil {
			t.Fatalf("credential version %v", version)
		}
	}
	slots := h.want(owner, "GET", lapsing+"/credential-slots", nil, nil, 200)
	slot := slots["items"].([]any)[0].(map[string]any)["id"].(string)
	if health := slots["health"].(map[string]any)[slot].(map[string]any); health["lapsed"] != true || health["revoked"] != false {
		t.Fatalf("slot health %v", health)
	}
	if refusal := h.want(owner, "POST", lapsing+"/credential-slots/"+slot+"/validate", nil, nil, 422); problemCode(t, refusal) != "credential_lapsed" {
		t.Fatalf("validated a lapsed grant: %v", refusal)
	}

	// 3. The operator re-enrolls the slot's grant and activates the provider.
	reenrolled := enrollSlot(t, h, owner, lapsing, slot)
	if reenrolled["principal"] != operator.Subject {
		t.Fatalf("re-enrolled %v", reenrolled)
	}
	slots = h.want(owner, "GET", lapsing+"/credential-slots", nil, nil, 200)
	if health := slots["health"].(map[string]any)[slot].(map[string]any); health["lapsed"] != false {
		t.Fatalf("the re-enrolled slot's health %v", health)
	}
	activateEnrolled(t, h, owner, lapsing, 200, slot)

	// 4. Service is restored: the route's first target serves again, with the
	// new grant.
	h.refresh()
	if reasons := simulated(); reasons[lapsingID] != "attempt 1" {
		t.Fatalf("planned %v after re-enrollment", reasons)
	}
	if account := servedBy(); account != operator.Subject {
		t.Fatalf("after re-enrollment the route served as %s", account)
	}
	if served, err := h.Runtime.Secret(t.Context(), h.Runtime.Release(), reenrolled["credential_id"].(string)); err != nil || len(served) == 0 {
		t.Fatalf("the re-enrolled grant is not served: %v", err)
	}
}

// A grant enrolled by device authorization lapses like any other, and
// re-enrolling its lapsed slot by device authorization, polling until the
// operator approves, stages a new credential version that serves once the
// provider is activated.
func TestALapsedSlotIsReenrolledByDeviceAuthorization(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	authority := testutil.NewOAuthServer(t)
	digest := installReferencePlugin(t, h, owner, newGrantUpstream(t, authority), "0.1.0", "-X=main.authority="+authority.URL)
	path := deviceProvider(t, h, owner, digest, "reference-device-chat")
	lapsedID := enrollByDevice(t, h, owner, path, "")["credential_id"].(string)
	certifyPluginProvider(t, h, owner, path)
	slots := h.want(owner, "GET", path+"/credential-slots", nil, nil, 200)
	slot := slots["items"].([]any)[0].(map[string]any)["id"].(string)

	authority.RevokeRefreshTokens()
	dueNow(t, h, lapsedID)
	if !pass(t, grantRefresher(t, h)) {
		t.Fatal("the failed refresh was not recorded")
	}
	if grant := readGrant(t, h, lapsedID); grant.lapsed == nil {
		t.Fatalf("after the upstream revoked its refresh token the grant is %+v", grant)
	}
	slots = h.want(owner, "GET", path+"/credential-slots", nil, nil, 200)
	if health := slots["health"].(map[string]any)[slot].(map[string]any); health["lapsed"] != true {
		t.Fatalf("slot health %v", health)
	}

	reenrolled := enrollByDevice(t, h, owner, path, slot)
	if reenrolled["principal"] != "operator@reference.example" || reenrolled["credential_version"] != float64(2) {
		t.Fatalf("re-enrolled %v", reenrolled)
	}
	slots = h.want(owner, "GET", path+"/credential-slots", nil, nil, 200)
	if health := slots["health"].(map[string]any)[slot].(map[string]any); health["lapsed"] != false {
		t.Fatalf("the re-enrolled slot's health %v", health)
	}
	activateEnrolled(t, h, owner, path, 200, slot)
	h.refresh()
	if lapsed, served := h.Runtime.Eligibility(lapsedID), h.Runtime.Eligibility(reenrolled["credential_id"].(string)); lapsed != runtime.Lapsed || served != runtime.Eligible {
		t.Fatalf("the gateway finds the lapsed version %q and the re-enrolled one %q", lapsed, served)
	}
}
