//go:build integration

package integration_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
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
	digest := installGrantPlugin(t, h, owner, upstream, "0.1.0")
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
	draft := strictDraft(fidelityDraft("reference-pool", lapsingID))
	draft["targets"] = append(draft["targets"].([]any), map[string]any{"provider_id": fallbackID, "provider_model": vendorModel, "priority": 1, "weight": 1, "timeout_ms": 5000})
	key := publishRoute(t, h, owner, draft, "Pool")
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
	h.Runtime.CredentialRefused(credentialID, 1)
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
	if served, _, err := h.Runtime.Secret(t.Context(), h.Runtime.Release(), reenrolled["credential_id"].(string)); err != nil || len(served) == 0 {
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
	digest := installGrantPlugin(t, h, owner, newGrantUpstream(t, authority), "0.1.0")
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

// A lapse is delivered once to each enabled rule subscribed to grant lapses,
// as a signed webhook like a budget alert. It reports the provider, the
// credential slot, the credential version and its observed principal, and no
// secret material, even when the slot's grant was re-enrolled before the old
// one lapsed and the active revision alone still binds the lapsed version.
func TestAGrantLapseIsDeliveredAsASignedWebhook(t *testing.T) {
	h := newAccessHarness(t)
	policy := alertPolicy()
	h.Server.Egress = policy
	owner := h.owner()
	hook := newWebhookFixture(t)
	authority := testutil.NewOAuthServer(t)
	digest := installGrantPlugin(t, h, owner, newGrantUpstream(t, authority), "0.1.0")
	path := grantProvider(t, h, owner, digest, nil)
	credentialID := enrollGrant(t, h, owner, path)
	certifyPluginProvider(t, h, owner, path)
	provider := h.want(owner, "GET", path, nil, nil, 200)
	slot := h.want(owner, "GET", path+"/credential-slots", nil, nil, 200)["items"].([]any)[0].(map[string]any)

	destination := func(name, secret string) string {
		t.Helper()
		body := map[string]any{"name": name, "url": hook.URL + "/" + name}
		if secret != "" {
			body["secret"] = secret
		}
		return h.want(owner, "POST", "/api/v1/notifications/destinations", body, idem(uuid.NewString()), 201)["id"].(string)
	}
	rule := func(body map[string]any, status int) map[string]any {
		t.Helper()
		return h.want(owner, "POST", "/api/v1/notifications/rules", body, idem(uuid.NewString()), status)
	}
	signed, plain, muted := destination("signed", "signing-key"), destination("plain", ""), destination("muted", "")
	lapses := rule(map[string]any{"name": "Lapses", "event": "provider.grant.lapsed", "destination_id": signed}, 201)
	if lapses["event"] != "provider.grant.lapsed" || lapses["subject_kind"] != nil || lapses["project_id"] != nil {
		t.Fatalf("lapse rule %v", lapses)
	}
	alsoLapses := rule(map[string]any{"name": "Also lapses", "event": "provider.grant.lapsed", "destination_id": plain}, 201)
	rule(map[string]any{"name": "Muted lapses", "event": "provider.grant.lapsed", "destination_id": muted, "enabled": false}, 201)
	// A destination is subscribed to grant lapses once, installation-wide,
	// and a lapse rule watches no budget.
	rule(map[string]any{"name": "Again", "event": "provider.grant.lapsed", "destination_id": signed}, 409)
	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Lapses"}, idem(uuid.NewString()), 201)["id"].(string)
	for field, value := range map[string]any{"project_id": project, "subject_kind": "api_key", "window_kind": "day", "threshold_percent": 50} {
		body := map[string]any{"name": "Refused", "event": "provider.grant.lapsed", "destination_id": plain, field: value}
		if problem := rule(body, 422); problemCode(t, problem) != "validation_failed" {
			t.Fatalf("a lapse rule with %s: %v", field, problem)
		}
	}

	// The operator re-enrolls the slot's grant, which the draft stages, and
	// the upstream then revokes the grant the active revision serves.
	reenrolled := enrollSlot(t, h, owner, path, slot["id"].(string))["credential_id"].(string)
	authority.RevokeRefreshTokens()
	dueNow(t, h, credentialID)
	if !pass(t, grantRefresher(t, h)) || readGrant(t, h, credentialID).lapsed == nil || readGrant(t, h, reenrolled).lapsed != nil {
		t.Fatal("only the served grant should have lapsed")
	}
	var enqueued map[string]int
	if err := h.Pool.QueryRow(t.Context(), `SELECT jsonb_object_agg(rule_id, n) FROM (SELECT rule_id, count(*) AS n
		FROM olp.notification_deliveries WHERE credential_id=$1 AND status='pending' GROUP BY rule_id) d`, credentialID).Scan(&enqueued); err != nil {
		t.Fatal(err)
	}
	if len(enqueued) != 2 || enqueued[lapses["id"].(string)] != 1 || enqueued[alsoLapses["id"].(string)] != 1 {
		t.Fatalf("the lapse enqueued deliveries %v", enqueued)
	}

	deliveryPass(t, h, policy)
	if hook.count() != 2 {
		t.Fatalf("delivered %d webhooks, want one per enabled lapse rule", hook.count())
	}
	for i := range hook.count() {
		hit := hook.hit(i)
		validateWebhookBody(t, "provider.grant.lapsed", hit.body)
		for _, secret := range append(authority.Issued(), "acct-reference", "invalid_grant") {
			if strings.Contains(string(hit.body), secret) {
				t.Fatalf("the lapse webhook carries %q: %s", secret, hit.body)
			}
		}
		var body map[string]any
		if err := json.Unmarshal(hit.body, &body); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"event": "provider.grant.lapsed", "rule_id": lapses["id"], "rule_name": "Lapses",
			"provider_id": provider["id"], "provider_name": provider["name"], "credential_version_id": credentialID, "credential_version": float64(1),
			"credential_slots": []any{map[string]any{"id": slot["id"], "name": slot["name"]}}, "observed_principal": "operator@reference.example"}
		signature := ""
		if hit.path == "/plain" {
			want["rule_id"], want["rule_name"] = alsoLapses["id"], "Also lapses"
		} else {
			mac := hmac.New(sha256.New, []byte("signing-key"))
			mac.Write(hit.body)
			signature = "sha256=" + hex.EncodeToString(mac.Sum(nil))
		}
		if hit.signature != signature {
			t.Fatalf("the %s webhook is signed %q, want %q", hit.path, hit.signature, signature)
		}
		for field, value := range want {
			if !reflect.DeepEqual(body[field], value) {
				t.Fatalf("the %s webhook reports %s %v, want %v: %s", hit.path, field, body[field], value, hit.body)
			}
		}
	}

	// The deliveries list shows the lapse, and no pass delivers it again.
	deliveries := h.want(owner, "GET", "/api/v1/notifications/deliveries?rule_id="+lapses["id"].(string), nil, nil, 200)["items"].([]any)
	if len(deliveries) != 1 {
		t.Fatalf("deliveries %v", deliveries)
	}
	if delivery := deliveries[0].(map[string]any); delivery["event"] != "provider.grant.lapsed" || delivery["status"] != "delivered" || delivery["attempts"] != float64(1) ||
		delivery["provider_id"] != provider["id"] || delivery["credential_version_id"] != credentialID || delivery["credential_version"] != float64(1) || delivery["window_id"] != nil {
		t.Fatalf("delivery %v", delivery)
	}
	deliveryPass(t, h, policy)
	if hook.count() != 2 {
		t.Fatalf("a delivered lapse was delivered again: %d webhooks", hook.count())
	}
}
